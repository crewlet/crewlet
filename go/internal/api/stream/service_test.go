package stream_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/stream"
)

func newService(t *testing.T, opts stream.Options) (*stream.Service, *stream.Client) {
	t.Helper()
	if opts.Now == nil {
		opts.Now = func() time.Time { return clock }
	}
	s := stream.NewService(livestate.New(), opts)
	t.Cleanup(s.Stop)
	c := stream.NewClient()
	s.Hub().Register(c)
	return s, c
}

func envelope(etype string, payload map[string]any) livestate.Envelope {
	return livestate.Envelope{
		ID: "e1", Type: etype, Timestamp: "2026-06-14T12:00:00Z",
		Category: "system", Payload: payload,
	}
}

// kindsOf lists the push kinds a client received, in order.
func kindsOf(c *stream.Client) []string {
	var out []string
	for _, env := range drain(c) {
		out = append(out, env.Kind)
	}
	return out
}

func TestIngestPushesTheResultOfApplyingAnEvent(t *testing.T) {
	t.Parallel()
	// A dashboard renders what arrives here; it does not re-derive it from
	// the raw event stream. Every tab used to keep its own copy of the
	// projection, and each drifted its own way.
	s, c := newService(t, stream.Options{})
	s.Ingest(envelope("task_started", map[string]any{"role": "Lead", "task_id": "t-1"}))

	got := drain(c)
	var agents *stream.Envelope
	for i := range got {
		if got[i].Kind == stream.KindAgents {
			agents = &got[i]
		}
	}
	if agents == nil {
		t.Fatalf("no agents push: %v", kindsOf(c))
	}
	// A LIST of rows, each carrying its own role. This test asserted a
	// map keyed by role and passed for it — while store.js guards
	// applyAgents with Array.isArray and DISCARDED every push, so a full
	// turn ran with the seat rendered idle from start to finish. The
	// client is the compatibility reference (rewrite/decisions/502), so
	// the assertion is written the way the client reads the frame.
	rows, ok := agents.Data.([]map[string]any)
	if !ok {
		t.Fatalf("agents data is %T, not the row list the client requires; "+
			"anything else fails its Array.isArray guard and is dropped",
			agents.Data)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want the one seat that moved", len(rows))
	}
	if got := rows[0]["role"]; got != "Lead" {
		t.Errorf("row role = %v; the client keys on this field and drops a "+
			"row without it", got)
	}
	if got := rows[0]["state"]; got != "working" {
		t.Errorf("row state = %v, want working", got)
	}
}

func TestTheEventArrivesBeforeItsConsequences(t *testing.T) {
	t.Parallel()
	// A derived push arriving before the event that caused it would
	// briefly show a consequence with no cause in the feed beside it.
	s, c := newService(t, stream.Options{})
	s.Ingest(envelope("task_started", map[string]any{"role": "Lead", "task_id": "t-1"}))

	kinds := kindsOf(c)
	if len(kinds) < 2 {
		t.Fatalf("kinds = %v, want the event and its consequence", kinds)
	}
	if kinds[0] != stream.KindEvent {
		t.Errorf("kinds = %v, want the event first", kinds)
	}
}

func TestAStreamOnlyEventPushesNoFeedRow(t *testing.T) {
	t.Parallel()
	// A progress round updates the live call and must never enter the
	// activity feed — the event store drops it, so a feed carrying it
	// would be one no reload could reproduce.
	s, c := newService(t, stream.Options{})
	s.Ingest(livestate.Envelope{
		ID: "e1", Type: "agent_turn_progress", Timestamp: "2026-06-14T12:00:00Z",
		Payload: map[string]any{
			"role": "Lead", "turn_id": "tn-1", "phase": "plan", "round_num": 0,
		},
	})
	kinds := kindsOf(c)
	for _, kind := range kinds {
		if kind == stream.KindEvent {
			t.Errorf("kinds = %v: a progress round was pushed as a feed row", kinds)
		}
	}
	if len(kinds) == 0 {
		t.Error("a progress round pushed nothing at all")
	}
}

func TestAnEventThatMovesNothingPushesNothing(t *testing.T) {
	t.Parallel()
	// Anything not named by the change did not move and is not re-sent.
	s, c := newService(t, stream.Options{})
	s.Ingest(livestate.Envelope{
		ID: "e1", Type: "agent_turn_progress", Timestamp: "2026-06-14T12:00:00Z",
		// No role: the round is dropped.
		Payload: map[string]any{"turn_id": "tn-1"},
	})
	if kinds := kindsOf(c); len(kinds) != 0 {
		t.Errorf("kinds = %v, want nothing", kinds)
	}
}

