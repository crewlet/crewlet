package livestate_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// --- the AFK hold -------------------------------------------------------- //

func TestATurnEndingDoesNotClearTheFailureThatCausedIt(t *testing.T) {
	t.Parallel()
	// An engine-detected failure publishes its AFK event and the turn's
	// own completion microseconds apart, in that order. Forcing idle on
	// the second would erase the cause the instant it was set — which is
	// why an agent whose provider died still showed as a healthy idle
	// seat, and why a reload showed the same.
	s := livestate.New()
	s.Apply(env("llm_unavailable", map[string]any{
		"role": "Lead", "kind": "provider_down", "detail": "429 forever",
	}, at("2026-06-14T12:00:00Z")))
	s.Apply(env("agent_turn_completed", map[string]any{"role": "Lead"},
		at("2026-06-14T12:00:01Z")))

	got := overlayOf(t, s, "Lead")
	if got.State != "afk" {
		t.Errorf("state = %q, want the seat to stay afk", got.State)
	}
	if got.AFKReason != "provider_down" {
		t.Errorf("afk reason = %q, want the cause kept", got.AFKReason)
	}
}

func TestRealWorkClearsTheAFKHold(t *testing.T) {
	t.Parallel()
	// A seat leaves AFK only when it does real work again.
	s := livestate.New()
	s.Apply(env("budget_exhausted", map[string]any{"role": "Lead", "kind": "budget"},
		at("2026-06-14T12:00:00Z")))
	s.Apply(env("agent_phase_started",
		map[string]any{"role": "Lead", "turn_id": "tn-2", "phase": "plan", "iteration": 0},
		at("2026-06-14T12:05:00Z")))

	got := overlayOf(t, s, "Lead")
	if got.State != "working" {
		t.Errorf("state = %q, want working", got.State)
	}
	if got.AFKReason != "" {
		t.Errorf("afk reason = %q, want cleared", got.AFKReason)
	}
	if got.LastError != nil {
		t.Errorf("last error = %+v, want cleared by forward progress", got.LastError)
	}
}

func TestARespawnClearsTheAFKHold(t *testing.T) {
	t.Parallel()
	// A spawn is a NEW instance of the seat, so whatever stopped the last
	// one is not this one's state. Without this the hold outlives an
	// engine restart and a healthy seat renders as broken until it happens
	// to do some work.
	s := livestate.New()
	s.Apply(env("llm_unavailable", map[string]any{"role": "Lead", "kind": "provider_down"},
		at("2026-06-14T12:00:00Z")))
	s.Apply(env("agent_spawned", map[string]any{"role": "Lead", "agent_id": "a-2"},
		at("2026-06-14T12:05:00Z")))

	got := overlayOf(t, s, "Lead")
	if got.State != "idle" {
		t.Errorf("state = %q, want idle", got.State)
	}
	if got.AFKReason != "" || got.LastError != nil {
		t.Errorf("overlay still carries the old run's failure: %+v", got)
	}
}

func TestASpawnDoesNotDisturbAWorkingSeat(t *testing.T) {
	t.Parallel()
	// The counterfactual to the respawn clear: only a stopped seat is
	// reset by a spawn. Resetting a working one would blank a live turn.
	s := livestate.New()
	s.Apply(env("agent_phase_started",
		map[string]any{"role": "Lead", "turn_id": "tn-1", "phase": "plan", "iteration": 0},
		at("2026-06-14T12:00:00Z")))
	s.Apply(env("agent_spawned", map[string]any{"role": "Lead", "agent_id": "a-2"},
		at("2026-06-14T12:05:00Z")))

	got := overlayOf(t, s, "Lead")
	if got.State != "working" {
		t.Errorf("state = %q, want the working seat left alone", got.State)
	}
	if got.LiveCall == nil {
		t.Error("a spawn wiped a working seat's live call")
	}
}

// --- phase failure ------------------------------------------------------- //

func failedPhase(ts string) *livestate.Envelope {
	return env("agent_phase_completed", map[string]any{
		"role": "Lead", "turn_id": "tn-1", "phase": "plan", "iteration": 0,
		"failed": true, "error_kind": "provider_error", "error": "429 from anthropic",
	}, at(ts))
}

func TestAPhaseFailureLandsOnTheSeat(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(failedPhase("2026-06-14T12:00:05Z"))

	got := overlayOf(t, s, "Lead").LastError
	if got == nil {
		t.Fatal("a failed phase recorded no error")
	}
	if got.Kind != "provider_error" || got.Message != "429 from anthropic" {
		t.Errorf("error = %+v", got)
	}
	if got.Phase != "plan" || got.TurnID != "tn-1" {
		t.Errorf("error = %+v, want the phase's coordinates", got)
	}
}

func TestTheFailedCallStaysOnScreen(t *testing.T) {
	t.Parallel()
	// A phase that dies mid-call is exactly when an operator most wants to
	// see the call. Clearing it would blank the row the moment the failure
	// lands.
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("agent_turn_progress",
		with(planCall(), map[string]any{"round_num": 1, "response": "half an answer"}),
		streamOnly, at("2026-06-14T12:00:03Z")))
	s.Apply(failedPhase("2026-06-14T12:00:05Z"))

	call := liveCallOf(t, s, "Lead")
	if call == nil {
		t.Fatal("the failed call was cleared")
	}
	if !call.Failed || call.InProgress {
		t.Errorf("call = %+v, want frozen and failed", call)
	}
	if call.Response != "half an answer" {
		t.Errorf("response = %q, want what the phase managed", call.Response)
	}
	if call.Error == nil || call.Error.Kind != "provider_error" {
		t.Errorf("call error = %+v", call.Error)
	}
}

