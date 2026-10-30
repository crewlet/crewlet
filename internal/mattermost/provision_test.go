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
)

// ---- the plan --------------------------------------------------------- //

func chatSeat(name, token string) *org.Role {
	return &org.Role{
		Name:       name,
		Mattermost: org.MattermostIdentity{BotToken: token},
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

// ---- the reconcile ---------------------------------------------------- //

// chatServer is a Mattermost that remembers what was done to it.
type chatServer struct {
	mu sync.Mutex

	bots  map[string]string // username -> user id
	names map[string]string // user id -> display name
	// descriptions is the bot record's own field, which is where this
	// engine records that IT disabled a bot. A user carries none, so a
	// fixture that only served users could not exercise the marker at all.
	descriptions map[string]string             // user id -> description
	off          map[string]bool               // user id -> disabled
	tokens       map[string][]mattermost.Token // user id -> its live tokens
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
	// it echo the address it is actually served at; a value is either the
	// address people browse while the engine dials another — healthy — or
	// a Site URL the doctor exists to find.
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
	// swallowRenames makes a bot's display-name write ANSWER SUCCESS and
	// record nothing, which is the instance that leaves a pass believing
	// it converged a world it did not.
	swallowRenames bool
	// swallowJoins makes a channel join ANSWER SUCCESS and record
	// nothing, which is the instance that leaves a pass believing it
	// converged a world it did not. Nothing in the pass can tell the
	// difference; only a harness reading the instance afterwards can,
	// which is what it is here for.
	swallowJoins bool
	teamOf       map[string]bool   // user id -> in team
	channels     map[string]string // channel name -> id
	members      map[string]bool   // memberKey(channelID, userID) -> joined

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

// ---- reading and breaking one instance ------------------------------- //
//
// The conformance harness proves its converged world really is converged by
// reading THIS instance rather than the Result of the pass that made it, and
// its guard test breaks one of those facts at a time. Both need the same
// handful of accessors, and they live beside the fixture so a route that
// changes shape changes them here rather than in two files.

// botID is the account a username was created under, empty when there is
// none.
func (s *chatServer) botID(username string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bots[username]
}

// lockDown is an administrator tightening this instance after the company
// was provisioned: the provisioning account loses its system_admin role and
// bot creation is switched off in the System Console.
//
// Neither is anything the engine can undo, and neither refuses a pass over a
// company that needs nothing — which is exactly why they have to be
// reported rather than discovered on the day a seat is added.
func (s *chatServer) lockDown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminRoles = "system_user"
	s.settings["EnableBotAccountCreation"] = "false"
}

// forgetBots empties the account table, which is the instance a pass never
// reached.
func (s *chatServer) forgetBots() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bots = map[string]string{}
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
// with it, for the reason
// [github.com/crewlet/crewlet/internal/httpx/httpxtest] states once for the
// whole tree: the process-global pool is swept by every other parallel test's
// Close.
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

	// ONE BOT'S OWN RECORD, which is where the description lives: a USER
	// carries none, so this is the only place the disconnect marker can be
	// read from. Matched ahead of the listing below, which is the same
	// prefix with nothing after it.
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/bots/"):
		id := strings.TrimPrefix(path, "/bots/")
		username := ""
		for name, botID := range s.bots {
			if botID == id {
				username = name
			}
		}
		json.NewEncoder(w).Encode(map[string]any{
			"user_id": id, "username": username,
			"display_name": s.names[id], "description": s.descriptions[id],
		})

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
		id := strings.TrimPrefix(path, "/bots/")
		// EACH FIELD ONLY WHERE IT WAS SENT, as Mattermost's own PUT
		// behaves: the display-name write and the marker write are two
		// different calls, and a fake that blanked the other would make
		// either look like it had undone the first.
		if name, set := body["display_name"]; set && !s.swallowRenames {
			s.names[id] = name
		}
		if description, set := body["description"]; set {
			if s.descriptions == nil {
				s.descriptions = map[string]string{}
			}
			s.descriptions[id] = description
		}
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
		if s.swallowJoins {
			return
		}
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

func reconcileChat(t *testing.T, srv *chatServer, roles []*org.Role) (*mattermost.Result, error) {
	t.Helper()
	return reconcileChatWith(t, srv, roles, func(*mattermost.Options) {})
}

// reconcileChatWith is the same run with the options a test wants to vary.
func reconcileChatWith(t *testing.T, srv *chatServer, roles []*org.Role,
	tune func(*mattermost.Options),
) (*mattermost.Result, error) {
	t.Helper()
	opts := mattermost.Options{
		Client: chatClient(t, srv), Config: enabledChat(),
		Org: &org.Organization{Name: "Nimbus", Roles: roles},
	}
	tune(&opts)
	return mattermost.Reconcile(context.Background(), opts)
}

// seedBot puts a bot the access sync created on the instance, under a display
// name, and answers its id.
func seedBot(srv *chatServer, handle, displayName string) string {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	id := "bot-" + handle
	srv.bots[mattermost.BotUsername(enabledChat().Provisioning, handle)] = id
	srv.names[id] = displayName
	return id
}

// ---- what a re-run does and does not touch ------------------------------ //

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
	seedBot(srv, "ceo", "A stale name")
	roles := []*org.Role{chatSeat("CEO", "")}
	before := srv.mutations()
	srv.mu.Lock()
	srv.botsFail = true
	srv.mu.Unlock()

	res, err := reconcileChat(t, srv, roles)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if after := srv.mutations(); after != before {
		t.Errorf("a run that could not list bots made %d write(s)", after-before)
	}
	if len(res.Renamed) != 0 {
		t.Errorf("Renamed = %v on a run that could not read a single name", res.Renamed)
	}
	if !strings.Contains(strings.Join(res.Notes, "\n"), "display names are not checked") {
		t.Errorf("notes = %q, want the unread listing reported", res.Notes)
	}
}

// A CANCELLED PASS RAISES RATHER THAN REPORTING HEALTH, on every shape of
// run — including the one that answers from the arguments alone.
//
// An error and an empty findings list are OPPOSITE claims to the reconcile
// loop: one is a fault it retries, the other is a statement that the
// integration is ready, which it trusts for a full settled interval, so a node
// shutting down mid-pass would record Mattermost as healthy on its way out and
// the next node to hold the duty would believe it.
func TestACancelledPassIsAFaultOnEveryShapeOfRun(t *testing.T) {
	t.Parallel()
	for name, roles := range map[string][]*org.Role{
		"a company with an agent seat": {chatSeat("CEO", "")},
		"a company with no agent seat": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			srv := newChatServer()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			res, err := mattermost.Reconcile(ctx, mattermost.Options{
				Client: chatClient(t, srv), Config: enabledChat(),
				Org: &org.Organization{Name: "Nimbus", Roles: roles},
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

// A RENAMED ROLE REACHES THE BOT. Provisioning is a reconcile, and a
// create-only display name lets the Mattermost roster drift from the org
// chart it mirrors with no way back but editing every bot by hand.
//
// WITH NO SEAT TO PROVISION: each agent's bot is the per-seat access sync's,
// so this pass sweeps the document's seats rather than any plan of its own.
func TestARenamedRoleUpdatesTheBotsDisplayName(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	seedBot(srv, "ceo", mattermost.BotDisplayName(enabledChat().Provisioning, "CEO"))

	// THE HANDLE IS PINNED, so this is the same seat under a new title —
	// which is the real drift case. A rename that also changes a DERIVED
	// handle is a different seat, and gets its own bot correctly.
	renamed := chatSeat("Chief Executive", "")
	renamed.DeclaredHandle = "ceo"
	res, err := reconcileChat(t, srv, []*org.Role{renamed})
	if err != nil {
		t.Fatalf("run: %v", err)
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

// AND AN UNCHANGED NAME IS NOT REWRITTEN, or every pass would report work it
// did not do.
func TestAnUnchangedDisplayNameIsLeftAlone(t *testing.T) {
	t.Parallel()
	srv := newChatServer()
	seedBot(srv, "ceo", mattermost.BotDisplayName(enabledChat().Provisioning, "CEO"))
	before := srv.mutations()
	res, err := reconcileChat(t, srv, []*org.Role{chatSeat("CEO", "")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Renamed) != 0 || srv.mutations() != before {
		t.Errorf("Renamed = %v and %d write(s) on an unchanged company",
			res.Renamed, srv.mutations()-before)
	}
}

// THE PREFLIGHT NAMES WHAT WOULD FAIL, before the first write.
//
// Each of these otherwise surfaces as a 403 on the access sync's first bot
// creation, with a message naming an endpoint rather than the setting an
// administrator must change.
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
			res, err := reconcileChat(t, srv, []*org.Role{
				chatSeat("CEO", ""),
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
	res, err := reconcileChat(t, newChatServer(), []*org.Role{
		chatSeat("CEO", "${MM_TOKEN_CEO}"),
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
