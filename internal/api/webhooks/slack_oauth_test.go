package webhooks_test

import (
	"context"
	"html"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/webhooks"
	"github.com/crewlet/crewlet/internal/slack"
)

func getLanding(t *testing.T, e *edge, query string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/webhooks/slack-oauth?"+query, nil)
	res := httptest.NewRecorder()
	e.mux.ServeHTTP(res, req)
	return res
}

func TestALinkFromTheRetiredCommandSaysWhereInstallsAreMadeNow(t *testing.T) {
	t.Parallel()
	// `crewlet slack provision` put the bare handle in the state and showed
	// the code for its prompt; nothing waits on it now, so the code is not
	// shown and the page says where installs are made.
	e := newEdge(t)
	res := getLanding(t, e, "code=abc123&state=ceo")
	if res.Code != http.StatusGone {
		t.Fatalf("got %d", res.Code)
	}
	page := res.Body.String()
	if strings.Contains(page, "abc123") {
		t.Error("the page shows a code nothing is waiting to use")
	}
	if !strings.Contains(page, "Agents tab") {
		t.Error("the page does not say where installs are made now")
	}
}

// A REFUSAL IS SAID IN THE PAGE'S OWN WORDS: Slack's code is the log's, and
// the remedy follows where the install began.
func TestTheOAuthLandingSaysWhatWentWrong(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	for _, tc := range []struct {
		query, says, remedy string
	}{
		{"error=access_denied", "The install was cancelled in Slack.", "app's settings in Slack"},
		{"error=invalid_scope", "Slack didn't approve the install.", "app's settings in Slack"},
		{"error=access_denied&state=sre", "The install was cancelled in Slack.", "Agents tab"},
	} {
		res := getLanding(t, e, tc.query)
		if res.Code != http.StatusBadRequest {
			t.Fatalf("%s: got %d, want 400", tc.query, res.Code)
		}
		body := html.UnescapeString(res.Body.String())
		if !strings.Contains(body, tc.says) || !strings.Contains(body, tc.remedy) {
			t.Errorf("%s: the page does not say %q with the remedy naming %q:\n%s",
				tc.query, tc.says, tc.remedy, body)
		}
		if strings.Contains(body, "access_denied") || strings.Contains(body, "invalid_scope") {
			t.Errorf("%s: the page printed Slack's code", tc.query)
		}
		if got := res.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s: Cache-Control = %q, want no-store", tc.query, got)
		}
	}
}

func TestTheOAuthLandingReachedDirectlyExplainsItself(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	res := getLanding(t, e, "")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", res.Code)
	}
	if !strings.Contains(res.Body.String(), "Agents tab") {
		t.Error("the page does not say where an install begins")
	}
}

func TestTheOAuthLandingEscapesWhatItIsGiven(t *testing.T) {
	t.Parallel()
	// Every value on this page comes from a query string an attacker
	// controls, and it is rendered into a browser. The page is served by
	// the same origin as the dashboard, so script running here runs beside
	// the operator's API token.
	e := newEdge(t)
	payload := `<script>alert(1)</script>`
	for _, query := range []string{
		"code=" + payload,
		"error=" + payload,
		"code=ok&state=" + payload,
	} {
		res := getLanding(t, e, query)
		if strings.Contains(res.Body.String(), "<script>") {
			t.Fatalf("query %q rendered unescaped markup:\n%s", query, res.Body)
		}
	}
}

func TestTheOAuthLandingNeedsNoToken(t *testing.T) {
	t.Parallel()
	// A browser reaches it mid-install with nothing in hand, which is why
	// it sits under the guard's /webhooks/ exemption. It also holds no
	// secret: the code alone grants nothing without the app's client
	// secret, which only the provisioning CLI has.
	e := newEdge(t)
	*e.secrets = webhooks.Secrets{}
	if got := getLanding(t, e, "code=abc").Code; got != http.StatusOK {
		t.Fatalf("got %d — the landing page must not depend on any secret", got)
	}
}

// stubInstaller answers one completion.
type stubInstaller struct {
	handle string
	err    error
	state  string
	code   string
}

func (s *stubInstaller) Complete(_ context.Context, state, code string) (string, error) {
	s.state, s.code = state, code
	return s.handle, s.err
}

const signedState = "crewlet_rt1.tag.swe~A0SWE~nonce.4102444800.sig"

// A SIGNED STATE IS FINISHED HERE: the engine completes the install, and the
// page says so without showing the code.
func TestTheLandingFinishesASignedInstall(t *testing.T) {
	t.Parallel()
	installer := &stubInstaller{handle: "swe"}
	e := newEdge(t, func(o *webhooks.Options) { o.SlackInstalls = installer })
	res := getLanding(t, e, "code=code-secret-1&state="+signedState)
	if res.Code != http.StatusOK {
		t.Fatalf("got %d", res.Code)
	}
	page := res.Body.String()
	if !strings.Contains(page, "@swe") || !strings.Contains(page, "its bot token") {
		t.Errorf("the page does not say the install finished: %s", page)
	}
	if strings.Contains(page, "code-secret-1") {
		t.Error("the page shows the code")
	}
	if installer.state != signedState || installer.code != "code-secret-1" {
		t.Errorf("the installer was handed %q, %q", installer.state, installer.code)
	}
}

// A REFUSED INSTALL NAMES THE REASON AND THE REMEDY, at the refusal's status,
// and never the code.
func TestTheLandingNamesWhyAnInstallWasRefused(t *testing.T) {
	t.Parallel()
	installer := &stubInstaller{err: &slack.InstallRefusal{
		Status: http.StatusConflict, Reason: "that approval belongs to another agent's Slack app",
		Remedy: "Use Install on the agent's own row",
	}}
	e := newEdge(t, func(o *webhooks.Options) { o.SlackInstalls = installer })
	res := getLanding(t, e, "code=code-secret-2&state="+signedState)
	page := res.Body.String()
	if res.Code != http.StatusConflict || !strings.Contains(page, "another agent") ||
		!strings.Contains(page, "own row") || strings.Contains(page, "code-secret-2") {
		t.Errorf("got %d: %s", res.Code, page)
	}

	unwired := newEdge(t)
	if res := getLanding(t, unwired, "code=x&state="+signedState); res.Code != http.StatusNotImplemented {
		t.Errorf("an engine with no installer answered %d", res.Code)
	}
}

// A REFUSAL THAT CLEARS ON ITS OWN SAYS WHEN.
func TestARetryableInstallRefusalSaysWhen(t *testing.T) {
	t.Parallel()
	installer := &stubInstaller{err: &slack.InstallRefusal{
		Status: http.StatusServiceUnavailable, Reason: "this engine could not confirm the install link was unused",
		Remedy: "Try again in a moment", RetryAfter: 30 * time.Second,
	}}
	e := newEdge(t, func(o *webhooks.Options) { o.SlackInstalls = installer })
	res := getLanding(t, e, "code=x&state="+signedState)
	if res.Header().Get("Retry-After") != "30" {
		t.Errorf("Retry-After = %q", res.Header().Get("Retry-After"))
	}
}
