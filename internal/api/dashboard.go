package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
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

var dashboardTemplate = template.Must(template.New("dashboard").Funcs(template.FuncMap{
	"displayName": func(deployment orchestrator.Deployment) string {
		if name := strings.TrimSpace(deployment.Name); name != "" {
			return name
		}
		return "Unnamed preview"
	},
	"statusClass": func(status string) string {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "running":
			return "status-running"
		case "created", "restarting":
			return "status-pending"
		case "exited", "dead":
			return "status-stopped"
		default:
			return "status-unknown"
		}
	},
	"hibernationClass": func(state string) string {
		switch state {
		case "active":
			return "status-running"
		case "hibernating", "resuming":
			return "status-pending"
		case "hibernated":
			return "status-hibernated"
		default:
			return "status-unknown"
		}
	},
	"hibernationLabel": func(state string) string {
		switch state {
		case "active":
			return "Active"
		case "hibernating":
			return "Hibernating"
		case "hibernated":
			return "Hibernated"
		case "resuming":
			return "Resuming"
		default:
			return "Unavailable"
		}
	},
	"createdTime": func(created time.Time) string {
		if created.IsZero() {
			return "Unknown"
		}
		return created.Local().Format("2 Jan 2006, 15:04 MST")
	},
}).Parse(`<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <meta http-equiv="refresh" content="10">
  <title>Preview deployments</title>
  <style>
    :root { color-scheme: dark; --bg:#070b12; --panel:#0f1623; --panel-2:#131d2d; --line:#26354d; --text:#edf4ff; --muted:#8fa2bd; --blue:#5ea8ff; --green:#54d69b; --amber:#ffca6a; --red:#ff7f8f; }
    * { box-sizing:border-box; }
    body { margin:0; min-height:100vh; background:radial-gradient(circle at 15% -10%,#18365d 0,transparent 34rem),var(--bg); color:var(--text); font:15px/1.5 ui-sans-serif,system-ui,-apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif; }
    main { width:min(1180px,calc(100% - 32px)); margin:0 auto; padding:52px 0 64px; }
    header { display:flex; align-items:flex-end; justify-content:space-between; gap:24px; margin-bottom:30px; }
    .eyebrow { margin:0 0 8px; color:var(--blue); font-size:12px; font-weight:800; letter-spacing:.16em; text-transform:uppercase; }
    h1 { margin:0; font-size:clamp(32px,5vw,54px); line-height:1; letter-spacing:-.045em; }
    .summary { color:var(--muted); text-align:right; }
    .summary strong { display:block; color:var(--text); font-size:30px; line-height:1; }
    .grid { display:grid; grid-template-columns:repeat(auto-fit,minmax(285px,1fr)); gap:16px; }
    article { min-width:0; padding:22px; border:1px solid var(--line); border-radius:18px; background:linear-gradient(145deg,rgba(19,29,45,.97),rgba(11,17,28,.97)); box-shadow:0 18px 44px rgba(0,0,0,.24); }
    .card-head { display:flex; align-items:flex-start; justify-content:space-between; gap:14px; }
    .badges { display:flex; flex:none; flex-wrap:wrap; justify-content:flex-end; gap:7px; }
    h2 { min-width:0; margin:0; overflow-wrap:anywhere; font-size:19px; letter-spacing:-.02em; }
    .status { flex:none; padding:4px 9px; border:1px solid currentColor; border-radius:999px; font-size:11px; font-weight:800; letter-spacing:.07em; text-transform:uppercase; }
    .status-running { color:var(--green); } .status-pending { color:var(--amber); } .status-stopped { color:var(--red); } .status-hibernated { color:var(--blue); } .status-unknown { color:var(--muted); }
    dl { display:grid; grid-template-columns:auto 1fr; gap:8px 14px; margin:20px 0; color:var(--muted); font-size:13px; }
    dt { color:#7085a2; } dd { min-width:0; margin:0; color:var(--text); overflow-wrap:anywhere; }
    code { color:#bed7f7; font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace; }
    .actions { display:grid; grid-template-columns:1fr auto; gap:9px; }
    .actions a { display:flex; align-items:center; justify-content:space-between; gap:12px; padding:11px 13px; border:1px solid #315781; border-radius:11px; background:#142945; color:#cfe6ff; text-decoration:none; font-weight:750; }
    .actions a:hover { border-color:var(--blue); background:#19375d; }
    form { margin:0; }
    button { min-height:44px; padding:10px 13px; border:1px solid #8a6530; border-radius:11px; background:#352713; color:#ffd99a; cursor:pointer; font:inherit; font-weight:800; }
    button:hover { border-color:var(--amber); background:#473419; }
    button:focus-visible, a:focus-visible { outline:3px solid #5ea8ff66; outline-offset:2px; }
    .empty { padding:56px 24px; border:1px dashed var(--line); border-radius:18px; color:var(--muted); text-align:center; }
    footer { margin-top:28px; color:#6f829d; font-size:12px; }
    @media (max-width:640px) { main { padding-top:32px; } header { align-items:flex-start; flex-direction:column; } .summary { text-align:left; } }
  </style>
</head>
<body>
<main>
  <header>
    <div><p class="eyebrow">Dire Kiwi infrastructure</p><h1>Preview deployments</h1></div>
    <div class="summary"><strong>{{len .Deployments}}</strong>{{if eq (len .Deployments) 1}}deployment{{else}}deployments{{end}}</div>
  </header>
  {{if .Deployments}}
  <section class="grid" aria-label="Deployments">
    {{range .Deployments}}
    <article>
      <div class="card-head">
        <h2>{{displayName .Deployment}}</h2>
        <div class="badges"><span class="status {{statusClass .Status}}">{{.Status}}</span><span class="status {{hibernationClass .HibernationState}}">{{hibernationLabel .HibernationState}}</span></div>
      </div>
      <dl>
        <dt>ID</dt><dd><code>{{.ID}}</code></dd>
        <dt>Hibernation</dt><dd>{{hibernationLabel .HibernationState}}</dd>
        <dt>Port</dt><dd>{{.Port}}</dd>
        <dt>Created</dt><dd>{{createdTime .CreatedAt}}</dd>
        {{if .StatusDetail}}<dt>Details</dt><dd>{{.StatusDetail}}</dd>{{end}}
      </dl>
      <div class="actions">
        <a href="{{.URL}}" target="_blank" rel="noopener noreferrer"><span>Open preview</span><span aria-hidden="true">↗</span></a>
        {{if .CanHibernate}}<form method="post" action="/dashboard/hibernate"><input type="hidden" name="id" value="{{.ID}}"><input type="hidden" name="csrf" value="{{.CSRFToken}}"><button type="submit">Hibernate now</button></form>{{end}}
      </div>
    </article>
    {{end}}
  </section>
  {{else}}<div class="empty">No preview deployments are currently running.</div>{{end}}
  <footer>{{if .ControlsEnabled}}Authenticated controls enabled{{else}}Read-only view{{end}} · refreshes every 10 seconds · generated {{.GeneratedAt}}</footer>
</main>
</body>
</html>`))

