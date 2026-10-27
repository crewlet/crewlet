package github_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/github"
)

// tokenServer mints installation tokens that live an hour from the fake
// clock's now, numbered so a test can tell one mint from the next, and
// records the scope each mint asked for. refuse makes every mint a 401.
type tokenServer struct {
	mu     sync.Mutex
	now    func() time.Time
	mints  int
	scopes []map[string]any
	refuse bool
}

func (s *tokenServer) start(t *testing.T) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/access_tokens") {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.refuse {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
			return
		}
		var scope map[string]any
		_ = json.NewDecoder(r.Body).Decode(&scope)
		s.scopes = append(s.scopes, scope)
		s.mints++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token":      "ghs_issued_" + string(rune('a'+s.mints)),
			"expires_at": s.now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func (s *tokenServer) minted() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mints
}

// clock is a settable fake clock.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// ONE MINT AN HOUR PER INSTALLATION, however many readers ask, and a new one
// exactly when the held token can no longer promise what the reader needs.
//
// The seat's tool servers, its coding runs, the poll keeping a long run's
// token current and the inbound participant lookup all read the same token,
// so minting per read would spend a request per delivery, per poll and per
// launch to learn something good for an hour.
func TestAnIssuerHandsOutTheHeldTokenUntilItRunsShort(t *testing.T) {
	t.Parallel()
	c := &clock{at: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	srv := &tokenServer{now: c.now}
	opts := github.SeatAppOptions{APIBase: srv.start(t)}
	seat := installedSeat(t, "senior-engineer", 7)
	issuer := &github.Issuer{Now: c.now}

	first, err := issuer.Token(t.Context(), opts, seat, github.RunTokenLife)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	for range 3 {
		again, err := issuer.Token(t.Context(), opts, seat, github.ToolTokenLife)
		if err != nil || again.Token != first.Token {
			t.Fatalf("a read inside the hour was handed %q, %v, want the held token", again.Token, err)
		}
	}
	if srv.minted() != 1 {
		t.Fatalf("four reads minted %d tokens, want one", srv.minted())
	}

	// TWENTY MINUTES ON, the held token has forty left: enough for a tool
	// server, not for a run that must be good for fifty.
	c.advance(20 * time.Minute)
	if tool, _ := issuer.Token(t.Context(), opts, seat, github.ToolTokenLife); tool.Token != first.Token {
		t.Error("a tool server was handed a new token while the held one had forty minutes left")
	}
	run, err := issuer.Token(t.Context(), opts, seat, github.RunTokenLife)
	if err != nil || run.Token == first.Token {
		t.Fatalf("a run was handed the forty-minute token (%v), want a fresh one", err)
	}

	// THE FRESH ONE IS WHAT THE OTHERS ARE HANDED NOW, until it in turn
	// reaches its renewal point.
	c.advance(time.Hour - github.RenewBefore)
	renewed, err := issuer.Token(t.Context(), opts, seat, github.ToolTokenLife)
	if err != nil || renewed.Token == run.Token {
		t.Fatalf("at its renewal point the token was handed out again (%v)", err)
	}
	if !github.RenewAt(renewed).Equal(renewed.ExpiresAt.Add(-github.RenewBefore)) {
		t.Errorf("RenewAt = %v, want %v before expiry", github.RenewAt(renewed), github.RenewBefore)
	}
}

// A TOKEN IS NEVER HANDED TO A SCOPE IT WAS NOT MINTED FOR. A seat's tier,
// workflows opt-in or repositories changed by a revision must be issued a
// token narrowed to the new scope, never the one the old scope was cut to.
func TestAnIssuedTokenIsScopedToTheTierAndRepositoriesItWasMintedFor(t *testing.T) {
	t.Parallel()
	c := &clock{at: time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)}
	srv := &tokenServer{now: c.now}
	opts := github.SeatAppOptions{APIBase: srv.start(t)}
	issuer := &github.Issuer{Now: c.now}

	seat := installedSeat(t, "qa", 9)
	seat.Tier, seat.Repos = github.TierReview, []string{"gophrase/backend"}
	reviewer, err := issuer.Token(t.Context(), opts, seat, github.ToolTokenLife)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	seat.Tier = github.TierFullAccess
	writer, err := issuer.Token(t.Context(), opts, seat, github.ToolTokenLife)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if writer.Token == reviewer.Token {
		t.Fatal("a full_access read was handed the token minted for review")
	}
	// The same repositories written differently are the same scope.
	seat.Repos = []string{" GOPHRASE/backend "}
	if again, _ := issuer.Token(t.Context(), opts, seat, github.ToolTokenLife); again.Token != writer.Token {
		t.Error("one repository spelt differently was minted a second token")
	}
	seat.Workflows = true
	workflows, err := issuer.Token(t.Context(), opts, seat, github.ToolTokenLife)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}
	if workflows.Token == writer.Token {
		t.Fatal("a seat opted in to workflow writes was handed the token minted without them")
	}

	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.scopes) != 3 {
		t.Fatalf("%d mints, want review, full_access and full_access with workflows", len(srv.scopes))
	}
	for i, want := range []string{"", "", "write"} {
		perms, _ := srv.scopes[i]["permissions"].(map[string]any)
		if got, _ := perms[github.PermissionWorkflows].(string); got != want {
			t.Errorf("mint %d asked for workflows=%q, want %q", i, got, want)
		}
	}
	perms, _ := srv.scopes[0]["permissions"].(map[string]any)
	if perms["contents"] != "read" || perms["pull_requests"] != "write" {
		t.Errorf("the review mint asked for %v, want the review tier's permissions", perms)
	}
	repos, _ := srv.scopes[0]["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "backend" {
		t.Errorf("the mint was narrowed to %v, want the seat's one repository", repos)
	}
}

