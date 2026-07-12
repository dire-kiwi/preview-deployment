package previewcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"
)

const maxResponseBytes = 16 << 20

// Deployment is the API representation returned by the orchestrator.
type Deployment struct {
	ID             string     `json:"id"`
	Name           string     `json:"name,omitempty"`
	URL            string     `json:"url"`
	Status         string     `json:"status"`
	StatusDetail   string     `json:"status_detail,omitempty"`
	ContainerID    string     `json:"container_id"`
	Image          string     `json:"image"`
	Port           int        `json:"port"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	OOMKilled      bool       `json:"oom_killed,omitempty"`
	ContainerError string     `json:"container_error,omitempty"`
}

type deploymentList struct {
	Deployments []Deployment `json:"deployments"`
	Count       int          `json:"count"`
}

// APIError retains the HTTP status and structured error code returned by the
// orchestrator. Message also works for plain-text proxy and router errors.
type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("API request failed (%d %s): %s", e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("API request failed (%d): %s", e.StatusCode, e.Message)
}

type apiErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// Client calls one preview-deployment API endpoint.
type Client struct {
	baseURL   *url.URL
	token     string
	userAgent string
	http      *http.Client
}

func NewClient(rawURL, token, userAgent string, timeout time.Duration) (*Client, error) {
	if rawURL == "" {
		return nil, errors.New("API URL must not be empty")
	}
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse API URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("API URL must use http or https")
	}
	if parsed.Host == "" {
		return nil, errors.New("API URL must include a host")
	}
	if parsed.User != nil {
		return nil, errors.New("API URL must not include embedded credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("API URL must not include a query or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	if timeout <= 0 {
		return nil, errors.New("timeout must be greater than zero")
	}
	return &Client{
		baseURL:   parsed,
		token:     token,
		userAgent: userAgent,
		http: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

func (c *Client) Deploy(ctx context.Context, archivePath string) (Deployment, error) {
	archive, err := os.Open(archivePath)
	if err != nil {
		return Deployment{}, fmt.Errorf("open archive: %w", err)
	}
	defer archive.Close()
	info, err := archive.Stat()
	if err != nil {
		return Deployment{}, fmt.Errorf("inspect archive: %w", err)
	}
	if !info.Mode().IsRegular() {
		return Deployment{}, errors.New("deployment archive must be a regular file")
	}

	var deployment Deployment
	err = c.do(ctx, http.MethodPost, "/v1/deployments", archive, info.Size(), "application/zip", http.StatusCreated, &deployment)
	return deployment, err
}

func (c *Client) List(ctx context.Context) ([]Deployment, error) {
	var response deploymentList
	if err := c.do(ctx, http.MethodGet, "/v1/deployments", nil, 0, "", http.StatusOK, &response); err != nil {
		return nil, err
	}
	return response.Deployments, nil
}

func (c *Client) Get(ctx context.Context, id string) (Deployment, error) {
	return c.deploymentOperation(ctx, http.MethodGet, id, "", http.StatusOK)
}

func (c *Client) Start(ctx context.Context, id string) (Deployment, error) {
	return c.deploymentOperation(ctx, http.MethodPost, id, "start", http.StatusOK)
}

func (c *Client) Stop(ctx context.Context, id string) (Deployment, error) {
	return c.deploymentOperation(ctx, http.MethodPost, id, "stop", http.StatusOK)
}

func (c *Client) Delete(ctx context.Context, id string) error {
	requestPath := "/v1/deployments/" + url.PathEscape(id)
	return c.do(ctx, http.MethodDelete, requestPath, nil, 0, "", http.StatusNoContent, nil)
}

func (c *Client) Logs(ctx context.Context, id string, tail int) ([]byte, bool, error) {
	requestPath := fmt.Sprintf("/v1/deployments/%s/logs?tail=%d", url.PathEscape(id), tail)
	request, err := c.newRequest(ctx, http.MethodGet, requestPath, nil)
	if err != nil {
		return nil, false, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, false, fmt.Errorf("request deployment logs: %w", err)
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body, maxResponseBytes)
	if err != nil {
		return nil, false, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, false, decodeAPIError(response.StatusCode, body)
	}
	return body, strings.EqualFold(response.Header.Get("X-Logs-Truncated"), "true"), nil
}

func (c *Client) Health(ctx context.Context) (map[string]any, error) {
	var response map[string]any
	if err := c.do(ctx, http.MethodGet, "/healthz", nil, 0, "", http.StatusOK, &response); err != nil {
		return nil, err
	}
	return response, nil
}

func (c *Client) deploymentOperation(ctx context.Context, method, id, operation string, status int) (Deployment, error) {
	requestPath := "/v1/deployments/" + url.PathEscape(id)
	if operation != "" {
		requestPath += "/" + operation
	}
	var deployment Deployment
	err := c.do(ctx, method, requestPath, nil, 0, "", status, &deployment)
	return deployment, err
}

func (c *Client) do(ctx context.Context, method, requestPath string, body io.Reader, contentLength int64, contentType string, expectedStatus int, destination any) error {
	request, err := c.newRequest(ctx, method, requestPath, body)
	if err != nil {
		return err
	}
	request.ContentLength = contentLength
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	request.Header.Set("Accept", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, request.URL.Redacted(), err)
	}
	defer response.Body.Close()
	bodyBytes, err := readLimited(response.Body, maxResponseBytes)
	if err != nil {
		return err
	}
	if response.StatusCode != expectedStatus {
		return decodeAPIError(response.StatusCode, bodyBytes)
	}
	if destination == nil || response.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.Unmarshal(bodyBytes, destination); err != nil {
		return fmt.Errorf("decode API response: %w", err)
	}
	return nil
}

func (c *Client) newRequest(ctx context.Context, method, requestPath string, body io.Reader) (*http.Request, error) {
	target := *c.baseURL
	target.Path = path.Join(c.baseURL.Path+"/", strings.TrimPrefix(strings.SplitN(requestPath, "?", 2)[0], "/"))
	if strings.Contains(requestPath, "?") {
		query := strings.SplitN(requestPath, "?", 2)[1]
		target.RawQuery = query
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), body)
	if err != nil {
		return nil, fmt.Errorf("create API request: %w", err)
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.userAgent != "" {
		request.Header.Set("User-Agent", c.userAgent)
	}
	return request, nil
}

func readLimited(reader io.Reader, maximum int64) ([]byte, error) {
	limited := io.LimitReader(reader, maximum+1)
	contents, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read API response: %w", err)
	}
	if int64(len(contents)) > maximum {
		return nil, fmt.Errorf("response exceeds %d bytes", maximum)
	}
	return contents, nil
}

func decodeAPIError(status int, body []byte) error {
	var envelope apiErrorEnvelope
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Message != "" {
		return &APIError{StatusCode: status, Code: envelope.Error.Code, Message: envelope.Error.Message}
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(status)
	}
	return &APIError{StatusCode: status, Message: message}
}
