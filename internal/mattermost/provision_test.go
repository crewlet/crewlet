package mattermost_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	// channelsOf is what every bot's channel list answers. Empty is a
	// bot that hears only direct messages.
	channelsOf []string
	teamOf     map[string]bool   // user id -> in team
	channels   map[string]string // channel name -> id
	members    map[string]bool   // "channelID:userID"

	next int
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
		channelsOf: []string{"eng"},
		teamOf:     map[string]bool{}, members: map[string]bool{},
		channels: map[string]string{"general": "ch-general", "leadership": "ch-lead"},
	}
}

func (s *chatServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v4")
	w.Header().Set("Content-Type", "application/json")

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
		out := make([]map[string]any, 0, len(s.channelsOf))
		for _, name := range s.channelsOf {
			out = append(out, map[string]any{
				"id": "chan-" + name, "name": name, "team_id": "team-1",
			})
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
		json.NewEncoder(w).Encode(map[string]any{"id": id, "username": username})

	case r.Method == http.MethodGet && strings.HasPrefix(path, "/bots"):
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
		key := path + body["user_id"]
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
}

func newChatSink() *chatSink { return &chatSink{values: map[string]string{}} }

func (s *chatSink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == s.failOn {
		return errors.New("the store is unreachable")
	}
	s.values[name] = value
	return nil
}

func (s *chatSink) Discard(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discards++
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

func TestAChatReconcileNeedsAClientAndASink(t *testing.T) {
	t.Parallel()
	if _, err := mattermost.Reconcile(context.Background(), mattermost.Options{}); err == nil {
		t.Error("a reconcile with no client was accepted")
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
