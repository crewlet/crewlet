package node_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/node"
	"github.com/crewlet/crewlet/internal/queue"
	qmem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// THE GATE AS THE NODE USES IT.
//
// gate_internal_test.go covers the primitive. These cases are the wiring: a
// delivery parked for a slot goes back to the broker when the node drains
// rather than starting a multi-minute turn inside a shutdown that waits for
// it, and the Tier A knob is what sets the size. Five docs pages called
// `max_concurrent` this node's ceiling while it capped one LLM provider's
// subprocesses and nothing else.

// gatedNode is one node over the in-memory twins with a chosen ceiling.
//
// Its own tiny harness rather than the fleet suite's: that one is about which
// node owns what, runs two nodes and a real broker, and has no seam for a
// handler that blocks — which is the only way to observe a gate at all.
type gatedNode struct {
	t     *testing.T
	n     *node.Node
	q     queue.EventQueue
	seats []string
}

func gatedFleet(t *testing.T, ceiling int, turn node.TurnFunc, seats ...string) *gatedNode {
	t.Helper()
	broker := qmem.NewBroker()
	backend := &coordmem.Backend{}

	client := func() queue.EventQueue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatalf("queue.Start: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		return q
	}
	nodeQueue, publisher := client(), client()
	for _, h := range seats {
		if _, err := publisher.EnsureSubscription(t.Context(),
			topics.AgentInbox(h), topics.AgentInboxGroup(h)); err != nil {
			t.Fatalf("EnsureSubscription(%s): %v", h, err)
		}
	}

	n, err := node.New(node.Config{
		Queue: nodeQueue, Coord: backend,
		NodeID: "node-a", Owner: "node-a:1",
		Seats: func() []placement.Seat {
			out := make([]placement.Seat, len(seats))
			for i, h := range seats {
				out[i] = placement.Seat{Handle: h}
			}
			return out
		},
		LeaseTTL: fleetTTL, HeartbeatInterval: fleetTTL / 4, SweepInterval: fleetTTL / 8,
		MaxConcurrent: ceiling,
		Turn:          turn,
	})
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if err := n.Start(t.Context()); err != nil {
		t.Fatalf("node.Start: %v", err)
	}
	t.Cleanup(func() { n.Stop(context.WithoutCancel(t.Context())) })

	g := &gatedNode{t: t, n: n, q: publisher, seats: seats}
	eventually(t, "the node to hold every seat", func() bool {
		return len(n.Host().Held()) == len(seats)
	})
	return g
}

// within polls until cond holds, failing with what was being waited for.
//
// Its own budget rather than the fleet suite's 30x lease TTL: every failure
// here is "the turn never ran", and a minute of waiting to be told that turns
// one broken guard into a suite that looks hung.
func within(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * fleetTTL)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// send publishes off the test goroutine, and that is not incidental: the
// in-memory broker dispatches INLINE on the publishing goroutine, so a
// handler that blocks blocks its own publisher. Sending from one goroutine
// would serialize the very concurrency these cases are about — the twin's
// dispatch shape, not the node's.
func (g *gatedNode) send(handle, work string) {
	g.t.Helper()
	ev := events.New(trigger{Work: work}, events.TraceContext{})
	ev.Payload = map[string]any{"conversation_key": "c/" + handle}
	ctx := context.WithoutCancel(g.t.Context())
	go func() {
		if err := g.q.Publish(ctx, topics.AgentInbox(handle), ev); err != nil {
			g.t.Errorf("Publish(%s): %v", handle, err)
		}
	}()
}

// THE CONFIGURED CEILING IS WHAT THE NODE ENFORCES.
func TestTheConfiguredCeilingIsWhatTheNodeEnforces(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		peak    int
		running int
	)
	release := make(chan struct{})
	g := gatedFleet(t, 2, func(context.Context, string, []*events.Event) queue.Result {
		mu.Lock()
		running++
		if running > peak {
			peak = running
		}
		mu.Unlock()
		<-release
		mu.Lock()
		running--
		mu.Unlock()
		return queue.Ack()
	}, "ceo", "cto", "dev")

	for _, handle := range g.seats {
		g.send(handle, "work")
	}
	// Wait until the gate is full, then hold long enough that an ungated
	// node would have all three inside the handler.
	// AT LEAST two, not exactly: an ungated node races past 2 to 3, and a
	// poll for equality would miss it and then fail for the wrong reason.
	within(t, "the ceiling to fill", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return running >= 2
	})
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	got := peak
	mu.Unlock()
	close(release)
	if got != 2 {
		t.Fatalf("peak concurrency = %d under max_concurrent: 2", got)
	}
}

