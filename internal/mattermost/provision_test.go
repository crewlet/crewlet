package mattermost_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// ---- the plan --------------------------------------------------------- //

func chatSeat(name, token, channel string) *org.Role {
	return &org.Role{
		Name: name,
		Mattermost: org.MattermostIdentity{
			BotToken: token, Channel: channel,
		},
	}
}

func enabledChat() *config.Mattermost {
	return &config.Mattermost{
		Enabled: true, URL: "https://chat.example.com", Team: "nimbus",
		Provisioning: &config.MattermostProvisioning{
			UsernamePrefix: "agent-", Channels: []string{"general"},
			DisplayNameSuffix: " (AI)",
		},
	}
}

func TestThePlanCoversSeatsWithAReferencedBotToken(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
		chatSeat("PM", "xoxb-written-out", ""),
		{Name: "NoChat"},
	}}
	plan, err := mattermost.PlanFor(o, enabledChat())
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	if len(plan.Seats) != 1 || plan.Seats[0].TokenVar != "MM_TOKEN_CEO" {
		t.Fatalf("seats = %+v", plan.Seats)
	}
	if len(plan.Notes) != 1 || !strings.Contains(plan.Notes[0], "pm") {
		t.Fatalf("notes = %v", plan.Notes)
	}
	if strings.Contains(plan.Notes[0], "xoxb-written-out") {
		t.Errorf("the note leaked a credential: %q", plan.Notes[0])
	}
}

// A USERNAME IS LOWERCASED, because Mattermost's are: a mixed-case handle
// would be created as one thing and looked up as another on the next run,
// which reads as "the bot is missing" and creates a second.
func TestBotUsernamesAreLowercased(t *testing.T) {
	t.Parallel()
	p := &config.MattermostProvisioning{UsernamePrefix: "Agent-"}
	if got := mattermost.BotUsername(p, "CTO"); got != "agent-cto" {
		t.Fatalf("username = %q", got)
	}
}

func TestThePlanNeedsAnEnabledIntegrationAndATeam(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Roles: []*org.Role{chatSeat("CEO", "${T}", "")}}
	if _, err := mattermost.PlanFor(o, nil); err == nil {
		t.Error("a nil integration was accepted")
	}
	noTeam := enabledChat()
	noTeam.Team = ""
	if _, err := mattermost.PlanFor(o, noTeam); err == nil {
		t.Error("an integration with no team was accepted")
	}
}

// ---- the reconcile ---------------------------------------------------- //

// chatServer is a Mattermost that remembers what was done to it.
type chatServer struct {
	mu sync.Mutex

	bots   map[string]string             // username -> user id
	names  map[string]string             // user id -> display name
	off    map[string]bool               // user id -> disabled
	tokens map[string][]mattermost.Token // user id -> its live tokens
	// adminRoles is what /users/me reports for the operator credential.
	// Empty means the shipped system_admin; a value is the under-privileged
	// token the preflight exists to catch.
	adminRoles string
	// settings overrides the client-config booleans the preflight reads.
	settings map[string]string
	revokes  int
	// revokeFails makes every revoke answer 500, which is the instance
	// that will not give a credential up.
	revokeFails bool
	// identityFails makes the identity route answer 500 for a BOT's
	// token, which is "cannot tell" rather than "this token is bad".
	identityFails bool
	// siteURL is what the server reports as its own address. Empty makes
	// it echo the address it is actually served at, which is the healthy
	// case; a value is the misconfiguration the doctor exists to find.
	siteURL string
	// refuseIdentity makes /users/me 401 for every token, which is what
	// a revoked operator credential does.
	refuseIdentity bool
	// base is the address this fake is actually served at, so the
	// healthy case reports the truth about itself.
	base string
	// refuseConfig makes the server's own configuration unreadable.
	refuseConfig bool
	// unreachable makes the unauthenticated ping fail, which is a URL
	// that does not point at a Mattermost server.
	unreachable bool
	// botsFail makes the bot listing answer 500, which is the run that
	// cannot read any bot's current display name.
	botsFail bool
	// membershipFails makes both membership READS answer 500 — the team
	// member lookup and a bot's channel list — which is the instance a
	// pass cannot ask "is it already in?" and has to join blind.
	membershipFails bool
	teamOf          map[string]bool   // user id -> in team
	channels        map[string]string // channel name -> id
	members         map[string]bool   // memberKey(channelID, userID) -> joined

	// writes counts every request this instance received that CHANGES it,
	// keyed by route. See [mutatingRoute] for what that means and why it
	// is not "every non-GET".
	writes int

	next int
}

// memberKey addresses one account's membership of one channel.
func memberKey(channelID, userID string) string { return channelID + ":" + userID }

// join records a channel membership as an earlier provisioning run would
// have left it, naming the channel into existence if the fixture has not
// already: a channel somebody is in is a channel that exists.
//
// A seat is in the TEAM as well, because on Mattermost a channel is
// team-scoped and there is no way to be in one without it.
func (s *chatServer) join(channelName, userID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.channels[channelName]
	if !ok {
		id = "ch-" + channelName
		s.channels[channelName] = id
	}
	s.members[memberKey(id, userID)] = true
	s.teamOf[userID] = true
}

// mutations is every write this instance has received.
func (s *chatServer) mutations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

// mutatingRoute reports whether a request CHANGES this instance, keyed by
// ROUTE rather than by HTTP method.
//
// integrationtest states that distinction and it is not pedantry: some
// vendors model a listing as a POST, so a counter keyed on the method makes
// "a converged pass writes nothing" impossible to satisfy — and a clause
// nobody can satisfy is one somebody eventually weakens until it passes.
// Mattermost has two POSTs that are reads, and both are named here rather
// than left to be discovered: /channels/direct FINDS the conversation
// between two accounts and returns the existing one instead of making a
// second, and /users/{id}/typing raises an ephemeral indicator there is
// nothing to undo. Neither is on the provisioning path.
//
// Everything else that is not a GET is a write here: a bot created, renamed,
// enabled or disabled, a membership set, a token minted or revoked. So the
// DEFAULT IS TO COUNT — a route nobody classified that turns out to be a
// read only makes the clause harder to pass, where one that turns out to be
// a write would make this suite pass over the exact bug it exists to find.
//
// Counted on ARRIVAL, whatever the answer: a duplicate join that the server
// refuses with "already a member" is still a request this pass sent to
// somebody's instance, which is the thing the clause is about.
func mutatingRoute(method, path string) bool {
	if method == http.MethodGet {
		return false
	}
	return path != "/channels/direct" && !strings.HasSuffix(path, "/typing")
}