// AN APP THAT CANNOT ISSUE ASKS GITHUB NOTHING. Not created, not installed,
// or a key that does not resolve are each a person's step, told on the card;
// a request for one would only be refused.
func TestAnAppThatIsNotInstalledIssuesNothing(t *testing.T) {
	t.Parallel()
	srv := &tokenServer{now: time.Now}
	opts := github.SeatAppOptions{APIBase: srv.start(t)}
	issuer := &github.Issuer{}

	installed := installedSeat(t, "pm", 3)
	for name, seat := range map[string]github.SeatApp{
		"no app":        {Handle: "pm", Tier: github.TierReadOnly},
		"not installed": {Handle: "pm", AppID: 3, Slug: "pm", Key: installed.Key},
		"no key":        {Handle: "pm", AppID: 3, Slug: "pm", InstallationID: 3},
	} {
		if _, err := issuer.Token(t.Context(), opts, seat, github.ToolTokenLife); !errors.Is(err, github.ErrNotInstalled) {
			t.Errorf("%s: err = %v, want ErrNotInstalled", name, err)
		}
	}
	if srv.minted() != 0 {
		t.Errorf("%d requests were made for apps that cannot issue", srv.minted())
	}
}

// A REFUSED MINT SAYS WHY AND CARRIES NO TOKEN, and nothing is held: the next
// read asks again rather than being handed a failure for the hour.
func TestARefusedMintIsAnErrorAndIsAskedAgain(t *testing.T) {
	t.Parallel()
	srv := &tokenServer{now: time.Now, refuse: true}
	opts := github.SeatAppOptions{APIBase: srv.start(t)}
	issuer := &github.Issuer{}
	seat := installedSeat(t, "pm", 3)

	if _, err := issuer.Token(t.Context(), opts, seat, github.ToolTokenLife); err == nil {
		t.Fatal("a refused mint issued a token")
	} else if strings.Contains(err.Error(), "ghs_") {
		t.Errorf("the error carries a token: %v", err)
	}
	srv.mu.Lock()
	srv.refuse = false
	srv.mu.Unlock()
	if _, err := issuer.Token(t.Context(), opts, seat, github.ToolTokenLife); err != nil {
		t.Fatalf("the read after GitHub recovered was refused: %v", err)
	}
}

