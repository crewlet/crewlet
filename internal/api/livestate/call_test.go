package livestate_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// planCall is the coordinates most of these tests share.
func planCall() map[string]any {
	return map[string]any{"role": "Lead", "turn_id": "tn-1", "phase": "plan", "iteration": 0}
}

func liveCallOf(t *testing.T, s *livestate.LiveState, role string) *livestate.LiveCall {
	t.Helper()
	return overlayOf(t, s, role).LiveCall
}

func TestAPhaseStartSeedsAPlaceholderCall(t *testing.T) {
	t.Parallel()
	// The placeholder makes the live row appear the instant a phase
	// starts, rather than at the first round the model returns.
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))

	call := liveCallOf(t, s, "Lead")
	if call == nil {
		t.Fatal("a phase start seeded no live call")
	}
	if call.TurnID != "tn-1" || call.Phase != "plan" {
		t.Errorf("call = %+v, want the phase's coordinates", call)
	}
	if !call.InProgress {
		t.Error("the placeholder is not marked in progress")
	}
	// -1, not 0: the FIRST real round is round 0, and a placeholder at 0
	// would make it read as a stale repeat and be dropped — leaving the
	// row empty until round 1.
	if call.RoundNum != -1 {
		t.Errorf("placeholder round = %d, want -1 so round 0 is newer", call.RoundNum)
	}
}

func TestTheLiveCallCarriesItsTriggerSource(t *testing.T) {
	t.Parallel()
	// So a refresh mid-call still shows what woke the turn.
	s := livestate.New()
	trigger := map[string]any{"type": "external_notification", "integration": "slack"}
	s.Apply(env("agent_phase_started", with(planCall(), map[string]any{"trigger": trigger})))

	call := liveCallOf(t, s, "Lead")
	if call.Trigger["integration"] != "slack" {
		t.Errorf("trigger = %v, want the phase's own", call.Trigger)
	}

	// A round that carries no trigger of its own falls back to the
	// placeholder's, so the source never blanks out mid-call.
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 0, "response": "hi"}),
		streamOnly))
	call = liveCallOf(t, s, "Lead")
	if call.Trigger["integration"] != "slack" {
		t.Errorf("trigger = %v: the source blanked out mid-call", call.Trigger)
	}
}

func TestAProgressRoundFillsInTheCall(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("agent_turn_progress", with(planCall(), map[string]any{
		"round_num":       2,
		"model":           "claude-sonnet-5",
		"response":        "thinking",
		"input_tokens":    10,
		"output_tokens":   4,
		"total_tokens":    14,
		"tool_executions": []any{map[string]any{"name": "search"}},
	}), streamOnly, at("2026-06-14T12:00:03+00:00")))

	call := liveCallOf(t, s, "Lead")
	if call.Model != "claude-sonnet-5" || call.Response != "thinking" {
		t.Errorf("call = %+v", call)
	}
	if call.RoundNum != 2 || call.Rounds != 3 {
		t.Errorf("round = %d, rounds = %d, want 2 and 3", call.RoundNum, call.Rounds)
	}
	if call.TotalTokens != 14 {
		t.Errorf("total tokens = %d, want 14", call.TotalTokens)
	}
	if len(call.ToolExecutions) != 1 {
		t.Errorf("tool executions = %v", call.ToolExecutions)
	}
	if call.UpdatedAt != "2026-06-14T12:00:03+00:00" {
		t.Errorf("updated_at = %q", call.UpdatedAt)
	}
}

func TestAProgressRoundNeverEntersTheActivityFeed(t *testing.T) {
	t.Parallel()
	// Stream-only: the event store drops these, and a projection that
	// buffered them would show a feed no reload could reproduce.
	s := livestate.New()
	change := s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 0}), streamOnly))
	if change.Events {
		t.Error("a progress round was recorded in the feed")
	}
	if len(s.RecentEvents(0)) != 0 {
		t.Errorf("feed = %v, want empty", s.RecentEvents(0))
	}
}

func TestACompletedPhaseClearsItsOwnCall(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("agent_phase_completed", planCall(), at("2026-06-14T12:00:05+00:00")))

	if call := liveCallOf(t, s, "Lead"); call != nil {
		t.Errorf("live call = %+v, want cleared", call)
	}
}

