package api

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/config"
	"github.com/dire-kiwi/preview-deployment/internal/docker"
	"github.com/dire-kiwi/preview-deployment/internal/orchestrator"
)

func TestResumePageIsRetryableAndNotCached(t *testing.T) {
	response := httptest.NewRecorder()

	writeResumePage(response, 1500*time.Millisecond)

	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusServiceUnavailable)
	}
	if got := response.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
	if got := response.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
	if got := response.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	body := response.Body.String()
	if !strings.Contains(body, "Resuming this preview") || !strings.Contains(body, "http-equiv=\"refresh\"") {
		t.Fatalf("resume body does not contain message and automatic refresh: %s", body)
	}
}

func TestInternalActivityRejectsMissingControlToken(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	payloadDir := t.TempDir()
	if err := os.Chmod(payloadDir, 0o700); err != nil {
		t.Fatal(err)
	}
	service, err := orchestrator.New(docker.New(filepath.Join(t.TempDir(), "unused-docker.sock")), config.Config{
		PayloadDir:       payloadDir,
		BuildConcurrency: 1,
	}, logger)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(service, nil, logger, 1024, 1024, "api-secret").Handler()
	request := httptest.NewRequest(http.MethodGet, "/internal/previews/abc123abc123/activity", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}
