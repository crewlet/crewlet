package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/workkey"
)

// besideDispatcher is a dispatcher whose seat's runs answer every delivery
// what beside says, counting the turns it starts, the deliveries it sets
// aside and the inbox holds it asks for.
type besideDispatcher struct {
	*Dispatcher
	asked, ran, holds, sandboxHolds int
	aside                           [][]*events.Event
}

func newBesideDispatcher(conds inbox.Conditions,
	beside func(ctx context.Context, evs []*events.Event) (besideAnswer, error)) *besideDispatcher {

	b := &besideDispatcher{}
	b.Dispatcher = &Dispatcher{
		Ledgered:    inbox.Ledgered,
		Completions: ledgerstore.NewMemoryCompletions(),
		Conditions:  func(string) inbox.Conditions { return conds },
		Turn: func(context.Context, Request) (turn.Result, error) {
			b.ran++
			return turn.Result{Decision: phase.Done}, nil
		},
		Beside: func(ctx context.Context, _ string, evs []*events.Event) (besideAnswer, error) {
			b.asked++
			return beside(ctx, evs)
		},
		SetAside: func(_ context.Context, _ string, _ besideAnswer, evs []*events.Event) {
			b.aside = append(b.aside, evs)
		},
		HoldAside:    func(context.Context, string) { b.holds++ },
		HoldSandbox:  func(context.Context, string) { b.sandboxHolds++ },
		NoteDeferred: func(string) {},
		Now:          time.Now,
	}
	return b
}

// personsMessage is a person's chat message to swe in conversation.
func personsMessage(conversation string) *events.Event {
	ev := events.New(types.ExternalNotification{NotificationSource: "chatlet", SourceEventType: "post",
		Sender: "ana", Body: "can you look at this", Addressed: true}, events.TraceContext{})
	ev.Payload = map[string]any{notify.ConversationField: conversation, notify.PartitionField: conversation}
	return ev
}

// A DELIVERY BELOW THE CAP WAITS ASIDE ON THE RUN IT RELATES TO, OR STARTS A
// TURN BESIDE THE SEAT'S RUNS (ADR-0169). What the seat's runs answer is
// carried out: a key sets the delivery aside under it and starts nothing; none
// starts a turn; a store that cannot say defers it; one past what the inbox
// keeps aside holds the inbox; one whose every wake the routing settled is
// acknowledged as worked. At the cap the inbox hold stays and the runs are not
// asked.
//
// Mutation: start a turn on a keyed answer, or set aside past the room, and
// this goes red.
func TestADeliveryBelowTheCapWaitsAsideOnTheRunItRelatesToOrStartsBeside(t *testing.T) {
	t.Parallel()
	free := inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true}
	waits := func(context.Context, []*events.Event) (besideAnswer, error) {
		return besideAnswer{Key: "chat:C1", Run: "run-1", Rule: ruleConversation}, nil
	}
	for name, tc := range map[string]struct {
		conds   inbox.Conditions
		beside  func(context.Context, []*events.Event) (besideAnswer, error)
		room    int
		outcome queue.Outcome
		ran     int
		aside   int
		holds   int
	}{
		"waiting on a run, it is set aside": {conds: free, beside: waits, room: queue.MaxAside,
			outcome: queue.OutcomeAside, aside: 1},
		"related to no run, it starts beside them": {conds: free, room: queue.MaxAside,
			beside:  func(context.Context, []*events.Event) (besideAnswer, error) { return besideAnswer{}, nil },
			outcome: queue.OutcomeAck, ran: 1},
		"a store that cannot say defers it": {conds: free, room: queue.MaxAside,
			beside: func(context.Context, []*events.Event) (besideAnswer, error) {
				return besideAnswer{}, errors.New("the store did not answer")
			},
			outcome: queue.OutcomeDefer},
		"past what the inbox keeps aside, the inbox is held": {conds: free, beside: waits, room: 0,
			outcome: queue.OutcomeDefer, holds: 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b := newBesideDispatcher(tc.conds, tc.beside)
			ctx := queue.WithAsideRoom(t.Context(), tc.room)
			got := b.Dispatch(ctx, "swe", []*events.Event{personsMessage("chat:C1")})
			if got.Outcome != tc.outcome || b.ran != tc.ran || len(b.aside) != tc.aside || b.holds != tc.holds {
				t.Fatalf("outcome %v, %d turns, %d aside, %d holds; want %v, %d, %d, %d",
					got.Outcome, b.ran, len(b.aside), b.holds, tc.outcome, tc.ran, tc.aside, tc.holds)
			}
			if got.Outcome == queue.OutcomeAside && got.Key != "chat:C1" {
				t.Fatalf("set aside under %q, want the run's conversation", got.Key)
			}
		})
	}

	t.Run("at the cap the hold stays and the runs are not asked", func(t *testing.T) {
		t.Parallel()
		atCap := free
		atCap.SeatHeldBySandbox = true
		b := newBesideDispatcher(atCap, waits)
		got := b.Dispatch(t.Context(), "swe", []*events.Event{personsMessage("chat:C1")})
		if got.Outcome != queue.OutcomeDefer || got.Cause != queue.DeferExecutorHolds || b.asked != 0 ||
			b.sandboxHolds != 1 {
			t.Fatalf("at the cap: %v (%s), runs asked %d times, %d holds; want deferred under the "+
				"executor's hold, asked never", got.Outcome, got.Cause, b.asked, b.sandboxHolds)
		}
	})

	t.Run("a delivery whose wakes the routing settled is acknowledged", func(t *testing.T) {
		t.Parallel()
		var b *besideDispatcher
		b = newBesideDispatcher(free, func(ctx context.Context, evs []*events.Event) (besideAnswer, error) {
			for _, ev := range evs {
				if err := b.Completions.Record(ctx, "swe", workkey.Derive([]string{ev.ID.String()}),
					"run-1", time.Now()); err != nil {
					t.Errorf("Record: %v", err)
				}
			}
			return waits(ctx, evs)
		})
		got := b.Dispatch(t.Context(), "swe", []*events.Event{personsMessage("chat:C1")})
		if got.Outcome != queue.OutcomeAck || b.ran != 0 || len(b.aside) != 0 {
			t.Fatalf("outcome %v, %d turns, %d aside; want acknowledged as worked", got.Outcome, b.ran,
				len(b.aside))
		}
	})
}

