package gitlab_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlab"
)

// instance is a fake GitLab that records what it was asked.
type instance struct {
	server *httptest.Server
	paths  []string
	tokens []string
	query  []string
	status int
	body   string
}

func fakeInstance(t *testing.T) *instance {
	t.Helper()
	inst := &instance{status: http.StatusOK, body: "[]"}
	inst.server = httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) {
			inst.paths = append(inst.paths, r.URL.Path)
			inst.tokens = append(inst.tokens, r.Header.Get("PRIVATE-TOKEN"))
			inst.query = append(inst.query, r.URL.RawQuery)
			w.WriteHeader(inst.status)
			w.Write([]byte(inst.body))
		}))
	t.Cleanup(inst.server.Close)
	return inst
}

func client(t *testing.T, url string) *gitlab.Client {
	t.Helper()
	c, err := gitlab.NewClient(gitlab.ClientOptions{URL: url, Token: "glpat-fake"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

// AN OPERATOR WRITES THE INSTANCE URL; A PROVISIONER WRITES THE API BASE.
// Accepting both is the difference between a working config and a 404 on
// every call with no clue why.
func TestEitherFormOfTheUrlReachesTheSameEndpoint(t *testing.T) {
	t.Parallel()
	for _, form := range []string{
		"https://gitlab.example.com",
		"https://gitlab.example.com/",
		"https://gitlab.example.com/api/v4",
		"https://gitlab.example.com/api/v4/",
	} {
		c, err := gitlab.NewClient(gitlab.ClientOptions{URL: form, Token: "t"})
		if err != nil {
			t.Fatalf("NewClient(%q): %v", form, err)
		}
		if got := c.URL(); got != "https://gitlab.example.com" {
			t.Fatalf("NewClient(%q).URL() = %q", form, got)
		}
	}
}

func TestAClientNeedsAnInstanceAndACredential(t *testing.T) {
	t.Parallel()
	if _, err := gitlab.NewClient(gitlab.ClientOptions{Token: "t"}); err == nil {
		t.Fatal("a client with no url was accepted")
	}
	if _, err := gitlab.NewClient(gitlab.ClientOptions{URL: "https://x"}); err == nil {
		t.Fatal("a client with no token was accepted")
	}
}

// PRIVATE-TOKEN, not a bearer: bearer is for OAuth tokens only and 401s on a
// personal access token — the same credential, rejected purely on which
// header carried it.
func TestTheCredentialTravelsAsAPrivateToken(t *testing.T) {
	t.Parallel()
	inst := fakeInstance(t)
	inst.body = `{"username":"crewlet-engine"}`
	if _, err := client(t, inst.server.URL).Me(t.Context()); err != nil {
		t.Fatalf("Me: %v", err)
	}
	if got := inst.tokens[0]; got != "glpat-fake" {
		t.Fatalf("PRIVATE-TOKEN = %q", got)
	}
}

// The boot identity check names the account, which is what stops the engine
// registering itself under a username somebody assumed.
func TestMeReportsTheAccountFolded(t *testing.T) {
	t.Parallel()
	inst := fakeInstance(t)
	inst.body = `{"username":"Crewlet-Engine","id":9}`
	got, err := client(t, inst.server.URL).Me(t.Context())
	if err != nil {
		t.Fatalf("Me: %v", err)
	}
	if got != "crewlet-engine" {
		t.Fatalf("Me = %q, want it lowercased", got)
	}
}

// The two collections live at different paths, and a lookup sent to the
// wrong one returns a perfectly valid list of the wrong thread's people.
func TestEachKindReadsItsOwnCollection(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ kind, path string }{
		{"merge_request", "/api/v4/projects/7/merge_requests/42/participants"},
		{"issue", "/api/v4/projects/7/issues/42/participants"},
		{"", "/api/v4/projects/7/issues/42/participants"},
	} {
		inst := fakeInstance(t)
		if _, err := client(t, inst.server.URL).
			ParticipantsOf(t.Context(), 7, tc.kind, 42); err != nil {
			t.Fatalf("ParticipantsOf(%q): %v", tc.kind, err)
		}
		if got := inst.paths[0]; got != tc.path {
			t.Fatalf("kind %q read %q, want %q", tc.kind, got, tc.path)
		}
	}
}

func TestParticipantsComeBackFolded(t *testing.T) {
	t.Parallel()
	inst := fakeInstance(t)
	inst.body = `[{"username":"Lead-Bot"},{"username":" swe-bot "},
		{"username":""},{"id":3}]`
	got, err := client(t, inst.server.URL).
		ParticipantsOf(t.Context(), 7, "issue", 42)
	if err != nil {
		t.Fatalf("ParticipantsOf: %v", err)
	}
	if !slices.Equal(got, []string{"lead-bot", "swe-bot"}) {
		t.Fatalf("participants = %v", got)
	}
}

// ONE PAGE, never a cursor walk: this runs inside the inbound consumer,
// before a delivery is acked, and an unbounded crawl there stalls the
// fleet's whole notification path.
func TestTheLookupAsksForOnePageAndStops(t *testing.T) {
	t.Parallel()
	inst := fakeInstance(t)
	// A full page, which is what would tempt a walker to ask for more.
	var rows []string
	for i := range 100 {
		rows = append(rows, `{"username":"u`+string(rune('a'+i%26))+`"}`)
	}
	inst.body = "[" + strings.Join(rows, ",") + "]"

	if _, err := client(t, inst.server.URL).
		ParticipantsOf(t.Context(), 7, "issue", 42); err != nil {
		t.Fatalf("ParticipantsOf: %v", err)
	}
	if len(inst.paths) != 1 {
		t.Fatalf("the lookup spent %d requests on a full page", len(inst.paths))
	}
	if got := inst.query[0]; !strings.Contains(got, "per_page=100") {
		t.Fatalf("query = %q, want a bounded page", got)
	}
}

// A REFUSED LOOKUP IS AN ERROR, never an empty list. The parser degrades to
// the payload's assignees on an error and treats an empty list as "nobody
// else is watching" — collapsing the two would silently narrow a thread's
// reach on every 401.
func TestARefusedLookupIsAnErrorNotAnEmptyThread(t *testing.T) {
	t.Parallel()
	inst := fakeInstance(t)
	inst.status, inst.body = http.StatusUnauthorized, `{"message":"401 Unauthorized"}`

	got, err := client(t, inst.server.URL).ParticipantsOf(t.Context(), 7, "issue", 42)
	if err == nil {
		t.Fatalf("a 401 came back as %v with no error", got)
	}
	if !strings.Contains(err.Error(), "401") {
		t.Fatalf("the error does not name the status: %v", err)
	}
}

// The adapter must not dereference a client it does not have: a company with
// no engine token still routes from what its payloads name, and a panic here
// would kill the inbound consumer rather than one integration.
func TestTheAdapterWithNoClientReportsRatherThanPanics(t *testing.T) {
	t.Parallel()
	got, err := gitlab.Lookup{}.Of(t.Context(), 7, "issue", 42)
	if err == nil {
		t.Fatalf("a lookup with no client answered %v", got)
	}
}
