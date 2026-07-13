package api

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/orchestrator"
)

func TestDashboardTemplateEscapesAndShowsDeployments(t *testing.T) {
	var output bytes.Buffer
	err := dashboardTemplate.Execute(&output, dashboardData{
		Deployments: []dashboardDeployment{{
			Deployment: orchestrator.Deployment{
				ID: "abc123abc123", Name: `<script>alert("x")</script>`,
				URL: "https://abc123abc123.preview.example.test", Status: "running",
				StatusDetail: "Up 2 minutes", Port: 8080, CreatedAt: time.Date(2026, 7, 13, 1, 2, 3, 0, time.UTC),
				HibernationEnabled: true, HibernationState: orchestrator.HibernationStateActive,
			},
			CSRFToken:          "csrf-token",
			CanHibernate:       true,
			PreviewActionLabel: "Open preview",
		}},
		GeneratedAt:     "01:02:03 UTC",
		ControlsEnabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, expected := range []string{
		"Preview deployments", "abc123abc123", "https://abc123abc123.preview.example.test",
		"status-running", "Active", "Up 2 minutes", "<strong>1</strong>deployment", "Open preview",
		`action="/dashboard/hibernate"`, `name="csrf" value="csrf-token"`, "Hibernate now",
	} {
		if !strings.Contains(html, expected) {
			t.Errorf("dashboard does not contain %q", expected)
		}
	}
	if strings.Contains(html, `<script>alert("x")</script>`) || !strings.Contains(html, "&lt;script&gt;") {
		t.Fatal("dashboard did not HTML-escape deployment name")
	}
}

func TestDashboardTemplateShowsHibernatedAndLegacyStatesWithoutControls(t *testing.T) {
	a := &API{}
	var output bytes.Buffer
	err := dashboardTemplate.Execute(&output, dashboardData{
		Deployments: []dashboardDeployment{
			a.dashboardCard(orchestrator.Deployment{ID: "abc123abc123", Status: "exited", HibernationEnabled: true, HibernationState: orchestrator.HibernationStateHibernated}),
			a.dashboardCard(orchestrator.Deployment{ID: "def456def456", Status: "running", HibernationState: orchestrator.HibernationStateUnavailable}),
		},
		GeneratedAt: "01:02:03 UTC",
	})
	if err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, expected := range []string{"Hibernated", "Unavailable", "Read-only view"} {
		if !strings.Contains(html, expected) {
			t.Errorf("dashboard does not contain %q", expected)
		}
	}
	if strings.Contains(html, "Hibernate now") || strings.Contains(html, "csrf-token") || strings.Contains(html, "Redeploy this preview") {
		t.Fatal("dashboard rendered controls for a non-active or legacy preview")
	}
}

func TestDashboardCardsMakeControlEligibilityExplicit(t *testing.T) {
	a := &API{dashboardControlsEnabled: true}
	tests := []struct {
		name          string
		deployment    orchestrator.Deployment
		previewAction string
		controlLabel  string
		controlHint   string
		canHibernate  bool
		wantCSRF      bool
	}{
		{
			name: "active", deployment: orchestrator.Deployment{ID: "abc123abc123", HibernationEnabled: true, HibernationState: orchestrator.HibernationStateActive},
			previewAction: "Open preview", canHibernate: true, wantCSRF: true,
		},
		{
			name: "hibernated", deployment: orchestrator.Deployment{ID: "def456def456", URL: "https://def456def456.preview.example.test", HibernationEnabled: true, HibernationState: orchestrator.HibernationStateHibernated},
			previewAction: "Resume preview", controlLabel: "Already hibernated", controlHint: "Opening the preview safely wakes the same container.",
		},
		{
			name: "hibernating", deployment: orchestrator.Deployment{ID: "0123456789ab", HibernationEnabled: true, HibernationState: orchestrator.HibernationStateHibernating},
			previewAction: "Open preview", controlLabel: "Hibernating…", controlHint: "A request that arrives during shutdown will resume this preview.",
		},
		{
			name: "resuming", deployment: orchestrator.Deployment{ID: "123456789abc", HibernationEnabled: true, HibernationState: orchestrator.HibernationStateResuming},
			previewAction: "Open preview", controlLabel: "Resuming…", controlHint: "The preview will become available after its application starts.",
		},
		{
			name: "legacy", deployment: orchestrator.Deployment{ID: "23456789abcd", HibernationState: orchestrator.HibernationStateUnavailable},
			previewAction: "Open preview", controlLabel: "Hibernation unavailable", controlHint: "Redeploy this preview to enable safe request-driven wake-up.",
		},
		{
			name: "unrecoverable", deployment: orchestrator.Deployment{ID: "3456789abcde", HibernationEnabled: true, HibernationState: orchestrator.HibernationStateUnavailable},
			previewAction: "Open preview", controlLabel: "Hibernation unavailable", controlHint: "Inspect this preview's Docker state before trying again.",
		},
	}

	cards := make([]dashboardDeployment, 0, len(tests))
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			card := a.dashboardCard(test.deployment)
			cards = append(cards, card)
			if card.PreviewActionLabel != test.previewAction || card.ControlLabel != test.controlLabel || card.ControlHint != test.controlHint || card.CanHibernate != test.canHibernate {
				t.Fatalf("dashboardCard() = %#v", card)
			}
			if (card.CSRFToken != "") != test.wantCSRF {
				t.Fatalf("CSRF presence = %t, want %t", card.CSRFToken != "", test.wantCSRF)
			}
		})
	}

	var output bytes.Buffer
	if err := dashboardTemplate.Execute(&output, dashboardData{Deployments: cards, ControlsEnabled: true}); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, expected := range []string{`href="https://def456def456.preview.example.test"`, "Resume preview", "Already hibernated", "Hibernating…", "Resuming…", "Hibernation unavailable", "Redeploy this preview to enable safe request-driven wake-up."} {
		if !strings.Contains(html, expected) {
			t.Errorf("dashboard does not contain %q", expected)
		}
	}
	if got := strings.Count(html, `action="/dashboard/hibernate"`); got != 1 {
		t.Errorf("hibernate forms = %d, want 1", got)
	}
	if got := strings.Count(html, `name="csrf"`); got != 1 {
		t.Errorf("CSRF fields = %d, want 1", got)
	}
}

