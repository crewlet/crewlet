package livestate_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/store"
)

// turnIDs is the ids of the turns an overlay holds, in the order it lists them.
func turnIDs(o livestate.Overlay) []string {
	var out []string
	for _, turn := range o.Turns {
		out = append(out, turn.TurnID)
	}
	return out
}

// callOf is the call an overlay holds for turn `turnID`, nil for none.
func callOf(o livestate.Overlay, turnID string) *livestate.LiveCall {
	for _, call := range o.LiveCalls {
		if call.TurnID == turnID {
			return call
		}
	}
	return nil
}

// TWO TURNS OF ONE SEAT ARE BOTH HELD, AND NEITHER DROPS THE OTHER'S EVENTS.
//
// A seat runs up to its sandbox.max_concurrent_runs turns at once, so turn A's
// phase may land after turn B started, stamped before B's newest event. Read
// against the seat's last turn move, A's events were dropped as a straggler of
// a turn already over: A sat at gathering context with no call while it ran.
// Here every event of both turns lands on its own turn and its own call,
// interleaved and out of order, and the end of one leaves the other running.
//
// Mutation: guard markTurn or applyProgress on the seat's newest turn move
// rather than the turn's own, or hold one call per seat, and this fails.
func TestTwoConcurrentTurnsOfOneSeatAreBothHeldAndNeitherDropsTheOthers(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	a := func(over map[string]any) map[string]any {
		return turnPayload("tn-a", with(map[string]any{"phase": "execute", "iteration": 0}, over))
	}
	b := func(over map[string]any) map[string]any {
		return turnPayload("tn-b", with(map[string]any{"phase": "plan", "iteration": 0}, over))
	}
	s.Apply(env("agent_turn_started", turnPayload("tn-a", nil), at("2026-06-14T12:00:10Z"), id("a0")))
	s.Apply(env("agent_turn_started", turnPayload("tn-b", nil), at("2026-06-14T12:00:20Z"), id("b0")))
	s.Apply(env("agent_phase_started", b(nil), at("2026-06-14T12:00:21Z"), id("b1")))
	// A's phase and rounds, older than B's newest event, arriving after it.
	s.Apply(env("agent_phase_started", a(nil), at("2026-06-14T12:00:15Z"), id("a1")))
	s.Apply(env("agent_turn_progress", a(map[string]any{"round_num": 0, "response": "a says"}),
		streamOnly, at("2026-06-14T12:00:16Z")))
	s.Apply(env("agent_turn_progress", b(map[string]any{"round_num": 0, "response": "b says"}),
		streamOnly, at("2026-06-14T12:00:22Z")))
	s.Apply(env("agent_turn_progress", a(map[string]any{"round_num": 1, "response": "a says more"}),
		streamOnly, at("2026-06-14T12:00:17Z")))

	o := overlayOf(t, s, "Lead")
	if got := turnIDs(o); len(got) != 2 || got[0] != "tn-a" || got[1] != "tn-b" {
		t.Fatalf("turns = %v, want tn-a and tn-b, oldest first", got)
	}
	for _, turn := range o.Turns {
		if turn.Stage != livestate.StagePhase {
			t.Errorf("%s stage = %q, want phase: its own phase moved it", turn.TurnID, turn.Stage)
		}
	}
	if len(o.LiveCalls) != 2 {
		t.Fatalf("live calls = %+v, want one for each turn", o.LiveCalls)
	}
	if call := callOf(o, "tn-a"); call == nil || call.Response != "a says more" || call.Phase != "execute" {
		t.Errorf("tn-a's call = %+v, want its own newest round", call)
	}
	if call := callOf(o, "tn-b"); call == nil || call.Response != "b says" || call.Phase != "plan" {
		t.Errorf("tn-b's call = %+v, want its own round", call)
	}
	if o.Activity != livestate.ActivityWorking {
		t.Errorf("activity = %q, want working", o.Activity)
	}

	// A ends; B runs on, with its call and its phase.
	s.Apply(env("agent_phase_completed", a(nil), at("2026-06-14T12:00:30Z"), id("a2")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-a", nil), at("2026-06-14T12:00:31Z"), id("a3")))
	o = overlayOf(t, s, "Lead")
	if got := turnIDs(o); len(got) != 1 || got[0] != "tn-b" {
		t.Errorf("turns = %v, want tn-b alone once tn-a ended", got)
	}
	if len(o.LiveCalls) != 1 || callOf(o, "tn-b") == nil {
		t.Errorf("live calls = %+v, want tn-b's alone", o.LiveCalls)
	}
	if o.CurrentPhase == nil || *o.CurrentPhase != "plan" {
		t.Errorf("current phase = %v, want tn-b's plan: tn-a's end is not tn-b's", o.CurrentPhase)
	}
	if o.LastTurn == nil || o.LastTurn.TurnID != "tn-a" {
		t.Errorf("last turn = %+v, want tn-a", o.LastTurn)
	}
	if o.Activity != livestate.ActivityWorking {
		t.Errorf("activity = %q, want working: tn-b runs", o.Activity)
	}

	s.Apply(env("agent_turn_completed", turnPayload("tn-b", nil), at("2026-06-14T12:00:40Z"), id("b9")))
	o = overlayOf(t, s, "Lead")
	if len(o.Turns) != 0 || len(o.LiveCalls) != 0 || o.Activity != livestate.ActivityIdle {
		t.Errorf("turns %v, calls %+v, activity %q: want an idle seat on nothing",
			turnIDs(o), o.LiveCalls, o.Activity)
	}
}

// A NEWER SPAWN TAKES EACH RUNNING TURN AND LEAVES EACH PARKED ONE, judged per
// turn: a turn whose newest event is newer than the spawn ran under it and
// stays.
//
// Mutation: judge every turn against one seat-wide stamp, or drop parked turns
// with the running ones, and this fails.
func TestASpawnIsJudgedAgainstEachTurnOnItsOwn(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_turn_started", turnPayload("tn-old", nil), at("2026-06-14T12:00:00Z"), id("o")))
	s.Apply(env("agent_turn_started", turnPayload("tn-parked", nil), at("2026-06-14T12:00:01Z"), id("p0")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-parked", map[string]any{"suspended": true}),
		at("2026-06-14T12:00:02Z"), id("p1")))
	s.Apply(env("agent_turn_started", turnPayload("tn-new", nil), at("2026-06-14T12:00:20Z"), id("n")))
	s.Apply(env("agent_spawned", map[string]any{"agent_id": "Lead"}, at("2026-06-14T12:00:10Z"), id("sp")))

	got := turnIDs(overlayOf(t, s, "Lead"))
	if len(got) != 2 || got[0] != "tn-parked" || got[1] != "tn-new" {
		t.Errorf("turns = %v, want tn-parked (a park outlives a spawn) and tn-new "+
			"(newer than the spawn), and not tn-old", got)
	}
}

// A FROZEN CALL OF AN ENDED TURN GIVES WAY TO THE SEAT'S NEXT CALL. A held
// failure keeps a failed phase's call on screen past its turn's end; with one
// slot per turn nothing would ever take it down while the seat runs other turns.
//
// Mutation: drop the sweep in holdCall, and the dead call stays beside the new.
func TestAFrozenCallOfAnEndedTurnGivesWayToTheNextCall(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	dead := turnPayload("tn-1", map[string]any{"phase": "execute", "iteration": 0})
	s.Apply(env("agent_phase_started", dead, at("2026-06-14T12:00:00Z"), id("d0")))
	s.Apply(env("agent_phase_completed", with(dead, map[string]any{"failed": true, "error": "boom"}),
		at("2026-06-14T12:00:01Z"), id("d1")))
	s.Apply(env("llm_unavailable", turnPayload("tn-1", nil), at("2026-06-14T12:00:02Z"), id("d2")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", nil), at("2026-06-14T12:00:03Z"), id("d3")))
	if call := soleCall(t, overlayOf(t, s, "Lead").LiveCalls); call == nil || !call.Failed {
		t.Fatalf("call = %+v, want the failed call held past its turn's end", call)
	}

	s.Apply(env("agent_phase_started", turnPayload("tn-2", map[string]any{"phase": "plan", "iteration": 0}),
		at("2026-06-14T12:01:00Z"), id("n0")))
	if call := soleCall(t, overlayOf(t, s, "Lead").LiveCalls); call == nil || call.TurnID != "tn-2" {
		t.Errorf("call = %+v, want the next turn's alone", call)
	}
}

// THE SEED PUTS BACK EVERY TURN A SEAT LEFT PARKED, not its newest alone: a
// seat running several turns may have parked more than one on its runs, and
// each is still its turn after a restart.
//
// Mutation: seed only the newest turn, and the older parked turn is lost.
func TestTheSeedPutsBackEveryParkedTurn(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	s.Seed(livestate.History{Turns: []store.Turn{
		{TurnID: "parked-1", AgentID: "Lead", StartedAt: base, EndedAt: base.Add(time.Minute), Parked: true},
		{TurnID: "parked-2", AgentID: "Lead", StartedAt: base.Add(2 * time.Minute),
			EndedAt: base.Add(3 * time.Minute), Parked: true},
		{TurnID: "running", AgentID: "Lead", StartedAt: base.Add(4 * time.Minute)},
	}})
	o := overlayOf(t, s, "Lead")
	if got := turnIDs(o); len(got) != 2 || got[0] != "parked-1" || got[1] != "parked-2" {
		t.Errorf("turns = %v, want both parked turns and not the one nobody can say runs", got)
	}
	for _, turn := range o.Turns {
		if turn.Stage != livestate.StageParked {
			t.Errorf("%s stage = %q, want parked", turn.TurnID, turn.Stage)
		}
	}
}
