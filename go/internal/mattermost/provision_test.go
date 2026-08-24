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

	bots     map[string]string   // username -> user id
	tokens   map[string][]string // user id -> live token ids
	teamOf   map[string]bool     // user id -> in team
	channels map[string]string   // channel name -> id
	members  map[string]bool     // "channelID:userID"

	failRecord string
	next       int
}

func newChatServer() *chatServer {
	return &chatServer{
		bots: map[string]string{}, tokens: map[string][]string{},
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
		s.next++
		id := fmt.Sprintf("tok-%d", s.next)
		s.tokens[userID] = append(s.tokens[userID], id)
		json.NewEncoder(w).Encode(map[string]any{
			"id": id, "token": "mmtok-" + id,
		})

	case r.Method == http.MethodGet && strings.HasSuffix(path, "/tokens"):
		userID := strings.TrimSuffix(strings.TrimPrefix(path, "/users/"), "/tokens")
		var out []map[string]any
		for _, id := range s.tokens[userID] {
			out = append(out, map[string]any{"id": id})
		}
		json.NewEncoder(w).Encode(out)

	case r.Method == http.MethodPost && path == "/users/tokens/revoke":
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		for user, ids := range s.tokens {
			var kept []string
			for _, id := range ids {
				if id != body["token_id"] {
					kept = append(kept, id)
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
	mu       sync.Mutex
	values   map[string]string
	failOn   string
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
func (s *chatSink) Holds(_ context.Context, name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[name] != "", nil
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
	http := httptest.NewServer(http.HandlerFunc(srv.serve))
	t.Cleanup(http.Close)
	client, err := mattermost.NewClient(mattermost.ClientOptions{
		URL: http.URL, Token: "admin-token",
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
	return mattermost.Reconcile(context.Background(), mattermost.Options{
		Client: client, Config: cfg, Org: o, Plan: plan, Sink: sink,
	})
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
