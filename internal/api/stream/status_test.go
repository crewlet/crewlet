package stream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tokens"
)

// statusSeat is the agent seat these cases watch, and statusLead the seat that
// leads it.
const (
	statusAgent  = "agent-status"
	statusHandle = "eng-bot"
	statusLead   = "eng-lead"
)

// leadsOne is a chart in which [statusLead] leads [statusHandle] and nobody
// else leads anybody.
type leadsOne struct{ authz.NoChart }

func (leadsOne) Leads(_ context.Context, actor, subject string) (bool, error) {
	return actor == statusLead && subject == statusHandle, nil
}

// bodyKeys are a live call's BODY fields as the wire names them — what the
// seat is saying, which only its trail's readers receive.
var bodyKeys = []string{"trigger", "prompt", "prompt_messages", "response",
	"tool_executions", "round_narration", "partial_round"}

// statusRig is a service holding one agent seat, and one client per audience
// a case compares: an admin, the seat's lead, a tracker holder who leads
// nothing, and a reader of the overview alone.
type statusRig struct {
	svc                               *stream.Service
	admin, lead, tracker, overviewOne *stream.Client
}

func newStatusRig(t *testing.T) *statusRig {
	t.Helper()
	svc := buildService(t, stream.Options{
		Seats: func() tokens.Seats {
			return tokens.Seats{statusAgent: {Handle: statusHandle}}
		},
		Chart: leadsOne{},
	})
	client := func(p iam.Principal) *stream.Client {
		c := stream.NewClient(svc.AudienceFor(t.Context(), p))
		svc.Hub().Register(c)
		return c
	}
	active := func(login, seat string, grants ...iam.Grant) iam.Principal {
		return iam.Principal{Login: login, Kind: iam.KindPerson, Seat: seat,
			Stage: iam.StageActive, Grants: grants}
	}
	return &statusRig{
		svc:         svc,
		admin:       client(active("erin.ng", "founder", iam.GrantAdmin)),
		lead:        client(active("lee.lead", statusLead, iam.GrantWorklet)),
		tracker:     client(active("sam.lee", "analyst", iam.GrantWorklet)),
		overviewOne: client(active("pat.kim", "intern")),
	}
}

// frame is one progress frame of the seat's call, as the runner publishes it.
func (r *statusRig) frame(t *testing.T, seq int, p types.AgentTurnProgress) {
	t.Helper()
	p.Agent, p.RoleName, p.TurnID, p.Phase, p.Iteration = statusAgent, "Engineer",
		"turn-1", "execute", 0
	p.Model = "claude-sonnet-4-5"
	p.WorkItem = &types.WorkItem{Backend: "native", ID: "t-1", Key: "ENG-1",
		Project: "ENG"}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	r.svc.Ingest(livestate.Envelope{ID: fmt.Sprintf("s%d", seq), Type: p.EventType(),
		Timestamp: clock.Add(time.Duration(seq) * time.Second).Format(time.RFC3339Nano),
		Category:  "task", Payload: payload})
}

// calls is every live call the `agents` pushes queued for c carried, in order.
func calls(t *testing.T, c *stream.Client) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, f := range drain(c) {
		if f.Kind() != stream.KindAgents {
			continue
		}
		out = append(out, pushedCall(t, f.Raw()))
	}
	if len(out) == 0 {
		t.Fatal("no agents push reached the client, so nothing below is asserted")
	}
	return out
}

// withheld asserts a pushed call is the seat's status and nothing else: still
// a call — its key and its rounds' bookkeeping — with no body key, no running
// call's arguments, every body version zero, and `body_withheld`.
func withheld(t *testing.T, who string, i int, call map[string]any) {
	t.Helper()
	if call == nil || call["turn_id"] != "turn-1" {
		t.Fatalf("%s push %d: the call's status did not arrive: %v", who, i, call)
	}
	for _, key := range bodyKeys {
		if _, ok := call[key]; ok {
			t.Errorf("%s push %d carries the body field %q", who, i, key)
		}
	}
	running, _ := call["running_calls"].([]any)
	for j, entry := range running {
		call, _ := entry.(map[string]any)
		if _, ok := call["arguments"]; ok {
			t.Errorf("%s push %d carries running call %d's arguments", who, i, j)
		}
		if call["name"] == nil {
			t.Errorf("%s push %d lost which tool running call %d is", who, i, j)
		}
	}
	versions, _ := call["versions"].(map[string]any)
	for _, body := range []string{"prompt", "response", "narration", "executions"} {
		if v, _ := versions[body].(float64); v != 0 {
			t.Errorf("%s push %d names %s version %v: a tab told a body moved "+
				"asks for a call it will never be sent", who, i, body, v)
		}
	}
	if masked, _ := call["body_withheld"].(bool); !masked {
		t.Errorf("%s push %d does not say its body was withheld", who, i)
	}
}

