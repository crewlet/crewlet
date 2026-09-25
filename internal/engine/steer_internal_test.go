package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// ask scatters one note the way steer_turn does, and returns every reply.
func ask(t *testing.T, q interface {
	Ask(context.Context, string, []byte, int) ([][]byte, error)
}, turnID string, want int) []steer.Reply {
	t.Helper()
	raw, err := json.Marshal(steer.Request{
		Version: steer.WireVersion, TurnID: turnID, NoteID: "req-1",
		Note: "use staging", By: "ops-token", BySeat: "founder",
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	replies, err := q.Ask(ctx, topics.SeatSteer, raw, want)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	out := make([]steer.Reply, 0, len(replies))
	for _, r := range replies {
		var reply steer.Reply
		if err := json.Unmarshal(r, &reply); err != nil {
			t.Fatalf("an unreadable reply: %v", err)
		}
		out = append(out, reply)
	}
	return out
}

// ONLY THE SEAT HOLDER ANSWERS A STEER.
//
// Two nodes serve the subject; one runs the turn. A scatter that waits out its
// whole deadline (want 0) must collect exactly ONE reply — the running node's.
// A second answer from a node that does not run the turn would be a node
// claiming to have handed a note to a turn it cannot reach.
func TestOnlyTheSeatHolderAnswersASteer(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	holder, other := broker.Client(), broker.Client()
	for _, q := range []*memory.Queue{holder, other} {
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	running, idle := newSteerDesk(), newSteerDesk()
	for q, d := range map[*memory.Queue]*steerDesk{holder: running, other: idle} {
		stop, err := q.Serve(t.Context(), topics.SeatSteer, d.answer)
		if err != nil {
			t.Fatalf("Serve: %v", err)
		}
		t.Cleanup(func() { _ = stop(context.Background()) })
	}
	box := steer.New()
	running.register("run-1", "swe", box)

	got := ask(t, other, "run-1", 0)
	if len(got) != 1 {
		t.Fatalf("%d nodes answered for one running turn: %+v", len(got), got)
	}
	if got[0].Status != steer.StatusAccepted || got[0].AgentHandle != "swe" {
		t.Errorf("the holder answered %+v", got[0])
	}
	if notes := box.Drain(); len(notes) != 1 || notes[0].BySeat != "founder" {
		t.Errorf("the turn holds %+v, want the one note from the founder", notes)
	}
	// NOBODY ANSWERS for a turn no node runs or remembers — which the tool
	// reports as unknown, never as not running.
	if got := ask(t, other, "run-unknown", 0); len(got) != 0 {
		t.Errorf("a turn nobody runs was answered: %+v", got)
	}
}

// A TURN THAT JUST ENDED IS ANSWERED CLOSED, not by silence: a note races a
// turn's end by seconds, and "nobody answered" would send a person to retry a
// note to a turn that is over. The memory is bounded.
func TestAnEndedTurnIsAnsweredClosedForAWhile(t *testing.T) {
	t.Parallel()
	d := newSteerDesk()
	d.register("run-1", "swe", steer.New())
	d.close("run-1")
	status, handle, err := d.offer("run-1", steer.Note{ID: "n", Text: "x"})
	if err != nil || status != steer.StatusClosed || handle != "swe" {
		t.Fatalf("an ended turn answered (%s, %q, %v), want closed", status, handle, err)
	}
	for i := range steerClosedMemory {
		id := "later-" + string(rune('a'+i%26)) + string(rune('a'+i/26))
		d.register(id, "swe", steer.New())
		d.close(id)
	}
	if _, _, err := d.offer("run-1", steer.Note{ID: "n", Text: "x"}); err == nil {
		t.Errorf("the desk still remembers a turn %d turns later — its memory is unbounded",
			steerClosedMemory)
	}
	if len(d.closed) != steerClosedMemory || len(d.order) != steerClosedMemory {
		t.Errorf("the desk remembers %d/%d ended turns, want %d", len(d.closed), len(d.order),
			steerClosedMemory)
	}
}

// launcherStub is an agent-mode executor, which no case launches.
type launcherStub struct{}

func (launcherStub) LaunchExecutor(context.Context, runner.AgentRunRequest) error { return nil }

// AN AGENT RUNTIME TURN REFUSES STEER. Its executor is a coding CLI's own loop,
// whose rounds the engine does not drive, so no round boundary exists to hand
// a note to — and a person is told so rather than promised a delivery. The box
// is filed all the same, so the refusal is an answer and not silence.
func TestAnAgentRuntimeTurnRefusesSteer(t *testing.T) {
	t.Parallel()
	if steerBox(nil).Supported() != true {
		t.Fatal("a native turn's box refuses notes")
	}
	d := newSteerDesk()
	d.register("run-1", "swe", steerBox(launcherStub{}))
	status, _, err := d.offer("run-1", steer.Note{ID: "n", Text: "x"})
	if err != nil || status != steer.StatusUnsupported {
		t.Errorf("an agent-runtime turn answered (%s, %v), want unsupported", status, err)
	}
}

// offeringProvider has a person steer the turn while its first model call is
// in flight, and then fails that call — so the turn ends before any round
// could read the note.
type offeringProvider struct {
	desk  *steerDesk
	runID string
}

func (offeringProvider) Model() string { return "offering" }

func (p offeringProvider) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	raw, _ := json.Marshal(steer.Request{
		Version: steer.WireVersion, TurnID: p.runID, NoteID: "req-1",
		Note: "use staging", By: "ops-token", BySeat: "founder",
	})
	if _, err := p.desk.answer(ctx, raw); err != nil {
		return nil, err
	}
	return nil, &llm.Error{Kind: llm.KindFatal, Provider: "test", Model: "offering"}
}

// A NOTE THAT MISSED THE TURN IS REPORTED EXPIRED.
//
// Taken while the turn ran, never read because the turn ended first: the
// person was answered `pending`, and the turn's record is the only place that
// can say what became of it. It says so before the turn's own completion, and
// a note sent after the end is answered closed.
func TestANoteThatMissedTheTurnIsReportedExpired(t *testing.T) {
	t.Parallel()
	desk := newSteerDesk()
	e, p := starting(t, modelsOf(t, offeringProvider{desk: desk, runID: "run-1"}))
	e.steers = desk
	tick := events.New(types.TaskAssigned{
		TaskID: "t-1", RoleName: "SWE", Description: "write the standup note",
	}, events.TraceContext{})
	if _, err := e.runTurn(t.Context(), Request{
		RunID: "run-1", Handle: "swe", WorkKey: "wk-1", Events: []*events.Event{tick},
	}); err == nil {
		t.Fatal("the turn succeeded; this case needs one that ends before its next round")
	}

	evs := p.published()
	at := position(evs, "agent_turn_steered")
	if at < 0 {
		t.Fatalf("no agent_turn_steered was published (%s)", typesOf(evs))
	}
	got, _ := events.DataAs[*types.AgentTurnSteered](evs[at])
	if got.Outcome != types.SteerExpired || got.NoteID != "req-1" || got.TurnID != "run-1" ||
		got.SteeredBySeat != "founder" || got.Note != "use staging" || got.Round != 0 {
		t.Errorf("the note is recorded %+v, want it expired, unread, naming who sent it", got)
	}
	if ended := position(evs, "agent_turn_completed"); ended < at {
		t.Errorf("the turn's completion (%d) precedes the note it missed (%d)", ended, at)
	}
	if status, _, _ := desk.offer("run-1", steer.Note{ID: "late", Text: "x"}); status != steer.StatusClosed {
		t.Errorf("a note after the turn ended answered %s, want closed", status)
	}
}

// modelsOf is a one-model registry under the key the starting fixture's seat
// runs on.
func modelsOf(t *testing.T, p llm.Provider) *phase.Registry {
	t.Helper()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "only", Provider: p}})
	if err != nil {
		t.Fatalf("phase.NewRegistry: %v", err)
	}
	return models
}
