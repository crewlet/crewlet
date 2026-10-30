package webhooks

import (
	"context"
	"errors"
	"html/template"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/pagepolicy"
	"github.com/crewlet/crewlet/internal/slack"
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
      <p>Slack reported an error: <code>{{.Error}}</code></p>
      <p>Close this tab and start the install again: with Install on the
      agent's row in Slack's Agents tab, or from
      <code>crewlet slack provision</code>.</p>
    {{else if .Done}}
      <p>Approved for agent <strong>@{{.Handle}}</strong>. Crewlet has sealed
      its bot token, and the agent starts answering in Slack shortly.</p>
      <p class="muted">You can close this tab.</p>
    {{else if .Reason}}
      <p>{{.Reason}}.</p>
      <p>{{.Remedy}}.</p>
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
      agent's section of Slack's settings, under Company › Integrations.</p>
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

// slackOAuthPolicy is the page's Content-Security-Policy: its one inline
// style, by hash, and no script at all.
//
// Every value on this page comes from the query string, and the page shares an
// origin with the dashboard, whose requests carry a signed-in person's session.
// The template's escaping is the first defence; this is the one that holds if
// the escaping is ever wrong.
var slackOAuthPolicy = pagepolicy.MustForTemplate(slackOAuthPage, slackOAuthView{})

type slackOAuthView struct {
	Heading, Error, Code, Handle string

	// Done is an install this engine finished; Reason and Remedy say why
	// one could not be, and never quote the code or a token.
	Done           bool
	Reason, Remedy string
}

// SlackInstaller finishes an install a seat's install link began
// ([slack.Installs]).
type SlackInstaller interface {
	Complete(ctx context.Context, state, code string) (string, error)
}

// slackOAuthLanding serves GET /webhooks/slack-oauth.
//
// Every Slack app this engine creates names this as its OAuth redirect. After
// the operator clicks Allow, Slack redirects here with a temporary code and
// the state the install link carried.
//
// THREE ARRIVALS, told apart by the state:
//
//   - A SIGNED STATE is an install link the Agents tab minted
//     ([slack.Installs.Link]). The engine finishes it here: the state is
//     verified and spent, the code exchanged, the app checked, and the bot
//     token sealed under the seat's own name. Nothing Slack sent is shown.
//   - A BARE HANDLE is `crewlet slack provision`'s authorize URL, with a
//     prompt waiting on the code: the page shows it to paste, and the command
//     finishes the install through the same completion.
//   - NO STATE is somebody who pressed Install to Workspace on the app's own
//     settings page, who needs the bot token on the page they just left.
//
// The code alone grants nothing without the app's client secret, which only
// the engine and the provisioning command hold.
func (r *Receiver) slackOAuthLanding(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	view := slackOAuthView{Error: q.Get("error"), Code: q.Get("code"), Handle: q.Get("state")}
	status := http.StatusOK
	switch {
	case view.Error != "":
		view.Heading, status = "failed", http.StatusBadRequest
	case slack.IsInstallState(view.Handle):
		status = r.completeSlackInstall(req.Context(), &view)
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
	pagepolicy.Set(w.Header(), slackOAuthPolicy)
	w.WriteHeader(status)
	// Rendered straight to the response: the template is parsed at
	// startup, so the only way this errors is a broken connection, and
	// there is no second status to write by then.
	if err := slackOAuthPage.Execute(w, view); err != nil {
		log.Warn("slack_oauth_page_failed", "error", err)
	}
}

// completeSlackInstall finishes an install a seat's link began, and fills the
// view with what happened. The code is cleared before anything renders.
func (r *Receiver) completeSlackInstall(ctx context.Context, view *slackOAuthView) int {
	state, code := view.Handle, view.Code
	view.Code, view.Handle = "", ""
	if r.slackInstalls == nil {
		view.Heading = "failed"
		view.Reason = "this engine does not finish Slack installs"
		view.Remedy = "Start the install again from the Agents tab of an engine serving the dashboard"
		return http.StatusServiceUnavailable
	}
	handle, err := r.slackInstalls.Complete(ctx, state, code)
	if err == nil {
		view.Heading, view.Done, view.Handle = "approved", true, handle
		log.InfoContext(ctx, "slack_install_completed", "handle", handle)
		return http.StatusOK
	}
	view.Heading = "failed"
	var refusal *slack.InstallRefusal
	if !errors.As(err, &refusal) {
		refusal = &slack.InstallRefusal{
			Status: http.StatusInternalServerError,
			Reason: "the install could not be finished",
			Remedy: "Start the install again with Install on the agent's row in Slack's Agents tab",
		}
	}
	view.Reason, view.Remedy = refusal.Reason, refusal.Remedy
	// THE ERROR IS LOGGED, never rendered: it may name Slack's refusal
	// code, which is fine to log and pointless to show, and it never
	// carries the code or a token ([slack.CompleteSeatInstall]).
	log.WarnContext(ctx, "slack_install_refused", "handle", handle, "error", err.Error())
	return refusal.Status
}