// ONE NAMING FOR EVERYTHING A SEAT'S APP KEEPS, and every name is one a
// `${VAR}` can reference: the vendor leads, so a handle beginning with a digit
// still makes a name the whole-reference grammar accepts.
func TestASeatsAppVariablesAreReferenceable(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"senior-engineer": "GITHUB_APP_TOKEN_SENIOR_ENGINEER",
		"7th-engineer":    "GITHUB_APP_TOKEN_7TH_ENGINEER",
		"qa":              "GITHUB_APP_TOKEN_QA",
	}
	for handle, want := range cases {
		got := github.SeatVar(handle, github.FieldAppToken)
		if got != want {
			t.Errorf("SeatVar(%q) = %q, want %q", handle, got, want)
		}
		if name, whole := envref.Whole("${" + got + "}"); !whole || name != got {
			t.Errorf("%q is not a name a reference can carry", got)
		}
	}
	if github.SeatVar("qa", github.FieldAppKey) != "GITHUB_APP_KEY_QA" {
		t.Errorf("the key is named %q", github.SeatVar("qa", github.FieldAppKey))
	}
}

// GITHUB CAN DELIVER ONLY WHERE IT CAN REACH. github.com reaches the public
// internet and nothing else; an Enterprise Server is inside the company's
// network and reaches its private addresses, but never another host's
// loopback.
func TestADeliverableAddressIsOneGitHubReaches(t *testing.T) {
	t.Parallel()
	cases := []struct {
		base       string
		enterprise bool
		want       bool
	}{
		{"https://hooks.example.com", false, true},
		{"http://hooks.example.com", false, true},
		{"https://hooks.example.com:8443/base", false, true},
		{"http://localhost:8200", false, false},
		{"http://localhost:8200", true, false},
		{"http://app.localhost:8200", false, false},
		{"http://127.0.0.1:8200", true, false},
		{"http://[::1]:8200", false, false},
		{"http://0.0.0.0:8200", true, false},
		{"http://10.0.0.5:8200", false, false},
		{"http://10.0.0.5:8200", true, true},
		{"http://192.168.1.20", false, false},
		{"http://169.254.1.1", false, false},
		{"http://devbox:8200", false, false},
		{"http://devbox:8200", true, true},
		{"https://crewlet.local", false, false},
		{"https://crewlet.internal", false, false},
		{"https://crewlet.lan", false, false},
		{"https://crewlet.home.arpa", false, false},
		{"https://93.184.215.14", false, true},
		{"ftp://hooks.example.com", false, false},
		{"", false, false},
		{"not a url", false, false},
	}
	for _, c := range cases {
		if got := github.Deliverable(c.base, c.enterprise); got != c.want {
			t.Errorf("Deliverable(%q, enterprise=%v) = %v, want %v", c.base, c.enterprise, got, c.want)
		}
	}
}

// AN APP FOR AN ADDRESS GITHUB CANNOT REACH IS CREATED WITH ITS WEBHOOK OFF,
// never pointed at a placeholder: an inactive hook is a state GitHub's own
// settings page shows, and an unreachable one fails every delivery while the
// app reads as healthy.
func TestAManifestGitHubCannotDeliverToHasNoWebhook(t *testing.T) {
	t.Parallel()
	off := github.BuildManifest(github.ManifestOptions{
		Seat: "pm", Name: "GoPhrase Product Manager",
		DeliveryURL: "http://localhost:8200/webhooks/github/pm",
		DeliveryOff: true,
		RedirectURL: "http://localhost:8200/webhooks/github-app",
		SetupURL:    "http://localhost:8200/webhooks/github-app?installed=pm",
		Access:      github.Access{Tier: github.TierReview},
	})
	// GitHub refuses an unreachable hook, active or not, and events with
	// no hook, so a local engine's app carries neither.
	if off.HookAttributes != nil || len(off.DefaultEvents) != 0 {
		t.Errorf("hook = %v, events = %v, want neither", off.HookAttributes, off.DefaultEvents)
	}
	if off.RedirectURL == "" || off.SetupURL == "" {
		t.Error("the browser's return addresses were dropped with the delivery address")
	}
	on := github.BuildManifest(github.ManifestOptions{
		Seat: "pm", DeliveryURL: "https://hooks.example.com/webhooks/github/pm",
		Access: github.Access{Tier: github.TierReview},
	})
	if on.HookAttributes["active"] != true || on.HookAttributes["url"] == nil {
		t.Errorf("hook = %v, want it active at its address", on.HookAttributes)
	}
}
