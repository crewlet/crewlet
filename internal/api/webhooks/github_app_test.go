package webhooks_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/webhooks"
)

// stubFlow is an app creation that has already been begun somewhere else.
//
// It COUNTS the calls the landing makes, because what this route may do is as
// much the contract as what it renders: it is unauthenticated, so a method
// reached from it is a method anyone can reach.
type stubFlow struct {
	seat      string
	err       error
	install   string
	completes int
}

func (f *stubFlow) Complete(context.Context, string, string) (string, error) {
	f.completes++
	return f.seat, f.err
}

func (f *stubFlow) InstallURL(string) string { return f.install }

// landing drives GET /webhooks/github-app and returns the page.
func landing(t *testing.T, flow webhooks.AppCompleter, query string) *httptest.ResponseRecorder {
	t.Helper()
	return landingOn(t, newEdge(t, func(o *webhooks.Options) { o.AppFlow = flow }), query)
}

// landingOn drives one arrival at an edge a test built itself, so a test about
// what a SECOND arrival does can reuse the receiver rather than a fresh one.
func landingOn(t *testing.T, e *edge, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/webhooks/github-app?"+query, nil)
	res := httptest.NewRecorder()
	e.mux.ServeHTTP(res, req)
	return res
}

// countingRechecker records how many times the route asked the loop to look.
type countingRechecker struct {
	mu   sync.Mutex
	asks int
}

func (c *countingRechecker) RecheckGitHub() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.asks++
}

func (c *countingRechecker) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.asks
}

// THE SECOND ACT FOLLOWS THE FIRST WITHOUT BEING ASKED.
//
// Creating an agent's app and installing it are two clicks at GitHub, and an
// operator who has just done the first is already going to do the second. A
// page that stops them with a button in between is a halt in the middle of one
// errand, so the page follows the link itself after a countdown, and the
// button stays for anyone who would rather not wait.
func TestTheCreatedAppPageTakesTheOperatorOnToTheInstall(t *testing.T) {
	t.Parallel()
	flow := &stubFlow{
		seat:    "sre-lead",
		install: "https://github.com/organizations/acme/settings/apps/acme-sre-lead/installations",
	}
	res := landing(t, flow, "code=abc&state=xyz")
	if res.Code != http.StatusOK {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	body := res.Body.String()

	if !strings.Contains(body, flow.install) {
		t.Fatalf("the page carries no install link: %s", body)
	}
	if !strings.Contains(body, `data-seconds="5"`) {
		t.Errorf("the page has no countdown to the install")
	}
	// THE ADDRESS IS NEVER WRITTEN INTO THE SCRIPT. It is read back off the
	// link, so a URL never enters a JavaScript context and there is no
	// escaping question here to get wrong.
	script := body[strings.Index(body, "<script>"):]
	if strings.Contains(script, "github.com") {
		t.Errorf("the install URL was interpolated into the page's script")
	}
	// AND THE BUTTON STAYS, because the countdown is a convenience and the
	// click is the flow: a browser with scripting off must still get there.
	if !strings.Contains(body, "Install on GitHub") {
		t.Error("the install button is gone, so a page with no scripting is a dead end")
	}
}

// A PAGE WITH NOWHERE TO SEND ANYBODY COUNTS DOWN TO NOTHING.
//
// The install arrival and a refusal both render this same page, and a timer
// on either would either reload the page under the reader or navigate to an
// address the view does not have.
func TestAPageWithNoInstallLinkHasNoCountdown(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		flow  webhooks.AppCompleter
		query string
	}{
		{"the install arrival", &stubFlow{}, "installed=sre-lead&installation_id=42"},
		{
			"a creation GitHub refused",
			&stubFlow{seat: "sre-lead", err: errors.New("that organization refused it")},
			"code=abc&state=xyz",
		},
		{"a creation with no code to convert", &stubFlow{}, "state=xyz"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			body := landing(t, tc.flow, tc.query).Body.String()
			if strings.Contains(body, `id="countdown"`) || strings.Contains(body, "<script") {
				t.Errorf("a page with no install link counts down to nothing")
			}
		})
	}
}

// THE INSTALL ARRIVAL WRITES NOTHING, however complete the query looks.
//
// This route is unauthenticated — a redirect from GitHub carries no engine
// credential — so everything in its query is attacker-supplied. The `code`
// arm survives that because a signed state stands in for the credential; the
// install arm has no state to check, because an agent's app is private and
// GitHub sends none back through the page such an app is installed from.
//
// So a well-formed install arrival is still only a page. It reads as the
// hostile case it is: `installed=` names any seat the caller likes and
// `installation_id=` any number, and if either reached the company document
// then anyone who can reach this port could point a seat's GitHub identity
// wherever they wanted and mint a config revision per request while doing it.
//
// The reconcile loop adopts the real installation by LISTING the app's own
// installations, which is the path that can tell a true id from a typed one.
func TestTheInstallArrivalWritesNothing(t *testing.T) {
	t.Parallel()
	for _, query := range []string{
		"installed=sre-lead&installation_id=159853568",
		"installed=sre-lead&installation_id=1&state=forged",
		"installed=../../etc&installation_id=-1",
	} {
		t.Run(query, func(t *testing.T) {
			t.Parallel()
			flow := &stubFlow{}
			res := landing(t, flow, query)
			if res.Code != http.StatusOK {
				t.Fatalf("got %d: %s", res.Code, res.Body)
			}
			// Complete is the ONLY method the landing may reach, and only
			// down the arm that verified a state first. An install
			// arrival that reached any of the flow at all would be this
			// route acting on a query nobody signed.
			if flow.completes != 0 {
				t.Errorf("the install arrival called the app flow %d times; "+
					"an unauthenticated query must reach nothing that writes",
					flow.completes)
			}
		})
	}
}

