package webhooks

import (
	"html/template"
	"net/http"
	"strings"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/iam"
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
    <title>Crewlet: Slack app install</title>
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
      <p>{{.Error}}</p>
      {{if .CLI}}<p>Close this tab and run <code>crewlet slack provision</code> again.</p>
      {{else}}<p>Go back to the app's settings in Slack and install it again.</p>{{end}}
    {{else if and .Code .CLI}}
      <p>{{if .Handle}}Approved for agent <strong>@{{.Handle}}</strong>.{{else}}Approved.{{end}}
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
      agent's section of Slack's settings, under Company › Integrations.</p>
      <p class="muted">You reached this page because you installed the app
      from its own settings rather than from
      <code>crewlet slack provision</code>, which is the ordinary way to do it
      by hand. You can close this tab.</p>
    {{else}}
      <p>Slack didn't send an install code with this page. Open it by
      installing a Crewlet agent's Slack app, or from the link
      <code>crewlet slack provision</code> prints.</p>
    {{end}}
  </body>
</html>
`))

// slackOAuthPolicy is the page's Content-Security-Policy: its one inline
// style, by hash, and no script at all.
//
// Every value on this page comes from the query string, and the page shares an
// origin with the dashboard, whose requests carry a signed-in person's session.
// The template's escaping is the first defence; this is the one that holds if
// the escaping is ever wrong.
var slackOAuthPolicy = pagepolicy.MustForTemplate(slackOAuthPage, slackOAuthView{})

type slackOAuthView struct {
	Heading, Error, Code string

	// Handle is the agent the CLI's install was for, shown only when it is
	// one a seat could have: the state is whatever the address that brought
	// the browser here says.
	Handle string

	// CLI is the install having begun at the CLI's own authorize URL,
	// whose state names the agent: the code goes to its waiting prompt.
	CLI bool
}

// What the page tells a person Slack refused, in its own words rather than
// Slack's code: the code is the log's.
const (
	// slackCancelled is Slack's `access_denied`: the person declined.
	slackCancelled = "The install was cancelled in Slack."

	// slackRefused is any other refusal Slack sent back.
	slackRefused = "Slack didn't approve the install."
)

// maxSlackCode is the longest `code` this page treats as one Slack sent.
//
// Slack documents no length. Its codes are two numeric ids and a hex digest,
// under a hundred characters, and 255 leaves them room to grow while keeping
// the page from printing whatever an address carries.
const maxSlackCode = 255

// slackCode is the query's `code` as the page shows it, or "" where it can't
// be one Slack sent: longer than [maxSlackCode], or holding anything but
// letters, digits, dots, dashes and underscores.
//
// THE CODE IS PRINTED, under "Approved for agent @x. Paste this code", on
// the dashboard's own origin, and the query is whatever the address that
// brought the browser here says. Shown as it came, a code reading "Your
// workspace is suspended. Call support at example.com" was printed as
// though Crewlet said it. A value that can't be a code is no code: the page
// says Slack sent none, which is true of it.
func slackCode(raw string) string {
	if raw == "" || len(raw) > maxSlackCode {
		return ""
	}
	for _, c := range raw {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '-', c == '_':
		default:
			return ""
		}
	}
	return raw
}

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
	state := strings.TrimSpace(q.Get("state"))
	view := slackOAuthView{Code: slackCode(q.Get("code")), CLI: state != ""}
	// ONLY A HANDLE IS NAMED, as the agent the install was for: anything
	// else in the state is no agent this page should claim was approved.
	if iam.ValidSeatHandle(state) {
		view.Handle = state
	}
	status := http.StatusOK
	switch refused := strings.TrimSpace(q.Get("error")); {
	case refused != "":
		// SLACK'S CODE IS THE LOG'S, and the page says what it means: the
		// query is whatever the address that brought the browser here
		// says, printed on the dashboard's origin.
		view.Heading, status = "failed", http.StatusBadRequest
		view.Error = slackRefused
		if refused == "access_denied" {
			view.Error = slackCancelled
		}
		log.Warn("slack_oauth_refused", "error", refused, "handle", view.Handle)
	case view.Code == "":
		status = http.StatusBadRequest
		if q.Get("code") != "" {
			log.Warn("slack_oauth_code_malformed", "handle", view.Handle,
				"length", len(q.Get("code")))
		}
	default:
		view.Heading = "approved"
		// NAMED, because the two arrivals are operationally different: one
		// has a CLI waiting on the code and the other is a person who has
		// to go back for a token. An operator reading the log after being
		// asked "did my install work" needs to see which happened.
		log.Info("slack_oauth_code_displayed", "handle", view.Handle,
			"flow", map[bool]string{true: "cli", false: "manual"}[view.CLI])
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	pagepolicy.Set(w.Header(), slackOAuthPolicy)
	// NEVER STORED: the page carries a code that installs an app for
	// whoever holds the app's client secret, and a browser's cache is a
	// place it would outlive the prompt waiting for it.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	// Rendered straight to the response: the template is parsed at
	// startup, so the only way this errors is a broken connection, and
	// there is no second status to write by then.
	if err := slackOAuthPage.Execute(w, view); err != nil {
		log.Warn("slack_oauth_page_failed", "error", err)
	}
}
