package livestate_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// turnPayload is one event of turn `turnID` on the Lead seat.
func turnPayload(turnID string, over map[string]any) map[string]any {
	return with(map[string]any{"role": "Lead", "agent_id": "a-1", "turn_id": turnID}, over)
}

func TestATurnIsWorkingFromItsStartEvent(t *testing.T) {
	t.Parallel()
	// The start is published before the prefetch, which is work: a seat that
	// read as idle until its first phase read as idle with a wake in hand.
	s := livestate.New()
	s.Apply(env("agent_spawned", map[string]any{"role": "Lead"}, at("2026-06-14T11:00:00Z")))
	change := s.Apply(env("agent_turn_started", turnPayload("tn-1", map[string]any{
		"node":            "core-1",
		"started_at":      "2026-06-14T11:59:59.5Z",
		"work_item_basis": "trigger",
		"work_item": map[string]any{
			"backend": "native", "id": "t-1", "key": "ENG-1", "project": "p-1",
		},
	})))
	if _, moved := change.Agents["Lead"]; !moved {
		t.Error("a turn's start did not move its seat, so nothing is pushed")
	}

	o := overlayOf(t, s, "Lead")
	if o.State != "working" {
		t.Errorf("state = %q, want working from the start event", o.State)
	}
	turn := o.Turn
	if turn == nil {
		t.Fatal("no turn on the seat after its start")
	}
	if turn.TurnID != "tn-1" || turn.Stage != livestate.StageContext || turn.Node != "core-1" ||
		turn.StartedAt != "2026-06-14T11:59:59.5Z" || turn.WorkItemBasis != "trigger" {
		t.Errorf("turn = %+v, want tn-1 gathering context on core-1 since its own start", turn)
	}
	if turn.WorkItem == nil || turn.WorkItem.Key != "ENG-1" {
		t.Errorf("work item = %+v, want ENG-1", turn.WorkItem)
	}

	s.Apply(env("agent_phase_started", turnPayload("tn-1", map[string]any{
		"phase": "execute", "iteration": 0,
	}), at("2026-06-14T12:00:01Z")))
	if got := overlayOf(t, s, "Lead").Turn; got == nil || got.Stage != livestate.StagePhase {
		t.Errorf("turn = %+v, want it in a phase once one started", got)
	}
}

func TestAStartThatLandsAfterItsPhaseDoesNotMoveTheStageBack(t *testing.T) {
	t.Parallel()
	// Two subjects, so the start can land second. It still states when the
	// turn began and what it is on, and moves nothing else.
	s := livestate.New()
	s.Apply(env("agent_phase_started", turnPayload("tn-1", map[string]any{
		"phase": "execute",
	}), at("2026-06-14T12:00:01Z")))
	s.Apply(env("agent_turn_started", turnPayload("tn-1", map[string]any{
		"started_at": "2026-06-14T12:00:00Z",
		"work_item":  map[string]any{"backend": "native", "id": "t-1", "key": "ENG-1"},
	}), at("2026-06-14T12:00:00Z")))

	turn := overlayOf(t, s, "Lead").Turn
	if turn == nil || turn.Stage != livestate.StagePhase {
		t.Fatalf("turn = %+v, want the phase stage kept", turn)
	}
	if turn.StartedAt != "2026-06-14T12:00:00Z" || turn.WorkItem == nil {
		t.Errorf("turn = %+v, want the late start's instant and item", turn)
	}
}

func TestAParkedTurnSaysParkedNotIdle(t *testing.T) {
	t.Parallel()
	// A turn that launched a detached coding run SUSPENDS: its segment
	// publishes a completion with suspended:true and the same turn
	// completes again when the run is collected. Read as an end, the seat
	// said it had finished while its work ran on in a box.
	s := livestate.New()
	s.Apply(env("agent_turn_started", turnPayload("tn-1", nil), at("2026-06-14T12:00:00Z")))
	s.Apply(env("agent_phase_started", turnPayload("tn-1", map[string]any{"phase": "execute"}),
		at("2026-06-14T12:00:01Z")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", map[string]any{"suspended": true}),
		at("2026-06-14T12:00:05Z")))

	o := overlayOf(t, s, "Lead")
	if o.Turn == nil || o.Turn.Stage != livestate.StageParked {
		t.Fatalf("turn = %+v, want tn-1 parked", o.Turn)
	}
	if o.LastTurn != nil {
		t.Errorf("last turn = %+v, want none: a suspension is not an end", o.LastTurn)
	}
	if o.LiveCall != nil {
		t.Errorf("live call = %+v, want none while nothing is being called", o.LiveCall)
	}

	// The run is collected and the SAME turn resumes, possibly elsewhere.
	s.Apply(env("agent_turn_started", turnPayload("tn-1", map[string]any{
		"resumed": true, "node": "core-2", "started_at": "2026-06-14T13:00:00Z",
	}), at("2026-06-14T13:00:00Z")))
	o = overlayOf(t, s, "Lead")
	if o.Turn == nil || o.Turn.Stage != livestate.StageContext || o.Turn.Node != "core-2" {
		t.Fatalf("turn = %+v, want the resumed segment on core-2", o.Turn)
	}
	if o.Turn.StartedAt != "2026-06-14T12:00:00Z" {
		t.Errorf("started at %q, want the turn's first start rather than the segment's",
			o.Turn.StartedAt)
	}

	s.Apply(env("agent_turn_completed", turnPayload("tn-1", nil), at("2026-06-14T13:00:09Z")))
	o = overlayOf(t, s, "Lead")
	if o.Turn != nil {
		t.Errorf("turn = %+v, want none once it ended", o.Turn)
	}
	want := livestate.LastTurn{TurnID: "tn-1", EndedAt: "2026-06-14T13:00:09Z",
		Outcome: livestate.OutcomeCompleted}
	if o.LastTurn == nil || *o.LastTurn != want {
		t.Errorf("last turn = %+v, want %+v", o.LastTurn, want)
	}
}

