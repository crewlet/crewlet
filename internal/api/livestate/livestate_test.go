package livestate_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

const defaultTS = "2026-06-14T12:00:00+00:00"

// env builds a serialized envelope, the shape Apply reads.
// env builds one frame. A POINTER, because Apply stamps the envelope it is
// given — the derived `failed` mark has to reach the frame the client is
// handed, and a value copy would be stamped and discarded.
func env(etype string, payload map[string]any, opts ...func(*livestate.Envelope)) *livestate.Envelope {
	if payload == nil {
		payload = map[string]any{}
	}
	e := &livestate.Envelope{
		ID: "e1", Type: etype, Timestamp: defaultTS,
		Category: "system", Payload: payload,
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

func at(ts string) func(*livestate.Envelope) {
	return func(e *livestate.Envelope) { e.Timestamp = ts }
}
func id(v string) func(*livestate.Envelope) {
	return func(e *livestate.Envelope) { e.ID = v }
}

// streamOnly marks an envelope the event store never persists, which is what
// keeps progress rounds and meter reports out of the activity feed.
func streamOnly(e *livestate.Envelope) { e.Category = "" }

// with copies a payload and overrides some keys, for the many tests that vary
// one field of a shared base.
func with(base map[string]any, over map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range over {
		out[k] = v
	}
	return out
}

func overlayOf(t *testing.T, s *livestate.LiveState, role string) livestate.Overlay {
	t.Helper()
	o := s.AgentOverlay(role)
	if o == nil {
		t.Fatalf("no live entry for %q", role)
	}
	return *o
}

// --- state transitions -------------------------------------------------- //

func TestASpawnMarksIdleAndRecordsTheRuntimeID(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_spawned", map[string]any{"role": "Lead", "agent_id": "a-1"}))

	got := overlayOf(t, s, "Lead")
	if got.State != "idle" {
		t.Errorf("state = %q, want idle", got.State)
	}
	if got.RuntimeID != "a-1" {
		t.Errorf("runtime id = %q, want a-1", got.RuntimeID)
	}
	if s.RuntimeIDFor("Lead") != "a-1" {
		t.Error("RuntimeIDFor disagrees with the overlay")
	}
	if s.RuntimeIDFor("Nobody") != "" {
		t.Error("a role with no live entry reported a runtime id")
	}
}

func TestATaskRunsAndFinishes(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("task_started", map[string]any{"role": "Lead", "task_id": "t-1"}))

	got := overlayOf(t, s, "Lead")
	if got.State != "working" {
		t.Errorf("state = %q, want working", got.State)
	}
	if got.CurrentTask == nil || *got.CurrentTask != "t-1" {
		t.Errorf("current task = %v, want t-1", got.CurrentTask)
	}

	s.Apply(env("task_completed", map[string]any{"role": "Lead"}, at("2026-06-14T12:01:00+00:00")))
	got = overlayOf(t, s, "Lead")
	if got.State != "idle" {
		t.Errorf("state = %q, want idle", got.State)
	}
	// Null, not "": the dashboard reads an absent task as no task, and an
	// empty string as a task with no name.
	if got.CurrentTask != nil {
		t.Errorf("current task = %v, want null", *got.CurrentTask)
	}
}

func TestAnAFKReasonComesFromTheEventsOwnKind(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("turn.guard_breach", map[string]any{"role": "Lead", "kind": "delegation_loop"}))

	got := overlayOf(t, s, "Lead")
	if got.State != "afk" {
		t.Errorf("state = %q, want afk", got.State)
	}
	if got.AFKReason != "delegation_loop" {
		t.Errorf("afk reason = %q, want the payload's own kind", got.AFKReason)
	}
}

func TestAnAFKReasonFallsBackToTheEventType(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("llm_unavailable", map[string]any{"role": "Lead"}))
	if got := overlayOf(t, s, "Lead").AFKReason; got != "llm_unavailable" {
		t.Errorf("afk reason = %q, want the event type", got)
	}
}

func TestReflectionReturnsTheSeatToIdle(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	base := map[string]any{"role": "Lead", "turn_id": "tn-1", "phase": "plan", "iteration": 0}
	s.Apply(env("agent_phase_started", base))
	s.Apply(env("reflection_completed", map[string]any{"role": "Lead"},
		at("2026-06-14T12:01:00+00:00")))

	got := overlayOf(t, s, "Lead")
	if got.State != "idle" {
		t.Errorf("state = %q, want idle", got.State)
	}
	if got.CurrentPhase != nil {
		t.Errorf("current phase = %v, want null", *got.CurrentPhase)
	}
	if got.LiveCall != nil {
		t.Error("a reflected turn left a live call on screen")
	}
}

func TestTerminationIsRecorded(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_terminated", map[string]any{"role": "Lead"}))
	if got := overlayOf(t, s, "Lead").State; got != "terminated" {
		t.Errorf("state = %q, want terminated", got)
	}
}

func TestAnOlderStateEventCannotClobberNewerState(t *testing.T) {
	t.Parallel()
	// The events arrive over a broker that guarantees order only within a
	// topic, and different event types are different topics — so a
	// state-affecting event can arrive out of order relative to another.
	s := livestate.New()
	s.Apply(env("task_started", map[string]any{"role": "Lead", "task_id": "t-2"},
		at("2026-06-14T12:05:00+00:00")))
	s.Apply(env("task_completed", map[string]any{"role": "Lead"},
		at("2026-06-14T12:01:00+00:00")))

	got := overlayOf(t, s, "Lead")
	if got.State != "working" {
		t.Errorf("state = %q: an older event clobbered newer state", got.State)
	}
	if got.CurrentTask == nil || *got.CurrentTask != "t-2" {
		t.Errorf("current task = %v, want the newer t-2", got.CurrentTask)
	}
}

