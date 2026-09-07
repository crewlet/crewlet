package webhooks

import (
	"context"
	"html/template"
	"net/http"
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
    <style>
      body { font: 15px/1.6 system-ui, sans-serif; margin: 3rem auto; max-width: 34rem; padding: 0 1.5rem; }
      h1 { font-size: 1.25rem; margin-bottom: .5rem; }
      p { color: #444; }
      .next { margin-top: 1.5rem; padding: 1rem; background: #f4f4f5; border-radius: 6px; }
      a { color: #5b48d9; }
    </style>
  </head>
  <body>
    <h1>{{.Heading}}</h1>
    {{if .Error}}<p>{{.Error}}</p>{{end}}
    {{if .Seat}}<p>Agent <strong>{{.Seat}}</strong>.</p>{{end}}
    {{if .InstallURL}}
      <div class="next">
        <p>One step left: install the app so it can see the repositories this agent works in.</p>
        <p><a href="{{.InstallURL}}">Install it on your organization</a></p>
      </div>
    {{else if .Done}}
      <div class="next">
        <p>Nothing else to do here. Crewlet picks the installation up on its next pass,
           usually within a minute, and the Integrations screen will say so.</p>
      </div>
    {{end}}
  </body>
</html>
`))

type githubAppView struct {
	Heading    string
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
}

// githubAppLanding serves GET /webhooks/github-app.
func (r *Receiver) githubAppLanding(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	view := githubAppView{Heading: "App created"}
	status := http.StatusOK

	switch {
	case q.Get("error") != "":
		// GitHub declines with a reason in the query, and it is the
		// operator's to read: they cancelled, or they may not create
		// apps on that organization.
		view.Heading, view.Error, status = "Not created", describeRefusal(q), http.StatusBadRequest

	case strings.TrimSpace(q.Get("installed")) != "":
		// The INSTALL arrival. Nothing to convert: the reconcile loop
		// finds the installation itself, which is why this says so
		// rather than pretending to have done something.
		view.Heading = "App installed"
		view.Seat = strings.TrimSpace(q.Get("installed"))
		view.Done = true

	case r.appFlow == nil:
		view.Heading, status = "Not created", http.StatusServiceUnavailable
		view.Error = "This engine cannot finish creating an app: it has no setup " +
			"surface. The app exists at GitHub and should be deleted there."

	case strings.TrimSpace(q.Get("code")) == "":
		view.Heading, status = "Not created", http.StatusBadRequest
		view.Error = "GitHub did not send a code, so there is nothing to convert."

	default:
		seat, err := r.appFlow.Complete(req.Context(), q.Get("code"), q.Get("state"))
		switch {
		case err != nil:
			view.Heading, status = "Not created", http.StatusBadRequest
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
