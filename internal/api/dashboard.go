package api

import (
	"html/template"
	"net/http"
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
    h2 { min-width:0; margin:0; overflow-wrap:anywhere; font-size:19px; letter-spacing:-.02em; }
    .status { flex:none; padding:4px 9px; border:1px solid currentColor; border-radius:999px; font-size:11px; font-weight:800; letter-spacing:.07em; text-transform:uppercase; }
    .status-running { color:var(--green); } .status-pending { color:var(--amber); } .status-stopped { color:var(--red); } .status-unknown { color:var(--muted); }
    dl { display:grid; grid-template-columns:auto 1fr; gap:8px 14px; margin:20px 0; color:var(--muted); font-size:13px; }
    dt { color:#7085a2; } dd { min-width:0; margin:0; color:var(--text); overflow-wrap:anywhere; }
    code { color:#bed7f7; font:12px/1.5 ui-monospace,SFMono-Regular,Menlo,monospace; }
    a { display:flex; align-items:center; justify-content:space-between; gap:12px; padding:11px 13px; border:1px solid #315781; border-radius:11px; background:#142945; color:#cfe6ff; text-decoration:none; font-weight:750; }
    a:hover { border-color:var(--blue); background:#19375d; }
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
      <div class="card-head"><h2>{{displayName .}}</h2><span class="status {{statusClass .Status}}">{{.Status}}</span></div>
      <dl>
        <dt>ID</dt><dd><code>{{.ID}}</code></dd>
        <dt>Port</dt><dd>{{.Port}}</dd>
        <dt>Created</dt><dd>{{createdTime .CreatedAt}}</dd>
        {{if .StatusDetail}}<dt>Details</dt><dd>{{.StatusDetail}}</dd>{{end}}
      </dl>
      <a href="{{.URL}}" target="_blank" rel="noopener noreferrer"><span>Open preview</span><span aria-hidden="true">↗</span></a>
    </article>
    {{end}}
  </section>
  {{else}}<div class="empty">No preview deployments are currently running.</div>{{end}}
  <footer>Read-only view · refreshes every 10 seconds · generated {{.GeneratedAt}}</footer>
</main>
</body>
</html>`))

type dashboardData struct {
	Deployments []orchestrator.Deployment
	GeneratedAt string
}

func (a *API) dashboard(writer http.ResponseWriter, request *http.Request) {
	setDashboardHeaders(writer.Header())
	deployments, err := a.service.List(request.Context())
	if err != nil {
		a.logger.Error("could not render deployment dashboard", "error", err)
		http.Error(writer, "Could not load preview deployments", http.StatusBadGateway)
		return
	}
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := dashboardTemplate.Execute(writer, dashboardData{
		Deployments: deployments,
		GeneratedAt: time.Now().Format("15:04:05 MST"),
	}); err != nil {
		a.logger.Error("could not write deployment dashboard", "error", err)
	}
}

func setDashboardHeaders(header http.Header) {
	header.Set("Cache-Control", "no-store")
	header.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
}