func TestSandboxAndBudgetPushTheirOwnKinds(t *testing.T) {
	t.Parallel()
	s, c := newService(t, stream.Options{})

	s.Ingest(envelope("sandbox_run_started", map[string]any{
		"turn_id": "tn-1", "role": "Coder",
	}))
	if kinds := kindsOf(c); !contains(kinds, stream.KindSandboxes) {
		t.Errorf("kinds = %v, want a sandboxes push", kinds)
	}

	s.Ingest(livestate.Envelope{
		ID: "e2", Type: "budget_reported", Timestamp: "2026-06-14T12:00:01Z",
		Payload: map[string]any{"meter_id": "m-1", "seq": 1, "org_used_tokens": 5},
	})
	if kinds := kindsOf(c); !contains(kinds, stream.KindBudget) {
		t.Errorf("kinds = %v, want a budget push", kinds)
	}

	s.Ingest(envelope("agent_phase_completed", map[string]any{
		"role": "Lead", "phase": "plan", "total_tokens": 10,
	}))
	if kinds := kindsOf(c); !contains(kinds, stream.KindTokens) {
		t.Errorf("kinds = %v, want a tokens push", kinds)
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// --- the snapshot -------------------------------------------------------- //

func TestASnapshotIsBuiltFromMemoryAlone(t *testing.T) {
	t.Parallel()
	// The whole reason the projection exists: a dashboard that rebuilt
	// agent history from the store on every reconnect would take a
	// thirty-day scan per tab, and would lose any call mid-flight while it
	// did.
	s, _ := newService(t, stream.Options{})
	s.Ingest(envelope("task_started", map[string]any{"role": "Lead", "task_id": "t-1"}))

	snap := s.Snapshot()
	for _, key := range []string{"health", "agents", "events", "sandboxes", "tokens", "budget"} {
		if _, present := snap[key]; !present {
			t.Errorf("snapshot is missing %q", key)
		}
	}
	events, _ := snap["events"].([]livestate.FeedRow)
	if len(events) != 1 {
		t.Errorf("snapshot events = %v", events)
	}
}

// --- the shared health tick ---------------------------------------------- //

func TestTheHealthTickIsSharedAndKeepsTicking(t *testing.T) {
	t.Parallel()
	// ONE timer for the whole service. What it keeps honest is the same
	// answer for every tab, so a timer per client would multiply identical
	// work by however many people happened to be watching.
	inFlight := 3
	s := stream.NewService(livestate.New(), stream.Options{
		Now:    func() time.Time { return clock },
		Health: func() stream.Health { return stream.Health{Status: "ok", InFlight: &inFlight} },
	})
	t.Cleanup(s.Stop)

	a, b := stream.NewClient(), stream.NewClient()
	s.Hub().Register(a)
	s.Hub().Register(b)
	s.StartHealthTicks(t.Context())
	// A second call must not start a second timer.
	s.StartHealthTicks(t.Context())

	for name, c := range map[string]*stream.Client{"a": a, "b": b} {
		select {
		case env := <-c.Out():
			if env.Kind != stream.KindHealth {
				t.Errorf("%s received %q, want a health tick", name, env.Kind)
			}
			health, ok := env.Data.(stream.Health)
			if !ok || health.InFlight == nil || *health.InFlight != 3 {
				t.Errorf("%s health = %#v", name, env.Data)
			}
		case <-time.After(3 * stream.HealthInterval):
			t.Fatalf("%s never received a health tick", name)
		}
	}
}

func TestAnAbsentEngineReportsNoInFlightRatherThanZero(t *testing.T) {
	t.Parallel()
	// ABSENT and ZERO are different answers. A dashboard that drew "0 in
	// flight" for "cannot see the engine" would report an idle company
	// during an outage.
	s, _ := newService(t, stream.Options{})
	health, _ := s.Snapshot()["health"].(stream.Health)
	if health.Status != "ok" {
		t.Errorf("status = %q", health.Status)
	}
	if health.InFlight != nil {
		t.Errorf("in flight = %d, want absent with no engine to ask", *health.InFlight)
	}
}

func TestStartingTheTickTwiceStartsOneTimer(t *testing.T) {
	t.Parallel()
	// The merged topology can reach this from either half, so a second
	// call is ordinary — and a second TIMER would double every tab's
	// health traffic and leave one of them running past Stop, since only
	// the later one is the one Stop knows about.
	const interval = 20 * time.Millisecond
	s := stream.NewService(livestate.New(), stream.Options{
		Now:            func() time.Time { return clock },
		HealthInterval: interval,
	})
	t.Cleanup(s.Stop)
	c := stream.NewClient()
	s.Hub().Register(c)

	s.StartHealthTicks(t.Context())
	s.StartHealthTicks(t.Context())
	s.StartHealthTicks(t.Context())

	const windows = 10
	time.Sleep(windows * interval)
	got := len(drain(c))

	// Generous on both sides: what is being told apart is one timer from
	// three, not one tick from two.
	if got > 2*windows {
		t.Errorf("health ticks = %d over %d intervals, want about %d: "+
			"a repeated start began a second timer", got, windows, windows)
	}
	if got == 0 {
		t.Error("no health ticks at all, so this measured nothing")
	}
}

func TestStoppingEndsTheTickAndTheClients(t *testing.T) {
	t.Parallel()
	s := stream.NewService(livestate.New(), stream.Options{
		Now: func() time.Time { return clock },
	})
	c := stream.NewClient()
	s.Hub().Register(c)
	s.StartHealthTicks(t.Context())
	s.Stop()

	// Whatever ticked before the stop drains, and then the channel is
	// closed — which is what releases a transport's writer goroutine.
	closed := false
	for range c.Out() {
	}
	select {
	case _, open := <-c.Out():
		closed = !open
	default:
	}
	if !closed {
		t.Error("Stop left a client's channel open")
	}
	if s.Hub().Clients() != 0 {
		t.Error("Stop left clients registered")
	}
	// Idempotent: the merged topology can reach this from either half.
	s.Stop()
}

func TestACancelledContextEndsTheTick(t *testing.T) {
	t.Parallel()
	s := stream.NewService(livestate.New(), stream.Options{
		Now: func() time.Time { return clock },
	})
	t.Cleanup(s.Stop)
	ctx, cancel := context.WithCancel(context.Background())
	s.StartHealthTicks(ctx)
	cancel()
	// Stop must still return rather than waiting on a goroutine that has
	// already left through the other door.
	done := make(chan struct{})
	go func() { defer close(done); s.Stop() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop hung after the tick's context was cancelled")
	}
}
