package webhooks

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"strconv"
	"time"

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
      <p>{{.Error}}</p>
      {{if .Manual}}<p>Go back to the app's settings in Slack and install it again.</p>
      {{else}}<p>Close this tab and start the install again with Install on the
      agent's row in Slack's Agents tab, under Company › Integrations.</p>{{end}}
    {{else if .Done}}
      <p>Approved for agent <strong>@{{.Handle}}</strong>. Crewlet has sealed
      its bot token, and the agent starts answering in Slack shortly.</p>
      <p class="muted">You can close this tab.</p>
    {{else if .Reason}}
      <p>{{.Reason}}.</p>
      <p>{{.Remedy}}.</p>
    {{else if .Code}}
      <p>Approved. The app is installed in your workspace, and there is
      nothing to do here.</p>
      <p>Go back to the app's <strong>OAuth &amp; Permissions</strong> page,
      copy the <strong>Bot User OAuth Token</strong>, and paste it into this
      agent's section of Slack's settings, under Company › Integrations.</p>
      <p class="muted">You reached this page because you installed the app
      from its own settings rather than with Install on the agent's row in
      Slack's Agents tab, which finishes the install on its own. You can
      close this tab.</p>
    {{else}}
      <p>Slack didn't send an install code with this page. Open it by
      installing a Crewlet agent's Slack app with Install on the agent's row
      in Slack's Agents tab, under Company › Integrations.</p>
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

	// Manual is an install pressed on the app's own settings in Slack,
	// which carries no state: a refusal sends the person back there.
	Manual bool
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
// the page from answering for whatever an address carries.
const maxSlackCode = 255

// slackCode is the query's `code`, or "" where it can't be one Slack sent:
// longer than [maxSlackCode], or holding anything but letters, digits, dots,
// dashes and underscores.
//
// A CODE IS WHAT MAKES THE PAGE SAY "Approved", on the dashboard's own origin,
// and the query is whatever the address that brought the browser here says.
// Taken as it came, `?code=<anything>` with no state was told the app was
// installed. A value that can't be a code is no code: the page says Slack
// sent none, which is true of it.
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
//   - ANY OTHER STATE is a link this engine no longer issues: the retired
//     `crewlet slack provision` put the bare handle there. It is answered with
//     where installs are made now, and the code is not shown, since nothing
//     is waiting to use it.
//   - NO STATE is somebody who pressed Install to Workspace on the app's own
//     settings page, who needs the bot token on the page they just left.
//
// The code alone grants nothing without the app's client secret.
func (r *Receiver) slackOAuthLanding(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	view := slackOAuthView{Code: q.Get("code"), Handle: q.Get("state")}
	status := http.StatusOK
	switch refused := q.Get("error"); {
	case refused != "":
		// SLACK'S CODE IS LOGGED, NOT SHOWN: a person reads access_denied as
		// a fault in their own access, and the query is whatever the address
		// that brought the browser here says, printed on the dashboard's
		// origin.
		view.Error = slackRefused
		if refused == "access_denied" {
			view.Error = slackCancelled
		}
		view.Manual = view.Handle == ""
		log.Warn("slack_oauth_refused", "error", refused)
		view.Code, view.Handle = "", ""
		view.Heading, status = "failed", http.StatusBadRequest
	case slack.IsInstallState(view.Handle):
		status = r.completeSlackInstall(req.Context(), w.Header(), &view)
	case view.Handle != "":
		view.Code, view.Handle = "", ""
		view.Heading, status = "failed", http.StatusGone
		view.Reason = "this install link came from crewlet slack provision, which is retired"
		view.Remedy = "Start the install again with Install on the agent's row in Slack's " +
			"Agents tab, under Company › Integrations"
	case slackCode(view.Code) == "":
		if view.Code != "" {
			log.Warn("slack_oauth_code_malformed", "length", len(view.Code))
		}
		view.Code = ""
		status = http.StatusBadRequest
	default:
		view.Heading = "approved"
		log.Info("slack_oauth_manual_install")
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// NOT CACHED: a code that installs an app should not outlive the page
	// that received it in a browser's cache.
	w.Header().Set("Cache-Control", "no-store")
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
func (r *Receiver) completeSlackInstall(ctx context.Context, header http.Header, view *slackOAuthView) int {
	state, code := view.Handle, view.Code
	view.Code, view.Handle = "", ""
	if r.slackInstalls == nil {
		// NOT IMPLEMENTED HERE, rather than unavailable: this node does not
		// finish installs at all, and waiting would not change that.
		view.Heading = "failed"
		view.Reason = "this engine does not finish Slack installs"
		view.Remedy = "Start the install again from the Agents tab of an engine serving the dashboard"
		return http.StatusNotImplemented
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
	// A REFUSAL THAT CLEARS ON ITS OWN SAYS WHEN, as every refusal of the
	// kind does on this engine.
	if refusal.RetryAfter > 0 {
		header.Set("Retry-After", strconv.Itoa(int(refusal.RetryAfter/time.Second)))
	}
	// THE ERROR IS LOGGED, never rendered: it may name Slack's refusal
	// code, which is fine to log and pointless to show, and it never
	// carries the code or a token ([slack.CompleteSeatInstall]).
	log.WarnContext(ctx, "slack_install_refused", "handle", handle, "error", err.Error())
	return refusal.Status
}
