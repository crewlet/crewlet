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
	if got.Activity != livestate.ActivityIdle {
		t.Errorf("activity = %q, want idle", got.Activity)
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

// A TURN RUNS AND FINISHES, which is what a seat's live state is about.
//
// It reads the PHASE events, not a task lifecycle: the five task_* types this
// projection used to branch on had no publisher at all — they described an
// engine-owned task object the native tracker replaced — so `working` and
// `idle` have always come from here.
func TestATurnRunsAndFinishes(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{
		"role": "Lead", "phase": "execute", "turn_id": "t-1",
	}))

	got := overlayOf(t, s, "Lead")
	if got.Activity != livestate.ActivityWorking {
		t.Errorf("activity = %q, want working", got.Activity)
	}

	s.Apply(env("agent_turn_completed", map[string]any{
		"role": "Lead", "turn_id": "t-1",
	}, at("2026-06-14T12:01:00+00:00")))
	got = overlayOf(t, s, "Lead")
	if got.Activity != livestate.ActivityIdle {
		t.Errorf("activity = %q, want idle", got.Activity)
	}
}

// A BREACHED GUARD IS A FAILED TURN, NOT A STOP: the seat takes its next wake
// like any other, so it is idle, and why its last turn failed is `last_error`,
// in the event's own kind.
func TestAGuardBreachIsAFailedTurnNotAStop(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("turn.guard_breach", map[string]any{"role": "Lead", "kind": "delegation_loop"}))

	got := overlayOf(t, s, "Lead")
	if got.Activity != livestate.ActivityIdle || got.StoppedReason != nil {
		t.Errorf("activity = %q (%v), want idle: a failed turn does not stop a seat",
			got.Activity, got.StoppedReason)
	}
	if got.LastError == nil || got.LastError.Kind != "delegation_loop" {
		t.Errorf("last error = %+v, want the payload's own kind", got.LastError)
	}
}

// AN UNREACHABLE PROVIDER STOPS THE SEAT, and says so in the reason — the one
// engine-detected failure that is a stop, because every wake the seat takes
// until the provider answers fails the same way.
func TestAnUnreachableProviderStopsTheSeat(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(env("llm_unavailable", map[string]any{"role": "Lead"}))
	got := overlayOf(t, s, "Lead")
	if got.Activity != livestate.ActivityStopped || got.StoppedReason == nil ||
		*got.StoppedReason != livestate.StoppedProvider {
		t.Errorf("activity = %q (%v), want stopped/provider", got.Activity, got.StoppedReason)
	}
	if got.LastError == nil || got.LastError.Kind != "llm_unavailable" {
		t.Errorf("last error = %+v, want the event type as its kind", got.LastError)
	}
}

// Reflection is the trailing pass after a turn: it clears the phase the turn
// was in and the call on screen. It does NOT end the turn — a parked turn is
// reflected on too, and its run is still in flight — so what the seat is doing
// stays the turn's to say.
func TestReflectionClearsThePhaseItFollows(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	base := map[string]any{"role": "Lead", "turn_id": "tn-1", "phase": "plan", "iteration": 0}
	s.Apply(env("agent_phase_started", base))
	s.Apply(env("reflection_completed", map[string]any{"role": "Lead"},
		at("2026-06-14T12:01:00+00:00")))

	got := overlayOf(t, s, "Lead")
	if got.CurrentPhase != nil {
		t.Errorf("current phase = %v, want null", *got.CurrentPhase)
	}
	if got.LiveCall != nil {
		t.Error("a reflected turn left a live call on screen")
	}
}

// A TERMINATED INSTANCE IS NOT A STATE OF ITS OWN. The instance on one node
// ended, which takes its call off the screen; whether the SEAT runs anywhere is
// the lease table's to say, so a seat a peer took over reads idle, and one no
// node took over reads stopped/unplaced.
func TestTerminationLeavesTheSeatToItsPlacement(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	base := map[string]any{"role": "Lead", "turn_id": "tn-1", "phase": "execute"}
	s.Apply(env("agent_phase_started", base))
	s.Apply(env("agent_terminated", map[string]any{"role": "Lead"},
		at("2026-06-14T12:00:01+00:00")))
	got := overlayOf(t, s, "Lead")
	if got.LiveCall != nil {
		t.Error("a terminated instance left its call on screen")
	}

	s.SetPlacement(map[string]bool{"Lead": true})
	if got := overlayOf(t, s, "Lead"); got.Activity != livestate.ActivityIdle {
		t.Errorf("activity = %q after a peer holds the seat, want idle", got.Activity)
	}
	s.SetPlacement(map[string]bool{"Lead": false})
	if got := overlayOf(t, s, "Lead"); got.Activity != livestate.ActivityStopped ||
		got.StoppedReason == nil || *got.StoppedReason != livestate.StoppedUnplaced {
		t.Errorf("activity = %q (%v) with no node holding it, want stopped/unplaced",
			got.Activity, got.StoppedReason)
	}
}

func TestAnOlderStateEventCannotClobberNewerState(t *testing.T) {
	t.Parallel()
	// The events arrive over a broker that guarantees order only within a
	// topic, and different event types are different topics — so a
	// state-affecting event can arrive out of order relative to another.
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{"role": "Lead", "phase": "execute"},
		at("2026-06-14T12:05:00+00:00")))
	s.Apply(env("agent_turn_completed", map[string]any{"role": "Lead"},
		at("2026-06-14T12:01:00+00:00")))

	if got := overlayOf(t, s, "Lead").CurrentPhase; got == nil {
		t.Error("an older event clobbered newer state: the phase was cleared")
	}
}

func TestSameInstantEventsAreBothApplied(t *testing.T) {
	t.Parallel()
	// The counterfactual to the reorder guard. Same-instant bursts are
	// ordinary, and refusing them would drop the second half of every one:
	// equal timestamps pass, and the later-applied wins.
	s := livestate.New()
	ts := at("2026-06-14T12:05:00+00:00")
	s.Apply(env("agent_phase_started", map[string]any{"role": "Lead", "phase": "execute"}, ts))
	s.Apply(env("agent_turn_completed", map[string]any{"role": "Lead"}, ts))

	if got := overlayOf(t, s, "Lead").CurrentPhase; got != nil {
		t.Errorf("current phase = %q: a same-instant event was refused as stale", *got)
	}
}

func TestTheReorderGuardComparesInstantsNotStrings(t *testing.T) {
	t.Parallel()
	// "Z" sorts AFTER "+" as a raw string, so an older event spelled with
	// a Z reads as newer than a "+00:00" one and walks the state
	// backwards.
	s := livestate.New()
	s.Apply(env("agent_phase_started", map[string]any{"role": "Lead", "phase": "execute"},
		at("2026-06-14T12:05:00+00:00")))
	s.Apply(env("agent_turn_completed", map[string]any{"role": "Lead"},
		at("2026-06-14T12:01:00Z")))

	if got := overlayOf(t, s, "Lead").CurrentPhase; got == nil {
		t.Error("an older event won on its encoding: the phase was cleared")
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
	change := s.Apply(env("agent_phase_started", map[string]any{"phase": "execute"}))
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
	if got.Activity != livestate.ActivityIdle {
		t.Errorf("activity = %q, want idle once the turn is over", got.Activity)
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
			if got.Activity != livestate.ActivityWorking {
				t.Errorf("activity = %q, want working — the seat is mid-turn", got.Activity)
			}
		})
	}
}