// besideRig is [newRouterRig]'s engine with a coordinator counting run-1 on
// swe's seat, capped at two, whose inbox hold and readmits are the engine's own.
func besideRig(t *testing.T) (*routerRig, *sandbox.Coordinator) {
	t.Helper()
	r := newRouterRig(t)
	provider, runner := sandbox.NewFakeProvider(), sandbox.NewFakeRunner("claude-code")
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: provider},
		Runners:   map[string]sandbox.Runner{"claude-code": runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers: testAnswers(), Pushes: noPushes{}, Audience: noAudience{}, Queue: r.q, Pending: r.store,
		Manager: manager, Resume: &resumeSpy{}, Hold: seatHold{engine: r.e},
		SeatCap: func(string) int { return 2 },
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	r.e.useSandbox(r.store, coordinator)
	if err := coordinator.OnStarted(t.Context(), types.ExecutorLaunched{AgentHandle: "swe", TurnID: "run-1"}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if count := coordinator.SeatCount("swe"); count.Holding != 1 {
		t.Fatalf("the seat counts %+v, want run-1 holding it", count)
	}
	return r, coordinator
}

// A BUSY SEAT'S WAKE IS ROUTED TO THE RUN IT RELATES TO AND WAITS ASIDE UNTIL THE
// RUN LEAVES; AN UNRELATED ONE STARTS BESIDE IT. The requester's follow-ups in
// the run's conversation are filed for its job and set aside under the
// conversation; another writer's there, a triage candidate, waits with them; a
// stranger's in another conversation starts at once, with no model asked. As
// the run leaves, the coordinator readmits the conversation: the follow-up the
// run read is dropped as joined, the unread one runs as a turn of its own, and
// so does the other writer's.
//
// Mutation: start a wake of the run's conversation beside it, readmit before the
// run leaves, or drop the unread follow-up with the read one, and this goes red.
func TestABusySeatsWakeIsRoutedSetAsideAndReadmittedAsTheRunLeaves(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	r, coordinator := besideRig(t)
	var ran [][]string
	d := &Dispatcher{
		Ledgered: inbox.Ledgered, Completions: r.ledger, Now: time.Now, NoteDeferred: func(string) {},
		Turn: func(_ context.Context, req Request) (turn.Result, error) {
			ran = append(ran, wakeIDsOf(req.Events))
			return turn.Result{Decision: phase.Done}, nil
		},
		Beside: r.e.beside, SetAside: r.e.setAside, HoldAside: r.e.holdAside,
		HoldSandbox: r.e.holdSandboxSeat,
	}
	if err := r.q.SubscribeBatch(ctx, r.subject, r.group, func(ctx context.Context, evs []*events.Event) queue.Result {
		return d.Dispatch(ctx, "swe", evs)
	}, notify.KeyOf, nil); err != nil {
		t.Fatalf("SubscribeBatch: %v", err)
	}

	read := r.post(t, "lead", "chat:C1", "use staging, not production", 7)
	box := boundInbox(t, r.runs, r.store, "run-1", r.job)
	viewHolds(t, box, 1)
	taken := box.Take(ctx, mcpbridge.SteerEventToolBatch, hookCall())
	if !strings.Contains(taken, "use staging") {
		t.Fatalf("the hook was not handed the follow-up set aside: %s", taken)
	}
	settled, err := r.store.SettleHook(ctx, "run-1", sandbox.HookSettle{Launch: r.job, Held: []string{takeOf(t, taken)}})
	if err != nil {
		t.Fatal(err)
	}
	r.runs.confirmed(ctx, mustRow(t, r.store, "run-1"), settled)

	unread := r.post(t, "lead", "chat:C1", "and ping me after", 8)
	other := r.post(t, "ceo", "chat:C1", "and fix the logo", 9)
	stranger := r.post(t, "ceo", "chat:C2", "what is the build status", 10)
	if len(ran) != 1 || !slices.Equal(ran[0], []string{stranger.ID.String()}) {
		t.Fatalf("turns ran on %v, want the stranger's message alone, beside the run", ran)
	}
	if got := r.filed(t); !slices.Contains(got, read.ID.String()) || !slices.Contains(got, unread.ID.String()) {
		t.Fatalf("the run's record holds wakes %v, want both follow-ups", got)
	}

	if err := r.store.SetStatus(ctx, "run-1", sandbox.StatusAwaiting, sandbox.Fence{}); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	if err := coordinator.OnStarted(ctx, types.ExecutorLaunched{AgentHandle: "swe", TurnID: "run-1"}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	var after []string
	for _, turnRan := range ran[1:] {
		after = append(after, turnRan...)
	}
	slices.Sort(after)
	want := []string{unread.ID.String(), other.ID.String()}
	slices.Sort(want)
	if !slices.Equal(after, want) {
		t.Fatalf("after the run left, turns ran on %v, want the unread follow-up and the other "+
			"writer's message, and never the follow-up the run read", after)
	}
}

// A WAKE SET ASIDE IS TOLD WHY ONCE, AND A REASON ITS ROUTING TOLD FIRST STANDS.
// A delivery is set aside again each time a readmit finds its run still there,
// and its person is told on the fold-in's own memory ([waitsTold]), so the
// aside's line is told once, and never over the approvers a fold-in named.
//
// Mutation: tell the aside's line by [waitsTold.tell], and a fold-in's line is
// replaced by it.
func TestAWakeSetAsideIsToldWhyOnce(t *testing.T) {
	t.Parallel()
	var told waitsTold
	if !told.first("swe", "w1", asideLine) {
		t.Fatal("a wake set aside for the first time was not told why")
	}
	if told.first("swe", "w1", asideLine) || told.first("swe", "w1", runsAfterLine) {
		t.Fatal("a wake set aside again was told again")
	}
	approval := approvalLine([]string{"lead"})
	if !told.tell("swe", "w2", approval) || told.first("swe", "w2", asideLine) {
		t.Fatal("setting aside a wake waiting on its fold-in told over the approvers it waits for")
	}
	told.forget("swe", []string{"w1"})
	if !told.first("swe", "w1", asideLine) {
		t.Fatal("a wake that ran and came back was not told again")
	}
}
