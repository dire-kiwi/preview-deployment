// Package api exposes the orchestrator over HTTP.
package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/bundle"
	"github.com/dire-kiwi/preview-deployment/internal/docker"
	"github.com/dire-kiwi/preview-deployment/internal/orchestrator"
)

var (
	errUploadTooLarge       = errors.New("upload is too large")
	errArchivePartMissing   = errors.New("multipart request must contain one archive file field")
	errArchivePartDuplicate = errors.New("multipart request contains more than one archive field")
	errUnsupportedMediaType = errors.New("unsupported media type")
	errTemporaryStorage     = errors.New("temporary upload storage failed")
)

type API struct {
	service        *orchestrator.Service
	docker         *docker.Client
	logger         *slog.Logger
	maxUploadBytes int64
	maxBinaryBytes int64
	authEnabled    bool
	authHeaderHash [sha256.Size]byte

	dashboardControlsEnabled bool
	dashboardOrigin          string
	dashboardAuthHeaderHash  [sha256.Size]byte
	dashboardCSRFKey         [sha256.Size]byte
	hibernateDeployment      func(context.Context, string) (orchestrator.Deployment, error)
}

// Option customizes optional API surfaces.
type Option func(*API)

// WithDashboardControls enables the authenticated dashboard and its manual
// hibernation action. Configuration validation is performed before this option
// is passed to the API.
func WithDashboardControls(token, origin string) Option {
	return func(a *API) {
		if token == "" {
			return
		}
		a.dashboardControlsEnabled = true
		a.dashboardOrigin = origin
		credentials := base64.StdEncoding.EncodeToString([]byte("preview:" + token))
		a.dashboardAuthHeaderHash = sha256.Sum256([]byte("Basic " + credentials))
		a.dashboardCSRFKey = sha256.Sum256([]byte("preview-deployment dashboard csrf\x00" + token))
	}
}

func New(service *orchestrator.Service, dockerClient *docker.Client, logger *slog.Logger, maxUploadBytes, maxBinaryBytes int64, apiToken string, options ...Option) *API {
	a := &API{
		service:        service,
		docker:         dockerClient,
		logger:         logger,
		maxUploadBytes: maxUploadBytes,
		maxBinaryBytes: maxBinaryBytes,
	}
	if service != nil {
		a.hibernateDeployment = service.Hibernate
	}
	if apiToken != "" {
		a.authEnabled = true
		a.authHeaderHash = sha256.Sum256([]byte("Bearer " + apiToken))
	}
	for _, option := range options {
		option(a)
	}
	return a
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", a.dashboard)
	mux.HandleFunc("POST /dashboard/hibernate", a.hibernateFromDashboard)
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /internal/previews/{id}/activity", a.previewActivity)
	mux.HandleFunc("GET /v1/deployments", a.listDeployments)
	mux.HandleFunc("POST /v1/deployments", a.createDeployment)
	mux.HandleFunc("GET /v1/deployments/{id}", a.getDeployment)
	mux.HandleFunc("DELETE /v1/deployments/{id}", a.deleteDeployment)
	mux.HandleFunc("POST /v1/deployments/{id}/start", a.startDeployment)
	mux.HandleFunc("POST /v1/deployments/{id}/stop", a.stopDeployment)
	mux.HandleFunc("GET /v1/deployments/{id}/logs", a.deploymentLogs)
	return a.recover(a.logRequests(a.authenticate(mux)))
}