// forget clears the counters, so a test can measure ONE run.
func (s *chatServer) forget() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.revokes = 0
}

func (s *chatServer) revoked() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revokes
}

// adminToken is the operator credential the fixture's client presents.
const adminToken = "admin-token"

// issue registers a token against a user, as a mint would have.
func (s *chatServer) issue(userID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens[userID] = append(s.tokens[userID], mattermost.Token{
		ID: "tok-" + userID, Description: "seeded", Value: token,
	})
}

// chatClient stands the fake up and points a client at it.
//
// THE SERVER'S OWN CLIENT, whose transport belongs to this server and dies
// with it. A client over http.DefaultTransport shares one connection pool
// with every other parallel test, so one server's Close breaks a request in
// flight against another.
func chatClient(t *testing.T, srv *chatServer) *mattermost.Client {
	t.Helper()
	return chatClientAs(t, srv, adminToken)
}

// chatClientWithout points a client at the fake with NO credential, which
// is how an operator runs the doctor having minted nothing.
// The listing a DESTRUCTIVE decision is made from. It asked for one page of
// 200 and took whatever came back, so on an instance with more bots than that
// every managed account past the first page was invisible to a decommission
// sweep and stayed live for ever — with the run reporting success.
func TestBotsAreWalkedToExhaustion(t *testing.T) {
	t.Parallel()
	var pages []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages = append(pages, r.URL.Query().Get("page"))
		// A full page, then a short one: the walk must ask twice.
		if r.URL.Query().Get("page") == "0" {
			rows := make([]string, 0, 200)
			for i := range 200 {
				rows = append(rows, fmt.Sprintf(`{"user_id":"u%d","username":"bot-%d"}`, i, i))
			}
			_, _ = fmt.Fprintf(w, "[%s]", strings.Join(rows, ","))
			return
		}
		_, _ = w.Write([]byte(`[{"user_id":"last","username":"bot-last"}]`))
	}))
	t.Cleanup(server.Close)

	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: server.URL, Token: "t", HTTP: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := client.Bots(context.Background())
	if err != nil {
		t.Fatalf("Bots: %v", err)
	}
	if len(got) != 201 {
		t.Errorf("bots = %d, want both pages (201)", len(got))
	}
	if len(pages) < 2 || pages[0] != "0" || pages[1] != "1" {
		t.Errorf("pages requested = %v, want the walk to continue past a full page", pages)
	}
	if got[len(got)-1].Username != "bot-last" {
		t.Error("the bot on the second page was not returned, so a sweep cannot see it")
	}
}

// The ceiling is a NON-CONVERGENCE guard, so it has to let a converging walk
// through — the same boundary internal/github's hook walk carries. Compared
// with >= it refuses an instance holding EXACTLY the ceiling, with an error
// saying it has "more than" that many bots though its next page is empty. The
// listing a DECOMMISSION reads is then unavailable rather than short, which is
// the better of the two failures but still a sweep that cannot run.
//
// The limit is read back out of the error rather than hardcoded, so this
// tracks the constant instead of pinning a copy of it.
func TestABotWalkStopsPastItsCeilingAndNotAtIt(t *testing.T) {
	t.Parallel()
	full := func(w http.ResponseWriter, page int) {
		rows := make([]string, 0, 200)
		for i := range 200 {
			rows = append(rows, fmt.Sprintf(
				`{"user_id":"u%d-%d","username":"bot-%d-%d"}`, page, i, page, i))
		}
		_, _ = fmt.Fprintf(w, "[%s]", strings.Join(rows, ","))
	}
	clientFor := func(h http.HandlerFunc) *mattermost.Client {
		t.Helper()
		server := httptest.NewServer(h)
		t.Cleanup(server.Close)
		client, err := mattermost.NewClient(mattermost.ClientOptions{
			URL: server.URL, Token: "t", HTTP: server.Client(),
		})
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
		return client
	}

	// An instance that never converges must be refused rather than walked for
	// ever, and the refusal must name the limit it hit.
	endless := clientFor(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		full(w, page)
	})
	_, err := endless.Bots(context.Background())
	if err == nil {
		t.Fatal("a walk that never converges was not refused")
	}
	m := regexp.MustCompile(`more than (\d+)`).FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("the refusal does not name the limit it hit: %v", err)
	}
	ceiling, _ := strconv.Atoi(m[1])

	// And an instance holding EXACTLY that many is listed. Pages are
	// zero-based here, so the empty page that ends the walk is page
	// ceiling/200.
	pages := ceiling / 200
	exact := clientFor(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if page >= pages {
			_, _ = w.Write([]byte("[]"))
			return
		}
		full(w, page)
	})
	got, err := exact.Bots(context.Background())
	if err != nil {
		t.Fatalf("an instance holding exactly the %d-bot ceiling was refused: %v", ceiling, err)
	}
	if len(got) != ceiling {
		t.Errorf("bots = %d, want the whole %d", len(got), ceiling)
	}
}

func chatClientWithout(t *testing.T, srv *chatServer) *mattermost.Client {
	t.Helper()
	return chatClientAs(t, srv, "")
}

func chatClientAs(t *testing.T, srv *chatServer, token string) *mattermost.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(srv.serve))
	t.Cleanup(server.Close)
	srv.mu.Lock()
	srv.base = server.URL
	srv.mu.Unlock()
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: server.URL, Token: token, HTTP: server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func newChatServer() *chatServer {
	return &chatServer{
		bots: map[string]string{}, names: map[string]string{}, off: map[string]bool{},
		tokens: map[string][]mattermost.Token{}, settings: map[string]string{},
		teamOf: map[string]bool{}, members: map[string]bool{},
		channels: map[string]string{"general": "ch-general", "leadership": "ch-lead"},
	}
}

