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

	bots    map[string]string             // username -> user id
	tokens  map[string][]mattermost.Token // user id -> its live tokens
	revokes int
	// identityFails makes the identity route answer 500 for a BOT's
	// token, which is "cannot tell" rather than "this token is bad".
	identityFails bool
	teamOf        map[string]bool   // user id -> in team
	channels      map[string]string // channel name -> id
	members       map[string]bool   // "channelID:userID"

	failRecord string
	next       int
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

func newChatServer() *chatServer {
	return &chatServer{
		bots: map[string]string{}, tokens: map[string][]mattermost.Token{},
		teamOf: map[string]bool{}, members: map[string]bool{},
		channels: map[string]string{"general": "ch-general", "leadership": "ch-lead"},
	}
}

func (s *chatServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v4")
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && path == "/users/me":
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
			json.NewEncoder(w).Encode(map[string]any{"id": "admin", "username": "root"})
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

	case r.Method == http.MethodPost && path == "/bots":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		s.next++
		id := fmt.Sprintf("user-%d", s.next)
		s.bots[body["username"]] = id
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
	http := httptest.NewServer(http.HandlerFunc(srv.serve))
	t.Cleanup(http.Close)
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: http.URL, Token: adminToken,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
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
