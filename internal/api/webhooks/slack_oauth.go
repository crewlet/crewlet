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
    {{else if and .Code .Handle}}
      <p>Approved for agent <strong>@{{.Handle}}</strong>.
      Paste this code into the waiting
      <code>crewlet slack provision</code> prompt:</p>
      <p><code>{{.Code}}</code></p>
      <p class="muted">This page is served by the Crewlet API for
      <code>crewlet slack provision</code>. The code expires after 10
      minutes and is useless without the app's client secret.</p>
    {{else if .Code}}
      <p>Approved. The app is installed in your workspace, and there is
      nothing to do here.</p>
      <p>Go back to the app's <strong>OAuth &amp; Permissions</strong> page,
      copy the <strong>Bot User OAuth Token</strong>, and paste it into this
      agent's block on the Integrations screen.</p>
      <p class="muted">You reached this page because you installed the app
      from its own settings rather than from
      <code>crewlet slack provision</code>, which is the ordinary way to do it
      by hand. You can close this tab.</p>
    {{else}}
      <p>No <code>code</code> query parameter. Open this page by installing
      a Crewlet agent's Slack app, or through the authorize URL printed by
      <code>crewlet slack provision</code>.</p>
    {{end}}
  </body>
</html>
`))

type slackOAuthView struct{ Heading, Error, Code, Handle string }

// slackOAuthLanding serves GET /webhooks/slack-oauth.
//
// Every provisioned Slack app names this as its OAuth redirect. After the
// operator clicks Allow, Slack redirects here with a temporary code and, when
// the install began at an authorize URL the CLI printed, the agent handle in
// state.
//
// TWO ARRIVALS, and the page has to tell them apart. `state` is set only by
// the CLI's own authorize URL, so a code WITHOUT one is somebody who pressed
// Install to Workspace on the app's settings page: the manual path the setup
// dialog walks an operator through, where there is no waiting prompt to paste
// a code into and the thing they actually need is the bot token, two clicks
// away on the page they just left. Told to paste into a CLI they are not
// running, an operator reasonably concludes the install did not work.
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
		// NAMED, because the two arrivals are operationally different: one
		// has a CLI waiting on the code and the other is a person who has
		// to go back for a token. An operator reading the log after being
		// asked "did my install work" needs to see which happened.
		log.Info("slack_oauth_code_displayed", "handle", view.Handle,
			"flow", map[bool]string{true: "cli", false: "manual"}[view.Handle != ""])
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
