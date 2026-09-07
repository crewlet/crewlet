package webhooks

import (
	"context"
	"html/template"
	"net/http"
	"strconv"
	"strings"
)

// Where GitHub returns a browser after an operator creates or installs one
// agent's app.
//
// UNAUTHENTICATED, and it has to be: a redirect from GitHub carries no engine
// credential, only the one-time code and the state that travelled with it. So
// the state is what stands in for a credential here, and it is a signed token
// this engine minted, naming the seat and its expiry. The completer validates
// it before doing anything at all.
//
// TWO ARRIVALS, one route. GitHub sends the browser here twice: once after
// the app is CREATED, with a code to convert, and once after it is INSTALLED,
// with nothing but the setup URL's own query. The second is a courtesy, since
// the reconcile loop discovers the installation on its own, so it renders the
// same page rather than failing for having no code.

var githubAppPage = template.Must(template.New("github-app").Parse(`<!doctype html>
<html lang="en">
  <head>
    <meta charset="utf-8">
    <meta name="viewport" content="width=device-width, initial-scale=1">
    <title>Crewlet: {{.Heading}}</title>
    <link rel="icon" href="/static/crewlet-icon.svg">
    <style>
      /* The dashboard's own dark palette, restated rather than imported: this
         page is served by the webhook mux to a browser arriving from GitHub,
         so it cannot depend on the dashboard bundle being loaded or even
         built. The values are tokens.css's dark set. */
      :root {
        color-scheme: dark;
        --bg: #0a0c11;
        --surface: #101319;
        --border: rgba(255, 255, 255, 0.11);
        --border-subtle: rgba(255, 255, 255, 0.06);
        --text: #e9ebf0;
        --muted: #9aa2b0;
        --heading: #f6f7fa;
        --accent: #8347ff;
        --accent-hover: #9463ff;
        --positive: #3fb27f;
        --critical: #e5484d;
      }
      * { box-sizing: border-box; }
      body {
        margin: 0;
        min-height: 100vh;
        display: flex;
        align-items: center;
        justify-content: center;
        padding: 2rem 1.25rem;
        background: var(--bg);
        color: var(--text);
        font: 15px/1.6 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
      }
      .wrap { width: 100%; max-width: 30rem; text-align: center; }
      .mark { width: 56px; height: 56px; margin: 0 auto 1.25rem; display: block; }
      h1 {
        margin: 0 0 1.5rem;
        font-size: 1.375rem;
        font-weight: 600;
        color: var(--heading);
        letter-spacing: -0.01em;
      }
      h1 .seat { color: var(--accent); }
      .card {
        background: var(--surface);
        border: 1px solid var(--border);
        border-radius: 12px;
        padding: 1.5rem;
        text-align: left;
      }
      .row { padding-bottom: 1rem; border-bottom: 1px solid var(--border-subtle); }
      .row + .row { padding-top: 1rem; }
      .row:last-child { border-bottom: none; padding-bottom: 0; }
      .state { display: flex; align-items: center; gap: .5rem; font-weight: 600; }
      .dot { width: 8px; height: 8px; border-radius: 50%; flex: 0 0 auto; }
      .dot.ok { background: var(--positive); }
      .dot.bad { background: var(--critical); }
      p { margin: .5rem 0 0; color: var(--muted); }
      .btn {
        display: inline-block;
        margin-top: 1rem;
        padding: .55rem 1.1rem;
        border-radius: 8px;
        background: var(--accent);
        color: #fff;
        font-weight: 600;
        text-decoration: none;
      }
      .btn:hover { background: var(--accent-hover); }
      .foot { margin-top: 1rem; font-size: 13px; color: var(--muted); text-align: center; }
    </style>
  </head>
  <body>
    <div class="wrap">
      <img class="mark" src="/static/crewlet-icon.svg" alt="Crewlet">
      <h1>{{.Heading}}{{if .Seat}} <span class="seat">{{.Seat}}</span>{{end}}</h1>
      <div class="card">
        <div class="row">
          <div class="state">
            <span class="dot {{if .Error}}bad{{else}}ok{{end}}"></span>
            {{if .Error}}Not completed{{else}}Done{{end}}
          </div>
          <p>{{if .Error}}{{.Error}}{{else}}{{.Message}}{{end}}</p>
        </div>
        {{if .InstallURL}}
        <div class="row">
          <div class="state"><span class="dot"></span>One step left</div>
          <p>Install the app so it can see the repositories this agent works in.</p>
          <a class="btn" href="{{.InstallURL}}">Install on GitHub</a>
        </div>
        {{end}}
      </div>
      <p class="foot">You can close this tab and go back to Crewlet.</p>
    </div>
  </body>
</html>
`))

type githubAppView struct {
	Heading    string
	Message    string
	Error      string
	Seat       string
	InstallURL string
	Done       bool
}