func (a *API) previewActivity(w http.ResponseWriter, r *http.Request) {
	result, err := a.service.ObservePreviewRequest(r.Context(), r.PathValue("id"), r.URL.Query().Get("token"))
	if errors.Is(err, orchestrator.ErrNotFound) {
		http.Error(w, "preview not found", http.StatusNotFound)
		return
	}
	if err != nil {
		a.logger.Warn("could not resume preview after request", "deployment_id", r.PathValue("id"), "error", err)
		writeResumePage(w, time.Second)
		return
	}
	if result.Ready {
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeResumePage(w, result.RetryAfter)
}

func writeResumePage(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := int((retryAfter + time.Second - 1) / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = io.WriteString(w, `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <meta http-equiv="refresh" content="2">
  <title>Resuming preview…</title>
  <style>
    :root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    * { box-sizing: border-box; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #0b1020; color: #eef2ff; }
    main { width: min(32rem, calc(100% - 2rem)); padding: 2.5rem; text-align: center; border: 1px solid #26304d; border-radius: 1.25rem; background: #121a30; box-shadow: 0 1.5rem 4rem #02061780; }
    .spinner { width: 2.75rem; height: 2.75rem; margin: 0 auto 1.5rem; border: .3rem solid #334155; border-top-color: #818cf8; border-radius: 999px; animation: spin .9s linear infinite; }
    h1 { margin: 0 0 .75rem; font-size: clamp(1.5rem, 5vw, 2rem); letter-spacing: -.025em; }
    p { margin: 0; color: #a5b4cf; line-height: 1.6; }
    @keyframes spin { to { transform: rotate(360deg); } }
    @media (prefers-reduced-motion: reduce) { .spinner { animation: none; border-top-color: #334155; } }
  </style>
</head>
<body>
  <main aria-live="polite">
    <div class="spinner" aria-hidden="true"></div>
    <h1>Resuming this preview…</h1>
    <p>It was paused while idle to save resources. This page will retry automatically.</p>
  </main>
</body>
</html>`)
}

func (a *API) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.dashboardControlsEnabled && (r.URL.Path == "/" || r.URL.Path == "/dashboard/hibernate") {
			authorization := r.Header.Values("Authorization")
			authorized := 0
			if len(authorization) == 1 {
				providedHash := sha256.Sum256([]byte(authorization[0]))
				authorized = subtle.ConstantTimeCompare(providedHash[:], a.dashboardAuthHeaderHash[:])
			}
			if authorized != 1 {
				setDashboardHeaders(w.Header())
				w.Header().Set("WWW-Authenticate", `Basic realm="preview-deployment dashboard", charset="UTF-8"`)
				http.Error(w, "Dashboard authentication required", http.StatusUnauthorized)
				return
			}
		}

		if a.authEnabled && (r.URL.Path == "/v1" || strings.HasPrefix(r.URL.Path, "/v1/")) {
			authorization := r.Header.Values("Authorization")
			authorized := 0
			if len(authorization) == 1 {
				providedHash := sha256.Sum256([]byte(authorization[0]))
				authorized = subtle.ConstantTimeCompare(providedHash[:], a.authHeaderHash[:])
			}
			if authorized != 1 {
				w.Header().Set("WWW-Authenticate", `Bearer realm="preview-deployment"`)
				writeAPIError(w, http.StatusUnauthorized, "unauthorized", "missing or invalid bearer token")
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := a.docker.Ping(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unhealthy", "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (a *API) createDeployment(w http.ResponseWriter, r *http.Request) {
	filename, err := receiveArchive(w, r, a.maxUploadBytes)
	if err != nil {
		switch {
		case errors.Is(err, errUploadTooLarge):
			writeAPIError(w, http.StatusRequestEntityTooLarge, "upload_too_large", fmt.Sprintf("archive must not exceed %d bytes", a.maxUploadBytes))
		case errors.Is(err, errArchivePartMissing), errors.Is(err, errArchivePartDuplicate):
			writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		case errors.Is(err, errUnsupportedMediaType):
			writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", err.Error())
		case errors.Is(err, errTemporaryStorage):
			a.logger.Error("could not store deployment upload", "error", err)
			writeAPIError(w, http.StatusInternalServerError, "upload_storage_failed", "could not store upload")
		default:
			writeAPIError(w, http.StatusBadRequest, "invalid_request", err.Error())
		}
		return
	}
	defer os.Remove(filename)

	deploymentBundle, err := bundle.Open(filename, a.maxBinaryBytes)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid_archive", err.Error())
		return
	}
	deployment, err := a.service.Deploy(r.Context(), deploymentBundle)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, deployment)
}

func (a *API) listDeployments(w http.ResponseWriter, r *http.Request) {
	deployments, err := a.service.List(r.Context())
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"deployments": deployments,
		"count":       len(deployments),
	})
}

func (a *API) getDeployment(w http.ResponseWriter, r *http.Request) {
	deployment, err := a.service.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deployment)
}

func (a *API) startDeployment(w http.ResponseWriter, r *http.Request) {
	deployment, err := a.service.Start(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deployment)
}

func (a *API) stopDeployment(w http.ResponseWriter, r *http.Request) {
	deployment, err := a.service.Stop(r.Context(), r.PathValue("id"))
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, deployment)
}