func TestAFollowingAFKEventDoesNotWipeTheFailedCall(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(failedPhase("2026-06-14T12:00:05Z"))
	s.Apply(env("llm_unavailable", map[string]any{"role": "Lead", "kind": "provider_down"},
		at("2026-06-14T12:00:06Z")))

	if call := liveCallOf(t, s, "Lead"); call == nil || !call.Failed {
		t.Errorf("call = %+v: the AFK event wiped the frozen call", call)
	}
}

func TestAnAFKEventClearsAHealthyCall(t *testing.T) {
	t.Parallel()
	// The counterfactual: only a FAILED call is protected. A healthy
	// in-flight row on a seat that has gone AFK is a call that will never
	// answer.
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("llm_unavailable", map[string]any{"role": "Lead", "kind": "provider_down"},
		at("2026-06-14T12:00:06Z")))

	if call := liveCallOf(t, s, "Lead"); call != nil {
		t.Errorf("call = %+v, want cleared", call)
	}
}

func TestACleanPhaseClearsTheCallAsBefore(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", planCall()))
	s.Apply(env("agent_phase_completed", planCall(), at("2026-06-14T12:00:05Z")))

	got := overlayOf(t, s, "Lead")
	if got.LiveCall != nil {
		t.Errorf("call = %+v, want cleared", got.LiveCall)
	}
	if got.LastError != nil {
		t.Errorf("a clean phase recorded an error: %+v", got.LastError)
	}
}

func TestTheNextTurnClearsTheFailure(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_completed", map[string]any{
		"role": "Lead", "phase": "execute", "turn_id": "tn-8", "failed": true,
		"error": "boom",
	}, at("2026-06-14T12:00:05Z")))
	s.Apply(env("agent_phase_started", map[string]any{
		"role": "Lead", "phase": "plan", "turn_id": "tn-9",
	}, at("2026-06-14T12:01:00Z")))

	got := overlayOf(t, s, "Lead")
	if got.LastError != nil {
		t.Errorf("last error = %+v, want cleared by the next task", got.LastError)
	}
}

// --- a redelivered trigger runs again ----------------------------------- //

// A RETRY IS A NEW CALL, not a continuation of the attempt it repeats.
//
// A turn that fails without reaching outside the engine is NAK'd and
// redelivered, so the same trigger runs again — and a turn id used to be the
// work key, which a redelivery reproduces. `agent_phase_started` for the retry
// therefore named the identity the failed attempt already held, `sameCall`
// matched, and the projection kept the FROZEN FAILED row: the seat read
// `working` while the retry ran and the live row showed the dead attempt's
// error for the whole of it. See ADR-0017.
func TestARetryOpensItsOwnCallRatherThanReusingTheFailedOnes(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	// Attempt one: the executor died on an auth failure and its call froze.
	s.Apply(env("agent_phase_started", map[string]any{
		"role": "CEO", "turn_id": "run-1", "work_key": "wk-1",
		"phase": "execute", "iteration": 1,
	}, at("2026-06-14T12:00:00Z")))
	s.Apply(env("agent_phase_completed", map[string]any{
		"role": "CEO", "turn_id": "run-1", "work_key": "wk-1",
		"phase": "execute", "iteration": 1,
		"failed": true, "error_kind": "auth", "error": "not authenticated",
	}, at("2026-06-14T12:00:01Z"), id("e2")))

	frozen := overlayOf(t, s, "CEO")
	if frozen.LiveCall == nil || !frozen.LiveCall.Failed {
		t.Fatalf("the failed attempt did not freeze its call: %+v", frozen.LiveCall)
	}

	// Attempt two: the broker redelivered the trigger, so the SAME work key
	// runs again — under a run id of its own.
	s.Apply(env("agent_phase_started", map[string]any{
		"role": "CEO", "turn_id": "run-2", "work_key": "wk-1",
		"phase": "execute", "iteration": 1,
	}, at("2026-06-14T12:02:00Z"), id("e3")))

	got := overlayOf(t, s, "CEO")
	if got.LiveCall == nil {
		t.Fatal("the retry opened no live call at all")
	}
	if got.LiveCall.TurnID != "run-2" {
		t.Errorf("the live call is %q, want the retry's own run — the reader watches "+
			"the dead attempt while the real one runs", got.LiveCall.TurnID)
	}
	if got.LiveCall.Failed {
		t.Error("the retry's call inherited the previous attempt's failure")
	}
	if got.LiveCall.WorkKey != "wk-1" {
		t.Errorf("work key = %q, want the trigger both attempts share", got.LiveCall.WorkKey)
	}
}

// AND THE KEY SURVIVES THE ROUNDS. A progress round rebuilds the live call
// WHOLESALE, so a field the rebuild forgets is blank for the rest of the call
// — and blank on every row a reader sees, because a phase publishes many
// rounds and the opening frame is one of them.
func TestTheWorkKeyOutlivesTheRoundsThatRebuildTheCall(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{
		"role": "CEO", "turn_id": "run-1", "work_key": "wk-1",
		"phase": "execute", "iteration": 1,
	}, at("2026-06-14T12:00:00Z")))
	// A round from a node that predates the field carries no work key. It
	// must not blank what the opening frame established.
	s.Apply(env("agent_turn_progress", map[string]any{
		"role": "CEO", "turn_id": "run-1",
		"phase": "execute", "iteration": 1, "round_num": 0,
	}, at("2026-06-14T12:00:02Z"), id("e2"), streamOnly))

	got := overlayOf(t, s, "CEO")
	if got.LiveCall == nil || got.LiveCall.WorkKey != "wk-1" {
		t.Errorf("work key = %+v, want it carried across the round", got.LiveCall)
	}
}