// AND IT STILL TELLS THE OPERATOR WHERE THEY STAND. Writing nothing is not
// the same as saying nothing: the page names the seat GitHub sent and says
// the loop will pick the installation up, which is the message this arm
// already showed whenever the adoption failed.
func TestTheInstallArrivalStillNamesTheSeatAndTheWait(t *testing.T) {
	t.Parallel()
	res := landing(t, &stubFlow{}, "installed=sre-lead&installation_id=159853568")
	body := res.Body.String()
	// AND THE WAIT IS THE REAL ONE, on a build with no loop to ask. It said
	// "usually within a minute", which is true only for the first few ticks
	// of a backoff that runs to ten minutes — so an operator who installed
	// the app and watched a card for a minute read the silence as the
	// install not having taken. Recheck is named because it makes the wait
	// zero. Where a loop IS wired the page says something else entirely:
	// see [TestTheInstallArrivalAsksTheLoopToLookNow].
	for _, want := range []string{
		"sre-lead", "App installed for", "next reconcile pass",
		"ten minutes", "Recheck",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not mention %q:\n%s", want, body)
		}
	}
}

// THE INSTALL ARRIVAL ASKS THE LOOP TO LOOK NOW.
//
// Writing nothing was right and waiting was not. The admin cadence is a
// backoff from fifteen seconds to ten minutes, and what it backs off from is
// asking a person to act at their third-party app — so the instant they do it
// is the instant the wait is longest. Measured: an install finished in about
// eight seconds, then minutes of a card still asking for it, reloaded by hand,
// read as the install not having worked.
//
// The ask is not a write and carries nothing. No installation id travels with
// it and nothing about the query is believed: the pass that follows is the
// ordinary verified one, listing the app's own installations. That is exactly
// what makes it safe from a route nobody signed — which is what the
// still-passing TestTheInstallArrivalWritesNothing beside this pins.
func TestTheInstallArrivalAsksTheLoopToLookNow(t *testing.T) {
	t.Parallel()
	look := &countingRechecker{}
	e := newEdge(t, func(o *webhooks.Options) {
		o.AppFlow = &stubFlow{}
		o.Recheck = look
	})
	res := landingOn(t, e, "installed=sre-lead&installation_id=159853568")
	if res.Code != http.StatusOK {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	if look.count() != 1 {
		t.Fatalf("the arrival asked the loop %d times, so the card waits out "+
			"a backoff for something already done", look.count())
	}
	// AND THE PAGE SAYS THE TRUE THING. Promising a ten-minute wait it is
	// not going to take teaches an operator to stop believing the page.
	body := res.Body.String()
	if !strings.Contains(body, "looking at GitHub now") {
		t.Errorf("the page still describes a wait it is not taking:\n%s", body)
	}
	if strings.Contains(body, "ten minutes") {
		t.Errorf("the page names a cadence it just short-circuited:\n%s", body)
	}
}

// AND AN UNAUTHENTICATED CALLER CANNOT DRIVE IT.
//
// A recheck believes nothing, so the exposure is work rather than trust: an
// engine made to run GitHub passes back to back spends a company's GitHub
// rate limit. Two bounds sit under this one — the loop coalesces asks that
// arrive before its tick, and a pass takes the surface lease — and this is
// the third, because together they still leave a caller able to drive passes
// continuously.
func TestRepeatedInstallArrivalsAskOnce(t *testing.T) {
	t.Parallel()
	look := &countingRechecker{}
	// THE CLOCK IS DRIVEN, so the WINDOW is what this pins rather than the
	// fixture's frozen instant. Written against `pinned` it passed with the
	// interval set to a nanosecond — a guard held up by the clock not
	// moving is a guard nothing is checking.
	now := pinned
	e := newEdge(t, func(o *webhooks.Options) {
		o.AppFlow = &stubFlow{}
		o.Recheck = look
		o.Now = func() time.Time { return now }
	})
	const arrival = "installed=sre-lead&installation_id=1"

	landingOn(t, e, arrival)
	if look.count() != 1 {
		t.Fatalf("the first arrival asked %d times", look.count())
	}
	// INSIDE THE WINDOW, nothing — however many arrive.
	// Cumulative 0s, 1s and 4s — strictly inside, since the boundary
	// itself is the first arrival that is allowed through.
	for _, step := range []time.Duration{0, time.Second, 3 * time.Second} {
		now = now.Add(step)
		landingOn(t, e, arrival)
	}
	if look.count() != 1 {
		t.Errorf("arrivals inside the window asked the loop %d times; an "+
			"unauthenticated route must not be a lever on somebody's GitHub "+
			"rate limit", look.count())
	}
	// AND PAST IT, ONE MORE — because a multi-agent company installs
	// several apps back to back and each of those is a real arrival.
	now = now.Add(2 * time.Second)
	landingOn(t, e, arrival)
	if look.count() != 2 {
		t.Errorf("asks = %d, want the next install after the window to get "+
			"its own look: a company setting up four agents does them in a "+
			"row", look.count())
	}
}
