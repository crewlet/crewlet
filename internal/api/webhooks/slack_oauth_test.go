package webhooks_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestTheOAuthLandingShowsTheCodeToPaste(t *testing.T) {
	t.Parallel()
	// This page is the whole hand-off in `crewlet slack provision`: the
	// operator clicks Allow, Slack redirects here, and the code shown is
	// what the waiting CLI prompt is asking for.
	e := newEdge(t)
	res := getLanding(t, e, "code=abc123&state=ceo")
	if res.Code != http.StatusOK {
		t.Fatalf("got %d", res.Code)
	}
	page := res.Body.String()
	if !strings.Contains(page, "abc123") {
		t.Error("the page does not show the code, so the install cannot be completed")
	}
	if !strings.Contains(page, "@ceo") {
		t.Error("the page does not say which agent was installed")
	}
}

func TestTheOAuthLandingSaysWhatWentWrong(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	res := getLanding(t, e, "error=access_denied")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", res.Code)
	}
	if !strings.Contains(res.Body.String(), "access_denied") {
		t.Error("the page does not report Slack's reason")
	}
}

func TestTheOAuthLandingReachedDirectlyExplainsItself(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	res := getLanding(t, e, "")
	if res.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", res.Code)
	}
	if !strings.Contains(res.Body.String(), "crewlet slack provision") {
		t.Error("the page does not say where the authorize URL comes from")
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
	if res := getLanding(t, unwired, "code=x&state="+signedState); res.Code != http.StatusServiceUnavailable {
		t.Errorf("an engine with no installer answered %d", res.Code)
	}
}
