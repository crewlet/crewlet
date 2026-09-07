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
type stubFlow struct {
	seat      string
	err       error
	install   string
	installed struct {
		seat string
		id   int64
	}
}

func (f *stubFlow) Complete(context.Context, string, string) (string, error) {
	return f.seat, f.err
}

func (f *stubFlow) InstallURL(string) string { return f.install }

func (f *stubFlow) RecordInstall(_ context.Context, seat string, id int64) error {
	f.installed.seat, f.installed.id = seat, id
	return nil
}

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

// THE INSTALLATION IS ADOPTED FROM THE REDIRECT, which is what makes the
// Integrations screen right when the operator gets back to it rather than a
// minute later. The loop would find the same id by listing the app's
// installations, so this is a head start rather than the only path.
func TestTheInstallArrivalAdoptsTheInstallationGitHubNames(t *testing.T) {
	t.Parallel()
	flow := &stubFlow{}
	res := landing(t, flow, "installed=sre-lead&installation_id=159853568")
	if res.Code != http.StatusOK {
		t.Fatalf("got %d: %s", res.Code, res.Body)
	}
	if flow.installed.seat != "sre-lead" || flow.installed.id != 159853568 {
		t.Fatalf("recorded %q/%d", flow.installed.seat, flow.installed.id)
	}
}
