package webhooks_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	e := newEdge(t, func(o *webhooks.Options) { o.AppFlow = flow })
	req := httptest.NewRequest(http.MethodGet, "/webhooks/github-app?"+query, nil)
	res := httptest.NewRecorder()
	e.mux.ServeHTTP(res, req)
	return res
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
	for _, want := range []string{"sre-lead", "App installed for", "next pass"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page does not mention %q:\n%s", want, body)
		}
	}
}
