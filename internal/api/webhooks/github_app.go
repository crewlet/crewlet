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
      .count { margin-top: .75rem; font-size: 13px; color: var(--muted); }
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
          <a class="btn" id="install" href="{{.InstallURL}}">Install on GitHub</a>
          <!-- EMPTY AND HIDDEN UNTIL THE SCRIPT OWNS IT. A page with
               scripting off would otherwise promise a redirect that never
               comes, and the button beside it is the whole flow either way.
               The sentence is written by the script as ONE string rather
               than a number beside its own noun, so the last second reads
               "1 second" and not "1 seconds". -->
          <p class="count" id="countdown" data-seconds="{{.InstallDelay}}" hidden></p>
        </div>
        {{end}}
      </div>
      <p class="foot">
        {{if .InstallURL}}After the install, GitHub brings you back here.
        {{else}}You can close this tab and go back to Crewlet.{{end}}
      </p>
    </div>
    {{if .InstallURL}}
    <script>
      // THE SECOND ACT FOLLOWS THE FIRST WITHOUT BEING ASKED. Creating an
      // app and installing it are two clicks at GitHub, and an operator who
      // has just done the first is already going to do the second: leaving
      // them on a page with a button is a stop in the middle of one errand.
      //
      // The address is read back off the link rather than written into this
      // script, so the URL never enters a JavaScript context and there is no
      // escaping question to get wrong. The delay is read off the element for
      // the same reason: one value, rendered once.
      (function () {
        var link = document.getElementById("install");
        var note = document.getElementById("countdown");
        if (!link || !note) {
          return;
        }
        var left = parseInt(note.getAttribute("data-seconds"), 10);
        if (!(left > 0)) {
          return;
        }
        var say = function (seconds) {
          note.textContent =
            "Taking you there in " + seconds + (seconds === 1 ? " second." : " seconds.");
        };
        say(left);
        note.hidden = false;
        var tick = setInterval(function () {
          left -= 1;
          if (left > 0) {
            say(left);
            return;
          }
          clearInterval(tick);
          note.textContent = "Taking you to GitHub.";
          window.location.href = link.href;
        }, 1000);
        // A PERSON WHO CLICKS FIRST IS NOT SENT TWICE: the timer would fire
        // mid-navigation and reload GitHub's install page under them.
        link.addEventListener("click", function () {
          clearInterval(tick);
          note.hidden = true;
        });
      })();
    </script>
    {{end}}
  </body>
</html>
`))

// installCountdownSeconds is how long the created-app page waits before it
// sends the operator on to the install.
//
// FIVE, and the two failure directions are not symmetrical. Shorter and the
// page is gone before a person has read which agent's app was just made,
// which is the one fact the page exists to report and the one they need if
// anything later goes wrong. Longer and it reads as a page that has finished
// and stopped, so they click the button anyway and the timer was decoration.
const installCountdownSeconds = 5

type githubAppView struct {
	Heading    string
	Message    string
	Error      string
	Seat       string
	InstallURL string
	Done       bool

	// InstallDelay is the countdown, in seconds, before the page follows
	// the install link itself. Zero leaves the button and no timer.
	InstallDelay int
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
		// THE INSTALL ARRIVAL. GitHub names the seat and the installation
		// in the query, and that query is the only thing this route has:
		// it is unauthenticated, so what arrives here is whatever the
		// caller typed.
		//
		// SO NOTHING IS WRITTEN, and that is the whole of this arm.
		// Adopting the id straight off the redirect made the Integrations
		// screen right about a minute sooner, and paid for it by letting
		// anyone who can reach this port record an installation onto any
		// seat that has an app — no signature, no token, no state, and a
		// fresh config revision on every request. The `code` arm below is
		// not exposed that way because a signed state stands in for the
		// credential a redirect cannot carry; there is no equivalent
		// here, since an agent's app is private and GitHub sends no state
		// back through the installations page such an app is installed
		// from.
		//
		// The reconcile loop adopts the same installation on its next
		// pass, having LISTED the app's own installations rather than
		// believed a query — the verified path, and the one that already
		// existed. What that costs is the minute, and the page below has
		// always said so: this is word for word the message it already
		// showed whenever the write failed.
		view.Heading = "App installed for"
		view.Seat = strings.TrimSpace(q.Get("installed"))
		view.Done = true
		view.Message = "Crewlet picks the installation up on its next pass, usually " +
			"within a minute, and the Integrations screen will say so."

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
			if view.InstallURL != "" {
				view.InstallDelay = installCountdownSeconds
			}
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