func (s *chatServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v4")
	w.Header().Set("Content-Type", "application/json")
	if mutatingRoute(r.Method, path) {
		s.writes++
	}

	switch {
	case r.Method == http.MethodGet && path == "/system/ping":
		if s.unreachable {
			// A 404 rather than a 502: this models the wrong URL, which
			// is the case the doctor exists to separate from a bad
			// credential. A 502 is a proxy mid-restart, which the
			// client deliberately waits out.
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Not found"}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"status": "OK"})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/users/") &&
		strings.Contains(path, "/teams/") && strings.HasSuffix(path, "/channels"):
		if s.membershipFails {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"500"}`))
			return
		}
		// WHOSE CHANNELS, derived from what was actually joined.
		//
		// This answered one fixed list for every account, and the moment a
		// pass READS membership before joining, that fiction is worse than
		// no fixture: it reports the bot in channels it never joined and
		// out of the ones it did, so a converged run that writes nothing
		// and a broken one that writes everything look identical here.
		userID := path[len("/users/"):strings.Index(path, "/teams/")]
		out := []map[string]any{}
		if s.teamOf[userID] {
			// A channel is team-scoped, so an account outside the team is
			// in none of them whatever the membership table says.
			for _, name := range slices.Sorted(maps.Keys(s.channels)) {
				if s.members[memberKey(s.channels[name], userID)] {
					out = append(out, map[string]any{
						"id": s.channels[name], "name": name, "team_id": "team-1",
					})
				}
			}
		}
		json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodGet && path == "/config/client":
		if s.refuseConfig {
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"message":"You do not have the appropriate permissions"}`))
			return
		}
		site := s.siteURL
		if site == "" {
			site = s.base
		}
		out := map[string]string{"SiteURL": site}
		for k, v := range s.settings {
			out[k] = v
		}
		json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodGet && path == "/users/me":
		if s.refuseIdentity {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"Invalid or expired session"}`))
			return
		}
		// WHOEVER PRESENTED THE TOKEN. The re-run check takes the value
		// a variable holds and asks the server who it is, so a fake
		// answering the same account for every token would prove
		// nothing. A revoked token is simply absent from the store,
		// which is how Mattermost serves it.
		presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if s.identityFails && presented != adminToken {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"500"}`))
			return
		}
		if presented == adminToken {
			roles := s.adminRoles
			if roles == "" {
				roles = "system_user system_admin"
			}
			json.NewEncoder(w).Encode(map[string]any{
				"id": "admin", "username": "root", "roles": roles,
			})
			return
		}
		for user, tokens := range s.tokens {
			for _, token := range tokens {
				if token.Value == presented {
					json.NewEncoder(w).Encode(map[string]any{"id": user})
					return
				}
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"message":"Invalid or expired session"}`))

	case r.Method == http.MethodGet && path == "/teams/name/nimbus":
		json.NewEncoder(w).Encode(map[string]any{"id": "team-1", "name": "nimbus"})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/users/username/"):
		username := strings.TrimPrefix(path, "/users/username/")
		id, ok := s.bots[username]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Unable to find the user."}`))
			return
		}
		row := map[string]any{"id": id, "username": username}
		// DEACTIVATED IS A STATE THE ACCOUNT REPORTS, and it is what a
		// disconnect leaves behind: Mattermost disables rather than
		// deletes, so the account is still found and still off.
		if s.off[id] {
			row["delete_at"] = 1
		}
		json.NewEncoder(w).Encode(row)

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/bots"):
		if s.botsFail {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"500"}`))
			return
		}
		out := []map[string]any{}
		for username, id := range s.bots {
			out = append(out, map[string]any{
				"user_id": id, "username": username, "display_name": s.names[id],
			})
		}
		json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodPut && strings.HasPrefix(path, "/bots/"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		s.names[strings.TrimPrefix(path, "/bots/")] = body["display_name"]
		w.Write([]byte(`{}`))

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/disable") &&
		strings.HasPrefix(path, "/bots/"):
		s.off[strings.TrimSuffix(strings.TrimPrefix(path, "/bots/"), "/disable")] = true
		w.Write([]byte(`{}`))

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/enable") &&
		strings.HasPrefix(path, "/bots/"):
		delete(s.off, strings.TrimSuffix(strings.TrimPrefix(path, "/bots/"), "/enable"))
		w.Write([]byte(`{}`))

	case r.Method == http.MethodPost && path == "/bots":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		s.next++
		id := fmt.Sprintf("user-%d", s.next)
		s.bots[body["username"]] = id
		s.names[id] = body["display_name"]
		json.NewEncoder(w).Encode(map[string]any{
			"user_id": id, "username": body["username"],
		})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/teams/team-1/members/"):
		// 404 IS THE ANSWER "not a member" — Mattermost has no boolean
		// here, which is why the client reads a status rather than a body.
		if s.membershipFails {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"500"}`))
			return
		}
		userID := strings.TrimPrefix(path, "/teams/team-1/members/")
		if !s.teamOf[userID] {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Unable to find the team member."}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"team_id": "team-1", "user_id": userID,
		})

	case r.Method == http.MethodPost && path == "/teams/team-1/members":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		if s.teamOf[body["user_id"]] {
			// MATTERMOST ANSWERS 400 HERE, not 409 — which is exactly the
			// asymmetry a reconcile has to know about or it fails on its
			// second run.
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"This user is already a team member."}`))
			return
		}
		s.teamOf[body["user_id"]] = true

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/teams/team-1/channels/name/"):
		name := strings.TrimPrefix(path, "/teams/team-1/channels/name/")
		id, ok := s.channels[name]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			w.Write([]byte(`{"message":"Channel does not exist."}`))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": id, "name": name})

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/members") &&
		strings.HasPrefix(path, "/channels/"):
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		channelID := strings.TrimSuffix(strings.TrimPrefix(path, "/channels/"), "/members")
		key := memberKey(channelID, body["user_id"])
		if s.members[key] {
			// And a duplicate CHANNEL membership is a 409. The two
			// endpoints disagree, which is the point.
			w.WriteHeader(http.StatusConflict)
			return
		}
		s.members[key] = true

	case r.Method == http.MethodPost && strings.HasSuffix(path, "/tokens"):
		userID := strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "/tokens")
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		s.next++
		token := mattermost.Token{
			ID: fmt.Sprintf("tok-%d", s.next), Description: body["description"],
		}
		token.Value = "mmtok-" + token.ID
		s.tokens[userID] = append(s.tokens[userID], token)
		json.NewEncoder(w).Encode(map[string]any{
			"id": token.ID, "token": token.Value, "description": token.Description,
		})

	case r.Method == http.MethodGet && strings.HasSuffix(path, "/tokens"):
		userID := strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "/tokens")
		out := make([]map[string]any, 0, len(s.tokens[userID]))
		for _, token := range s.tokens[userID] {
			// THE VALUE IS NEVER LISTED — the server returns it from the
			// mint call alone.
			out = append(out, map[string]any{
				"id": token.ID, "description": token.Description,
			})
		}
		json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodPost && path == "/users/tokens/revoke":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		s.revokes++
		if s.revokeFails {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"message":"Unable to revoke the token."}`))
			return
		}
		// A REVOKED TOKEN IS GONE from the listing, which is how
		// Mattermost serves it — there is no revoked flag.
		for user, tokens := range s.tokens {
			var kept []mattermost.Token
			for _, token := range tokens {
				if token.ID != body["token_id"] {
					kept = append(kept, token)
				}
			}
			s.tokens[user] = kept
		}

	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"not found"}`))
	}
}

func (s *chatServer) liveTokens() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ids := range s.tokens {
		n += len(ids)
	}
	return n
}

type chatSink struct {
	mu     sync.Mutex
	values map[string]string
	failOn string
	// holdsErr makes the sink unreadable, which must never be read as
	// "nothing is held" — that would rotate every live credential.
	holdsErr error
	discards int
	// writes counts what this deployment's own sealed store received.
	//
	// A WRITE IS NOT ONLY A REQUEST TO MATTERMOST. A pass that re-seals a
	// seat's credential on every converged run is writing just as surely
	// — the value an operator would have to put back is in here, not on
	// the instance — so the conformance harness counts these alongside
	// the routes. Discard counts too: it clears what a previous run
	// recorded, which is a change to the same store.
	writes int
}

func newChatSink() *chatSink { return &chatSink{values: map[string]string{}} }

func (s *chatSink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == s.failOn {
		return errors.New("the store is unreachable")
	}
	s.writes++
	s.values[name] = value
	return nil
}

func (s *chatSink) Discard(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discards++
	s.writes++
	s.values = map[string]string{}
	return nil
}

// Holds implements the sink contract: this fixture starts empty, so
// nothing is held until this run records it.
func (s *chatSink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.holdsErr != nil {
		return "", false, s.holdsErr
	}
	return s.values[name], s.values[name] != "", nil
}

// seed puts a value in the sink as an EARLIER run would have.
func (s *chatSink) seed(name, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = value
}

// mutations is everything this sink was asked to change.
func (s *chatSink) mutations() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writes
}

func (s *chatSink) value(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[name]
}

func (s *chatSink) Flush(context.Context) error { return nil }
func (s *chatSink) Describe() string            { return "a test sink" }
func (s *chatSink) NextStep() string            { return "a test next step" }

func (s *chatSink) recorded() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.values {
		out[k] = v
	}
	return out
}

func reconcileChat(t *testing.T, srv *chatServer, sink provision.TokenSink,
	roles []*org.Role,
) (*mattermost.Result, error) {
	t.Helper()
	return reconcileChatWith(t, srv, sink, roles, func(*mattermost.Options) {})
}

// reconcileChatWith is the same run with the options a test wants to vary.
func reconcileChatWith(t *testing.T, srv *chatServer, sink provision.TokenSink,
	roles []*org.Role, tune func(*mattermost.Options),
) (*mattermost.Result, error) {
	t.Helper()
	client := chatClient(t, srv)
	o := &org.Organization{Name: "Nimbus", Roles: roles}
	cfg := enabledChat()
	plan, err := mattermost.PlanFor(o, cfg)
	if err != nil {
		t.Fatalf("PlanFor: %v", err)
	}
	opts := mattermost.Options{
		Client: client, Config: cfg, Org: o, Plan: plan, Sink: sink,
	}
	tune(&opts)
	return mattermost.Reconcile(context.Background(), opts)
}

func TestAReconcileCreatesJoinsAndMints(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	res, err := reconcileChat(t, srv, sink, []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Created) != 1 || len(res.Rotated) != 1 {
		t.Fatalf("result = %+v", res)
	}
	// BOTH the company-wide channel and the seat's own.
	joined := strings.Join(res.Joined["ceo"], ",")
	if joined != "general,leadership" {
		t.Fatalf("joined %q, want both channels in a stable order", joined)
	}
	if !strings.HasPrefix(sink.recorded()["MM_TOKEN_CEO"], "mmtok-") {
		t.Fatalf("recorded %q", sink.recorded()["MM_TOKEN_CEO"])
	}
}

// THE SECOND RUN MUST NOT FAIL ON WHAT THE FIRST ONE DID — and the two
// membership endpoints answer a duplicate with DIFFERENT statuses, which is
// the asymmetry that makes a naive provisioner work exactly once.
func TestASecondRunSurvivesBothDuplicateShapes(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	roles := []*org.Role{chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership")}
	if _, err := reconcileChat(t, srv, newChatSink(), roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := reconcileChat(t, srv, newChatSink(), roles)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Created) != 0 {
		t.Errorf("the second run created %v", res.Created)
	}
	if len(res.Rotated) != 1 {
		t.Errorf("the second run rotated %v, want the token minted again", res.Rotated)
	}
}

// A BOT IN NO CHANNEL NEVER WAKES, and nothing about the account looks
// wrong — so the run says so rather than reporting a clean result.
func TestABotThatJoinedNothingIsReported(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	srv.channels = map[string]string{} // no channels exist at all
	res, err := reconcileChat(t, srv, newChatSink(), []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", ""),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	joined := strings.Join(res.Notes, "\n")
	if !strings.Contains(joined, "never receive a message") {
		t.Fatalf("notes = %v, want the silent bot reported", res.Notes)
	}
	// AND THE MISSING CHANNEL IS NAMED, with the fix. A note added during
	// the run has to reach the report — one collected before it would
	// silently drop every such note.
	if !strings.Contains(joined, "general") || !strings.Contains(joined, "slug") {
		t.Fatalf("notes = %v, want the missing channel named", res.Notes)
	}
}

// A MISSING CHANNEL DOES NOT ABORT. Half a fleet joined and the run stopped
// is a worse state than every bot joined to what exists plus a line saying
// what did not — especially since the usual cause is a typo.
func TestAMissingChannelDoesNotStopTheRun(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	res, err := reconcileChat(t, srv, newChatSink(), []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "no-such-channel"),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if strings.Join(res.Joined["ceo"], ",") != "general" {
		t.Fatalf("joined %v, want the channel that exists", res.Joined["ceo"])
	}
	if len(res.Rotated) != 1 {
		t.Error("the run did not finish provisioning the seat")
	}
}

func TestAFailedRecordRevokesTheMintedToken(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	sink.failOn = "MM_TOKEN_CEO"

	_, err := reconcileChat(t, srv, sink, []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", ""),
	})
	if err == nil {
		t.Fatal("a failed record was reported as a successful run")
	}
	if live := srv.liveTokens(); live != 0 {
		t.Fatalf("%d token(s) still live after a rollback", live)
	}
	if sink.discards == 0 {
		t.Error("the sink was not asked to discard")
	}
	if !strings.Contains(err.Error(), "unreachable") {
		t.Errorf("the error lost the cause: %v", err)
	}
}

// EVERY INPUT THIS PASS DEREFERENCES IS REFUSED WHEN IT IS ABSENT — and
// REFUSED rather than panicked over.
//
// The team comes off the config and a seat's own channel off the org, both
// without a nil check further down. A panic and an error are not the same
// failure here: the reconcile worker is a fleet singleton, so a panic takes
// the node down, where an error is a fault the loop retries and reports
// against the surface it belongs to.
func TestAChatReconcileRefusesAnIncompleteRun(t *testing.T) {
	t.Parallel()
	full := func(t *testing.T) mattermost.Options {
		t.Helper()
		o := &org.Organization{Name: "Nimbus", Roles: []*org.Role{
			chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
		}}
		cfg := enabledChat()
		plan, err := mattermost.PlanFor(o, cfg)
		if err != nil {
			t.Fatalf("PlanFor: %v", err)
		}
		return mattermost.Options{
			Client: chatClient(t, newChatServer()), Config: cfg,
			Org: o, Plan: plan, Sink: newChatSink(),
		}
	}
	for name, drop := range map[string]func(*mattermost.Options){
		"no client": func(o *mattermost.Options) { o.Client = nil },
		"no config": func(o *mattermost.Options) { o.Config = nil },
		"no org":    func(o *mattermost.Options) { o.Org = nil },
		"no sink":   func(o *mattermost.Options) { o.Sink = nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			opts := full(t)
			drop(&opts)
			if _, err := mattermost.Reconcile(context.Background(), opts); err == nil {
				t.Errorf("a reconcile with %s was accepted", name)
			}
		})
	}
}

// ---- what a re-run does and does not touch ------------------------------ //

// A PLAIN RE-RUN KEEPS A WORKING TOKEN. Rotating it revokes the credential
// every bot's websocket is currently authenticated with — an operator
// adding one seat would take the others down.
func TestARerunKeepsAWorkingBotToken(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	roles := []*org.Role{chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")}
	if _, err := reconcileChat(t, srv, sink, roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := sink.value("MM_TOKEN_SWE")
	srv.forget()

	res, err := reconcileChat(t, srv, sink, roles)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 0 || len(res.Kept) != 1 {
		t.Fatalf("rotated %v, kept %v", res.Rotated, res.Kept)
	}
	if sink.value("MM_TOKEN_SWE") != first {
		t.Error("the recorded credential changed under a running engine")
	}
	if srv.revoked() != 0 {
		t.Errorf("a plain re-run revoked %d tokens", srv.revoked())
	}
}

// A CONVERGED RE-RUN SENDS NO WRITE AT ALL — the clause the shared
// certification suite exists for, pinned here on the two requests this pass
// used to make unconditionally.
//
// Both membership endpoints tolerate a duplicate, and that tolerance is what
// made an unconditional POST look free: the run stayed correct, so nothing
// failed and nothing said anything. It is not free. The reconcile loop runs
// this pass every few minutes for the life of the deployment, so a company
// that needs nothing was still having its team membership set and every one
// of its channels re-joined, for ever, on somebody else's instance.
func TestAConvergedRerunSetsNoMembership(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	roles := []*org.Role{chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership")}
	if _, err := reconcileChat(t, srv, sink, roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := srv.mutations() + sink.mutations()

	res, err := reconcileChat(t, srv, sink, roles)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if after := srv.mutations() + sink.mutations(); after != before {
		t.Errorf("a converged re-run made %d write(s)", after-before)
	}
	// AND IT STILL REPORTS THE CHANNELS THE SEAT CAN HEAR. Joined is what
	// an operator reads to find the bot that will never wake, not a list
	// of what this run changed — narrowed to the latter, a converged
	// company reads as a fleet of deaf bots and the "joined no channel"
	// note starts firing on seats that are in two.
	if got := strings.Join(res.Joined["ceo"], ","); got != "general,leadership" {
		t.Errorf("Joined = %q, want every channel the bot is in", got)
	}
}

// A MEMBERSHIP THIS RUN CANNOT READ IS JOINED ANYWAY.
//
// The opposite of what the token check does with "cannot tell", and
// deliberately: re-minting a credential destroys one that works, where a
// duplicate join is a no-op the server itself absorbs. Skipping on an
// unreadable membership would leave a seat out of the team it was supposed
// to be in — an agent that hears nothing, from a run that reported success.
func TestAnUnreadableMembershipJoinsRatherThanSkips(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	roles := []*org.Role{chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership")}
	if _, err := reconcileChat(t, srv, sink, roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := srv.mutations()
	srv.mu.Lock()
	srv.membershipFails = true
	srv.mu.Unlock()

	res, err := reconcileChat(t, srv, sink, roles)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	// The team join plus one per channel: everything this pass would have
	// sent before the reads existed.
	if got := srv.mutations() - before; got != 3 {
		t.Errorf("a blind run sent %d write(s), want the team join and both "+
			"channel joins", got)
	}
	if got := strings.Join(res.Joined["ceo"], ","); got != "general,leadership" {
		t.Errorf("Joined = %q", got)
	}
	// AND IT SAYS SO, because the loop's promise of a quiet steady state
	// depends on those reads: an instance that refuses them turns every
	// pass back into a write, and nothing else would report it.
	notes := strings.Join(res.Notes, "\n")
	if !strings.Contains(notes, "already in team") ||
		!strings.Contains(notes, "which channels the bot is already") {
		t.Errorf("notes = %q, want both unreadable memberships named", res.Notes)
	}
}

// AN UNREADABLE BOT LISTING DOES NOT REWRITE A DISPLAY NAME.
//
// The listing is the only thing carrying a bot's current display name, so a
// run whose listing failed does not know the name is already right — and read
// as a plain map lookup, which is what this was, a missing entry IS an empty
// display name. So an instance whose /bots was unreachable had every seat's
// name written back on every pass, from the same run whose note says display
// names are not checked this time.
func TestAnUnreadableBotListingDoesNotRewriteDisplayNames(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	roles := []*org.Role{chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership")}
	if _, err := reconcileChat(t, srv, sink, roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := srv.mutations() + sink.mutations()
	srv.mu.Lock()
	srv.botsFail = true
	srv.mu.Unlock()

	res, err := reconcileChat(t, srv, sink, roles)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if after := srv.mutations() + sink.mutations(); after != before {
		t.Errorf("a run that could not list bots made %d write(s)", after-before)
	}
	if len(res.Renamed) != 0 {
		t.Errorf("Renamed = %v on a run that could not read a single name", res.Renamed)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "display names are not checked") {
		t.Errorf("notes = %q, want the unread listing reported", res.Notes)
	}
}

// A DEPARTED SEAT'S REPORT DOES NOT MOVE BETWEEN PASSES.
//
// Decommissioned is walked out of a MAP, and [mattermost.Result.Findings]
// turns that same slice straight into one finding per departed seat. Ranged
// over the map, two passes over one unchanged company produced finding lists
// that were not equal — so the reconcile loop saw the surface change every
// time, which resets the attempt counter and re-reads an integration that
// needs nothing at the shortest interval the schedule has. The command line
// prints the same slice, so its report flapped run to run too.
func TestDepartedSeatsAreReportedInAStableOrder(t *testing.T) {
	t.Parallel()
	seats := make([]*org.Role, 0, 8)
	for _, name := range []string{"CEO", "CTO", "CFO", "COO", "PM", "QA", "SRE", "SWE"} {
		seats = append(seats, chatSeat(name, "${MM_TOKEN_"+name+"}", "eng"))
	}
	// Every seat but the first leaves, so seven bots are swept and a map
	// walk has 7! orders to pick from.
	departed := func(t *testing.T) []string {
		t.Helper()
		srv := newChatServer()
		if _, err := reconcileChat(t, srv, newChatSink(), seats); err != nil {
			t.Fatalf("first run: %v", err)
		}
		res, err := reconcileChatWith(t, srv, newChatSink(), seats[:1],
			func(o *mattermost.Options) { o.Decommission = true })
		if err != nil {
			t.Fatalf("decommission: %v", err)
		}
		return res.Decommissioned
	}

	first, second := departed(t), departed(t)
	if len(first) != len(seats)-1 {
		t.Fatalf("Decommissioned = %v, want every departed seat", first)
	}
	if !slices.IsSorted(first) {
		t.Errorf("Decommissioned = %v, which is a map's order rather than one", first)
	}
	if !slices.Equal(first, second) {
		t.Errorf("two identical companies were swept in different orders:\n %v\n %v",
			first, second)
	}
}

// A CANCELLED PASS RAISES RATHER THAN REPORTING HEALTH, on every shape of
// run — including the two that answer from the arguments alone.
//
// An error and an empty findings list are OPPOSITE claims to the reconcile
// loop: one is a fault it retries, the other is a statement that the
// integration is ready, which it trusts for a full settled interval. Both
// early returns below decided without consulting the world, so a node
// shutting down mid-pass recorded Mattermost as healthy on its way out and
// the next node to hold the duty believed it. The empty plan is not a corner
// either: a company that enables the mattermost block before any seat carries
// a whole ${VAR} bot token has one on every single pass.
func TestACancelledPassIsAFaultOnEveryShapeOfRun(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		roles []*org.Role
		sink  provision.TokenSink
	}{
		"a converged company": {
			roles: []*org.Role{chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership")},
			sink:  newChatSink(),
		},
		"no seat carries a bot token, so the plan is empty": {
			roles: []*org.Role{chatSeat("CEO", "written-out-by-hand", "leadership")},
			sink:  newChatSink(),
		},
		"this node has no keyring": {
			roles: []*org.Role{chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership")},
			sink:  provision.ReadOnly(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newChatServer()
			client := chatClient(t, srv)
			o := &org.Organization{Name: "Nimbus", Roles: tc.roles}
			cfg := enabledChat()
			plan, err := mattermost.PlanFor(o, cfg)
			if err != nil {
				t.Fatalf("PlanFor: %v", err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			res, err := mattermost.Reconcile(ctx, mattermost.Options{
				Client: client, Config: cfg, Org: o, Plan: plan, Sink: tc.sink,
			})
			if err == nil {
				t.Fatalf("a cancelled pass reported %+v, which the loop reads "+
					"as a converged integration", res)
			}
			if !errors.Is(err, context.Canceled) {
				t.Errorf("error = %v, want the cancellation the caller can "+
					"recognise", err)
			}
		})
	}
}

// -rotate IS THE OPERATOR ASKING, and it retires only this tool's own.
func TestRotateMintsAfreshAndSparesTheAdminsBotToken(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	roles := []*org.Role{chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")}
	if _, err := reconcileChat(t, srv, sink, roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	first := sink.value("MM_TOKEN_SWE")
	srv.mu.Lock()
	for user := range srv.tokens {
		srv.tokens[user] = append(srv.tokens[user], mattermost.Token{
			ID: "tok-by-hand", Description: "set up by an admin",
		})
	}
	srv.mu.Unlock()

	res, err := reconcileChatWith(t, srv, sink, roles,
		func(o *mattermost.Options) { o.Rotate = true })
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Fatalf("rotated = %v", res.Rotated)
	}
	if sink.value("MM_TOKEN_SWE") == first {
		t.Error("-rotate left the credential alone")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	byHand, recorded := false, false
	for _, tokens := range srv.tokens {
		for _, token := range tokens {
			if token.ID == "tok-by-hand" {
				byHand = true
			}
			if token.Value == sink.value("MM_TOKEN_SWE") {
				recorded = true
			}
		}
	}
	if !byHand {
		t.Error("rotation revoked a token it did not mint")
	}
	// THE RECORDED VALUE MUST STILL BE LIVE. Retiring the previous
	// tokens re-lists them AFTER the mint, so the fresh one is in that
	// list — revoking it would record a credential that is already dead.
	if !recorded {
		t.Error("rotation revoked the token it had just recorded")
	}
}

// A VARIABLE NOBODY RECORDED IS MINTED INTO even though the bot has a
// working token: the value cannot be read back.
func TestAnUnrecordedBotVariableIsMintedInto(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	roles := []*org.Role{chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")}
	if _, err := reconcileChat(t, srv, newChatSink(), roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	fresh := newChatSink() // a second machine: nothing recorded here
	res, err := reconcileChat(t, srv, fresh, roles)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Fatalf("rotated = %v, kept = %v", res.Rotated, res.Kept)
	}
	if fresh.value("MM_TOKEN_SWE") == "" {
		t.Error("nothing was recorded")
	}
}

// A REVOKED TOKEN IS MINTED OVER whatever the variable holds.
func TestARevokedBotTokenIsReplaced(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	roles := []*org.Role{chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")}
	if _, err := reconcileChat(t, srv, sink, roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	srv.mu.Lock()
	for user := range srv.tokens {
		srv.tokens[user] = nil
	}
	srv.mu.Unlock()

	res, err := reconcileChat(t, srv, sink, roles)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 {
		t.Errorf("rotated = %v, kept = %v", res.Rotated, res.Kept)
	}
}

// AN UNREADABLE SINK IS NOT AN EMPTY ONE.
func TestAnUnreadableSinkStopsTheChatRun(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	roles := []*org.Role{chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")}
	if _, err := reconcileChat(t, srv, sink, roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	srv.forget()
	sink.holdsErr = errors.New("the store is unreachable")
	if _, err := reconcileChat(t, srv, sink, roles); err == nil {
		t.Fatal("an unreadable sink was read as holding nothing")
	}
	if srv.revoked() != 0 {
		t.Errorf("%d tokens were revoked on an unreadable sink", srv.revoked())
	}
}

// A ROLLBACK ON A PRE-EXISTING BOT REVOKES ONLY WHAT IT MINTED. Sweeping
// the account would take an administrator's own token with no way to tell
// that it had.
func TestARollbackOnAnExistingBotSparesTheAdminsToken(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	roles := []*org.Role{chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")}
	if _, err := reconcileChat(t, srv, newChatSink(), roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	srv.mu.Lock()
	for user := range srv.tokens {
		srv.tokens[user] = append(srv.tokens[user], mattermost.Token{
			ID: "tok-by-hand", Description: "set up by an admin",
		})
	}
	srv.mu.Unlock()

	failing := newChatSink()
	failing.failOn = "MM_TOKEN_SWE"
	if _, err := reconcileChatWith(t, srv, failing, roles,
		func(o *mattermost.Options) { o.Rotate = true }); err == nil {
		t.Fatal("the run reported success with nothing recorded")
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	for _, tokens := range srv.tokens {
		for _, token := range tokens {
			if token.ID == "tok-by-hand" {
				return
			}
		}
	}
	t.Fatal("the rollback revoked a token it did not mint")
}

// A COPY-PASTED VARIABLE IS CAUGHT AT THE SERVER.
func TestABotTokenBelongingToAnotherAccountStopsTheRun(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	roles := []*org.Role{chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")}
	if _, err := reconcileChat(t, srv, sink, roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	srv.mu.Lock()
	srv.tokens["user-other"] = []mattermost.Token{{
		ID: "tok-other", Description: mattermost.TokenDescription("qa"),
		Value: "mmtok-other",
	}}
	srv.mu.Unlock()
	sink.seed("MM_TOKEN_SWE", "mmtok-other")

	if _, err := reconcileChat(t, srv, sink, roles); err == nil {
		t.Fatal("a token belonging to another account was accepted")
	} else if !strings.Contains(err.Error(), "different account") {
		t.Errorf("error = %v", err)
	}
}

// "CANNOT TELL" LEAVES THE SEAT EXACTLY AS IT WAS.
func TestAnUnverifiableBotTokenIsLeftAloneWithANote(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	sink := newChatSink()
	roles := []*org.Role{chatSeat("SWE", "${MM_TOKEN_SWE}", "eng")}
	if _, err := reconcileChat(t, srv, sink, roles); err != nil {
		t.Fatalf("first run: %v", err)
	}
	before := sink.value("MM_TOKEN_SWE")
	srv.forget()
	srv.mu.Lock()
	srv.identityFails = true
	srv.mu.Unlock()

	res, err := reconcileChat(t, srv, sink, roles)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(res.Rotated) != 0 || len(res.Kept) != 1 {
		t.Fatalf("rotated %v, kept %v", res.Rotated, res.Kept)
	}
	if sink.value("MM_TOKEN_SWE") != before {
		t.Error("a token that could not be checked was replaced")
	}
	found := false
	for _, note := range res.Notes {
		if strings.Contains(note, "could not check") {
			found = true
		}
	}
	if !found {
		t.Errorf("notes = %q", res.Notes)
	}
	if srv.revoked() != 0 {
		t.Errorf("%d tokens were revoked on an unverifiable seat", srv.revoked())
	}
}

// THE FOUR THINGS THE PORT DROPPED.
//
// GitLab and Plane kept `-decommission`; Slack kept `-handles`; the previous
// engine kept a bot's display name current and ran a preflight before its
// first write. Mattermost lost all four in the rewrite, and each is invisible
// until an operator needs it: a departed seat's bot keeps posting, a renamed
// role never reaches the roster, and a token without system_admin fails on
// the first bot creation with a 403 naming an endpoint rather than the role.

// A RENAMED ROLE REACHES THE BOT. Provisioning is a reconcile, and a
// create-only display name lets the Mattermost roster drift from the org
// chart it mirrors with no way back but editing every bot by hand.
func TestARenamedRoleUpdatesTheBotsDisplayName(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	if _, err := reconcileChat(t, srv, newChatSink(), []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	// THE HANDLE IS PINNED, so this is the same seat under a new title —
	// which is the real drift case. A rename that also changes a DERIVED
	// handle is a different seat, and gets its own bot correctly.
	renamed := chatSeat("Chief Executive", "${MM_TOKEN_CEO}", "leadership")
	renamed.DeclaredHandle = "ceo"
	res, err := reconcileChat(t, srv, newChatSink(), []*org.Role{renamed})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	srv.mu.Lock()
	got := srv.names[srv.bots["agent-ceo"]]
	srv.mu.Unlock()
	if !strings.Contains(got, "Chief Executive") {
		t.Errorf("display name = %q, want the renamed role — the roster drifts "+
			"from the org chart otherwise", got)
	}
	if len(res.Renamed) != 1 {
		t.Errorf("Renamed = %v, want the rename reported", res.Renamed)
	}
}

// AND AN UNCHANGED NAME IS NOT REWRITTEN, or every re-run would report work
// it did not do — the same silence-vs-noise rule Kept exists for.
func TestAnUnchangedDisplayNameIsLeftAlone(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	seats := []*org.Role{chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership")}
	if _, err := reconcileChat(t, srv, newChatSink(), seats); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := reconcileChat(t, srv, newChatSink(), seats)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Renamed) != 0 {
		t.Errorf("Renamed = %v on an unchanged company", res.Renamed)
	}
}

// A DEPARTED SEAT'S BOT IS DISABLED, not deleted: a deleted bot takes its
// posts with it, silently rewriting the history of every channel it spoke in.
func TestDecommissionDisablesADepartedSeatsBot(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	if _, err := reconcileChat(t, srv, newChatSink(), []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
		chatSeat("CTO", "${MM_TOKEN_CTO}", "eng"),
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}

	res, err := reconcileChatWith(t, srv, newChatSink(),
		[]*org.Role{chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership")},
		func(o *mattermost.Options) { o.Decommission = true })
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Decommissioned) != 1 || !strings.Contains(res.Decommissioned[0], "cto") {
		t.Fatalf("Decommissioned = %v, want the departed seat", res.Decommissioned)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if !srv.off[srv.bots["agent-cto"]] {
		t.Error("the departed bot was reported disabled and is still enabled")
	}
	if srv.off[srv.bots["agent-ceo"]] {
		t.Error("a seat still in the company was disabled")
	}
}

// WITHOUT THE FLAG NOTHING IS DISABLED. Decommissioning is destructive enough
// that it must never be what a plain re-run does.
func TestARerunWithoutDecommissionDisablesNothing(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	if _, err := reconcileChat(t, srv, newChatSink(), []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
		chatSeat("CTO", "${MM_TOKEN_CTO}", "eng"),
	}); err != nil {
		t.Fatalf("first run: %v", err)
	}
	res, err := reconcileChat(t, srv, newChatSink(), []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
	})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Decommissioned) != 0 {
		t.Errorf("Decommissioned = %v without the flag", res.Decommissioned)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.off[srv.bots["agent-cto"]] {
		t.Error("a plain re-run disabled a bot")
	}
}

// -handles NARROWS THE PROVISIONING LOOP AND NOT THE KEEP-SET.
//
// This is the footgun the flag has to avoid: filtering the plan itself is the
// obvious implementation, and `-handles ceo -decommission` would then read
// every other seat as departed and disable the whole company.
func TestHandlesNarrowsProvisioningWithoutWideningDecommission(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	seats := []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
		chatSeat("CTO", "${MM_TOKEN_CTO}", "eng"),
	}
	if _, err := reconcileChat(t, srv, newChatSink(), seats); err != nil {
		t.Fatalf("first run: %v", err)
	}

	res, err := reconcileChatWith(t, srv, newChatSink(), seats,
		func(o *mattermost.Options) {
			o.Only, o.Decommission, o.Rotate = []string{"ceo"}, true, true
		})
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(res.Rotated) != 1 || res.Rotated[0] != "ceo" {
		t.Errorf("Rotated = %v, want only the named handle", res.Rotated)
	}
	if len(res.Decommissioned) != 0 {
		t.Fatalf("Decommissioned = %v — a narrowed run must not read the "+
			"seats it skipped as departed", res.Decommissioned)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if srv.off[srv.bots["agent-cto"]] {
		t.Error("-handles ceo -decommission disabled the seat it merely skipped")
	}
}

// THE PREFLIGHT NAMES WHAT WOULD FAIL, before the first write.
//
// Each of these otherwise surfaces as a 403 on bot creation — after the team
// lookup succeeded, so the run looks like it was working — with a message
// naming an endpoint rather than the setting an administrator must change.
func TestThePreflightNamesWhatWouldFail(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		tune func(*chatServer)
		want string
	}{
		"not a system administrator": {
			tune: func(s *chatServer) { s.adminRoles = "system_user" },
			want: "system administrator",
		},
		"bot creation disabled": {
			tune: func(s *chatServer) { s.settings["EnableBotAccountCreation"] = "false" },
			want: "EnableBotAccountCreation",
		},
		"access tokens disabled": {
			tune: func(s *chatServer) { s.settings["EnableUserAccessTokens"] = "false" },
			want: "EnableUserAccessTokens",
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := newChatServer()
			tc.tune(srv)
			res, err := reconcileChat(t, srv, newChatSink(), []*org.Role{
				chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
			})
			if err != nil {
				t.Fatalf("Reconcile: %v", err)
			}
			if !strings.Contains(strings.Join(res.Notes, "\n"), tc.want) {
				t.Errorf("notes did not name %q:\n%s", tc.want,
					strings.Join(res.Notes, "\n"))
			}
		})
	}
}

// A HEALTHY INSTANCE GETS NO PREFLIGHT NOISE, or the notes stop being read.
func TestAHealthyInstanceRaisesNoPreflightNote(t *testing.T) {
	t.Parallel()
	res, err := reconcileChat(t, newChatServer(), newChatSink(), []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
	})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for _, note := range res.Notes {
		if strings.Contains(note, "system administrator") ||
			strings.Contains(note, "EnableBot") || strings.Contains(note, "EnableUser") {
			t.Errorf("healthy instance produced a preflight note: %q", note)
		}
	}
}

// RECONNECTING TURNS THE BOT BACK ON, because disconnecting turned it off.
//
// A decommission DISABLES rather than deletes, deliberately: a deleted
// Mattermost account takes its posts with it, so a disconnect would rewrite
// the history of every channel the agent ever spoke in. That leaves the
// account here to be found, and a reconnect that merely found it, joined it
// to the team and minted a fresh token reported ready over an agent that
// could not sign in: every socket that token opened was refused, and nothing
// anywhere said why.
func TestReconnectingReEnablesABotADisconnectDisabled(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	both := []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}", "leadership"),
		chatSeat("CTO", "${MM_TOKEN_CTO}", "eng"),
	}
	if _, err := reconcileChat(t, srv, newChatSink(), both); err != nil {
		t.Fatalf("first run: %v", err)
	}
	srv.mu.Lock()
	id := srv.bots["agent-cto"]
	srv.mu.Unlock()
	if id == "" {
		t.Fatal("the first run created no bot for the seat")
	}

	// THE DISCONNECT, which is what leaves the account off.
	if _, err := reconcileChatWith(t, srv, newChatSink(), both[:1],
		func(o *mattermost.Options) { o.Decommission = true }); err != nil {
		t.Fatalf("decommission: %v", err)
	}
	srv.mu.Lock()
	disabled := srv.off[id]
	srv.mu.Unlock()
	if !disabled {
		t.Fatal("the decommission left the bot enabled, so this test proves nothing")
	}

	res, err := reconcileChat(t, srv, newChatSink(), both)
	if err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	srv.mu.Lock()
	stillOff, now := srv.off[id], srv.bots["agent-cto"]
	srv.mu.Unlock()
	if stillOff {
		t.Error("the bot is still disabled after a reconnect, so its token " +
			"authenticates as a deactivated account and it can never open a socket")
	}
	// AND THE RUN SAYS SO. A pass that fixed this silently would be
	// indistinguishable from one that had nothing to do.
	if len(res.Enabled) != 1 || res.Enabled[0] != "cto" {
		t.Errorf("Enabled = %v, and the report does not name the bot it turned back on",
			res.Enabled)
	}
	// THE SAME ACCOUNT, not a second one: keeping the username and its
	// history is the whole reason a disconnect disables rather than deletes.
	if now != id {
		t.Errorf("the reconnect made a new bot %q, orphaning %q", now, id)
	}
}
