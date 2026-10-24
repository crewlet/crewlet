package webhooks_test

import (
	"html"
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

// A REFUSAL IS SAID IN THE PAGE'S WORDS, never Slack's code: a person reads
// "access_denied" as a fault of their own access, and the query is whatever
// the address that brought the browser here says, printed on the dashboard's
// origin. The code is the log's.
//
// AND ITS REMEDY IS THE PATH THE PERSON TOOK: an install begun from the CLI
// carries a state and goes back to the command, and one begun from the app's
// own settings page carries none and goes back there. Every refusal told
// everybody to run a command the manual path never used.
//
// Mutation: put the query's error back on the page, and the code reaches it;
// tell every refusal to run the command, and the manual cases fail.
func TestTheOAuthLandingSaysWhatWentWrong(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	for _, path := range []struct {
		state, remedy, never string
	}{
		{"&state=ceo", "crewlet slack provision", "app's settings in Slack"},
		{"", "Go back to the app's settings in Slack and install it again.", "crewlet slack provision"},
	} {
		for query, says := range map[string]string{
			"error=access_denied":                                 "The install was cancelled in Slack.",
			"error=invalid_team_for_non_distributed_app":          "Slack didn't approve the install.",
			"error=Your+workspace+was+suspended,+see+example.com": "Slack didn't approve the install.",
		} {
			query += path.state
			res := getLanding(t, e, query)
			if res.Code != http.StatusBadRequest {
				t.Fatalf("%s: got %d, want 400", query, res.Code)
			}
			// AS A PERSON READS IT: the template escapes an apostrophe.
			page := html.UnescapeString(res.Body.String())
			if !strings.Contains(page, says) {
				t.Errorf("%s: the page does not say %q", query, says)
			}
			for _, raw := range []string{"access_denied", "invalid_team", "suspended", "example.com"} {
				if strings.Contains(page, raw) {
					t.Errorf("%s: the page prints the query's own %q", query, raw)
				}
			}
			if !strings.Contains(page, path.remedy) || strings.Contains(page, path.never) {
				t.Errorf("%s: the page does not say to start again with %q:\n%s",
					query, path.remedy, page)
			}
		}
	}
}

// A CODE THAT CAN'T BE SLACK'S IS NO CODE: the page prints the code under
// "Approved for agent", on the dashboard's origin, so a sentence carried in
// the query's `code` was printed as though Crewlet said it.
//
// Mutation: show the code whatever it is, and the stranger's words appear.
func TestTheOAuthLandingShowsOnlyACodeSlackCouldHaveSent(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	for _, query := range []string{
		"code=Your+workspace+is+suspended.+Call+support+at+example.com&state=ceo",
		"code=Your+workspace+is+suspended.+Call+support+at+example.com",
		"code=" + strings.Repeat("a", 256) + "&state=ceo",
	} {
		res := getLanding(t, e, query)
		page := html.UnescapeString(res.Body.String())
		if res.Code != http.StatusBadRequest {
			t.Errorf("%.40s: got %d, want 400", query, res.Code)
		}
		if strings.Contains(page, "suspended") || strings.Contains(page, strings.Repeat("a", 256)) ||
			strings.Contains(page, "Approved") {
			t.Errorf("%.40s: the page shows a code Slack couldn't have sent:\n%s", query, page)
		}
	}
	// AND ONE IT COULD is shown whole.
	const code = "4724469134.4644010092847.232b4e6d82c333b475fc30f5f5a341c2"
	if page := getLanding(t, e, "code="+code+"&state=ceo").Body.String(); !strings.Contains(page, code) {
		t.Errorf("the page does not show a code Slack sends:\n%s", page)
	}
}

// A STATE THAT CANNOT BE A HANDLE NAMES NO AGENT: the page still hands the
// waiting CLI its code, without claiming whom it was approved for.
//
// Mutation: show the state whatever it is, and the stranger's words appear.
func TestTheOAuthLandingNamesOnlyAHandle(t *testing.T) {
	t.Parallel()
	e := newEdge(t)
	page := getLanding(t, e, "code=abc123&state=Your+install+expired,+call+us").Body.String()
	if strings.Contains(page, "install expired") {
		t.Error("the page names an agent the state cannot be")
	}
	if !strings.Contains(page, "abc123") || !strings.Contains(page, "Paste this code") {
		t.Errorf("the CLI flow lost its code to paste:\n%s", page)
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
