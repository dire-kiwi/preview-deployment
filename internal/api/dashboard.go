package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"errors"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/orchestrator"
)

//go:embed dashboard.html
var dashboardHTML string

//go:embed dashboard.js
var dashboardScript string

type dashboardDeployment struct {
	orchestrator.Deployment
	CSRFToken          string
	CanHibernate       bool
	PreviewActionLabel string
	ControlLabel       string
	ControlHint        string
}

type dashboardData struct {
	Deployments     []dashboardDeployment
	GeneratedAt     string
	ControlsEnabled bool
}

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"displayName": func(deployment orchestrator.Deployment) string {
		if name := strings.TrimSpace(deployment.Name); name != "" {
			return name
		}
		return "Unnamed preview"
	},
	"statusClass":      dashboardStatusClass,
	"statusLabel":      dashboardStatusLabel,
	"statusCount":      dashboardStatusCount,
	"hibernationClass": dashboardHibernationClass,
	"hibernationLabel": dashboardHibernationLabel,
	"hibernationCount": dashboardHibernationCount,
	"createdTime": func(created time.Time) string {
		if created.IsZero() {
			return "Unknown"
		}
		return created.Local().Format("2 Jan 2006, 15:04 MST")
	},
}).Parse(dashboardHTML))

func dashboardStatusClass(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "running":
		return "status-running"
	case "created", "restarting", "starting", "removing":
		return "status-pending"
	case "exited", "dead", "paused":
		return "status-stopped"
	default:
		return "status-unknown"
	}
}

func dashboardStatusLabel(status string) string {
	normalized := strings.ToLower(strings.TrimSpace(status))
	switch normalized {
	case "running":
		return "Running"
	case "created":
		return "Created"
	case "restarting":
		return "Restarting"
	case "starting":
		return "Starting"
	case "removing":
		return "Removing"
	case "exited":
		return "Exited"
	case "dead":
		return "Failed"
	case "paused":
		return "Paused"
	case "":
		return "Unknown"
	default:
		return status
	}
}

func dashboardStatusCount(deployments []dashboardDeployment, class string) int {
	count := 0
	for _, deployment := range deployments {
		if dashboardStatusClass(deployment.Status) == class {
			count++
		}
	}
	return count
}

func dashboardHibernationClass(state string) string {
	switch state {
	case orchestrator.HibernationStateActive:
		return "status-running"
	case orchestrator.HibernationStateHibernating, orchestrator.HibernationStateResuming:
		return "status-pending"
	case orchestrator.HibernationStateHibernated:
		return "status-hibernated"
	default:
		return "status-unknown"
	}
}

func dashboardHibernationLabel(state string) string {
	switch state {
	case orchestrator.HibernationStateActive:
		return "Active"
	case orchestrator.HibernationStateHibernating:
		return "Hibernating"
	case orchestrator.HibernationStateHibernated:
		return "Hibernated"
	case orchestrator.HibernationStateResuming:
		return "Resuming"
	default:
		return "Unavailable"
	}
}

func dashboardHibernationCount(deployments []dashboardDeployment, state string) int {
	count := 0
	for _, deployment := range deployments {
		if deployment.HibernationState == state || state == "transitioning" && (deployment.HibernationState == orchestrator.HibernationStateHibernating || deployment.HibernationState == orchestrator.HibernationStateResuming) {
			count++
		}
	}
	return count
}

func (a *API) dashboard(writer http.ResponseWriter, request *http.Request) {
	if serveDashboardAsset(writer, request) {
		return
	}
	setDashboardHeaders(writer.Header())
	deployments, err := a.service.List(request.Context())
	if err != nil {
		a.logger.Error("could not render deployment dashboard", "error", err)
		http.Error(writer, "Could not load preview deployments", http.StatusBadGateway)
		return
	}
	cards := make([]dashboardDeployment, 0, len(deployments))
	for _, deployment := range deployments {
		cards = append(cards, a.dashboardCard(deployment))
	}
	data := dashboardData{
		Deployments:     cards,
		GeneratedAt:     time.Now().Format("15:04:05 MST"),
		ControlsEnabled: a.dashboardControlsEnabled,
	}
	if err := renderDashboardResponse(writer, request, data); err != nil {
		a.logger.Error("could not write deployment dashboard", "error", err)
	}
}

func renderDashboardResponse(writer http.ResponseWriter, request *http.Request, data dashboardData) error {
	templateName := "dashboard"
	if request.Header.Get("X-Dashboard-Refresh") == "1" {
		templateName = "dashboardState"
		writer.Header().Set("Vary", "X-Dashboard-Refresh")
	}
	setDashboardHeaders(writer.Header())
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	return dashboardTemplate.ExecuteTemplate(writer, templateName, data)
}

func serveDashboardAsset(writer http.ResponseWriter, request *http.Request) bool {
	assets, requested := request.URL.Query()["asset"]
	if !requested {
		return false
	}
	setDashboardHeaders(writer.Header())
	if len(assets) != 1 || assets[0] != "dashboard.js" {
		http.NotFound(writer, request)
		return true
	}
	writer.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	writer.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	_, _ = io.WriteString(writer, dashboardScript)
	return true
}