func (a *API) deleteDeployment(w http.ResponseWriter, r *http.Request) {
	if err := a.service.Delete(r.Context(), r.PathValue("id")); err != nil {
		a.writeServiceError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) deploymentLogs(w http.ResponseWriter, r *http.Request) {
	tail := 200
	if rawTail := r.URL.Query().Get("tail"); rawTail != "" {
		parsed, err := strconv.Atoi(rawTail)
		if err != nil || parsed < 1 || parsed > 5000 {
			writeAPIError(w, http.StatusBadRequest, "invalid_tail", "tail must be an integer between 1 and 5000")
			return
		}
		tail = parsed
	}
	logs, truncated, err := a.service.Logs(r.Context(), r.PathValue("id"), tail)
	if err != nil {
		a.writeServiceError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Logs-Truncated", strconv.FormatBool(truncated))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(logs)
	if truncated {
		_, _ = io.WriteString(w, "\n[logs truncated by orchestrator]\n")
	}
}

func (a *API) writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orchestrator.ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "deployment not found")
	case errors.Is(err, orchestrator.ErrCapacity):
		writeAPIError(w, http.StatusConflict, "capacity_reached", err.Error())
	case errors.Is(err, context.DeadlineExceeded):
		writeAPIError(w, http.StatusGatewayTimeout, "operation_timed_out", "orchestrator operation timed out")
	case errors.Is(err, context.Canceled):
		writeAPIError(w, 499, "request_canceled", "request was canceled")
	default:
		a.logger.Error("orchestrator operation failed", "error", err)
		writeAPIError(w, http.StatusBadGateway, "docker_error", err.Error())
	}
}

func receiveArchive(w http.ResponseWriter, r *http.Request, maxBytes int64) (string, error) {
	// Leave room for multipart headers while independently enforcing the exact
	// archive-byte limit during the copy.
	r.Body = http.MaxBytesReader(w, r.Body, maxBytes+1024*1024)
	mediaType, parameters, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		return "", fmt.Errorf("%w: Content-Type must be multipart/form-data or application/zip", errUnsupportedMediaType)
	}

	temporary, err := os.CreateTemp("", "preview-upload-*.zip")
	if err != nil {
		return "", fmt.Errorf("%w: create file: %v", errTemporaryStorage, err)
	}
	filename := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(filename)
		}
	}()

	switch mediaType {
	case "application/zip", "application/octet-stream":
		if err := copyBounded(temporary, r.Body, maxBytes); err != nil {
			return "", err
		}
	case "multipart/form-data":
		boundary := parameters["boundary"]
		if boundary == "" {
			return "", fmt.Errorf("%w: multipart boundary is missing", errUnsupportedMediaType)
		}
		reader := multipart.NewReader(r.Body, boundary)
		found := false
		for {
			part, partErr := reader.NextPart()
			if errors.Is(partErr, io.EOF) {
				break
			}
			if partErr != nil {
				if isTooLarge(partErr) {
					return "", errUploadTooLarge
				}
				return "", fmt.Errorf("read multipart upload: %w", partErr)
			}
			if part.FormName() != "archive" {
				_, _ = io.Copy(io.Discard, io.LimitReader(part, 1024*1024))
				_ = part.Close()
				continue
			}
			if found {
				_ = part.Close()
				return "", errArchivePartDuplicate
			}
			found = true
			if err := copyBounded(temporary, part, maxBytes); err != nil {
				_ = part.Close()
				return "", err
			}
			_ = part.Close()
		}
		if !found {
			return "", errArchivePartMissing
		}
	default:
		return "", fmt.Errorf("%w %q; use multipart/form-data or application/zip", errUnsupportedMediaType, mediaType)
	}

	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("%w: save file: %v", errTemporaryStorage, err)
	}
	keep = true
	return filename, nil
}

func copyBounded(destination io.Writer, source io.Reader, maxBytes int64) error {
	written, err := io.Copy(destination, io.LimitReader(source, maxBytes+1))
	if err != nil {
		if isTooLarge(err) {
			return errUploadTooLarge
		}
		return fmt.Errorf("read upload: %w", err)
	}
	if written > maxBytes {
		return errUploadTooLarge
	}
	return nil
}

func isTooLarge(err error) bool {
	var maxBytesError *http.MaxBytesError
	return errors.As(err, &maxBytesError)
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]string{
			"code":    code,
			"message": message,
		},
	})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

type responseRecorder struct {
	http.ResponseWriter
	status int
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status != 0 {
		return
	}
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *responseRecorder) Write(contents []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(contents)
}

func (a *API) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		recorder := &responseRecorder{ResponseWriter: w}
		next.ServeHTTP(recorder, r)
		status := recorder.status
		if status == 0 {
			status = http.StatusOK
		}
		if !strings.HasPrefix(r.URL.Path, "/internal/previews/") {
			a.logger.Info("http request",
				"method", r.Method,
				"path", r.URL.Path,
				"status", status,
				"duration_ms", time.Since(started).Milliseconds(),
			)
		}
	})
}

func (a *API) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				a.logger.Error("panic in HTTP handler", "panic", recovered, "method", r.Method, "path", r.URL.Path)
				writeAPIError(w, http.StatusInternalServerError, "internal_error", "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
