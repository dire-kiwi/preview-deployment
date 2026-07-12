package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestBearerAuthentication(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(nil, nil, logger, 1024, 1024, "test-secret").Handler()

	tests := []struct {
		name          string
		authorization []string
	}{
		{name: "missing"},
		{name: "wrong token", authorization: []string{"Bearer wrong-secret"}},
		{name: "wrong scheme", authorization: []string{"Basic test-secret"}},
		{name: "wrong capitalization", authorization: []string{"bearer test-secret"}},
		{name: "extra whitespace", authorization: []string{"Bearer  test-secret"}},
		{name: "multiple headers", authorization: []string{"Bearer test-secret", "Bearer test-secret"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/v1/not-a-route", nil)
			request.Header["Authorization"] = test.authorization
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
			if got := response.Header().Get("WWW-Authenticate"); got != `Bearer realm="preview-deployment"` {
				t.Fatalf("WWW-Authenticate = %q", got)
			}
			var body struct {
				Error struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if body.Error.Code != "unauthorized" || body.Error.Message != "missing or invalid bearer token" {
				t.Fatalf("error = %#v", body.Error)
			}
		})
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/not-a-route", nil)
	request.Header.Set("Authorization", "Bearer test-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("valid token status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestAuthenticationDisabledForEmptyToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(nil, nil, logger, 1024, 1024, "").Handler()
	request := httptest.NewRequest(http.MethodGet, "/v1/not-a-route", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestHealthEndpointDoesNotRequireAuthentication(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler := New(nil, nil, logger, 1024, 1024, "test-secret").Handler()
	request := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
}

func TestReceiveArchiveMultipart(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("archive", "deployment.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("zip contents")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/deployments", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	response := httptest.NewRecorder()
	filename, err := receiveArchive(response, request, 1024)
	if err != nil {
		t.Fatalf("receiveArchive() error = %v", err)
	}
	defer os.Remove(filename)
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if string(contents) != "zip contents" {
		t.Fatalf("contents = %q", contents)
	}
}

func TestReceiveArchiveRawZIP(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/deployments", bytes.NewBufferString("raw zip"))
	request.Header.Set("Content-Type", "application/zip")
	filename, err := receiveArchive(httptest.NewRecorder(), request, 1024)
	if err != nil {
		t.Fatalf("receiveArchive() error = %v", err)
	}
	defer os.Remove(filename)
}

func TestReceiveArchiveEnforcesLimit(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/deployments", bytes.NewReader(make([]byte, 11)))
	request.Header.Set("Content-Type", "application/zip")
	_, err := receiveArchive(httptest.NewRecorder(), request, 10)
	if !errors.Is(err, errUploadTooLarge) {
		t.Fatalf("receiveArchive() error = %v, want errUploadTooLarge", err)
	}
}

func TestReceiveArchiveRequiresArchivePart(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("other", "value"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/deployments", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	_, err := receiveArchive(httptest.NewRecorder(), request, 1024)
	if !errors.Is(err, errArchivePartMissing) {
		t.Fatalf("receiveArchive() error = %v, want missing archive", err)
	}
}
