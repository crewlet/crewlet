package webhooks

import (
	"html/template"
	"net/http"
)

// slackOAuthPage is the install landing page every provisioned Slack app
// redirects to.
//
// html/template, not string concatenation: every value on this page comes from
// a query string an attacker controls, and it is rendered into a browser. The
// contextual escaping is the whole reason to use a template here rather than
// build the markup by hand.
var slackOAuthPage = template.Must(template.New("slack-oauth").Parse(`<!doctype html>
<html>
  <head>
    <meta charset="utf-8">
    <title>Crewlet — Slack app install</title>
    <style>
      body { font-family: system-ui, sans-serif; max-width: 40rem;
             margin: 4rem auto; padding: 0 1rem; line-height: 1.5; }
      code { background: rgba(127, 127, 127, .15); padding: .35rem .6rem;
             border-radius: .35rem; font-size: 1.05rem;
             word-break: break-all; display: inline-block; }
      .muted { opacity: .7; font-size: .9rem; }
    </style>
  </head>
  <body>
    <h1>Slack app install {{.Heading}}</h1>
    {{if .Error}}
      <p>Slack reported an error: <code>{{.Error}}</code></p>
      <p>Close this tab and re-run the install from the CLI.</p>
    {{else if .Code}}
      <p>Approved{{if .Handle}} for agent <strong>@{{.Handle}}</strong>{{end}}.
      Paste this code into the waiting
      <code>crewlet slack provision</code> prompt:</p>
      <p><code>{{.Code}}</code></p>
    {{else}}
      <p>No <code>code</code> query parameter — open this page via the
      authorize URL printed by <code>crewlet slack provision</code>.</p>
    {{end}}
    <p class="muted">This page is served by the Crewlet API for
    <code>crewlet slack provision</code>. The code expires after 10
    minutes and is useless without the app's client secret.</p>
  </body>
</html>
`))

type slackOAuthView struct{ Heading, Error, Code, Handle string }

// slackOAuthLanding serves GET /webhooks/slack-oauth.
//
// Every provisioned Slack app names this as its OAuth redirect. After the
// operator clicks Allow, Slack redirects here with a temporary code and the
// agent handle in state; the page shows the code for pasting back into the
// waiting CLI prompt.
//
// No engine, no queue, no auth — and no secret either: the code alone grants
// nothing without the app's client secret, which only the provisioning CLI
// holds. It is a function rather than a method for exactly that reason.
func slackOAuthLanding(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	view := slackOAuthView{
		Error:  q.Get("error"),
		Code:   q.Get("code"),
		Handle: q.Get("state"),
	}
	status := http.StatusOK
	switch {
	case view.Error != "":
		view.Heading, status = "failed", http.StatusBadRequest
	case view.Code == "":
		status = http.StatusBadRequest
	default:
		view.Heading = "approved"
		log.Info("slack_oauth_code_displayed", "handle", view.Handle)
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	// Rendered straight to the response: the template is parsed at
	// startup, so the only way this errors is a broken connection, and
	// there is no second status to write by then.
	if err := slackOAuthPage.Execute(w, view); err != nil {
		log.Warn("slack_oauth_page_failed", "error", err)
	}
}