// A STATUS AUDIENCE GETS NO BODY, AND NO BODY VERSION THAT MOVES.
//
// The roster is everybody's and a seat's live call is on it: what the seat is
// DOING — its phase, its round, its tokens — reaches every reader, and what it
// is SAYING — the prompt it was sent, what the model answered, the tools it
// called and with what — reaches an admin and the seat's lead, who read its
// trail (`audit.seat.read`), and nobody else. A withheld call names no body
// version, so the dashboard's stale check never sends the reader round for a
// call it will not be given. And the work item the call is charged to is the
// tracker's.
//
// Mutation: hand every audience the row unmasked, or keep the body versions
// on a withheld call, or keep the work item for a reader without Worklet,
// and this fails.
func TestAStatusAudienceGetsNoBodyAndNoMovingBodyVersions(t *testing.T) {
	t.Parallel()
	r := newStatusRig(t)
	r.frame(t, 1, types.AgentTurnProgress{
		Prompt: "fix the flaky payments test", RoundNum: -1,
		Trigger: types.Trigger{ID: "e-1", Type: "chat_message", Summary: "please fix it"},
		PromptMessages: []types.PromptMessage{
			{Role: "system", Content: "you are the engineer"},
			{Role: "user", Content: "fix the flaky payments test"}},
	})
	r.frame(t, 2, types.AgentTurnProgress{
		Response: "reading the suite", RoundNum: 0,
		RoundNarration: []types.RoundNarration{{"round": 1, "content": "reading"}},
		RunningCalls: []types.RunningCall{
			{ID: "call-1", Round: 1, Name: "read_file",
				Arguments: `{"path":"payments_test.go"}`, StartedAt: clock},
			{ID: "call-2", Round: 1, Name: "read_file",
				Arguments: `{"path":"ledger_test.go"}`, StartedAt: clock},
		},
		PartialRound: map[string]any{"round": 2, "content": "thinking"},
	})

	for _, tc := range []struct {
		who     string
		client  *stream.Client
		body    bool
		tracker bool
	}{
		{"an admin", r.admin, true, true},
		{"the seat's lead", r.lead, true, true},
		{"a Worklet holder who leads nothing", r.tracker, false, true},
		{"a reader of the overview alone", r.overviewOne, false, false},
	} {
		got := calls(t, tc.client)
		for i, call := range got {
			if tc.body {
				if masked, _ := call["body_withheld"].(bool); masked {
					t.Errorf("%s push %d was withheld its body", tc.who, i)
				}
			} else {
				withheld(t, tc.who, i, call)
			}
			if _, ok := call["work_item"]; ok != tc.tracker {
				t.Errorf("%s push %d carries the work item = %v, want %v",
					tc.who, i, ok, tc.tracker)
			}
		}
		if tc.body {
			if got[0]["prompt"] != "fix the flaky payments test" {
				t.Errorf("%s was not sent the prompt: %v", tc.who, got[0]["prompt"])
			}
			running, _ := got[len(got)-1]["running_calls"].([]any)
			if len(running) != 2 {
				t.Fatalf("%s was sent running calls %v, want both", tc.who, running)
			}
			for j, entry := range running {
				if call, _ := entry.(map[string]any); call["arguments"] == nil {
					t.Errorf("%s was not sent running call %d's arguments", tc.who, j)
				}
			}
		}
	}
}

// A STATUS AUDIENCE GETS NO BODY FROM A LEAN CALL EITHER.
//
// The second push of one call leaves out the heavy fields the first carried at
// the same version ([livestate.LiveCall.Without]), so the row the hub masks
// holds a lean call, not the projection's own — and a mask that recognised
// only the projection's shape would hand that one through whole, or drop it.
// Both pushes reach a status reader as the call's status and nothing more,
// and the body reader on the same broadcast is sent exactly what the push
// decided to carry: the response that moved, and not the prompt it holds.
//
// Mutation: make [livestate.MaskCall] handle only the projection's own call,
// and the second push is withheld from everybody or sent whole to the status
// reader.
func TestAStatusAudienceGetsNoBodyFromALeanCall(t *testing.T) {
	t.Parallel()
	r := newStatusRig(t)
	r.frame(t, 1, types.AgentTurnProgress{Prompt: "fix the flaky payments test",
		RoundNum: -1})
	r.frame(t, 2, types.AgentTurnProgress{Prompt: "fix the flaky payments test",
		Response: "reading the suite", RoundNum: 0})

	status := calls(t, r.tracker)
	if len(status) != 2 {
		t.Fatalf("the status reader received %d pushes, want two", len(status))
	}
	for i, call := range status {
		withheld(t, "a Worklet holder who leads nothing", i, call)
	}

	lead := calls(t, r.lead)
	if len(lead) != 2 {
		t.Fatalf("the lead received %d pushes, want two", len(lead))
	}
	second := lead[1]
	if second["turn_id"] != "turn-1" {
		t.Fatalf("the lead's second push carries no call: %v", second)
	}
	if second["response"] != "reading the suite" {
		t.Errorf("the lead's second push leaves out the response that moved: %v",
			second["response"])
	}
	if _, ok := second["prompt"]; ok {
		t.Error("the lead's second push re-sends the prompt the first carried at " +
			"the same version: the push's own decision was not kept")
	}
}
