package livestate_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// turnPayload is one event of turn `turnID` on the Lead seat — the seat named by
// its agent id, beside a role name a second seat could share.
func turnPayload(turnID string, over map[string]any) map[string]any {
	return with(map[string]any{"agent_id": "Lead", "role": "Engineer", "turn_id": turnID}, over)
}

func TestATurnIsWorkingFromItsStartEvent(t *testing.T) {
	t.Parallel()
	// The start is published before the prefetch, which is work: a seat that
	// read as idle until its first phase read as idle with a wake in hand.
	s := livestate.New()
	s.Apply(env("agent_spawned", map[string]any{"agent_id": "Lead"}, at("2026-06-14T11:00:00Z")))
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
	if o.Activity != livestate.ActivityWorking {
		t.Errorf("activity = %q, want working from the start event", o.Activity)
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

// parked is a seat whose turn tn-1 launched its executor's run and parked.
func parked(t *testing.T) *livestate.LiveState {
	t.Helper()
	s := livestate.New()
	s.Apply(env("agent_turn_started", turnPayload("tn-1", nil), at("2026-06-14T12:00:00Z")))
	s.Apply(env("agent_phase_started", turnPayload("tn-1", map[string]any{"phase": "execute", "iteration": 1}),
		at("2026-06-14T12:00:01Z")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", map[string]any{"suspended": true}),
		at("2026-06-14T12:00:05Z")))
	if turn := overlayOf(t, s, "Lead").Turn; turn == nil || turn.Stage != livestate.StageParked {
		t.Fatalf("turn = %+v, want tn-1 parked before the case begins", turn)
	}
	return s
}

// A WORKER THE RUN DELEGATED TO LEAVES ITS TURN PARKED. `delegate` is the
// executor's alone and every executor is its coding CLI's run in a box, so
// every worker runs while its turn is parked, and its record — nested under
// the execute phase — is the only event the turn publishes until the run is
// collected. Read as one of the turn's own phases it un-parked the turn: the
// seat stopped saying its coding run was working, and the seat's next spawn,
// which spares a parked turn alone, took the turn off the seat while the run
// worked on.
//
// Mutation: stage every phase record as `phase`, and the turn reads phase.
func TestAWorkerItsRunDelegatedToLeavesItsTurnParked(t *testing.T) {
	t.Parallel()
	s := parked(t)
	s.Apply(env("agent_phase_completed", turnPayload("tn-1", map[string]any{
		"phase": "subagent", "iteration": 1, "host_phase": "execute", "host_iteration": 1,
		"task_id": "survey", "node": "core-2", "failed": true,
	}), at("2026-06-14T12:03:00Z")))

	o := overlayOf(t, s, "Lead")
	if o.Turn == nil || o.Turn.Stage != livestate.StageParked {
		t.Fatalf("turn = %+v, want tn-1 still parked on its run", o.Turn)
	}
	if o.Turn.Node != "core-2" {
		t.Errorf("node = %q, want core-2, the node that published the turn's newest event", o.Turn.Node)
	}
	// NOT WORK OF ITS OWN: a parked turn is working only through its run's
	// record, and this case holds none. Un-parked by its worker's report, the
	// turn kept the seat working for as long as the turn lasted, over a run
	// whose loss no event may ever report.
	if o.Activity != livestate.ActivityIdle {
		t.Errorf("activity = %q, want idle: no run of the seat's is on the record", o.Activity)
	}

	// The seat's next instance finds the turn still parked on its run.
	s.Apply(env("agent_spawned", map[string]any{"agent_id": "Lead"}, at("2026-06-14T12:04:00Z")))
	if turn := overlayOf(t, s, "Lead").Turn; turn == nil || turn.Stage != livestate.StageParked {
		t.Fatalf("turn = %+v, want tn-1 to outlive the spawn, parked", turn)
	}

	// The failed worker is still the turn's failure when the turn ends.
	s.Apply(env("agent_turn_started", turnPayload("tn-1", map[string]any{"resumed": true}),
		at("2026-06-14T13:00:00Z")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", nil), at("2026-06-14T13:00:09Z")))
	if last := overlayOf(t, s, "Lead").LastTurn; last == nil || last.Outcome != livestate.OutcomeFailed {
		t.Errorf("last turn = %+v, want tn-1 failed for its failed worker", last)
	}
}

// A COLLECTED LAUNCH LEAVES ITS TURN PARKED, and the turn's own segment is
// what moves it on. The collection publishes the launch's own record
// (`executor_collected`) before it resumes the turn, and a resume can wait — on
// another node, behind the seat's budget, on a retry — so the turn is parked
// on its collected run until the resumed segment says otherwise. Read as a
// step of the turn, it showed a phase running that nothing was running.
//
// Mutation: stage the launch's record as `phase`, and the turn reads phase;
// let no phase record move a parked turn, and the resumed execute record
// leaves it parked.
func TestACollectedLaunchLeavesItsTurnParked(t *testing.T) {
	t.Parallel()
	s := parked(t)
	s.Apply(env("executor_collected", turnPayload("tn-1", map[string]any{
		"iteration": 1, "launch_id": "job-1", "total_tokens": 900,
	}), at("2026-06-14T12:30:00Z")))
	if turn := overlayOf(t, s, "Lead").Turn; turn == nil || turn.Stage != livestate.StageParked {
		t.Fatalf("turn = %+v, want tn-1 parked on its collected run", turn)
	}

	// The resumed segment's own execute record — its start overtaken on its
	// subject — is one of the turn's phases, and moves it.
	s.Apply(env("agent_phase_completed", turnPayload("tn-1", map[string]any{
		"phase": "execute", "iteration": 1,
	}), at("2026-06-14T12:30:02Z")))
	if turn := overlayOf(t, s, "Lead").Turn; turn == nil || turn.Stage != livestate.StagePhase {
		t.Fatalf("turn = %+v, want tn-1 in a phase once its own segment ran one", turn)
	}
}

// A COLLECTED LAUNCH OPENS ITS TURN, PARKED, on a seat that holds none: a
// projection that missed the park — it started after it, and its seed read no
// parked turn — learns of the turn from the launch, and learns it parked, since
// a launch is collected only while its turn waits on it.
//
// Mutation: route the launch's record nowhere in the turn, and the seat holds
// no turn; open it in a phase, and it reads phase.
func TestACollectedLaunchOpensItsTurnParked(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("executor_collected", turnPayload("tn-1", map[string]any{
		"iteration": 1, "launch_id": "job-1",
	}), at("2026-06-14T12:30:00Z")))
	if turn := overlayOf(t, s, "Lead").Turn; turn == nil || turn.TurnID != "tn-1" ||
		turn.Stage != livestate.StageParked {
		t.Fatalf("turn = %+v, want tn-1 opened parked on its collected run", turn)
	}
}

// A LAUNCH THAT FAILED IS THE SEAT'S LAST ERROR, as a failed phase is — in the
// executor's phase, whose run it was — and the turn it belongs to is failed
// when it ends. A launch that stopped to ask is no failure, and says nothing
// about the seat's errors.
//
// Mutation: read the launch's record as no state event, and the seat has no
// last error; take its notes for a failure, and the asking launch has one.
func TestAFailedLaunchIsTheSeatsLastError(t *testing.T) {
	t.Parallel()
	s := parked(t)
	s.Apply(env("executor_collected", turnPayload("tn-1", map[string]any{
		"iteration": 1, "launch_id": "job-1", "notes": "stopped to ask: which branch?",
	}), id("asked"), at("2026-06-14T12:30:00Z")))
	if got := overlayOf(t, s, "Lead").LastError; got != nil {
		t.Fatalf("last error = %+v after a launch that stopped to ask, want none", got)
	}

	s.Apply(env("executor_collected", turnPayload("tn-1", map[string]any{
		"iteration": 1, "launch_id": "job-2", "failed": true,
		"error": "the coding agent exited with status 1", "error_kind": "coding_run_failed",
	}), id("broke"), at("2026-06-14T12:40:00Z")))
	got := overlayOf(t, s, "Lead").LastError
	if got == nil || got.Kind != "coding_run_failed" || got.Phase != "execute" ||
		got.TurnID != "tn-1" || got.EventID != "broke" {
		t.Fatalf("last error = %+v, want the failed launch's kind, in execute, on its turn", got)
	}
	if turn := overlayOf(t, s, "Lead").Turn; turn == nil || turn.Stage != livestate.StageParked {
		t.Fatalf("turn = %+v, want tn-1 still parked: a failed launch's round is resumed", turn)
	}
	s.Apply(env("agent_turn_started", turnPayload("tn-1", map[string]any{"resumed": true}),
		at("2026-06-14T13:00:00Z")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", nil), at("2026-06-14T13:00:09Z")))
	if last := overlayOf(t, s, "Lead").LastTurn; last == nil || last.Outcome != livestate.OutcomeFailed {
		t.Errorf("last turn = %+v, want tn-1 failed for its failed launch", last)
	}
}

// A NESTED RECORD OPENS A TURN THE SEAT DOES NOT HOLD AT THE STAGE ITS HOST
// IMPLIES: a projection that missed the park — it started after it, and its
// seed read no parked turn — still learns of the turn from its worker, and
// learns it parked, since a worker runs only while its turn is parked on the
// executor's run; a judge consulted by a phase the engine drives is in that
// phase.
//
// Mutation: open every nested record's turn in a phase, and the worker's
// reads phase.
func TestANestedRecordOpensItsTurnAtTheStageItsHostImplies(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		payload map[string]any
		want    livestate.Stage
	}{
		"a worker under the executor's run": {
			payload: map[string]any{"phase": "subagent", "iteration": 1, "host_phase": "execute"},
			want:    livestate.StageParked,
		},
		"the judge under the onboarding pass": {
			payload: map[string]any{"phase": "judge", "iteration": 0, "host_phase": "onboarding"},
			want:    livestate.StagePhase,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := livestate.New()
			s.Apply(env("agent_phase_completed", turnPayload("tn-1", tc.payload), at("2026-06-14T12:03:00Z")))
			if turn := overlayOf(t, s, "Lead").Turn; turn == nil || turn.TurnID != "tn-1" || turn.Stage != tc.want {
				t.Errorf("turn = %+v, want tn-1 at %s", turn, tc.want)
			}
		})
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
	change := s.Apply(env("executor_failed", turnPayload("tn-1", map[string]any{
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
	s.Apply(env("agent_spawned", map[string]any{"agent_id": "Lead"}))
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

// A TURN A PERSON STOPPED ENDS AS STOPPED, never as failed, even after a
// failure in it and even where it was parked on its coding run: a stop ends a
// parked turn with a completion of its own, newer than the park, and the seat's
// last turn says the person ended it.
func TestAStoppedTurnEndsAsStoppedAndNotFailed(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_turn_started", turnPayload("tn-1", nil), at("2026-06-14T12:00:00Z")))
	// A provider call that failed and recovered, earlier in the turn.
	s.Apply(env("llm_unavailable", turnPayload("tn-1", nil), at("2026-06-14T12:00:02Z")))
	s.Apply(env("agent_turn_completed", turnPayload("tn-1", map[string]any{"suspended": true}),
		at("2026-06-14T12:00:05Z")))
	change := s.Apply(env("agent_turn_completed", turnPayload("tn-1", map[string]any{
		"stopped": true, "error_kind": "stopped",
	}), at("2026-06-14T12:30:00Z")))

	if _, moved := change.Agents["Lead"]; !moved {
		t.Error("ending the parked turn did not move its seat")
	}
	o := overlayOf(t, s, "Lead")
	if o.Turn != nil {
		t.Errorf("turn = %+v, want the stopped turn off the seat", o.Turn)
	}
	if o.LastTurn == nil || o.LastTurn.TurnID != "tn-1" || o.LastTurn.Outcome != livestate.OutcomeStopped {
		t.Errorf("last turn = %+v, want tn-1 stopped", o.LastTurn)
	}
}
