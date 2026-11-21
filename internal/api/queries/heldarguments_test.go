package queries_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/secrets"
)

// heldToken is a provider token from redact's corpus, sealed in a held call's
// arguments.
func heldToken(t *testing.T) string {
	t.Helper()
	for _, s := range redacttest.Shapes() {
		if s.Name == "github-token" {
			return s.Sample
		}
	}
	t.Fatal("redacttest holds no github-token shape")
	return ""
}

// heldOn is a run of seat parked on call, its arguments sealed under seal with
// body, in each of the three states a reader can find it: waiting on its
// answer, being made, and answered.
func heldOn(t *testing.T, seal *sandbox.CallSeal, seat, turn string, call sandbox.HeldCall,
	body string) (waiting, calling, answered sandbox.PendingRun) {

	t.Helper()
	call.LaunchID, call.HeldAt = "launch-"+turn, time.Now().UTC()
	var err error
	if call.Arguments, err = seal.SealArguments(turn, call, map[string]any{"body": body}); err != nil {
		t.Fatal(err)
	}
	waiting = sandbox.PendingRun{TurnID: turn, LaunchID: call.LaunchID, AgentHandle: seat,
		AgentID: uuid.NewSHA1(uuid.Nil, []byte(seat)).String(), Status: sandbox.StatusAwaiting,
		Question: "May " + seat + " call it?", AudienceHandles: []string{"cto"}, HeldCall: &call}
	made := call
	made.CallingAt = time.Now().UTC()
	calling = waiting
	calling.HeldCall = &made
	answered = waiting
	answered.Status, answered.Answer = sandbox.StatusResumed, &sandbox.RecordedAnswer{RecordedAt: time.Now().UTC()}
	return waiting, calling, answered
}

// heldSeal is a held calls' seal under a keyring of the test's own.
func heldSeal(t *testing.T) *sandbox.CallSeal {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	return sandbox.NewCallSeal(cipher)
}

// readsAsSent checks one reader's view of a held call's body: the token as
// sent where sent, else a marker in its place.
func readsAsSent(t *testing.T, name, body, token string, sent bool) {
	t.Helper()
	switch {
	case sent && !strings.Contains(body, token):
		t.Errorf("%s reads %q, want the arguments as sent", name, body)
	case !sent && (strings.Contains(body, token) || !strings.Contains(body, redact.Marker)):
		t.Errorf("%s reads %q, want the token as a marker", name, body)
	}
}

// AN EXECUTOR ROW SHOWS A HELD CALL'S ARGUMENTS AS SENT ONLY TO WHO MAY ANSWER
// IT WHILE IT WAITS: an administrator reads an operate command's token while
// it waits; the seat's lead, who reads its trail and may not answer an operate
// command, reads a marker; and once it is being made or answered the
// administrator reads a marker too.
//
// Mutation: drop either arm of [sandbox.CallSeal.OpenArgumentsFor]'s rule, or
// open the row's arguments for a reader who may answer whatever the board
// decided, and a case reads the token.
func TestAnExecutorRowShowsAHeldCallsArgumentsAsSentOnlyToWhoMayAnswerItWhileItWaits(t *testing.T) {
	t.Parallel()
	token, seal := heldToken(t), heldSeal(t)
	operate := sandbox.HeldCall{ID: "0190f3a8-0000-7000-8000-0000000000c1", Server: "crewlet", Tool: "ssh_operate",
		Raw: "ssh_operate", Fixed: true, Approvers: sandbox.HeldApproversSSH}
	waiting, calling, answered := heldOn(t, seal, "swe", "t-op", operate, "deploy --token "+token)
	admin := iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin}, ReauthAt: time.Now().Add(-time.Hour)})
	for _, c := range []struct {
		name string
		ctx  context.Context
		run  sandbox.PendingRun
		sent bool
	}{
		{"an administrator, who may answer it, while it waits", admin, waiting, true},
		{"the seat's lead, who may not answer it, while it waits", asSeat(t, "lead"), waiting, false},
		{"an administrator once it is being made", admin, calling, false},
		{"an administrator once it is answered", admin, answered, false},
	} {
		r := queries.NewRegistry()
		queries.Register(r, queries.Sources{Sandbox: fixedRuns{c.run}, HeldCalls: seal,
			Chart: leadsChart{lead: "lead", report: "swe"}})
		got, err := r.Answer(c.ctx, "executors", nil)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		rows := got.(map[string]any)["runs"].([]any)
		args, _ := rows[0].(map[string]any)["held_arguments"].(map[string]any)
		body, _ := args["body"].(string)
		readsAsSent(t, c.name, body, token, c.sent)
	}
}

// THE MCP STATUS ANSWER LISTS A HELD CALL'S ARGUMENTS AS SENT ONLY TO WHO MAY
// ANSWER IT WHILE IT WAITS: an administrator reads the waiting call's token;
// an agent credential holding admin, which reads the administrator's answer
// and may answer no held call, reads a marker; and the answered call lists a
// marker to the administrator.
//
// Mutation: drop either arm of [sandbox.CallSeal.OpenArgumentsFor]'s rule, or
// decide the list's reader as one who may answer, and a case reads the token.
func TestTheStatusAnswerListsAHeldCallsArgumentsAsSentOnlyToWhoMayAnswerItWhileItWaits(t *testing.T) {
	t.Parallel()
	token, seal := heldToken(t), heldSeal(t)
	issue := sandbox.HeldCall{Server: "github", Tool: "gh_create_issue", Raw: "create_issue", Digest: "sha256:d1"}
	waitingCall, answeredCall := issue, issue
	waitingCall.ID, answeredCall.ID = "0190f3a8-0000-7000-8000-0000000000c2", "0190f3a8-0000-7000-8000-0000000000c3"
	waiting, _, _ := heldOn(t, seal, "backend-dev", "t-waiting", waitingCall, "token "+token)
	_, _, answered := heldOn(t, seal, "backend-dev", "t-answered", answeredCall, "token "+token)
	r := queries.NewRegistry()
	src := mcpSources(t, mcpFleet(t))
	src.Sandbox, src.HeldCalls = fixedRuns{waiting, answered}, seal
	queries.Register(r, src)
	listed := func(p iam.Principal) map[string]string {
		raw, err := r.Answer(iam.WithPrincipal(t.Context(), p), "mcp_servers_status", nil)
		if err != nil {
			t.Fatal(err)
		}
		status, ok := raw.(queries.MCPStatusAnswer)
		if !ok {
			t.Fatalf("%s reads %T, want the administrator's answer", p.Login, raw)
		}
		out := map[string]string{}
		for _, a := range status.Approvals {
			out[a.TurnID], _ = a.Args["body"].(string)
		}
		return out
	}
	admin := listed(person(iam.StageActive, iam.GrantAdmin))
	agent := listed(iam.Principal{ID: uuid.New(), Login: "ops", Seat: "ops", Kind: iam.KindSeat,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin}, ReauthAt: time.Now().Add(-time.Hour)})
	readsAsSent(t, "an administrator, who may answer it, while it waits", admin["t-waiting"], token, true)
	readsAsSent(t, "an agent holding admin, who may not answer it, while it waits", agent["t-waiting"], token, false)
	readsAsSent(t, "an administrator once it is answered", admin["t-answered"], token, false)
}