func TestDashboardSecurityHeaders(t *testing.T) {
	header := make(http.Header)
	setDashboardHeaders(header)
	for _, name := range []string{"Cache-Control", "Content-Security-Policy", "Referrer-Policy", "X-Content-Type-Options", "X-Frame-Options", "X-Robots-Tag"} {
		if header.Get(name) == "" {
			t.Errorf("missing %s", name)
		}
	}
	if got := header.Get("Content-Security-Policy"); !strings.Contains(got, "form-action 'self'") || strings.Contains(got, "script-src") {
		t.Fatalf("Content-Security-Policy = %q", got)
	}
	if got := header.Get("Referrer-Policy"); got != "same-origin" {
		t.Fatalf("Referrer-Policy = %q, want same-origin", got)
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
		{method: http.MethodGet, path: "/dashboard/hibernate", want: http.StatusMethodNotAllowed},
		{method: http.MethodPost, path: "/dashboard/hibernate", want: http.StatusNotFound},
	} {
		request := httptest.NewRequest(test.method, test.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != test.want {
			t.Errorf("%s %s status = %d, want %d", test.method, test.path, response.Code, test.want)
		}
	}
}

func TestDashboardControlsRequireSeparateBasicAuthentication(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	handler := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, 1024, "api-token", WithDashboardControls(token, "https://api.preview.example.test")).Handler()

	for _, test := range []struct {
		name     string
		username string
		password string
	}{
		{name: "missing"},
		{name: "API bearer is not accepted", username: "preview", password: "api-token"},
		{name: "wrong username", username: "admin", password: token},
		{name: "wrong password", username: "preview", password: token + "x"},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/dashboard/hibernate", strings.NewReader("id=abc123abc123&csrf=wrong"))
			if test.username != "" {
				request.SetBasicAuth(test.username, test.password)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
			}
			if got := response.Header().Get("WWW-Authenticate"); !strings.HasPrefix(got, `Basic realm="preview-deployment dashboard"`) {
				t.Fatalf("WWW-Authenticate = %q", got)
			}
		})
	}
}

