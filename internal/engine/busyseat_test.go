package engine_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A BUSY SEAT'S MAIL WAITS ON THE BROKER, AND NOTHING CIRCLES, on BOTH backends.
//
// What used to spin: while a detached coding run held a seat, every delivery to
// it was PARKED — requeued onto the inbox it had just been fetched from, then
// acked — and nothing stopped the consumer, so the copy was the next thing it
// fetched. For the length of the run each waiting message went round fetch,
// screen, answer lookup, publish, ack, at whatever rate the broker served.
//
// Now the coordinator holds the seat's inbox from the moment the run starts
// holding it, so the mail is not fetched at all: no dispatch, no requeue, no
// publish. When the run stops holding the seat — here, its completion resumes
// the turn — the hold lifts and each message is worked exactly once.
func TestABusySeatsMailWaitsWithoutCirclingOnEveryBackend(t *testing.T) {
	for name, open := range map[string]func(t *testing.T) queue.EventQueue{
		"memory_twin": func(t *testing.T) queue.EventQueue {
			return memory.New()
		},
		"embedded_jetstream": func(t *testing.T) queue.EventQueue {
			srv, err := jetstream.StartServer(t.Context(), jetstream.Config{
				AckWait: 2 * time.Second, FetchWait: 25 * time.Millisecond,
				NakDelay: 25 * time.Millisecond, NakCeiling: 50 * time.Millisecond,
			})
			if err != nil {
				t.Fatalf("StartServer: %v", err)
			}
			t.Cleanup(srv.Shutdown)
			q, err := srv.Client(t.Context())
			if err != nil {
				t.Fatalf("Client: %v", err)
			}
			return q
		},
	} {
		t.Run(name, func(t *testing.T) {
			runBusySeat(t, open(t))
		})
	}
}

// resumes is a resumer that records the turns it re-entered.
type resumes struct {
	mu    sync.Mutex
	turns []string
}

func (r *resumes) Resume(_ context.Context, req sandbox.ResumeRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.turns = append(r.turns, req.Run.TurnID)
	return nil
}

func (r *resumes) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.turns)
}

func runBusySeat(t *testing.T, q queue.EventQueue) {
	ctx := t.Context()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })

	store := sandbox.NewCoordStore(coordmemory.NewFleet())
	provider, runner := sandbox.NewFakeProvider(), sandbox.NewFakeRunner("claude-code")
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	resumer := &resumes{}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Audience: noAudience{}, Queue: q, Pending: store, Manager: manager,
		Resume: resumer, Hold: inboxHold{q: q}, After: (&handCranked{}).after,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)

	var dispatched, parked atomic.Int32
	ordinary := &turns{}
	d := &engine.Dispatcher{
		Ledgered:    func(kind string) bool { return kind == notificationType },
		Turn:        ordinary.run,
		Completions: ledgerstore.NewMemoryCompletions(),
		Answer:      coordinator.TryResumeFromAnswer,
		HoldSandbox: coordinator.HoldSeat,
		Conditions: func(handle string) inbox.Conditions {
			held, awaits := coordinator.SeatRuns(handle)
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true,
				SeatHeldBySandbox: held, SandboxAwaitsAnswer: awaits}
		},
		// THE OLD SHAPE OF THE BUG, kept reachable: a park republishes onto
		// the inbox, so a screening that parked a held seat's mail would
		// show here as a count that climbs for as long as the run holds.
		Park: func(ctx context.Context, handle string, evs []*events.Event) error {
			parked.Add(1)
			for _, ev := range evs {
				if err := q.Publish(ctx, topics.AgentInbox(handle), ev); err != nil {
					return err
				}
			}
			return nil
		},
		NoteDeferred: func(string) {},
	}
	if err := q.Subscribe(ctx, topics.AgentInbox("swe"), topics.AgentInboxGroup("swe"),
		func(ctx context.Context, ev *events.Event) queue.Result {
			dispatched.Add(1)
			return d.Dispatch(ctx, "swe", []*events.Event{ev})
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	// THE RUN STARTS, and with it the seat is held.
	if _, err := sandbox.Launch(ctx, manager, store, q, sandbox.LaunchRequest{
		Turn: sandbox.TurnRef{TurnID: "t1", AgentID: "a-1", AgentHandle: "swe", Role: "SWE",
			WorkKey: "wk-1", WorkSince: time.Now().UTC(),
			ConversationKey: "slack:C1:1.0", PartitionKey: "slack:C1:1.0", Reply: "tool"},
		Brief: "fix the flaky test", Task: "get CI green",
		Spec: sandbox.Spec{CodingAgent: "claude-code", PauseTTLSec: 1800},
	}); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if _, err := store.MarkSuspended(ctx, "t1",
		sandbox.Suspension{State: []byte(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	if err := coordinator.OnStarted(ctx, types.SandboxRunStarted{AgentHandle: "swe", TurnID: "t1"}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if !coordinator.SeatHeldBySandbox("swe") {
		t.Fatal("the running job does not hold its seat")
	}

	// MAIL ARRIVES WHILE THE JOB RUNS.
	for _, body := range []string{"is it done yet?", "also, the docs"} {
		if err := q.Publish(ctx, topics.AgentInbox("swe"), threadReply(body)); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}
	settle()
	if n := dispatched.Load(); n != 0 {
		t.Fatalf("the busy seat was handed %d deliveries while its job ran: its inbox is not held", n)
	}
	if n := parked.Load(); n != 0 {
		t.Fatalf("the busy seat requeued its mail %d times while its job ran", n)
	}
	if ran := ordinary.ran(); len(ran) != 0 {
		t.Fatalf("a turn ran beside the coding job: %q", ran)
	}

	// THE JOB COMPLETES: its completion resumes the turn, the seat stops
	// being held, and the mail that waited is worked — each message once.
	runner.Finish(sandbox.Result{Success: true, Text: "fixed"})
	run, _, err := store.Get(ctx, "t1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	completed := types.SandboxRunCompleted{Agent: run.AgentID, AgentHandle: "swe", RoleName: run.Role,
		TurnID: "t1", LaunchID: run.LaunchID, SandboxID: run.SandboxID, CodingAgent: run.CodingAgent}
	if err := coordinator.OnCompleted(ctx, completed, events.New(completed, events.TraceContext{})); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if resumer.count() != 1 {
		t.Fatalf("the completion resumed %d turns, want 1", resumer.count())
	}
	eventually(t, "the mail that waited to be worked", func() bool {
		return len(ordinary.ran()) == 2
	})
	settle()
	ran := ordinary.ran()
	if len(ran) != 2 || !strings.Contains(strings.Join(ran, "\n"), "is it done yet?") ||
		!strings.Contains(strings.Join(ran, "\n"), "also, the docs") {
		t.Fatalf("turns = %q, want each waiting message worked once", ran)
	}
	if n := parked.Load(); n != 0 {
		t.Fatalf("the seat requeued its mail %d times", n)
	}
	if n := dispatched.Load(); n != 2 {
		t.Fatalf("%d deliveries for 2 messages: something circled", n)
	}
}
