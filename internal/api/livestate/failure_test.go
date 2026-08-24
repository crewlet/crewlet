package livestate_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// --- the AFK hold -------------------------------------------------------- //

func TestATaskFailedDoesNotClearTheFailureThatCausedIt(t *testing.T) {
	t.Parallel()
	// An engine-detected failure publishes its AFK event and TaskFailed
	// microseconds apart, in that order. Forcing idle on the second would
	// erase the cause the instant it was set — which is why an agent whose
	// provider died still showed as a healthy idle seat, and why a reload
	// showed the same.
	s := livestate.New()
	s.Apply(env("llm_unavailable", map[string]any{
		"role": "Lead", "kind": "provider_down", "detail": "429 forever",
	}, at("2026-06-14T12:00:00Z")))
	s.Apply(env("task_failed", map[string]any{"role": "Lead", "error": "gave up"},
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

func failedPhase(ts string) livestate.Envelope {
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

func TestAFailedTaskRecordsWhy(t *testing.T) {
	t.Parallel()
	// TaskFailed carries the error and nothing else recorded it, so a task
	// that died for a reason the engine does not treat as AFK — an
	// unhandled handler exception, a rejected delegation — left the seat
	// looking like a healthy idle one, with the cause visible only as one
	// line in the feed.
	s := livestate.New()
	s.Apply(env("task_failed", map[string]any{
		"role": "Lead", "error": "delegation refused", "turn_id": "tn-3",
	}, at("2026-06-14T12:00:05Z")))

	got := overlayOf(t, s, "Lead")
	if got.State != "idle" {
		t.Errorf("state = %q, want idle", got.State)
	}
	if got.LastError == nil {
		t.Fatal("a failed task recorded no error")
	}
	if got.LastError.Kind != "task_failed" || got.LastError.Message != "delegation refused" {
		t.Errorf("error = %+v", got.LastError)
	}
	if got.LastError.TurnID != "tn-3" {
		t.Errorf("error turn id = %q", got.LastError.TurnID)
	}
}

func TestTheNextTaskClearsTheFailure(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("task_failed", map[string]any{"role": "Lead", "error": "boom"},
		at("2026-06-14T12:00:05Z")))
	s.Apply(env("task_started", map[string]any{"role": "Lead", "task_id": "t-9"},
		at("2026-06-14T12:01:00Z")))

	got := overlayOf(t, s, "Lead")
	if got.LastError != nil {
		t.Errorf("last error = %+v, want cleared by the next task", got.LastError)
	}
}