func TestDashboardHibernateValidatesOriginAndCSRFThenRedirects(t *testing.T) {
	const (
		token  = "0123456789abcdef0123456789abcdef"
		origin = "https://api.preview.example.test"
		id     = "abc123abc123"
	)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := New(nil, nil, logger, 1024, 1024, "api-token", WithDashboardControls(token, origin))
	calls := 0
	a.hibernateDeployment = func(_ context.Context, gotID string) (orchestrator.Deployment, error) {
		calls++
		if gotID != id {
			t.Fatalf("deployment ID = %q", gotID)
		}
		return orchestrator.Deployment{ID: gotID}, nil
	}
	handler := a.Handler()

	request := dashboardHibernateRequest(t, token, origin, id, a.dashboardCSRFToken(id))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/" {
		t.Fatalf("response = %d Location %q", response.Code, response.Header().Get("Location"))
	}
	if calls != 1 {
		t.Fatalf("hibernate calls = %d, want 1", calls)
	}

	for _, test := range []struct {
		name   string
		origin string
		id     string
		csrf   string
	}{
		{name: "missing origin", csrf: a.dashboardCSRFToken(id)},
		{name: "opaque origin", origin: "null", csrf: a.dashboardCSRFToken(id)},
		{name: "sibling preview origin", origin: "https://abc123abc123.preview.example.test", csrf: a.dashboardCSRFToken(id)},
		{name: "wrong CSRF", origin: origin, csrf: strings.Repeat("0", 64)},
		{name: "token for another preview", origin: origin, id: "def456def456", csrf: a.dashboardCSRFToken(id)},
	} {
		t.Run(test.name, func(t *testing.T) {
			requestID := test.id
			if requestID == "" {
				requestID = id
			}
			request := dashboardHibernateRequest(t, token, test.origin, requestID, test.csrf)
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusForbidden)
			}
		})
	}
	duplicateOrigin := dashboardHibernateRequest(t, token, origin, id, a.dashboardCSRFToken(id))
	duplicateOrigin.Header.Add("Origin", origin)
	duplicateResponse := httptest.NewRecorder()
	handler.ServeHTTP(duplicateResponse, duplicateOrigin)
	if duplicateResponse.Code != http.StatusForbidden {
		t.Fatalf("duplicate Origin status = %d, want %d", duplicateResponse.Code, http.StatusForbidden)
	}
	if calls != 1 {
		t.Fatalf("hibernate calls after rejected requests = %d, want 1", calls)
	}
}

func TestDashboardHibernateMapsUnavailablePreviewToConflict(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	const origin = "https://api.preview.example.test"
	a := New(nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), 1024, 1024, "", WithDashboardControls(token, origin))
	a.hibernateDeployment = func(context.Context, string) (orchestrator.Deployment, error) {
		return orchestrator.Deployment{}, orchestrator.ErrHibernationUnavailable
	}
	request := dashboardHibernateRequest(t, token, origin, "abc123abc123", a.dashboardCSRFToken("abc123abc123"))
	response := httptest.NewRecorder()
	a.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusConflict)
	}
}

func TestDashboardHibernateRejectsMalformedForms(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/dashboard/hibernate", strings.NewReader(url.Values{"id": {"abc123abc123"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	if _, _, err := readDashboardHibernateForm(response, request); err == nil {
		t.Fatal("readDashboardHibernateForm accepted a missing CSRF token")
	}

	request = httptest.NewRequest(http.MethodPost, "/dashboard/hibernate?csrf=wrong", strings.NewReader(url.Values{"id": {"abc123abc123"}, "csrf": {"wrong"}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, _, err := readDashboardHibernateForm(httptest.NewRecorder(), request); err == nil {
		t.Fatal("readDashboardHibernateForm accepted query parameters")
	}
}

func dashboardHibernateRequest(t *testing.T, password, origin, id, csrf string) *http.Request {
	t.Helper()
	body := url.Values{"id": {id}, "csrf": {csrf}}.Encode()
	request := httptest.NewRequest(http.MethodPost, "/dashboard/hibernate", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", origin)
	request.SetBasicAuth("preview", password)
	return request
}
