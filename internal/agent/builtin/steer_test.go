package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// fleetAsker answers a scattered note from a scripted set of replies, and
// records what it was asked.
type fleetAsker struct {
	replies []steer.Reply
	raw     [][]byte
	err     error
	asked   []steer.Request
	subject string
	want    int
}

func (a *fleetAsker) Ask(_ context.Context, subject string, request []byte, want int) ([][]byte, error) {
	a.subject, a.want = subject, want
	var req steer.Request
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, err
	}
	a.asked = append(a.asked, req)
	if a.err != nil {
		return nil, a.err
	}
	out := append([][]byte(nil), a.raw...)
	for _, r := range a.replies {
		b, _ := json.Marshal(r)
		out = append(out, b)
	}
	return out, nil
}

// steerFleet answers the steer feature gate.
type steerFleet struct {
	lacks bool
	err   error
}

func (steerFleet) SeatFeature(context.Context, string, coord.Feature) (bool, error) {
	return true, nil
}

func (f steerFleet) AllLiveHave(_ context.Context, feature coord.Feature) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	return feature == coord.FeatureSteer && !f.lacks, nil
}

func steerTool(t *testing.T, asker builtin.FleetAsker, fleet builtin.Fleet, requestKey string) tools.Callable {
	t.Helper()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Fleet: fleet,
		Steer: builtin.SteerDeps{Asker: asker, Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
			return builtin.Actor{Handle: "founder-token", Kind: tracker.AuthorOperator,
				Seat: "founder", RequestKey: requestKey}, nil
		}},
	}) {
		if tool.Name() == builtin.SteerTurnTool {
			return tool
		}
	}
	t.Fatal("the operator catalogue serves no steer_turn with an asker wired")
	return nil
}

func steerCall(t *testing.T, tool tools.Callable, turnID, note string) (tools.Result, map[string]any) {
	t.Helper()
	res, err := tool.Call(t.Context(), map[string]any{"turn_id": turnID, "note": note})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var out map[string]any
	if !res.Failed {
		if err := json.Unmarshal([]byte(res.Output), &out); err != nil {
			t.Fatalf("the answer is not JSON: %s", res.Output)
		}
	}
	return res, out
}

// A TAKEN NOTE IS PENDING, sent as the person, under the request's id. What
// became of it is the running node's to record — this one cannot see that far.
func TestSteerTurnSendsTheNoteAsThePersonAndAnswersPending(t *testing.T) {
	t.Parallel()
	asker := &fleetAsker{replies: []steer.Reply{{Version: 1, TurnID: "run-1",
		AgentHandle: "swe", Status: steer.StatusAccepted}}}
	res, out := steerCall(t, steerTool(t, asker, steerFleet{}, "r-42"), "run-1", "  use staging  ")
	if res.Failed {
		t.Fatalf("refused: %s", res.Output)
	}
	if out["outcome"] != "pending" || out["agent_handle"] != "swe" || out["note_id"] != "req-r-42" {
		t.Errorf("answered %v", out)
	}
	if asker.subject != topics.SeatSteer || asker.want != 1 {
		t.Errorf("asked %q for %d replies, want the steer subject and the one node running it",
			asker.subject, asker.want)
	}
	got := asker.asked[0]
	if got.TurnID != "run-1" || got.Note != "use staging" || got.By != "founder-token" ||
		got.BySeat != "founder" || got.NoteID != "req-r-42" {
		t.Errorf("sent %+v", got)
	}
}

// NO REPLY IS UNKNOWN, NOT NOT RUNNING. A reply lost on its way back is
// indistinguishable from none, so the note may well have been taken: the
// person is told nobody confirmed it — which a retry settles, since the retry
// is the same note — and never that the turn is over, which nobody said.
func TestNoReplyIsUnknownNotNotRunning(t *testing.T) {
	t.Parallel()
	for name, asker := range map[string]*fleetAsker{
		"silence":             {},
		"an unreadable reply": {raw: [][]byte{[]byte("{not json")}},
		"another turn's reply": {replies: []steer.Reply{{TurnID: "run-2",
			Status: steer.StatusClosed}}},
		"a status this build does not know": {replies: []steer.Reply{{TurnID: "run-1",
			Status: "deferred"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			res, out := steerCall(t, steerTool(t, asker, steerFleet{}, "r-1"), "run-1", "use staging")
			if res.Failed {
				t.Fatalf("refused %s: %s", res.Refusal, res.Output)
			}
			if out["outcome"] != "unknown" {
				t.Errorf("answered %v, want unknown", out)
			}
		})
	}
}

// WHAT THE RUNNING NODE SAID is what the person is told, each in the class
// their surface acts on.
func TestSteerTurnRefusesWhatTheRunningNodeRefused(t *testing.T) {
	t.Parallel()
	for status, want := range map[steer.Status]tools.Refusal{
		steer.StatusClosed:      tools.RefusalNotRunning,
		steer.StatusFull:        tools.RefusalConflict,
		steer.StatusUnsupported: tools.RefusalSteerUnsupported,
	} {
		asker := &fleetAsker{replies: []steer.Reply{{TurnID: "run-1", AgentHandle: "swe", Status: status}}}
		res, _ := steerCall(t, steerTool(t, asker, steerFleet{}, "r-1"), "run-1", "use staging")
		if !res.Failed || res.Refusal != want {
			t.Errorf("%s: answered failed=%v %s (%s), want %s", status, res.Failed,
				res.Refusal, res.Output, want)
		}
	}
}

// ASKED NOBODY BEFORE THE FLEET CAN CARRY IT: an older node running the turn
// would answer nothing, and the person would retry an `unknown` that can never
// succeed there. An unreadable fleet is `unavailable`, not an upgrade.
func TestSteerTurnIsGatedOnEveryLiveNode(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		fleet steerFleet
		want  tools.Refusal
	}{
		"a node lacks it":     {steerFleet{lacks: true}, tools.RefusalPeerUpgrading},
		"the fleet is unread": {steerFleet{err: errors.New("store down")}, tools.RefusalUnavailable},
	} {
		asker := &fleetAsker{}
		res, _ := steerCall(t, steerTool(t, asker, tc.fleet, "r-1"), "run-1", "use staging")
		if !res.Failed || res.Refusal != tc.want {
			t.Errorf("%s: answered %s (%s), want %s", name, res.Refusal, res.Output, tc.want)
		}
		if len(asker.asked) != 0 {
			t.Errorf("%s: the note was scattered anyway", name)
		}
	}
}

func TestSteerTurnRefusesABadNote(t *testing.T) {
	t.Parallel()
	asker := &fleetAsker{}
	tool := steerTool(t, asker, steerFleet{}, "r-1")
	for name, args := range map[string][2]string{
		"no turn":  {"", "use staging"},
		"no note":  {"run-1", "   "},
		"too long": {"run-1", strings.Repeat("a", steer.MaxNoteRunes+1)},
	} {
		res, _ := steerCall(t, tool, args[0], args[1])
		if !res.Failed || res.Refusal != tools.RefusalInvalid {
			t.Errorf("%s: answered %s (%s), want invalid", name, res.Refusal, res.Output)
		}
	}
	if len(asker.asked) != 0 {
		t.Error("a note that was refused was scattered anyway")
	}
	// The ask itself failing is nothing sent — unavailable, not unknown.
	failing := &fleetAsker{err: errors.New("no broker")}
	res, _ := steerCall(t, steerTool(t, failing, steerFleet{}, "r-1"), "run-1", "x")
	if res.Refusal != tools.RefusalUnavailable {
		t.Errorf("an ask that could not be made answered %s", res.Refusal)
	}
}