func TestAFailureDuringATurnIsItsOutcome(t *testing.T) {
	t.Parallel()
	// The turn list calls a turn failed when ANY of its events was a
	// failure; the live projection says the same about the same turn, or a
	// seeded seat and a watched one disagree.
	s := livestate.New()
	s.Apply(env("agent_turn_started", turnPayload("tn-1", nil), at("2026-06-14T12:00:00Z")))
	s.Apply(env("llm_unavailable", turnPayload("tn-1", map[string]any{"kind": "llm_unavailable"}),
		at("2026-06-14T12:00:02Z")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", nil), at("2026-06-14T12:00:03Z")))

	if last := overlayOf(t, s, "Lead").LastTurn; last == nil || last.Outcome != livestate.OutcomeFailed {
		t.Errorf("last turn = %+v, want failed", last)
	}
}

func TestAStragglerDoesNotPutAnEndedTurnBack(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_turn_started", turnPayload("tn-1", nil), at("2026-06-14T12:00:00Z")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", nil), at("2026-06-14T12:00:09Z")))
	// The phase start lost a cross-topic race to its own turn's end.
	s.Apply(env("agent_phase_started", turnPayload("tn-1", map[string]any{"phase": "execute"}),
		at("2026-06-14T12:00:01Z")))

	if turn := overlayOf(t, s, "Lead").Turn; turn != nil {
		t.Errorf("turn = %+v, want the ended turn kept off the seat", turn)
	}
}

func TestACompletionThatLostARaceToTheNextTurnStillEndsIt(t *testing.T) {
	t.Parallel()
	// The state machine drops the completion as older than the next turn's
	// start; the turn bookkeeping must not, or the seat never has a last
	// turn and its current one is never the right one.
	s := livestate.New()
	s.Apply(env("agent_turn_started", turnPayload("tn-1", nil), at("2026-06-14T12:00:00Z")))
	s.Apply(env("agent_turn_started", turnPayload("tn-2", nil), at("2026-06-14T12:00:10Z")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", nil), at("2026-06-14T12:00:09Z")))

	o := overlayOf(t, s, "Lead")
	if o.Turn == nil || o.Turn.TurnID != "tn-2" {
		t.Errorf("turn = %+v, want tn-2", o.Turn)
	}
	if o.LastTurn == nil || o.LastTurn.TurnID != "tn-1" {
		t.Errorf("last turn = %+v, want tn-1", o.LastTurn)
	}
}

func TestALostRunEndsItsParkedTurn(t *testing.T) {
	t.Parallel()
	// Nothing resumes a turn whose coding run was lost: the run is settled
	// like any other lost turn. Left on the seat, it read as parked for good.
	s := livestate.New()
	s.Apply(env("agent_turn_started", turnPayload("tn-1", nil), at("2026-06-14T12:00:00Z")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", map[string]any{"suspended": true}),
		at("2026-06-14T12:00:05Z")))
	change := s.Apply(env("sandbox_run_failed", turnPayload("tn-1", map[string]any{
		"reason": "collect_unreachable",
	}), at("2026-06-14T12:30:00Z")))

	if _, moved := change.Agents["Lead"]; !moved {
		t.Error("ending the parked turn did not move its seat")
	}
	o := overlayOf(t, s, "Lead")
	if o.Turn != nil {
		t.Errorf("turn = %+v, want the lost run's turn ended", o.Turn)
	}
	if o.LastTurn == nil || o.LastTurn.TurnID != "tn-1" || o.LastTurn.Outcome != livestate.OutcomeFailed {
		t.Errorf("last turn = %+v, want tn-1 failed", o.LastTurn)
	}
}

func TestTheReflectionPassExtendsTheLastTurnsEnd(t *testing.T) {
	t.Parallel()
	// The turn list's ended_at is the turn's newest event, which is the
	// reflection pass after the completion: a seeded seat and a watched one
	// say the same instant.
	s := livestate.New()
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", nil), at("2026-06-14T12:00:09Z")))
	s.Apply(env("reflection_completed", turnPayload("tn-1", nil), at("2026-06-14T12:00:15Z")))

	if last := overlayOf(t, s, "Lead").LastTurn; last == nil || last.EndedAt != "2026-06-14T12:00:15Z" {
		t.Errorf("last turn = %+v, want it ended at the reflection", last)
	}
}

func TestTheOverlayAlwaysCarriesTheTurnKeys(t *testing.T) {
	t.Parallel()
	// The client MERGES a pushed row over the one it holds, so a key left
	// out reads as unchanged: a finished turn would stay on the card.
	s := livestate.New()
	s.Apply(env("agent_spawned", map[string]any{"role": "Lead"}))
	rows := s.OverlayRows([]string{"Lead"})
	if len(rows) != 1 {
		t.Fatalf("rows = %+v", rows)
	}
	for _, key := range []string{"turn", "last_turn"} {
		if _, ok := rows[0][key]; !ok {
			t.Errorf("the pushed row has no %q key", key)
		}
	}
}

func TestTheStagesAndOutcomesAreClosedSets(t *testing.T) {
	t.Parallel()
	for _, stage := range livestate.Stages {
		if !stage.Valid() {
			t.Errorf("%q is listed and not valid", stage)
		}
	}
	for _, outcome := range livestate.TurnOutcomes {
		if !outcome.Valid() {
			t.Errorf("%q is listed and not valid", outcome)
		}
	}
	if livestate.Stage("thinking").Valid() || livestate.TurnOutcome("done").Valid() {
		t.Error("a value this build does not know was accepted")
	}
}