type dashboardDeployment struct {
	orchestrator.Deployment
	CSRFToken    string
	CanHibernate bool
}

type dashboardData struct {
	Deployments     []dashboardDeployment
	GeneratedAt     string
	ControlsEnabled bool
}

func (a *API) dashboard(writer http.ResponseWriter, request *http.Request) {
	setDashboardHeaders(writer.Header())
	deployments, err := a.service.List(request.Context())
	if err != nil {
		a.logger.Error("could not render deployment dashboard", "error", err)
		http.Error(writer, "Could not load preview deployments", http.StatusBadGateway)
		return
	}
	cards := make([]dashboardDeployment, 0, len(deployments))
	for _, deployment := range deployments {
		card := dashboardDeployment{Deployment: deployment}
		if a.dashboardControlsEnabled && deployment.HibernationEnabled && deployment.HibernationState == orchestrator.HibernationStateActive {
			card.CanHibernate = true
			card.CSRFToken = a.dashboardCSRFToken(deployment.ID)
		}
		cards = append(cards, card)
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplate.Execute(writer, dashboardData{
		Deployments:     cards,
		GeneratedAt:     time.Now().Format("15:04:05 MST"),
		ControlsEnabled: a.dashboardControlsEnabled,
	}); err != nil {
		a.logger.Error("could not write deployment dashboard", "error", err)
	}
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
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Robots-Tag", "noindex, nofollow")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
}