func TestACompletedPhaseLeavesAnotherPhasesCallAlone(t *testing.T) {
	t.Parallel()
	// A late completion for a prior phase must not wipe a newer phase's
	// live row.
	s := livestate.New()
	s.Apply(env("agent_phase_started", with(planCall(), map[string]any{"phase": "execute"}),
		at("2026-06-14T12:00:04+00:00")))
	s.Apply(env("agent_phase_completed", planCall(), at("2026-06-14T12:00:05+00:00")))

	call := liveCallOf(t, s, "Lead")
	if call == nil {
		t.Fatal("a completion for a different phase wiped the live call")
	}
	if call.Phase != "execute" {
		t.Errorf("phase = %q, want execute", call.Phase)
	}
}

func TestAStaleRoundOfTheSameCallIsIgnored(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 3, "response": "newest"}),
		streamOnly, at("2026-06-14T12:00:03+00:00")))
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 1, "response": "stale"}),
		streamOnly, at("2026-06-14T12:00:04+00:00")))

	if got := liveCallOf(t, s, "Lead").Response; got != "newest" {
		t.Errorf("response = %q: an earlier round overwrote a later one", got)
	}
}

func TestAStragglerDoesNotResurrectAFinishedPhase(t *testing.T) {
	t.Parallel()
	// A phase publishes its last round and its completion back to back on
	// DIFFERENT topics, and the API reads them through one wildcard
	// subscription where cross-topic order is not guaranteed. A round
	// arriving after its own completion would find no live call and seed a
	// fresh one — an in-flight row on a finished phase that nothing would
	// ever clear.
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("agent_phase_completed", planCall(), at("2026-06-14T12:00:05+00:00")))
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 3, "response": "late"}),
		streamOnly, at("2026-06-14T12:00:04+00:00")))

	if call := liveCallOf(t, s, "Lead"); call != nil {
		t.Errorf("live call = %+v: a straggler resurrected a finished phase", call)
	}
}

func TestTheStragglerGuardComparesInstantsNotEncodings(t *testing.T) {
	t.Parallel()
	// The same moment, spelled two ways. "Z" sorts AFTER "+" as a raw
	// string, so a straggler encoded one way reads as newer than the
	// completion encoded the other.
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("agent_phase_completed", planCall(), at("2026-06-14T12:00:05+00:00")))
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 3, "response": "late"}),
		streamOnly, at("2026-06-14T12:00:05Z")))

	if call := liveCallOf(t, s, "Lead"); call != nil {
		t.Errorf("live call = %+v: a straggler won on its encoding", call)
	}
}

func TestAResumedPhaseComesBackOnScreen(t *testing.T) {
	t.Parallel()
	// The counterfactual to the straggler guard, and the reason it is a
	// timestamp rather than a flag: a suspended Execute phase publishes a
	// completion CHECKPOINT under these exact coordinates, then resumes
	// the same loop when the detached run lands and streams more rounds
	// under them. Those rounds are strictly newer, and swallowing them
	// would leave a resumed sandbox phase invisible for the rest of its
	// run.
	//
	// The resumed round is spelled naive and the checkpoint aware, which
	// is the mixed encoding that made this break: compared raw, nine
	// minutes later sorts BEFORE the checkpoint.
	base := with(planCall(), map[string]any{"phase": "execute"})
	s := livestate.New()
	s.Apply(env("agent_phase_started", base))
	s.Apply(env("agent_phase_completed",
		with(base, map[string]any{"notes": "suspended: run_sandbox"}),
		at("2026-06-14T12:00:05+00:00")))
	s.Apply(env("agent_turn_progress",
		with(base, map[string]any{"round_num": 4, "response": "back from the sandbox"}),
		streamOnly, at("2026-06-14T12:09:00")))

	call := liveCallOf(t, s, "Lead")
	if call == nil {
		t.Fatal("the resumed phase never came back on screen")
	}
	if call.Response != "back from the sandbox" {
		t.Errorf("response = %q", call.Response)
	}
}

func TestTheCallKeepsTheTimestampEncodingItArrivedIn(t *testing.T) {
	t.Parallel()
	// Normalizing is for COMPARISON only. updated_at goes back out on the
	// wire and must not be rewritten on the way through.
	s := livestate.New()
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 0, "response": "hi"}),
		streamOnly, at("2026-06-14T12:00:05+00:00")))

	if got := liveCallOf(t, s, "Lead").UpdatedAt; got != "2026-06-14T12:00:05+00:00" {
		t.Errorf("updated_at = %q, want the encoding it arrived in", got)
	}
}