func (a *API) dashboardCard(deployment orchestrator.Deployment) dashboardDeployment {
	card := dashboardDeployment{
		Deployment:         deployment,
		PreviewActionLabel: "Open preview",
	}
	if !a.dashboardControlsEnabled {
		return card
	}
	if !deployment.HibernationEnabled {
		card.ControlLabel = "Hibernation unavailable"
		card.ControlHint = "Redeploy this preview to enable safe request-driven wake-up."
		return card
	}

	switch deployment.HibernationState {
	case orchestrator.HibernationStateActive:
		card.CanHibernate = true
		card.CSRFToken = a.dashboardCSRFToken(deployment.ID)
	case orchestrator.HibernationStateHibernated:
		card.PreviewActionLabel = "Resume preview"
		card.ControlLabel = "Already hibernated"
		card.ControlHint = "Opening the preview safely wakes the same container."
	case orchestrator.HibernationStateHibernating:
		card.ControlLabel = "Hibernating…"
		card.ControlHint = "A request that arrives during shutdown will resume this preview."
	case orchestrator.HibernationStateResuming:
		card.ControlLabel = "Resuming…"
		card.ControlHint = "The preview will become available after its application starts."
	default:
		card.ControlLabel = "Hibernation unavailable"
		card.ControlHint = "Inspect this preview's Docker state before trying again."
	}
	return card
}

func (a *API) hibernateFromDashboard(writer http.ResponseWriter, request *http.Request) {
	setDashboardHeaders(writer.Header())
	if !a.dashboardControlsEnabled {
		http.NotFound(writer, request)
		return
	}
	origins := request.Header.Values("Origin")
	if len(origins) != 1 || origins[0] != a.dashboardOrigin {
		http.Error(writer, "Dashboard request origin was rejected", http.StatusForbidden)
		return
	}
	id, csrfToken, err := readDashboardHibernateForm(writer, request)
	if err != nil {
		http.Error(writer, "Invalid dashboard hibernation request", http.StatusBadRequest)
		return
	}
	expectedToken := a.dashboardCSRFToken(id)
	if subtle.ConstantTimeCompare([]byte(csrfToken), []byte(expectedToken)) != 1 {
		http.Error(writer, "Dashboard request token was rejected", http.StatusForbidden)
		return
	}
	if a.hibernateDeployment == nil {
		http.Error(writer, "Dashboard hibernation is unavailable", http.StatusServiceUnavailable)
		return
	}
	if _, err := a.hibernateDeployment(request.Context(), id); err != nil {
		a.writeDashboardServiceError(writer, err)
		return
	}
	http.Redirect(writer, request, "/", http.StatusSeeOther)
}

func (a *API) dashboardCSRFToken(id string) string {
	digest := hmac.New(sha256.New, a.dashboardCSRFKey[:])
	_, _ = digest.Write([]byte("POST\x00/dashboard/hibernate\x00" + id))
	return hex.EncodeToString(digest.Sum(nil))
}

func readDashboardHibernateForm(writer http.ResponseWriter, request *http.Request) (string, string, error) {
	if request.URL.RawQuery != "" {
		return "", "", errors.New("query parameters are not allowed")
	}
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/x-www-form-urlencoded" {
		return "", "", errors.New("Content-Type must be application/x-www-form-urlencoded")
	}
	request.Body = http.MaxBytesReader(writer, request.Body, 4096)
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return "", "", err
	}
	values, err := url.ParseQuery(string(body))
	if err != nil || len(values) != 2 {
		return "", "", errors.New("form must contain exactly id and csrf")
	}
	id, ok := singleDashboardFormValue(values, "id")
	if !ok {
		return "", "", errors.New("form must contain one id")
	}
	csrfToken, ok := singleDashboardFormValue(values, "csrf")
	if !ok {
		return "", "", errors.New("form must contain one csrf token")
	}
	return id, csrfToken, nil
}

func singleDashboardFormValue(values url.Values, name string) (string, bool) {
	items, ok := values[name]
	if !ok || len(items) != 1 || items[0] == "" {
		return "", false
	}
	return items[0], true
}

func (a *API) writeDashboardServiceError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, orchestrator.ErrNotFound):
		http.Error(writer, "Preview deployment was not found", http.StatusNotFound)
	case errors.Is(err, orchestrator.ErrHibernationUnavailable):
		http.Error(writer, "This preview does not have a safe hibernation wake route", http.StatusConflict)
	case errors.Is(err, context.DeadlineExceeded):
		http.Error(writer, "Preview hibernation timed out", http.StatusGatewayTimeout)
	case errors.Is(err, context.Canceled):
		http.Error(writer, "Preview hibernation was canceled", 499)
	default:
		a.logger.Error("dashboard hibernation failed", "error", err)
		http.Error(writer, "Could not hibernate preview deployment", http.StatusBadGateway)
	}
}

func setDashboardHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; connect-src 'self'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	// A same-origin policy keeps cross-origin navigation private while allowing
	// basic HTML form POSTs to carry their real Origin instead of "null".
	header.Set("Referrer-Policy", "same-origin")
	header.Set("X-Robots-Tag", "noindex, nofollow")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
}
