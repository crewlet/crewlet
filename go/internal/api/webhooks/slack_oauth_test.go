package webhooks_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/webhooks"
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