func TestAProgressRoundDoesNotOverwriteAFrozenFailedCall(t *testing.T) {
	t.Parallel()
	// A failed call is the most informative thing on a seat's page. A
	// straggler round must not replace it with a healthy-looking one.
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("agent_phase_completed",
		with(planCall(), map[string]any{"failed": true, "error": "boom"}),
		at("2026-06-14T12:00:05+00:00")))
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 9, "response": "as if nothing happened"}),
		streamOnly, at("2026-06-14T12:00:04+00:00")))

	call := liveCallOf(t, s, "Lead")
	if call == nil {
		t.Fatal("the frozen call disappeared")
	}
	if !call.Failed {
		t.Error("the call is no longer marked failed")
	}
	if call.Response == "as if nothing happened" {
		t.Error("a straggler overwrote a frozen failed call")
	}
}

func TestAProgressRoundForAnotherCallLosesToANewerOne(t *testing.T) {
	t.Parallel()
	// Not the same call, and the one held is newer: a delivery that
	// crossed with a phase transition must not roll the row back.
	s := livestate.New()
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"phase": "execute", "round_num": 0, "response": "current"}),
		streamOnly, at("2026-06-14T12:00:10+00:00")))
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 5, "response": "older phase"}),
		streamOnly, at("2026-06-14T12:00:02+00:00")))

	call := liveCallOf(t, s, "Lead")
	if call.Response != "current" {
		t.Errorf("response = %q: an older call replaced a newer one", call.Response)
	}
}

func TestAProgressRoundWakesASeatThatWasNotWorking(t *testing.T) {
	t.Parallel()
	// A round IS work, whatever the last state event said. Leaving the
	// seat idle while its call streamed would put a live row on a seat the
	// roster shows as doing nothing.
	s := livestate.New()
	s.Apply(env("llm_unavailable", map[string]any{"role": "Lead"}))
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 0}),
		streamOnly, at("2026-06-14T12:00:10+00:00")))

	got := overlayOf(t, s, "Lead")
	if got.State != "working" {
		t.Errorf("state = %q, want working", got.State)
	}
	if got.AFKReason != "" {
		t.Errorf("afk reason = %q, want cleared", got.AFKReason)
	}
}

func TestARoundWithNoRoleIsDropped(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	change := s.Apply(env("agent_turn_progress",
		map[string]any{"turn_id": "tn-1", "round_num": 0}, streamOnly))
	if len(change.Agents) != 0 {
		t.Errorf("a role-less round moved %v", change.Agents)
	}
}

func TestAnOverlayDoesNotAliasTheProjection(t *testing.T) {
	t.Parallel()
	// A reader holds an overlay while the stream keeps applying events,
	// and the failure path writes into the live call IN PLACE. Handing out
	// the projection's own pointers would let a push serialize a call
	// while it was being frozen underneath it.
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 0, "response": "first"}),
		streamOnly, at("2026-06-14T12:00:03+00:00")))

	held := liveCallOf(t, s, "Lead")
	s.Apply(env("agent_phase_completed",
		with(planCall(), map[string]any{"failed": true, "error": "boom", "response": "died"}),
		at("2026-06-14T12:00:05+00:00")))

	if held.Failed {
		t.Error("an overlay handed out earlier was mutated by a later event")
	}
	if held.Response != "first" {
		t.Errorf("held response = %q, want the value at the time it was read", held.Response)
	}
}

func TestACompletionWithNoLiveCallStillClosesThePhase(t *testing.T) {
	t.Parallel()
	// The API can come up mid-phase, so a completion can arrive with
	// nothing to clear. The phase is still over, and a straggler round
	// that arrives afterwards must not seed a fresh in-flight row on it —
	// which is why the completion is recorded BEFORE the "nothing to
	// clear" return rather than after it.
	s := livestate.New()
	s.Apply(env("agent_phase_completed", planCall(), at("2026-06-14T12:00:05+00:00")))
	if call := liveCallOf(t, s, "Lead"); call != nil {
		t.Fatalf("a completion with no live call seeded one: %+v", call)
	}

	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 3, "response": "late"}),
		streamOnly, at("2026-06-14T12:00:04+00:00")))

	if call := liveCallOf(t, s, "Lead"); call != nil {
		t.Errorf("live call = %+v: a straggler seeded a row on a finished phase "+
			"that had nothing to clear", call)
	}
}

