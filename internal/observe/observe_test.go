package observe_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store"
)

// sink records what was appended, and can be made to fail.
type sink struct {
	mu   sync.Mutex
	rows []store.EventRecord
	err  error
}

func (s *sink) Append(_ context.Context, rec store.EventRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.rows = append(s.rows, rec)
	return nil
}

func (s *sink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// ingester records what the projection was pushed.
type ingester struct {
	mu   sync.Mutex
	envs []livestate.Envelope
}

func (i *ingester) Ingest(env livestate.Envelope) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.envs = append(i.envs, env)
}

func (i *ingester) types() []string {
	i.mu.Lock()
	defer i.mu.Unlock()
	out := make([]string, 0, len(i.envs))
	for _, e := range i.envs {
		out = append(out, e.Type)
	}
	return out
}

func phaseEvent(role string) *events.Event {
	ev := events.New(types.AgentPhaseStarted{
		RoleName: role, TurnID: "t1", Phase: types.PhaseExecute,
	}, events.TraceContext{})
	ev.Source = role
	return ev
}

func TestAPublishedEventIsPersistedByThePublisher(t *testing.T) {
	t.Parallel()
	// The whole reason the writer is a publish LISTENER: the node that
	// published certainly has the event, so there is no round trip and no
	// group — and therefore no way for two nodes to write the same row.
	s := &sink{}
	q := memory.New()
	q.AddPublishListener(observe.NewWriter(s).Listen())
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	ev := phaseEvent("CEO")
	if err := q.Publish(t.Context(), topics.Event(ev.Type), ev); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if s.count() != 1 {
		t.Fatalf("rows = %d, want the one published event", s.count())
	}
	if got := s.rows[0].Type; got != "agent_phase_started" {
		t.Errorf("type = %q", got)
	}
}

func TestAFailedWriteDoesNotFailThePublish(t *testing.T) {
	t.Parallel()
	// An observability write that took a turn down with it would trade the
	// thing being observed for the observation. The listener signature
	// already enforces this by returning nothing; this is the assertion
	// that the queue honours it rather than, say, logging and aborting.
	q := memory.New()
	q.AddPublishListener(observe.NewWriter(&sink{err: errors.New("disk gone")}).Listen())
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	if err := q.Publish(t.Context(), topics.Event("agent_phase_started"),
		phaseEvent("CEO")); err != nil {
		t.Fatalf("a broken event store failed the publish: %v", err)
	}
}

func TestANodeWithNoStoreRegistersNoListener(t *testing.T) {
	t.Parallel()
	// A node with no store is a supported deployment. NewWriter returns
	// nil so the caller registers what it gets back without a branch —
	// which only works if a nil writer yields a nil listener rather than a
	// method value that panics on the first publish.
	if l := observe.NewWriter(nil).Listen(); l != nil {
		t.Error("a storeless node got a publish listener")
	}
}

func TestTheProjectionSeesEventsFromAnyNode(t *testing.T) {
	t.Parallel()
	// The reason the projector is a broadcast subscription and NOT a
	// publish listener: a dashboard served HERE must show turns that ran
	// on a peer. Two clients of one broker stand in for two nodes.
	broker := memory.NewBroker()
	local, peer := broker.Client(), broker.Client()
	for _, q := range []*memory.Queue{local, peer} {
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("start: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
	}

	live := &ingester{}
	p := observe.NewProjector(local, live)
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("projector start: %v", err)
	}
	t.Cleanup(func() { p.Stop(context.Background()) })

	// Published by the OTHER node.
	ev := phaseEvent("CTO")
	if err := peer.Publish(t.Context(), topics.Event(ev.Type), ev); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, "the peer's event to reach this node's projection", func() bool {
		return len(live.types()) == 1
	})
	if got := live.types()[0]; got != "agent_phase_started" {
		t.Errorf("projected %q", got)
	}
}

func TestALiveOnlyEventReachesTheProjectionAndNotTheStore(t *testing.T) {
	t.Parallel()
	// The two halves disagree about this type ON PURPOSE, and both must
	// hold: agent_turn_progress is what moves a seat into `working`
	// mid-phase, and persisting it would fill the log with intermediate
	// states of rows it also holds finished.
	broker := memory.NewBroker()
	q := broker.Client()
	s := &sink{}
	q.AddPublishListener(observe.NewWriter(s).Listen())
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	live := &ingester{}
	p := observe.NewProjector(q, live)
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("projector start: %v", err)
	}
	t.Cleanup(func() { p.Stop(context.Background()) })

	ev := events.New(types.AgentTurnProgress{
		RoleName: "CEO", TurnID: "t1", Phase: types.PhaseExecute, RoundNum: 0,
	}, events.TraceContext{})
	if err := q.Publish(t.Context(), topics.Event(ev.Type), ev); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, "the progress round to reach the projection", func() bool {
		return len(live.types()) == 1
	})
	if s.count() != 0 {
		t.Errorf("a live-only event was persisted: %+v", s.rows)
	}
}

func TestStoppingTheProjectorEndsDelivery(t *testing.T) {
	t.Parallel()
	// The subscription is ephemeral BY CONTRACT, not by construction: a
	// backend that materialises one durably has to be told this one is
	// finished, or a node that comes and goes leaves a trail of them
	// accruing mail nobody reads.
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.Background()) })

	live := &ingester{}
	p := observe.NewProjector(q, live)
	if err := p.Start(t.Context()); err != nil {
		t.Fatalf("projector start: %v", err)
	}
	if err := q.Publish(t.Context(), topics.Event("agent_phase_started"),
		phaseEvent("CEO")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, "the first event", func() bool { return len(live.types()) == 1 })

	p.Stop(context.Background())
	if err := q.Publish(t.Context(), topics.Event("agent_phase_started"),
		phaseEvent("CEO")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	// Given a moment to arrive if it were going to.
	waitQuiet(t, func() int { return len(live.types()) }, 1)
}

func TestANodeWithNoDashboardBuildsNoProjector(t *testing.T) {
	t.Parallel()
	// Nil on both arms, and Start/Stop on a nil projector must be no-ops:
	// a worker-only node calls them unconditionally.
	if p := observe.NewProjector(memory.New(), nil); p != nil {
		t.Error("a projector was built with nowhere to project")
	}
	var p *observe.Projector
	if err := p.Start(context.Background()); err != nil {
		t.Errorf("nil projector Start: %v", err)
	}
	p.Stop(context.Background())
}

// waitFor polls until cond holds, failing the test if it never does.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// waitQuiet asserts a counter STAYS at want, which is what "nothing more
// arrived" means. A single read would pass before a late delivery landed.
func waitQuiet(t *testing.T, count func() int, want int) {
	t.Helper()
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if got := count(); got != want {
			t.Fatalf("count = %d, want %d: delivery continued after the stop", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
