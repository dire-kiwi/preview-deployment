// Package docker implements the small subset of the Docker Engine API used by
// the preview orchestrator. It talks directly to the daemon over its Unix
// socket, keeping the orchestrator image independent of the Docker CLI.
package docker

import (
	"archive/tar"
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	maxErrorBody              = 1024 * 1024
	codexAuthSecretPath       = "/run/secrets/codex-auth.json"
	previewEntrypointPath     = "/app/preview-entrypoint"
	previewEntrypointFilename = "preview-entrypoint"
	previewEntrypoint         = `#!/bin/bash
set -euo pipefail

auth_source=/run/secrets/codex-auth.json
if [[ -e "$auth_source" ]]; then
    export CODEX_HOME="${CODEX_HOME:-${HOME:-/home/preview}/.codex}"
    mkdir -p "$CODEX_HOME"
    temporary="$(mktemp "$CODEX_HOME/.auth.json.preview.XXXXXX")"
    cleanup_auth_copy() {
        rm -f "$temporary"
    }
    trap cleanup_auth_copy EXIT
    cp "$auth_source" "$temporary"
    chmod 0600 "$temporary"
    mv -f "$temporary" "$CODEX_HOME/auth.json"
    trap - EXIT
fi

exec /app/app "$@"
`
)

// APIError is an error response returned by the Docker Engine.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Docker API returned %d: %s", e.StatusCode, e.Message)
}

func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// Client is a Docker Engine API client using a Unix domain socket.
type Client struct {
	httpClient *http.Client
	apiVersion string
}

// New creates a client. Connect must be called before versioned operations.
func New(socketPath string) *Client {
	transport := &http.Transport{
		DisableCompression: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var dialer net.Dialer
			return dialer.DialContext(ctx, "unix", socketPath)
		},
	}
	return &Client{httpClient: &http.Client{Transport: transport}}
}