func TestAFailedPhaseStillClosesItselfToStragglers(t *testing.T) {
	t.Parallel()
	// The other early return the recording has to precede. A failed phase
	// keeps its frozen call, and a straggler must still not be able to
	// replace it with a healthy-looking one — the frozen-call guard and
	// this one protect the same row from two directions.
	s := livestate.New()
	s.Apply(env("agent_phase_completed",
		with(planCall(), map[string]any{"failed": true, "error": "boom"}),
		at("2026-06-14T12:00:05+00:00")))
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 3, "response": "late"}),
		streamOnly, at("2026-06-14T12:00:04+00:00")))

	if call := liveCallOf(t, s, "Lead"); call != nil {
		t.Errorf("live call = %+v: a straggler seeded a row on a failed phase", call)
	}
}

func TestADiscardedStragglerDoesNotLeaveTheSeatLookingBusy(t *testing.T) {
	t.Parallel()
	// The seat state used to be set BEFORE the discard guards, so a
	// straggler that was about to be thrown away still flipped the seat to
	// "working" — and then the guard returned "", so nothing was pushed and
	// nothing ever corrected it. The seat sat rendering as working with no
	// live call to show, until some later round happened to arrive.
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("agent_phase_completed", planCall(), at("2026-06-14T12:00:05+00:00")))
	s.Apply(env("reflection_completed", map[string]any{"role": "Lead"},
		at("2026-06-14T12:00:06+00:00")))

	if got := overlayOf(t, s, "Lead"); got.State != "idle" {
		t.Fatalf("state = %q before the straggler, want idle", got.State)
	}

	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 3, "response": "late"}),
		streamOnly, at("2026-06-14T12:00:04+00:00")))

	got := overlayOf(t, s, "Lead")
	if got.State == "working" && got.LiveCall == nil {
		t.Error("a discarded straggler left the seat working with no call to show for it")
	}
	if got.State != "idle" {
		t.Errorf("state = %q, want the seat left as the reflection found it", got.State)
	}
}

func TestAnOpeningRoundThatBeatsItsPhaseStartIsNotBlanked(t *testing.T) {
	t.Parallel()
	// agent_phase_started and agent_turn_progress travel on DIFFERENT
	// subjects and reach the API through one wildcard subscription, so the
	// first round can land first. The seed used to be unconditional: it
	// replaced a call that already had a model, a response and tool calls
	// with an empty placeholder, and the reorder guard could not catch it
	// because applyProgress never advances the state clock.
	s := livestate.New()
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{
			"round_num": 0, "response": "already thinking", "model": "gpt-x",
		}),
		streamOnly, at("2026-06-14T12:00:01+00:00")))
	s.Apply(env("agent_phase_started", planCall(), at("2026-06-14T12:00:00+00:00")))

	call := liveCallOf(t, s, "Lead")
	if call == nil {
		t.Fatal("the phase start cleared the call its own round had already seeded")
	}
	if call.Response != "already thinking" {
		t.Errorf("response = %q — a late phase start blanked the round that beat it", call.Response)
	}
	if call.Model != "gpt-x" {
		t.Errorf("model = %q, want the round's own", call.Model)
	}
}

func TestAPhaseStartStillSeedsADifferentCall(t *testing.T) {
	t.Parallel()
	// The counterfactual: the guard is keyed on the CALL, so a genuinely
	// new phase still replaces whatever the seat was showing.
	s := livestate.New()
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 0, "response": "plan work"}),
		streamOnly))
	s.Apply(env("agent_phase_started",
		with(planCall(), map[string]any{"phase": "execute"}),
		at("2026-06-14T12:00:02+00:00")))

	call := liveCallOf(t, s, "Lead")
	if call == nil || call.Phase != "execute" {
		t.Fatalf("live call = %+v, want the new execute phase", call)
	}
	if call.Response != "" {
		t.Errorf("response = %q, want a fresh placeholder for a new phase", call.Response)
	}
}
