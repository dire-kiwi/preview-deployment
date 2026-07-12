package api

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/orchestrator"
)

func TestDashboardTemplateEscapesAndShowsDeployments(t *testing.T) {
	var output bytes.Buffer
	err := dashboardTemplate.Execute(&output, dashboardData{
		Deployments: []orchestrator.Deployment{{
			ID: "abc123abc123", Name: `<script>alert("x")</script>`,
			URL: "https://abc123abc123.preview.example.test", Status: "running",
			StatusDetail: "Up 2 minutes", Port: 8080, CreatedAt: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC),
		}},
		GeneratedAt: "01:02:03 UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, expected := range []string{
		"Preview deployments", "abc123abc123", "https://abc123abc123.preview.example.test",
		"status-running", "Up 2 minutes", "<strong>1</strong>deployment",
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("dashboard does not contain %q", expected)
		}
	}
	if strings.Contains(html, `<script>alert("x")</script>`) || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("dashboard did not HTML-escape deployment name")
	}
}

func TestDashboardSecurityHeaders(t *testing.T) {
	header := make(http.Header)
	setDashboardHeaders(header)
	for _, name := range []string{"Cache-Control", "Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options"} {
		if header.Get(name) == "" {
			t.Errorf("missing %s", name)
		}
	}
}

func TestDashboardRouteIsExactAndReadOnly(t *testing.T) {
	handler := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, 1024, "").Handler()
	for _, test := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodPost, path: "/", want: http.StatusMethodNotAllowed},
		{method: http.MethodGet, path: "/unknown", want: http.StatusNotFound},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("%s %s status = %d, want %d", test.method, test.path, response.Code, test.want)
		}
	}
}