// A DRAIN RELEASES THE TURNS STILL WAITING FOR A SLOT, and leaves the running
// one alone. Without the split, a backlog admitted before the quiesce runs
// full Plan → Execute → Review turns one after another through a shutdown
// that waits for them indefinitely.
func TestADrainDefersTheTurnsStillWaitingForASlot(t *testing.T) {
	t.Parallel()
	var (
		mu      sync.Mutex
		started int
	)
	running := make(chan struct{}, 1)
	hold := make(chan struct{})
	g := gatedFleet(t, 1, func(context.Context, string, []*events.Event) queue.Result {
		mu.Lock()
		started++
		mu.Unlock()
		select {
		case running <- struct{}{}:
		default:
		}
		<-hold
		return queue.Ack()
	}, "ceo", "cto")

	g.send("ceo", "first")
	select {
	case <-running:
	case <-time.After(3 * fleetTTL):
		t.Fatal("the first turn never started")
	}
	g.send("cto", "second")
	// Long enough for the second delivery to reach the gate and park.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	if started != 1 {
		mu.Unlock()
		t.Fatalf("started = %d, want 1 — the gate did not hold the second turn", started)
	}
	mu.Unlock()

	drained := make(chan struct{})
	go func() {
		g.n.Drain(context.WithoutCancel(t.Context()))
		close(drained)
	}()

	// The drain cannot finish while the first turn holds its slot, and the
	// parked one must never enter the handler.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	ran := started
	mu.Unlock()
	if ran != 1 {
		t.Fatalf("started = %d — a parked turn ran during the drain", ran)
	}
	select {
	case <-drained:
		t.Fatal("the drain completed while a turn was still running")
	default:
	}

	close(hold)
	select {
	case <-drained:
	case <-time.After(10 * fleetTTL):
		t.Fatal("the drain never completed")
	}
	mu.Lock()
	defer mu.Unlock()
	if started != 1 {
		t.Fatalf("started = %d — the parked turn ran after all", started)
	}
}

// A DRAINED NODE THAT CONVERGES SERVES AGAIN. The posture path sheds on
// config divergence and comes back; a node whose gate latched shut would hold
// its seats, stay attached to their mailboxes, and refuse every turn.
func TestAResumedNodeRunsTurnsAgain(t *testing.T) {
	t.Parallel()
	var (
		mu   sync.Mutex
		seen []string
	)
	g := gatedFleet(t, 2, func(_ context.Context, handle string, _ []*events.Event) queue.Result {
		mu.Lock()
		seen = append(seen, handle)
		mu.Unlock()
		return queue.Ack()
	}, "ceo")

	g.n.Drain(context.WithoutCancel(t.Context()))
	g.n.ResumeClaiming(t.Context())

	g.send("ceo", "after the shed")
	within(t, "a turn to run on the node that drained and converged", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(seen) > 0
	})
}

// THE CONFIG DEFAULT IS THE RUNTIME'S, not a second number that can drift
// from it. An absent key decodes as zero and the node is what resolves it.
func TestAnAbsentCeilingLeavesTheNumberInOnePlace(t *testing.T) {
	t.Parallel()
	if got := config.DefaultBootstrap().Node.MaxConcurrent; got != 0 {
		t.Fatalf("DefaultBootstrap carries max_concurrent = %d — the number now "+
			"lives in two places and can drift from node.DefaultMaxConcurrent", got)
	}
	if node.DefaultMaxConcurrent < 1 {
		t.Fatalf("DefaultMaxConcurrent = %d", node.DefaultMaxConcurrent)
	}
}