// AppCompleter finishes an app creation begun elsewhere in this engine.
//
// An interface declared by the CONSUMER, holding only what this route needs:
// the completion and the address to send the operator to next. The setup
// surface implements it, and this package does not import that one.
type AppCompleter interface {
	// Complete converts the one-time code and records the app on the seat
	// the state names, returning that seat.
	Complete(ctx context.Context, code, state string) (string, error)

	// InstallURL is where the operator installs the app a seat now has,
	// or empty when it has none yet.
	InstallURL(seat string) string

	// RecordInstall adopts an installation GitHub named on the redirect.
	//
	// THE LOOP WOULD FIND IT ANYWAY, on its next pass, by listing the
	// app's installations. Taking it here as well is not redundant: it is
	// the difference between a screen that is right when the operator
	// looks at it and one that is right a minute later, and the id is in
	// the query GitHub already sent.
	RecordInstall(ctx context.Context, seat string, installationID int64) error
}

// githubAppLanding serves GET /webhooks/github-app.
func (r *Receiver) githubAppLanding(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	view := githubAppView{
		Heading: "App created for",
		Message: "The app's key is sealed in this company's secret store.",
	}
	status := http.StatusOK

	switch {
	case q.Get("error") != "":
		// GitHub declines with a reason in the query, and it is the
		// operator's to read: they cancelled, or they may not create
		// apps on that organization.
		view.Heading, view.Error, status = "App not created", describeRefusal(q), http.StatusBadRequest

	case strings.TrimSpace(q.Get("installed")) != "":
		// THE INSTALL ARRIVAL, and GitHub names the installation in the
		// query. Recording it here is what makes the Integrations screen
		// right when the operator gets back to it rather than a minute
		// later; the loop would find the same id by listing the app's
		// installations, so a failure here costs the wait and nothing
		// more, which is why it is reported as a note and not an error.
		view.Heading = "App installed for"
		view.Seat = strings.TrimSpace(q.Get("installed"))
		view.Done = true
		view.Message = "Crewlet picks the installation up on its next pass, usually " +
			"within a minute, and the Integrations screen will say so."
		if id := installationID(q.Get("installation_id")); id != 0 && r.appFlow != nil {
			if err := r.appFlow.RecordInstall(req.Context(), view.Seat, id); err != nil {
				log.Warn("github_install_not_recorded", "seat", view.Seat,
					"error", err.Error(),
					"detail", "the next reconcile pass finds the same installation")
			} else {
				view.Message = "This agent now acts as itself on GitHub."
				log.Info("github_install_recorded", "seat", view.Seat, "installation", id)
			}
		}

	case r.appFlow == nil:
		view.Heading, status = "App not created", http.StatusServiceUnavailable
		view.Error = "This engine cannot finish creating an app: it has no setup " +
			"surface. The app exists at GitHub and should be deleted there."

	case strings.TrimSpace(q.Get("code")) == "":
		view.Heading, status = "App not created", http.StatusBadRequest
		view.Error = "GitHub did not send a code, so there is nothing to convert."

	default:
		seat, err := r.appFlow.Complete(req.Context(), q.Get("code"), q.Get("state"))
		switch {
		case err != nil:
			view.Heading, status = "App not created", http.StatusBadRequest
			// NAMED, NEVER QUOTED FROM THE RESPONSE. The conversion body
			// carries the app's private key, so the message an operator
			// reads is this engine's own wording.
			view.Error = err.Error()
			view.Seat = seat
			log.Warn("github_app_create_failed", "seat", seat, "error", err.Error())
		default:
			view.Seat = seat
			view.InstallURL = r.appFlow.InstallURL(seat)
			view.Done = view.InstallURL == ""
			log.Info("github_app_created", "seat", seat,
				"detail", "the app's key is sealed; the operator installs it next")
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// Rendered straight to the response: the template is parsed at
	// startup, so the only way this errors is a broken connection, and
	// there is no second status to write by then.
	if err := githubAppPage.Execute(w, view); err != nil {
		log.Warn("github_app_page_failed", "error", err)
	}
}

// describeRefusal turns GitHub's own refusal into a sentence.
//
// Its `error_description` is written for a person and is the better text when
// it is there; the bare code is all there is otherwise.
func describeRefusal(q map[string][]string) string {
	first := func(key string) string {
		if values := q[key]; len(values) > 0 {
			return strings.TrimSpace(values[0])
		}
		return ""
	}
	if described := first("error_description"); described != "" {
		return described
	}
	return "GitHub refused the app creation: " + first("error")
}

// installationID reads the id GitHub put on the redirect, or zero.
//
// ZERO FOR ANYTHING UNREADABLE, which is the safe direction: the loop
// discovers the installation by asking GitHub, so a query this cannot parse
// costs a minute rather than an adoption.
func installationID(raw string) int64 {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0
	}
	return id
}