func TestSameInstantEventsAreBothApplied(t *testing.T) {
	t.Parallel()
	// The counterfactual to the reorder guard. Same-instant bursts are
	// ordinary, and refusing them would drop the second half of every one:
	// equal timestamps pass, and the later-applied wins.
	s := livestate.New()
	ts := at("2026-06-14T12:05:00+00:00")
	s.Apply(env("task_started", map[string]any{"role": "Lead", "task_id": "t-1"}, ts))
	s.Apply(env("task_completed", map[string]any{"role": "Lead"}, ts))

	if got := overlayOf(t, s, "Lead").State; got != "idle" {
		t.Errorf("state = %q: a same-instant event was refused as stale", got)
	}
}

func TestTheReorderGuardComparesInstantsNotStrings(t *testing.T) {
	t.Parallel()
	// "Z" sorts AFTER "+" as a raw string, so an older event spelled with
	// a Z reads as newer than a "+00:00" one and walks the state
	// backwards.
	s := livestate.New()
	s.Apply(env("task_started", map[string]any{"role": "Lead", "task_id": "t-2"},
		at("2026-06-14T12:05:00+00:00")))
	s.Apply(env("task_completed", map[string]any{"role": "Lead"},
		at("2026-06-14T12:01:00Z")))

	if got := overlayOf(t, s, "Lead").State; got != "working" {
		t.Errorf("state = %q: an older event won on its encoding", got)
	}
}

func TestAnUnknownEventTypeMovesNothing(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	change := s.Apply(env("something_else", map[string]any{"role": "Lead"}))
	// It still lands in the feed — it carries a category — but the seat
	// state machine does not know it.
	if _, moved := change.Agents["Lead"]; moved {
		t.Error("an event outside the state map moved the seat")
	}
	if !change.Events {
		t.Error("a categorized event was not recorded in the feed")
	}
}

func TestAnEventWithNoRoleIsHarmless(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	change := s.Apply(env("task_started", map[string]any{"task_id": "t-1"}))
	if len(change.Agents) != 0 {
		t.Errorf("a role-less event moved %v", change.Agents)
	}
	if s.AgentOverlay("") != nil {
		t.Error("a role-less event created an empty-named seat")
	}
}

func TestATurnCompletingReturnsTheSeatToIdle(t *testing.T) {
	t.Parallel()
	// WITHOUT A REFLECTION BEHIND IT. reflection_completed was the only entry
	// in the state map that anything publishes AND that returns a seat to
	// idle, and the reflector returns without publishing it on five paths —
	// among them "no workers configured", which is every turn of a company
	// running with learning off. Those seats went to working on their first
	// turn and stayed there for the life of the process, rendering mid-phase
	// in a phase that had ended.
	s := livestate.New()
	base := map[string]any{"role": "Lead", "turn_id": "tn-1", "phase": "review", "iteration": 1}
	s.Apply(env("agent_phase_started", base))
	s.Apply(env("agent_phase_completed", base, at("2026-06-14T12:00:05+00:00")))
	s.Apply(env("agent_turn_completed",
		map[string]any{"role": "Lead", "turn_id": "tn-1", "total_tokens": 10},
		at("2026-06-14T12:00:06+00:00")))

	got := overlayOf(t, s, "Lead")
	if got.State != "idle" {
		t.Errorf("state = %q, want idle once the turn is over", got.State)
	}
	if got.CurrentPhase != nil {
		t.Errorf("current phase = %v, want null", *got.CurrentPhase)
	}
	if got.LiveCall != nil {
		t.Error("a completed turn left a live call on screen")
	}
}

func TestTheEndOfONETurnDoesNotClearTheNEXTOne(t *testing.T) {
	t.Parallel()
	// Both events that end a turn arrive asynchronously and neither is
	// ordered against the next turn's work: reflection runs on its own
	// consumer seconds later, and the projection reads every type through one
	// wildcard subscription where cross-topic order is not guaranteed. The
	// clear used to be unconditional, so a seat several rounds into its next
	// turn had the row a reader was watching wiped and its state reported as
	// idle.
	//
	// The timestamp guard does not cover it: applyProgress deliberately never
	// advances stateTS, so a seat whose only events since the phase boundary
	// are progress rounds still carries the older stamp.
	for _, ending := range []string{"agent_turn_completed", "reflection_completed"} {
		t.Run(ending, func(t *testing.T) {
			t.Parallel()
			s := livestate.New()
			next := map[string]any{
				"role": "Lead", "turn_id": "tn-2", "phase": "execute", "iteration": 1,
			}
			s.Apply(env("agent_phase_started", next, at("2026-06-14T12:00:10+00:00")))
			s.Apply(env("agent_turn_progress",
				with(next, map[string]any{"round_num": 2, "response": "working"}),
				streamOnly, at("2026-06-14T12:00:11+00:00")))

			// The previous turn's ending, arriving late.
			s.Apply(env(ending, map[string]any{"role": "Lead", "turn_id": "tn-1"},
				at("2026-06-14T12:00:12+00:00")))

			got := overlayOf(t, s, "Lead")
			if got.LiveCall == nil {
				t.Fatal("the next turn's live call was wiped by the previous turn's ending")
			}
			if got.LiveCall.TurnID != "tn-2" {
				t.Errorf("live call turn = %q, want tn-2", got.LiveCall.TurnID)
			}
			if got.State != "working" {
				t.Errorf("state = %q, want working — the seat is mid-turn", got.State)
			}
		})
	}
}