// Connect verifies daemon access and negotiates the Engine API version.
func (c *Client) Connect(ctx context.Context) error {
	if err := c.Ping(ctx); err != nil {
		return err
	}

	response, err := c.request(ctx, http.MethodGet, "/version", nil, nil, false)
	if err != nil {
		return fmt.Errorf("read Docker version: %w", err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		return fmt.Errorf("read Docker version: %w", err)
	}
	var version struct {
		APIVersion string `json:"ApiVersion"`
	}
	if err := json.NewDecoder(response.Body).Decode(&version); err != nil {
		return fmt.Errorf("decode Docker version: %w", err)
	}
	if !validAPIVersion(version.APIVersion) {
		return fmt.Errorf("Docker returned invalid API version %q", version.APIVersion)
	}
	c.apiVersion = version.APIVersion
	return nil
}

// Ping checks whether the Docker daemon is available.
func (c *Client) Ping(ctx context.Context) error {
	response, err := c.request(ctx, http.MethodGet, "/_ping", nil, nil, false)
	if err != nil {
		return fmt.Errorf("connect to Docker: %w", err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		return fmt.Errorf("ping Docker: %w", err)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 16))
	if err != nil {
		return fmt.Errorf("read Docker ping: %w", err)
	}
	if strings.TrimSpace(string(body)) != "OK" {
		return fmt.Errorf("unexpected Docker ping response %q", string(body))
	}
	return nil
}

// BuildImage builds an image containing app and the generated Dockerfile.
func (c *Client) BuildImage(ctx context.Context, image, runtimeImage, deploymentID string, app []byte) error {
	dockerfile := generatedDockerfile(runtimeImage, deploymentID)

	reader, writer := io.Pipe()
	writeResult := make(chan error, 1)
	go func() {
		writeResult <- writeBuildContext(writer, dockerfile, app)
	}()

	query := url.Values{}
	query.Set("t", image)
	query.Set("dockerfile", "Dockerfile")
	query.Set("rm", "1")
	query.Set("forcerm", "1")
	headers := http.Header{"Content-Type": []string{"application/x-tar"}}
	response, err := c.request(ctx, http.MethodPost, "/build", query, reader, true, headers)
	if err != nil {
		_ = reader.CloseWithError(err)
		<-writeResult
		return fmt.Errorf("build image: %w", err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		_ = reader.CloseWithError(err)
		<-writeResult
		return fmt.Errorf("build image: %w", err)
	}

	decoder := json.NewDecoder(response.Body)
	for {
		var message struct {
			Error       string `json:"error"`
			ErrorDetail struct {
				Message string `json:"message"`
			} `json:"errorDetail"`
		}
		if err := decoder.Decode(&message); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			_ = reader.CloseWithError(err)
			<-writeResult
			return fmt.Errorf("decode Docker build output: %w", err)
		}
		if message.Error != "" || message.ErrorDetail.Message != "" {
			buildMessage := message.ErrorDetail.Message
			if buildMessage == "" {
				buildMessage = message.Error
			}
			_ = reader.CloseWithError(errors.New(buildMessage))
			<-writeResult
			return fmt.Errorf("Docker image build failed: %s", strings.TrimSpace(buildMessage))
		}
	}
	if err := <-writeResult; err != nil {
		return fmt.Errorf("create Docker build context: %w", err)
	}
	return nil
}

func generatedDockerfile(runtimeImage, deploymentID string) string {
	return fmt.Sprintf(`FROM %s
USER 0:0
RUN set -eux; \
    if command -v apt-get >/dev/null 2>&1; then \
        apt-get update; \
        DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends bash ca-certificates; \
        rm -rf /var/lib/apt/lists/*; \
    elif command -v apk >/dev/null 2>&1; then \
        apk add --no-cache bash ca-certificates; \
    elif command -v bash >/dev/null 2>&1 && [ -s /etc/ssl/certs/ca-certificates.crt ]; then \
        :; \
    else \
        echo "runtime image must provide apt-get or apk, or already contain Bash and CA certificates" >&2; \
        exit 1; \
    fi; \
    mkdir -p /run/secrets; \
    chmod 0755 /run/secrets
WORKDIR /app
COPY app /app/app
COPY %s %s
LABEL com.preview-deployment.managed="true" com.preview-deployment.id="%s"
USER 65534:65534
ENTRYPOINT ["%s"]
`, runtimeImage, previewEntrypointFilename, previewEntrypointPath, deploymentID, previewEntrypointPath)
}

func writeBuildContext(pipe *io.PipeWriter, dockerfile string, app []byte) error {
	tarWriter := tar.NewWriter(pipe)
	write := func(name string, mode int64, contents []byte) error {
		header := &tar.Header{
			Name:       name,
			Mode:       mode,
			Size:       int64(len(contents)),
			ModTime:    time.Unix(0, 0),
			AccessTime: time.Unix(0, 0),
			ChangeTime: time.Unix(0, 0),
			Format:     tar.FormatPAX,
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			return err
		}
		_, err := tarWriter.Write(contents)
		return err
	}

	if err := write("Dockerfile", 0o644, []byte(dockerfile)); err != nil {
		_ = pipe.CloseWithError(err)
		return err
	}
	if err := write("app", 0o555, app); err != nil {
		_ = pipe.CloseWithError(err)
		return err
	}
	if err := write(previewEntrypointFilename, 0o555, []byte(previewEntrypoint)); err != nil {
		_ = pipe.CloseWithError(err)
		return err
	}
	if err := tarWriter.Close(); err != nil {
		_ = pipe.CloseWithError(err)
		return err
	}
	return pipe.Close()
}

// CreateOptions defines a sandboxed preview container.
type CreateOptions struct {
	Name          string
	Image         string
	Args          []string
	Env           []string
	Labels        map[string]string
	Port          int
	Network       string
	MemoryBytes   int64
	NanoCPUs      int64
	PIDsLimit     int64
	TmpfsBytes    int64
	RestartPolicy string
	CodexAuthPath string
}

// CreateContainer creates, but does not start, a preview container.
func (c *Client) CreateContainer(ctx context.Context, options CreateOptions) (string, error) {
	port := strconv.Itoa(options.Port) + "/tcp"
	requestBody := struct {
		Image        string              `json:"Image"`
		User         string              `json:"User"`
		WorkingDir   string              `json:"WorkingDir"`
		Cmd          []string            `json:"Cmd,omitempty"`
		Env          []string            `json:"Env,omitempty"`
		Labels       map[string]string   `json:"Labels"`
		ExposedPorts map[string]struct{} `json:"ExposedPorts"`
		HostConfig   struct {
			NetworkMode    string            `json:"NetworkMode"`
			ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
			CapDrop        []string          `json:"CapDrop"`
			SecurityOpt    []string          `json:"SecurityOpt"`
			Memory         int64             `json:"Memory"`
			NanoCPUs       int64             `json:"NanoCpus"`
			PidsLimit      int64             `json:"PidsLimit"`
			Tmpfs          map[string]string `json:"Tmpfs"`
			Mounts         []struct {
				Type     string `json:"Type"`
				Source   string `json:"Source"`
				Target   string `json:"Target"`
				ReadOnly bool   `json:"ReadOnly"`
			} `json:"Mounts,omitempty"`
			RestartPolicy struct {
				Name string `json:"Name"`
			} `json:"RestartPolicy"`
			LogConfig struct {
				Type   string            `json:"Type"`
				Config map[string]string `json:"Config"`
			} `json:"LogConfig"`
		} `json:"HostConfig"`
	}{
		Image:        options.Image,
		User:         "65534:65534",
		WorkingDir:   "/app",
		Cmd:          options.Args,
		Env:          options.Env,
		Labels:       options.Labels,
		ExposedPorts: map[string]struct{}{port: {}},
	}
	requestBody.HostConfig.NetworkMode = options.Network
	requestBody.HostConfig.ReadonlyRootfs = true
	requestBody.HostConfig.CapDrop = []string{"ALL"}
	requestBody.HostConfig.SecurityOpt = []string{"no-new-privileges:true"}
	requestBody.HostConfig.Memory = options.MemoryBytes
	requestBody.HostConfig.NanoCPUs = options.NanoCPUs
	requestBody.HostConfig.PidsLimit = options.PIDsLimit
	requestBody.HostConfig.Tmpfs = map[string]string{
		"/tmp":          fmt.Sprintf("rw,nosuid,nodev,noexec,size=%d", options.TmpfsBytes),
		"/home/preview": fmt.Sprintf("rw,nosuid,nodev,exec,uid=65534,gid=65534,mode=0755,size=%d", options.TmpfsBytes),
	}
	if options.CodexAuthPath != "" {
		requestBody.HostConfig.Mounts = append(requestBody.HostConfig.Mounts, struct {
			Type     string `json:"Type"`
			Source   string `json:"Source"`
			Target   string `json:"Target"`
			ReadOnly bool   `json:"ReadOnly"`
		}{Type: "bind", Source: options.CodexAuthPath, Target: codexAuthSecretPath, ReadOnly: true})
	}
	requestBody.HostConfig.RestartPolicy.Name = options.RestartPolicy
	requestBody.HostConfig.LogConfig.Type = "json-file"
	requestBody.HostConfig.LogConfig.Config = map[string]string{"max-size": "10m", "max-file": "3"}

	body, err := json.Marshal(requestBody)
	if err != nil {
		return "", fmt.Errorf("encode container request: %w", err)
	}
	query := url.Values{"name": []string{options.Name}}
	headers := http.Header{"Content-Type": []string{"application/json"}}
	response, err := c.request(ctx, http.MethodPost, "/containers/create", query, bytes.NewReader(body), true, headers)
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}
	var result struct {
		ID       string   `json:"Id"`
		Warnings []string `json:"Warnings"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode create-container response: %w", err)
	}
	if result.ID == "" {
		return "", fmt.Errorf("Docker created a container without returning its ID")
	}
	return result.ID, nil
}

// ContainerSummary is returned by the Docker list endpoint.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	Created int64             `json:"Created"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
}

// ContainerDetails is the subset of inspection data used by the API.
type ContainerDetails struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	Config  struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status     string `json:"Status"`
		Running    bool   `json:"Running"`
		OOMKilled  bool   `json:"OOMKilled"`
		Dead       bool   `json:"Dead"`
		Pid        int    `json:"Pid"`
		ExitCode   int    `json:"ExitCode"`
		Error      string `json:"Error"`
		StartedAt  string `json:"StartedAt"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
}

// ListContainers lists containers matching every supplied label.
func (c *Client) ListContainers(ctx context.Context, labels map[string]string) ([]ContainerSummary, error) {
	labelFilters := make([]string, 0, len(labels))
	for key, value := range labels {
		labelFilters = append(labelFilters, key+"="+value)
	}
	filters, err := json.Marshal(map[string][]string{"label": labelFilters})
	if err != nil {
		return nil, err
	}
	query := url.Values{"all": []string{"1"}, "filters": []string{string(filters)}}
	response, err := c.request(ctx, http.MethodGet, "/containers/json", query, nil, true)
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}
	var containers []ContainerSummary
	if err := json.NewDecoder(response.Body).Decode(&containers); err != nil {
		return nil, fmt.Errorf("decode container list: %w", err)
	}
	return containers, nil
}

// InspectContainer returns current state for a container.
func (c *Client) InspectContainer(ctx context.Context, id string) (ContainerDetails, error) {
	response, err := c.request(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, true)
	if err != nil {
		return ContainerDetails{}, fmt.Errorf("inspect container: %w", err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		return ContainerDetails{}, fmt.Errorf("inspect container: %w", err)
	}
	var details ContainerDetails
	if err := json.NewDecoder(response.Body).Decode(&details); err != nil {
		return ContainerDetails{}, fmt.Errorf("decode container inspection: %w", err)
	}
	return details, nil
}

func (c *Client) StartContainer(ctx context.Context, id string) error {
	response, err := c.request(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, true)
	if err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return nil
	}
	if err := checkResponse(response); err != nil {
		return fmt.Errorf("start container: %w", err)
	}
	return nil
}

func (c *Client) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	query := url.Values{"t": []string{strconv.Itoa(int(timeout.Seconds()))}}
	response, err := c.request(ctx, http.MethodPost, "/containers/"+id+"/stop", query, nil, true)
	if err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return nil
	}
	if err := checkResponse(response); err != nil {
		return fmt.Errorf("stop container: %w", err)
	}
	return nil
}

func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	query := url.Values{"force": []string{"1"}, "v": []string{"1"}}
	response, err := c.request(ctx, http.MethodDelete, "/containers/"+id, query, nil, true)
	if err != nil {
		return fmt.Errorf("remove container: %w", err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		return fmt.Errorf("remove container: %w", err)
	}
	return nil
}

func (c *Client) RemoveImage(ctx context.Context, image string) error {
	query := url.Values{"force": []string{"1"}, "noprune": []string{"0"}}
	response, err := c.request(ctx, http.MethodDelete, "/images/"+image, query, nil, true)
	if err != nil {
		return fmt.Errorf("remove image: %w", err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		return fmt.Errorf("remove image: %w", err)
	}
	return nil
}

// ContainerLogs returns a bounded, combined stdout/stderr log stream.
func (c *Client) ContainerLogs(ctx context.Context, id string, tail int, maxBytes int64) ([]byte, bool, error) {
	query := url.Values{
		"stdout":     []string{"1"},
		"stderr":     []string{"1"},
		"timestamps": []string{"1"},
		"tail":       []string{strconv.Itoa(tail)},
	}
	response, err := c.request(ctx, http.MethodGet, "/containers/"+id+"/logs", query, nil, true)
	if err != nil {
		return nil, false, fmt.Errorf("read container logs: %w", err)
	}
	defer response.Body.Close()
	if err := checkResponse(response); err != nil {
		return nil, false, fmt.Errorf("read container logs: %w", err)
	}

	// Containers created by this client always have TTY disabled, so Docker
	// returns its 8-byte multiplexed stdout/stderr framing. Engine versions do
	// not consistently advertise that framing with the same Content-Type.
	return decodeLogStream(response.Body, maxBytes)
}

func decodeLogStream(reader io.Reader, maxBytes int64) ([]byte, bool, error) {
	buffered := bufio.NewReader(reader)
	var output bytes.Buffer
	truncated := false
	for {
		header := make([]byte, 8)
		if _, err := io.ReadFull(buffered, header); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, false, fmt.Errorf("decode Docker log header: %w", err)
		}
		frameSize := int64(binary.BigEndian.Uint32(header[4:8]))
		remaining := maxBytes - int64(output.Len())
		if remaining > 0 {
			toCopy := min(frameSize, remaining)
			if _, err := io.CopyN(&output, buffered, toCopy); err != nil {
				return nil, false, fmt.Errorf("decode Docker log frame: %w", err)
			}
			frameSize -= toCopy
		}
		if frameSize > 0 {
			truncated = true
			if _, err := io.CopyN(io.Discard, buffered, frameSize); err != nil {
				return nil, false, fmt.Errorf("discard Docker log frame: %w", err)
			}
		}
	}
	return output.Bytes(), truncated, nil
}

func (c *Client) request(ctx context.Context, method, endpoint string, query url.Values, body io.Reader, versioned bool, extraHeaders ...http.Header) (*http.Response, error) {
	pathPrefix := ""
	if versioned {
		if c.apiVersion == "" {
			return nil, fmt.Errorf("Docker client is not connected")
		}
		pathPrefix = "/v" + c.apiVersion
	}
	requestURL := &url.URL{Scheme: "http", Host: "docker", Path: pathPrefix + endpoint}
	if query != nil {
		requestURL.RawQuery = query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, requestURL.String(), body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("User-Agent", "preview-orchestrator/1")
	for _, headers := range extraHeaders {
		for key, values := range headers {
			for _, value := range values {
				request.Header.Add(key, value)
			}
		}
	}
	return c.httpClient.Do(request)
}

func checkResponse(response *http.Response) error {
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	contents, _ := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	var body struct {
		Message string `json:"message"`
	}
	message := strings.TrimSpace(string(contents))
	if json.Unmarshal(contents, &body) == nil && body.Message != "" {
		message = body.Message
	}
	if message == "" {
		message = http.StatusText(response.StatusCode)
	}
	return &APIError{StatusCode: response.StatusCode, Message: message}
}

func validAPIVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}
