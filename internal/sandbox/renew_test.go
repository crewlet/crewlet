package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// renewBudget is the broker's budget in these cases, in redeliveries: a
// message's eight deliveries span eight minutes at a minute a read, so one
// message's budget runs out well inside the host grace.
const renewBudget = 7

// readWait is how long the box's host takes to answer each read, on the
// bubble's clock: what moves time on, since the memory twin hands a message
// back at once.
const readWait = time.Minute

// errBrokerRefused is a publish the broker did not take.
var errBrokerRefused = errors.New("nats: no responders available for request")

// controlTee is a coordinator's publisher that records every publish as the
// rig's recorder does and hands one to the seat's control topic to the broker
// as well, refusing the next failing of those first.
type controlTee struct {
	*recorder
	broker  *queuemem.Queue
	control string

	mu      sync.Mutex
	failing int
}

func (p *controlTee) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if topic != p.control {
		return p.recorder.Publish(ctx, topic, ev)
	}
	p.mu.Lock()
	refuse := p.failing > 0
	if refuse {
		p.failing--
	}
	p.mu.Unlock()
	if refuse {
		return errBrokerRefused
	}
	if err := p.recorder.Publish(ctx, topic, ev); err != nil {
		return err
	}
	return p.broker.Publish(ctx, topic, ev)
}

// refuseNext has the broker refuse the next n publishes to the control topic.
func (p *controlTee) refuseNext(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.failing = n
}

// timedHost is the box's host as these cases time it: every read first waits
// out wait, which the rig sets to take [readWait] on the bubble's clock and
// then, where at is set, to have at say what changes at the time elapsed
// since the rig was built ([newRenewRig]).
//
// THE WAIT IS THE RIG'S, written inside its bubble-only builder, never in this
// method: a method of an exported name is reached where no caller can be held
// to a bubble, so a sleep written here is the wall clock's to
// internal/fixedwait, however every case runs it.
type timedHost struct {
	BoxReader
	wait func()
	at   func(elapsed time.Duration)
}

func (h *timedHost) ReadBox(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error) {
	h.wait()
	return h.BoxReader.ReadBox(ctx, run, read)
}

// delivered is one delivery of the seat's control topic: the announcement it
// carried, how many deliveries it had left, and how much other mail the topic
// held beside it.
type delivered struct {
	id      uuid.UUID
	left    int
	waiting int
}

// renewRig is node-b holding swe's seat on a memory broker whose budget is
// [renewBudget] redeliveries, subscribed to the seat's control topic as the
// engine subscribes it, collecting t1 off its box on node-a's host, whose
// coordination store does not answer node-a's read of the run's row: node-b's
// own store answers throughout, every read of the box takes [readWait], and the
// coordinator's clock is the bubble's.
type renewRig struct {
	*coordRig
	broker *queuemem.Queue
	tee    *controlTee
	store  *silentStore
	host   *timedHost
	start  time.Time

	mu   sync.Mutex
	seen []delivered
	// before, where set, runs before the coordinator is handed each delivery,
	// told how many the topic has delivered with this one: a case's way to
	// change what the stores answer at a delivery rather than at a time, since
	// a hand-back that reads no box moves no clock on.
	before func(n int)
}

// newRenewRig builds a [renewRig]. Its host's every read waits a minute on the
// clock, so it runs only inside a synctest bubble, where that wait moves the
// bubble's time on at once rather than holding the case for a minute a read.
//
//fixedwait:bubble
func newRenewRig(t *testing.T) *renewRig {
	t.Helper()
	broker := queuemem.New(queuemem.WithMaxRedeliveries(renewBudget))
	if err := broker.Start(t.Context()); err != nil {
		t.Fatalf("starting the broker: %v", err)
	}
	control, group := sweControl(), topics.AgentControlGroup(uuid.MustParse(sweID))
	var tee *controlTee
	rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
		o.Now = time.Now
		rec, ok := o.Queue.(*recorder)
		if !ok {
			t.Fatalf("the rig's publisher is a %T, not the recorder", o.Queue)
		}
		tee = &controlTee{recorder: rec, broker: broker, control: control}
		o.Queue = tee
	})
	rig.now = time.Now()
	store, _ := unsettledHost(t, rig)
	r := &renewRig{coordRig: rig, broker: broker, tee: tee, store: store, start: time.Now()}
	r.host = &timedHost{BoxReader: rig.coordinator.boxes}
	r.host.wait = func() {
		time.Sleep(readWait)
		if r.host.at != nil {
			r.host.at(time.Since(r.start))
		}
	}
	rig.coordinator.boxes = r.host
	// THE ENGINE'S CONTROL HANDLER, for what these cases meet ([controlResult]).
	if err := broker.Subscribe(t.Context(), control, group, func(ctx context.Context, ev *events.Event) queue.Result {
		left, _ := queue.DeliveriesLeftFor(ctx, ev.ID)
		r.mu.Lock()
		waiting, err := broker.Backlog(ctx, control, group, 1<<12)
		if err != nil {
			t.Errorf("read the control topic's mail: %v", err)
		}
		r.seen = append(r.seen, delivered{id: ev.ID, left: left, waiting: len(waiting)})
		n, before := len(r.seen), r.before
		r.mu.Unlock()
		if before != nil {
			before(n)
		}
		return controlResult(rig.coordinator.OnEvent(ctx, ev))
	}); err != nil {
		t.Fatalf("subscribing to the seat's control topic: %v", err)
	}
	return r
}

// controlResult is what the engine's control handler leaves a delivery with,
// for what these cases meet (internal/engine's controlOutcome): nil is
// acknowledged; a refusal the handler hands on, a drain's and a seat let go
// of's ([Draining], [SeatLost]), is deferred; a store's silence, every
// delivery the coordinator marks as one no waiter announces again and a claim
// the seat's recovery gate refused ([ErrStoreSilent], [ErrUnannounced],
// [ErrSeatRecovering]) are NAK'd as the node's own, a refusal so marked
// included; any other refusal on a condition that clears by waiting
// is acknowledged; every other error is NAK'd.
func controlResult(err error) queue.Result {
	if err == nil {
		return queue.Ack()
	}
	var refused *TurnRefused
	waits := errors.As(err, &refused) && refused.Condition != ""
	switch {
	case waits && refused.Condition == Draining:
		return queue.Defer(queue.DeferDraining, err.Error())
	case waits && refused.Condition == SeatLost:
		return queue.Defer(queue.DeferSeatMoved, err.Error())
	case errors.Is(err, ErrStoreSilent), errors.Is(err, ErrUnannounced), errors.Is(err, ErrSeatRecovering):
		return queue.NakNode(err)
	case waits:
		return queue.Ack()
	}
	return queue.Nak(err)
}

// announce publishes t1's completion as the host's waiter did before its store
// went silent, and returns once the broker has nothing left to deliver: the
// memory twin dispatches on the publishing goroutine.
func (r *renewRig) announce(t *testing.T) {
	t.Helper()
	_, ev := r.completion("t1")
	if err := r.broker.Publish(t.Context(), sweControl(), ev); err != nil {
		t.Fatalf("announcing the completion: %v", err)
	}
}

// atDelivery has before run ahead of each delivery the coordinator is handed
// ([renewRig.before]).
func (r *renewRig) atDelivery(before func(n int)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.before = before
}

// deliveries is every delivery of the control topic, in order.
func (r *renewRig) deliveries() []delivered {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]delivered(nil), r.seen...)
}

// announcements is how often each announcement was delivered, in the order
// each was first delivered.
func announcements(seen []delivered) (order []uuid.UUID, times map[uuid.UUID]int) {
	times = map[uuid.UUID]int{}
	for _, d := range seen {
		if times[d.id] == 0 {
			order = append(order, d.id)
		}
		times[d.id]++
	}
	return order, times
}

// lostWithItsHostOnTheBroker holds the rig to a job recorded lost with its
// host and its round carried on, once, nothing ended or dead-lettered.
func (r *renewRig) lostWithItsHostOnTheBroker(t *testing.T) {
	t.Helper()
	resumed := r.resumer.calls()
	if len(resumed) != 1 || resumed[0].Lost == nil || resumed[0].Lost.Cause != LossLaunchNodeStoreSilent {
		t.Fatalf("the turn resumed %d times (%+v); want its round carried on once, the job lost with "+
			"its host", len(resumed), resumed)
	}
	if failed := r.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v; want the job lost with its host, never its run ended", failed)
	}
	if dead := r.broker.DeadLetters(sweControl(), topics.AgentControlGroup(uuid.MustParse(sweID))); len(dead) != 0 {
		t.Fatalf("%d of the job's announcements were dead-lettered", len(dead))
	}
}

// resumedOnTheBroker holds the rig to a turn resumed once on its box's
// account, its completion renewed through the wait, one announcement in flight
// at every delivery, and nothing ended or dead-lettered.
func (r *renewRig) resumedOnTheBroker(t *testing.T) {
	t.Helper()
	if dead := r.broker.DeadLetters(sweControl(), topics.AgentControlGroup(uuid.MustParse(sweID))); len(dead) != 0 {
		t.Fatalf("%d of the job's announcements were dead-lettered while the clock on its host ran", len(dead))
	}
	resumed := r.resumer.calls()
	if len(resumed) != 1 || resumed[0].Lost != nil || resumed[0].Ended.Text != wholeAccount().Text {
		t.Fatalf("the turn resumed %d times (%+v); want it resumed once on its box's account", len(resumed),
			resumed)
	}
	if failed := r.failures(); len(failed) != 0 {
		t.Fatalf("announced %+v", failed)
	}
	seen := r.deliveries()
	for i, d := range seen {
		if d.waiting != 0 {
			t.Fatalf("delivery %d of %d was handed over with %d more announcements waiting; want one in "+
				"flight", i+1, len(seen), d.waiting)
		}
	}
	if order, _ := announcements(seen); len(order) < 2 {
		t.Fatalf("%d deliveries carried %d announcement; want the completion renewed", len(seen), len(order))
	}
}

// A COMPLETION WHOSE BOX'S HOST ALONE FINDS THE STORE SILENT IS RENEWED BEFORE
// ITS DELIVERY RUNS OUT, through the bound and past it until the round is
// carried on; a re-fire renews at once unless its attempt settled the run, and
// a hand-back this node's own store made is NAK'd only. See the package doc, "A
// completion is renewed while the clock on its box's host runs". The memory
// twin hands a NAK'd message back at once, so time moves on the host's own
// answers, a minute each, on the bubble's clock.
//
// Mutation: renew nothing (a dead letter); NAK a renewed delivery (a growing
// pile); renew on every hand-back or the last only, or under the delivery's id;
// ack a refused renewal; skip a re-fire or an untaken loss; renew a re-fire
// that ended its run; drop the clock at the loss; renew this node's own.
func TestACompletionWhoseHostAloneFindsTheStoreSilentIsRenewed(t *testing.T) {
	t.Parallel()
	if (renewBudget+1)*readWait >= DefaultHostGrace {
		t.Fatalf("the premise: one message's %d deliveries span %v, not inside the %v host grace",
			renewBudget+1, (renewBudget+1)*readWait, DefaultHostGrace)
	}

	t.Run("the job is lost with its host past a budget shorter than the host grace", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			r.announce(t)
			if waited := time.Since(r.start); waited < DefaultHostGrace {
				t.Fatalf("the broker had nothing left to deliver %v in, inside the %v host grace",
					waited, DefaultHostGrace)
			}
			r.lostWithItsHostOnTheBroker(t)
		})
	})

	t.Run("a job lost with its host whose carry fails is renewed until its round is carried on", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			// THIS NODE'S OWN STORE DOES NOT ANSWER THE WATCH of the run's
			// log that a lost job's carry waits on its calls by, for past
			// one message's whole budget: the loss is recorded at the
			// bound, and its round is not carried on for a while after.
			mine := &unansweredWatches{PendingStore: r.coordinator.pending, silent: 2 * (renewBudget + 1)}
			r.coordinator.pending = mine
			r.announce(t)
			if waited := time.Since(r.start); waited < DefaultHostGrace {
				t.Fatalf("the broker had nothing left to deliver %v in, inside the %v host grace",
					waited, DefaultHostGrace)
			}
			r.lostWithItsHostOnTheBroker(t)
			if watched := mine.watched(); watched <= mine.silent {
				t.Fatalf("the premise: the carry watched the run's log %d times; want it handed back past the %d "+
					"watches its store did not answer", watched, mine.silent)
			}
		})
	})

	t.Run("a completion never dead-letters while it waits short of the bound", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			answerAt := DefaultHostGrace - 3*time.Minute
			r.host.at = func(elapsed time.Duration) {
				if elapsed >= answerAt {
					r.store.answer()
				}
			}
			r.announce(t)
			if waited := time.Since(r.start); waited < answerAt {
				t.Fatalf("the broker had nothing left to deliver %v in, before the host's store answered "+
					"at %v", waited, answerAt)
			}
			if dead := r.broker.DeadLetters(sweControl(), topics.AgentControlGroup(uuid.MustParse(sweID))); len(dead) != 0 {
				t.Fatalf("%d of the job's announcements were dead-lettered while it waited for the host's store",
					len(dead))
			}
			resumed := r.resumer.calls()
			if len(resumed) != 1 || resumed[0].Lost != nil || resumed[0].Ended.Text != wholeAccount().Text {
				t.Fatalf("the turn resumed %d times (%+v); want it resumed once on its box's account", len(resumed),
					resumed)
			}
			if failed := r.failures(); len(failed) != 0 {
				t.Fatalf("announced %+v", failed)
			}
		})
	})

	t.Run("each renewal leaves one announcement in flight", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			r.announce(t)
			r.lostWithItsHostOnTheBroker(t)
			seen := r.deliveries()
			for i, d := range seen {
				if d.waiting != 0 {
					t.Fatalf("delivery %d of %d was handed over with %d more announcements waiting; want one "+
						"in flight", i+1, len(seen), d.waiting)
				}
			}
			order, times := announcements(seen)
			if len(order) < 2 {
				t.Fatalf("%d deliveries carried %d announcement; want the completion renewed", len(seen), len(order))
			}
			// EACH ONE DELIVERED UNTIL ITS DELIVERY IN HAND HAS THE RESERVE
			// LEFT, then replaced: never renewed sooner, at the pace the
			// host answers rather than the broker's backoff, and never let
			// run to its last.
			want := renewBudget - renewReserve + 1
			for i, id := range order[:len(order)-1] {
				if times[id] != want {
					t.Fatalf("announcement %d was delivered %d times before it was renewed; want %d, until "+
						"%d deliveries were left", i+1, times[id], want, renewReserve)
				}
			}
		})
	})

	t.Run("a renewal the broker does not take is made again on a delivery still left", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			r.tee.refuseNext(1)
			r.announce(t)
			r.lostWithItsHostOnTheBroker(t)
			order, times := announcements(r.deliveries())
			if want := renewBudget - renewReserve + 2; len(order) < 2 || times[order[0]] != want {
				t.Fatalf("the first announcement was delivered %d times; want %d, its renewal made again on the "+
					"delivery after the one the broker refused it on", times[order[0]], want)
			}
		})
	})

	t.Run("a loss this node's store does not record is renewed as the wait is", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			mine := &unrecordedLoss{PendingStore: r.coordinator.pending, silent: true}
			r.coordinator.pending = mine
			// PAST ONE MESSAGE'S WHOLE BUDGET past the bound, so a loss's
			// hand-back NAK'd and nothing more is dead-lettered first.
			recordsAt := DefaultHostGrace + (renewBudget+2)*readWait
			r.host.at = func(elapsed time.Duration) {
				if elapsed >= recordsAt {
					mine.answer()
				}
			}
			r.announce(t)
			if waited := time.Since(r.start); waited < recordsAt {
				t.Fatalf("the broker had nothing left to deliver %v in, before this node's store took the "+
					"loss at %v", waited, recordsAt)
			}
			r.lostWithItsHostOnTheBroker(t)
		})
	})

	t.Run("a hand-back this node's own store made is NAK'd and nothing more", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			// THE HOST'S STORE ANSWERS, AND THIS NODE'S DOES NOT ANSWER THE
			// DRAIN'S WATCH of the run's log: the waiter that drives the box
			// announces the job again on its schedule, so nothing renews it.
			r.store.answer()
			own := &silentStore{PendingStore: r.coordinator.pending, silent: []string{"WatchLog"}}
			r.coordinator.pending = own
			// AND ANSWERS AGAIN well past one message's budget, so a renewal
			// that carried the completion on ends the case rather than
			// circling for ever.
			r.host.at = func(elapsed time.Duration) {
				if elapsed >= 3*(renewBudget+1)*readWait {
					own.answer()
				}
			}
			r.announce(t)
			order, times := announcements(r.deliveries())
			if len(order) != 1 || times[order[0]] != renewBudget+1 {
				t.Fatalf("%d announcements were delivered (%v); want the one, delivered its whole budget of %d",
					len(order), times, renewBudget+1)
			}
			if dead := r.broker.DeadLetters(sweControl(), topics.AgentControlGroup(uuid.MustParse(sweID))); len(dead) != 1 {
				t.Fatalf("%d dead letters; want the one announcement's, its budget spent", len(dead))
			}
			if resumed := r.resumer.calls(); len(resumed) != 0 {
				t.Fatalf("the turn resumed %d times through a store that did not answer its drain", len(resumed))
			}
		})
	})

	t.Run("a re-fire with no delivery in hand is renewed at once", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		gate := gated(rig)
		gate.refuse(true)
		unsettledHost(t, rig)
		payload, kept := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, kept); !errors.Is(err, errLeaving) {
			t.Fatalf("OnCompleted = %v, want the gate's refusal", err)
		}
		gate.refuse(false)
		rig.coordinator.Readmit(Draining, "swe")
		rig.coordinator.inflight.Wait()

		var renewed []*events.Event
		rig.queue.mu.Lock()
		for _, p := range rig.queue.published {
			if _, ok := p.event.Data.(*types.ExecutorFinished); ok && p.topic == sweControl() {
				renewed = append(renewed, p.event)
			}
		}
		rig.queue.mu.Unlock()
		if len(renewed) != 1 {
			t.Fatalf("the re-fire handed back for the host's silent store announced the completion %d times; "+
				"want it announced afresh once", len(renewed))
		}
		got, ok := renewed[0].Data.(*types.ExecutorFinished)
		if !ok || got.TurnID != payload.TurnID || got.LaunchID != payload.LaunchID ||
			renewed[0].ID == kept.ID || renewed[0].TraceID != kept.TraceID {
			t.Fatalf("announced %+v as %s (trace %q); want job %s announced afresh under a new id, in the "+
				"trace of %s", renewed[0].Data, renewed[0].ID, renewed[0].TraceID, payload.LaunchID, kept.ID)
		}
		if got := rig.get("t1"); got.Status != StatusRunning {
			t.Fatalf("the re-fire left the run %s; want it handed back to running", got.Status)
		}
	})

	t.Run("a re-fire whose run its own hand-back ended announces nothing", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		gate := gated(rig)
		gate.refuse(true)
		unsettledHost(t, rig)
		payload, kept := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, kept); !errors.Is(err, errLeaving) {
			t.Fatalf("OnCompleted = %v, want the gate's refusal", err)
		}
		// THE RE-FIRE'S OWN COLLECTION STARTS THE CLOCK, and the store then
		// refuses its hand-back, a definite answer no retry changes: the run
		// is ended there, settled within the re-fire's own attempt, and the
		// clock goes with it.
		rig.coordinator.pending = &unansweredRelease{silentStore: &silentStore{PendingStore: rig.coordinator.pending,
			silent: []string{"ReleaseClaim"}}, refused: true}
		gate.refuse(false)
		rig.coordinator.Readmit(Draining, "swe")
		rig.coordinator.inflight.Wait()
		failed := rig.failures()
		if len(failed) != 1 || failed[0].Reason != types.ExecutorFailureClaimStranded || clocked(rig) {
			t.Fatalf("the premise: announced %+v, clocked %v; want the run ended once its hand-back was "+
				"refused, and the clock gone with it", failed, clocked(rig))
		}
		rig.fireRetries()
		if renewed := renewedOn(rig); len(renewed) != 0 {
			t.Fatalf("a re-fire whose run its own hand-back ended announced the completion %d times; want "+
				"none, the run settled", len(renewed))
		}
	})
}

// unansweredClaims is a run store whose coordination store answers the first
// before claims of a completion, does not answer the silent after them, and has
// answered run as the last of those goes unanswered; it answers every other call
// as the store beneath it does.
type unansweredClaims struct {
	PendingStore
	before, silent int
	answered       func()

	mu     sync.Mutex
	claims int
}

func (s *unansweredClaims) ClaimForResume(ctx context.Context, turnID string, tail Tail, fence Fence,
) (PendingRun, bool, error) {
	s.mu.Lock()
	s.claims++
	n := s.claims
	s.mu.Unlock()
	if n > s.before && n <= s.before+s.silent {
		if n == s.before+s.silent {
			s.answered()
		}
		return PendingRun{}, false, errStoreUnavailable("claim the run", turnID)
	}
	return s.PendingStore.ClaimForResume(ctx, turnID, tail, fence)
}

// unansweredWatches is a run store whose coordination store does not answer
// the first silent watches of a run's log, which a collection waits on its
// calls in flight by, and answers every other call, and every watch after
// those, as the store beneath it does.
type unansweredWatches struct {
	PendingStore
	silent int

	mu      sync.Mutex
	watches int
}

func (s *unansweredWatches) WatchLog(ctx context.Context, turnID string) (<-chan LogView, error) {
	s.mu.Lock()
	s.watches++
	n := s.watches
	s.mu.Unlock()
	if n <= s.silent {
		return nil, errStoreUnavailable("watch the run's log", turnID)
	}
	return s.PendingStore.WatchLog(ctx, turnID)
}

// watched is how many watches of a run's log the store was asked for.
func (s *unansweredWatches) watched() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watches
}

// hostReads is the box's host as a case scripts it: from the from-th read of
// the box on, for silent reads, the host does not answer at all
// ([ErrBoxHostSilent]); answered runs before the read after them, which the
// host makes as the reader beneath it does, as it makes every other.
type hostReads struct {
	BoxReader
	from, silent int
	answered     func()

	mu    sync.Mutex
	reads int
}

func (h *hostReads) ReadBox(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error) {
	h.mu.Lock()
	h.reads++
	n := h.reads
	h.mu.Unlock()
	switch {
	case n >= h.from && n < h.from+h.silent:
		return Result{}, false, fmt.Errorf("%w: node-a, within a minute", ErrBoxHostSilent)
	case n == h.from+h.silent:
		h.answered()
	}
	return h.BoxReader.ReadBox(ctx, run, read)
}

// EVERY HAND-BACK OF A COMPLETION IS RENEWED WHILE THE CLOCK ON ITS BOX'S HOST
// RUNS, WHATEVER ITS KIND: a claim this node's store does not answer, a claim
// refused for a release owed, and a host that does not answer are each NAK'd
// and renewed before their last, never acknowledged, each renewal leaving one
// announcement in flight.
//
// Mutation: renew only the host's own answer that its store did not answer, and
// the other hand-backs run one message's budget out to its dead letter;
// acknowledge a host that did not answer while the clock runs, and nothing
// announces the job again.
func TestEveryHandBackIsRenewedWhileTheClockOnItsHostRuns(t *testing.T) {
	t.Parallel()
	// past is how many deliveries a hand-back goes on for: past one message's
	// whole budget, so one NAK'd and nothing more is dead-lettered first.
	past := 2 * (renewBudget + 1)

	t.Run("a claim this node's store does not answer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			// THE FIRST CLAIM LANDS, and its collection starts the clock on
			// the host; the claims after it go unanswered past a budget.
			r.coordinator.pending = &unansweredClaims{PendingStore: r.coordinator.pending, before: 1,
				silent: past, answered: r.store.answer}
			r.announce(t)
			r.resumedOnTheBroker(t)
		})
	})

	t.Run("a claim refused while a release this node owes is out", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			// THE FIRST COLLECTION'S HAND-BACK GOES UNANSWERED, so this node
			// owes its release and refuses every claim of the job meanwhile,
			// past a budget, before the release is made again and lands.
			mine := &unansweredRelease{silentStore: &silentStore{PendingStore: r.coordinator.pending,
				silent: []string{"ReleaseClaim"}}}
			r.coordinator.pending = mine
			r.atDelivery(func(n int) {
				if n == past {
					mine.answer()
					r.store.answer()
					r.fireRetries()
				}
			})
			r.announce(t)
			r.resumedOnTheBroker(t)
		})
	})

	t.Run("a host that does not answer the read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			// THE FIRST READ IS THE HOST'S ANSWER THAT ITS STORE DID NOT
			// ANSWER, which starts the clock; the host then answers nothing
			// for past one message's budget, a minute a read, inside the host
			// grace, and comes back with its store answering.
			r.host.BoxReader = &hostReads{BoxReader: r.host.BoxReader, from: 2, silent: renewBudget + 2,
				answered: r.store.answer}
			r.announce(t)
			r.resumedOnTheBroker(t)
		})
	})
}

// refusingGate is a [TurnGate] that admits the first admit resumes it is asked
// about, refuses the refuse after them on [conditionLimit], has cleared run as
// the last of those is refused, and admits every resume after them.
type refusingGate struct {
	admit, refuse int
	cleared       func()

	mu    sync.Mutex
	asked int
}

func (g *refusingGate) gate(ctx context.Context, _, _ string, _ ResumeRoute) (context.Context, func(), error) {
	g.mu.Lock()
	g.asked++
	n := g.asked
	g.mu.Unlock()
	if n > g.admit && n <= g.admit+g.refuse {
		if n == g.admit+g.refuse {
			g.cleared()
		}
		return nil, nil, &TurnRefused{Refusal: Refusal{Condition: conditionLimit,
			Reason: "every model of the seat's chain is at its usage limit"}}
	}
	return ctx, func() {}, nil
}

// delivery is the context ev's first control-topic delivery is handed to its
// handler under: the broker's count of the deliveries ev has left after it, a
// whole budget's, as the queue states it.
func delivery(ctx context.Context, ev *events.Event) context.Context {
	return queue.WithHeadroom(ctx, []queue.Headroom{{ID: ev.ID, Left: renewBudget}})
}

// A REFUSAL WHILE THE CLOCK ON ITS BOX'S HOST RUNS IS HANDED BACK, NEVER
// ACKNOWLEDGED ([ErrUnannounced]), renewed before its last beside being kept:
// the host's waiter announces nothing and the keep is this node's memory alone.
// So are a completion held here after its resume met a limit and an answer that
// settled nothing. A re-fire announces afresh only where its own attempt
// started the clock, never beside a delivery in flight.
//
// Mutation: acknowledge a refusal, or an answer that settled nothing, while the
// clock runs, and the job is never announced again; renew a re-fire whenever
// the clock runs after it, or wherever it started after the re-fire began, and
// the completion has two announcements in flight.
func TestARefusalWhileTheClockOnItsHostRunsIsHandedBack(t *testing.T) {
	t.Parallel()

	t.Run("a refusal of the node's gate that nothing signals", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			// THE FIRST RESUME IS ADMITTED, and its collection starts the
			// clock on the host; the gate then refuses past one message's
			// budget, and nothing signals that its condition cleared.
			gate := &refusingGate{admit: 1, refuse: 2 * (renewBudget + 1), cleared: r.store.answer}
			r.coordinator.gate = gate.gate
			r.announce(t)
			r.resumedOnTheBroker(t)
		})
	})

	t.Run("a completion this node holds off itself", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		payload, ev := rig.completion("t1")
		rig.coordinator.keepRefused(payload, ev, Refusal{Condition: conditionLimit,
			Reason: "the review's models are at their limit", Until: rig.now.Add(time.Hour)}, true,
			rig.coordinator.signalsSeen())
		err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev)
		var refused *TurnRefused
		if !errors.As(err, &refused) || refused.Condition != conditionLimit || !errors.Is(err, ErrUnannounced) {
			t.Fatalf("a held completion while the clock on its host runs = %v; want its hold handed back as "+
				"no waiter's to announce again", err)
		}
		if got := controlResult(err); got.Outcome != queue.OutcomeNak {
			t.Fatalf("the held completion's delivery was left %s; want it handed back", got.Outcome)
		}
	})

	t.Run("a resume its row holds, which settles nothing", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		// THE JOB IS LOST WITH ITS HOST at the bound, and the resume that
		// carries its round on is held for the seat's budget: the round is
		// not carried on, so the clock runs on.
		rig.now = rig.now.Add(DefaultHostGrace)
		rig.resumer.failWith(budgetHeld(rig.now.Add(time.Hour)))
		_, ev := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); !errors.Is(err, ErrUnannounced) {
			t.Fatalf("the carry its row holds for the seat's budget = %v; want its delivery handed back as no "+
				"waiter's to announce again", err)
		}
		if got := rig.get("t1"); got.Status != StatusRunning || got.HeldFor != "budget" || got.Lost == nil {
			t.Fatalf("the row is %s, held for %q, lost %+v; want it running, lost with its host, its resume "+
				"held for the budget", got.Status, got.HeldFor, got.Lost)
		}
		// AND SO IS THE NEXT DELIVERY, whose claim the hold refuses.
		_, again := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), again), again); !errors.Is(err, ErrUnannounced) {
			t.Fatalf("a delivery while the row holds the resume = %v; want it handed back as no waiter's to "+
				"announce again", err)
		}
	})

	t.Run("a re-fire whose claim lost to a delivery that started the clock", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		host := &heldRead{BoxReader: rig.coordinator.boxes, reading: make(chan struct{}), read: make(chan struct{})}
		rig.coordinator.boxes = host
		gate := &racingGate{gating: make(chan struct{}), pass: make(chan struct{}),
			admitted: make(chan struct{}), proceed: make(chan struct{})}
		rig.coordinator.gate = gate.gate
		// THE GATE REFUSES A COMPLETION while no clock runs: acknowledged,
		// and kept for its condition's signal.
		payload, kept := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, kept); !errors.Is(err, errLeaving) {
			t.Fatalf("OnCompleted = %v, want the gate's refusal", err)
		}
		// THE CONDITION CLEARS, and the re-fire is held at its admission
		// while a delivery of the completion claims the run, its read of the
		// box held in turn; the re-fire's claim then loses to that
		// delivery's.
		rig.coordinator.Readmit(Draining, "swe")
		<-gate.gating
		_, ev := rig.completion("t1")
		handed := make(chan error, 1)
		go func() { handed <- rig.coordinator.OnEvent(delivery(t.Context(), ev), ev) }()
		<-host.reading
		close(gate.pass)
		<-gate.admitted
		// THE DELIVERY'S READ FINDS THE HOST'S STORE SILENT, which starts the
		// clock, and it is handed back, before the re-fire's attempt is done.
		close(host.read)
		if err := <-handed; !errors.Is(err, ErrUnannounced) {
			t.Fatalf("the delivery whose collection started the clock = %v; want it handed back as no "+
				"waiter's to announce again", err)
		}
		if !clocked(rig) {
			t.Fatal("the premise: the delivery's collection did not start the clock on the host")
		}
		close(gate.proceed)
		rig.coordinator.inflight.Wait()
		// ONE ANNOUNCEMENT IN FLIGHT: the delivery handed back, and nothing
		// the re-fire announced beside it.
		if renewed := renewedOn(rig); len(renewed) != 0 {
			t.Fatalf("a re-fire whose claim lost to a delivery that started the clock announced the "+
				"completion %d times; want none, that delivery in flight carrying it", len(renewed))
		}
	})

	t.Run("a re-fire while its refused delivery is in flight", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		gate := gated(rig)
		gate.refuse(true)
		_, ev := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); !errors.Is(err,
			errLeaving) || !errors.Is(err, ErrUnannounced) {
			t.Fatalf("a refusal while the clock on the host runs = %v; want it handed back as no waiter's to "+
				"announce again", err)
		}
		// THE CONDITION CLEARS while that delivery is in flight, and the
		// re-fire's own collection is handed back as the host's store
		// still does not answer.
		gate.refuse(false)
		rig.coordinator.Readmit(Draining, "swe")
		rig.coordinator.inflight.Wait()
		if asked, granted, _ := gate.counts(); asked != 2 || granted != 1 {
			t.Fatalf("the gate was asked %d times and admitted %d; want the re-fire admitted", asked, granted)
		}
		if renewed := renewedOn(rig); len(renewed) != 0 {
			t.Fatalf("a re-fire made while its refused delivery was in flight announced the completion %d "+
				"times; want none, that delivery carrying it", len(renewed))
		}
		if got := rig.get("t1"); got.Status != StatusRunning {
			t.Fatalf("the re-fire left the run %s; want it handed back to running", got.Status)
		}
	})
}

// racingGate is a [TurnGate] that refuses the first resume it is asked about
// on [Draining]; holds the second, saying so on gating, until pass, and
// then admits it with a release that says so on admitted and waits for
// proceed; and admits every other at once: a completion kept for its
// condition, its re-fire, held at its admission and again as its attempt
// returns, and a delivery of it that races that re-fire.
type racingGate struct {
	gating, pass, admitted, proceed chan struct{}

	mu    sync.Mutex
	asked int
}

func (g *racingGate) gate(ctx context.Context, _, _ string, _ ResumeRoute) (context.Context, func(), error) {
	g.mu.Lock()
	g.asked++
	n := g.asked
	g.mu.Unlock()
	switch n {
	case 1:
		return nil, nil, &TurnRefused{Cause: errLeaving,
			Refusal: Refusal{Condition: Draining, Reason: errLeaving.Error()}}
	case 2:
		close(g.gating)
		<-g.pass
		return ctx, func() {
			close(g.admitted)
			<-g.proceed
		}, nil
	}
	return ctx, func() {}, nil
}

// heldRead is the box's host as a case holds it: its first read says so on
// reading and waits for read before the host makes it.
type heldRead struct {
	BoxReader
	reading, read chan struct{}
	once          sync.Once
}

func (h *heldRead) ReadBox(ctx context.Context, run PendingRun, read BoxRead) (Result, bool, error) {
	h.once.Do(func() {
		close(h.reading)
		<-h.read
	})
	return h.BoxReader.ReadBox(ctx, run, read)
}

// renewedOn is every completion the rig's coordinator announced on the seat's
// control topic itself, in order.
func renewedOn(rig *coordRig) []*events.Event {
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	var renewed []*events.Event
	for _, p := range rig.queue.published {
		if _, ok := p.event.Data.(*types.ExecutorFinished); ok && p.topic == sweControl() {
			renewed = append(renewed, p.event)
		}
	}
	return renewed
}

// A RENEWAL THE BROKER DOES NOT TAKE IS SAID BY WHAT IT LEAVES: on a delivery,
// handed back to make it again (sandbox_completion_renew_failed); on a re-fire,
// published again on this node's timer (sandbox_completion_refire_unsettled),
// never said as a hand-back, since a reader told so waits for a delivery that
// never comes.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutation: say the delivery's line wherever a renewal is refused, or say it
// nowhere.
func TestARefusedRenewalIsSaidByWhatItLeaves(t *testing.T) {
	t.Run("a delivery in hand is handed back to make it again", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := captureLogs(t)
			r := newRenewRig(t)
			r.tee.refuseNext(1)
			r.announce(t)
			failed := linesSaying(logs.String(), "sandbox_completion_renew_failed")
			if len(failed) != 1 || !strings.Contains(failed[0], "level=WARN") ||
				!strings.Contains(failed[0], errBrokerRefused.Error()) {
				t.Fatalf("the renewal the broker refused on a delivery was said as %q; want it once, at "+
					"warning, with the broker's refusal", failed)
			}
			if refire := linesSaying(logs.String(), "sandbox_completion_refire_unsettled"); len(refire) != 0 {
				t.Fatalf("a delivery's refused renewal was said as a re-fire's: %q", refire)
			}
		})
	})

	t.Run("a re-fire says it publishes the completion again, and no more", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			logs := captureLogs(t)
			refusedRefire(t)

			if failed := linesSaying(logs.String(), "sandbox_completion_renew_failed"); len(failed) != 0 {
				t.Fatalf("a re-fire's refused renewal said a delivery was handed back to make it again: %q", failed)
			}
			refire := linesSaying(logs.String(), "sandbox_completion_refire_unsettled")
			if len(refire) != 1 || !strings.Contains(refire[0], "level=WARN") ||
				!strings.Contains(refire[0], errBrokerRefused.Error()) ||
				!strings.Contains(refire[0], fmt.Sprintf("retry_in_s=%v", answerRetryDelay(1).Seconds())) {
				t.Fatalf("the re-fire's refused renewal was said as %q; want it once, at warning, with the "+
					"broker's refusal and when it is published again", refire)
			}
		})
	})
}

// refusedRefire has the rig's gate refuse t1's completion while no clock runs
// on its box's host, so its delivery is acknowledged and the completion kept,
// and re-fires it once the condition clears into a collection that host's store
// does not answer, with the broker refusing every publish: the re-fire's own
// collection starts the clock, and its announcement afresh is refused. It
// returns the rig and that host's store. Inside a synctest bubble, whose Wait
// is how it waits for the re-fire.
func refusedRefire(t *testing.T) (*coordRig, *silentStore) {
	t.Helper()
	rig := newCoordRig(t)
	gate := gated(rig)
	gate.refuse(true)
	store, _ := unsettledHost(t, rig)
	payload, kept := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, kept); !errors.Is(err, errLeaving) {
		t.Fatalf("OnCompleted = %v, want the gate's refusal", err)
	}
	gate.refuse(false)
	rig.queue.mu.Lock()
	rig.queue.err = errBrokerRefused
	rig.queue.mu.Unlock()
	rig.coordinator.Readmit(Draining, "swe")
	synctest.Wait()
	if renewed := renewedOn(rig); len(renewed) != 0 {
		t.Fatalf("the broker took %d announcements it was to refuse", len(renewed))
	}
	if got := rig.get("t1"); got.Status != StatusRunning || !clocked(rig) {
		t.Fatalf("the re-fire left the run %s, clocked %v; want it handed back to running, its own "+
			"collection having started the clock", got.Status, clocked(rig))
	}
	return rig, store
}

// brokerTakes has the rig's broker take every publish again.
func brokerTakes(rig *coordRig) {
	rig.queue.mu.Lock()
	defer rig.queue.mu.Unlock()
	rig.queue.err = nil
}

// A RE-FIRE'S ANNOUNCEMENT THE BROKER REFUSED IS PUBLISHED AGAIN on this node's
// timer, on the hand-back's spacing (a second, doubling to thirty), until one
// lands, kept past the seat's release and a lost lease, and dropped once this
// node settles the run or a later re-fire's announcement lands
// ([refirePublish]).
//
// Mutation: leave it unmade; arm it at a fixed spacing; drop it at the seat's
// release or a lost lease, and the next holder is never told; keep it past the
// run's settle, or past a later re-fire's announcement that landed, and the
// completion has two in flight.
func TestARefiresRefusedAnnouncementIsPublishedAgain(t *testing.T) {
	t.Parallel()

	t.Run("until one lands, on the hand-back's spacing", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rig, _ := refusedRefire(t)
			if delays := rig.retries.delays(); !slices.Equal(delays, []time.Duration{answerRetryDelay(1)}) {
				t.Fatalf("the refused announcement was armed again after %v; want once, after %v", delays,
					answerRetryDelay(1))
			}
			// REFUSED AGAIN, and armed again after twice as long.
			rig.fireRetries()
			if delays := rig.retries.delays(); !slices.Equal(delays, []time.Duration{answerRetryDelay(2)}) {
				t.Fatalf("the announcement refused twice was armed again after %v; want once, after %v", delays,
					answerRetryDelay(2))
			}
			brokerTakes(rig)
			rig.fireRetries()
			renewed := renewedOn(rig)
			if len(renewed) != 1 {
				t.Fatalf("the broker took %d announcements of the completion once it answered; want one", len(renewed))
			}
			if got, ok := renewed[0].Data.(*types.ExecutorFinished); !ok || got.TurnID != "t1" {
				t.Fatalf("published %+v; want t1's completion", renewed[0].Data)
			}
			if fired := rig.fireRetries(); fired != 0 {
				t.Fatalf("%d attempts were still armed once the announcement landed", fired)
			}
		})
	})

	t.Run("kept past the seat's release, for its next holder", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rig, _ := refusedRefire(t)
			rig.coordinator.ReleaseSeat("swe")
			announcedOnceItLands(t, rig)
		})
	})

	t.Run("kept past a lease this node lost", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rig, _ := refusedRefire(t)
			rig.coordinator.lease = notLeased
			announcedOnceItLands(t, rig)
		})
	})

	t.Run("dropped when the run settles", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rig, store := refusedRefire(t)
			brokerTakes(rig)
			// THE HOST'S STORE ANSWERS, and another announcement of the job
			// is collected and resumed, and the run ended: it settled.
			store.answer()
			if err := collectAgain(t, rig); err != nil {
				t.Fatalf("the collection once the host's store answered = %v", err)
			}
			if resumed := rig.resumer.calls(); len(resumed) != 1 {
				t.Fatalf("the turn resumed %d times; want once", len(resumed))
			}
			rig.fireRetries()
			if renewed := renewedOn(rig); len(renewed) != 0 {
				t.Fatalf("a settled run still had its completion announced %d times", len(renewed))
			}
		})
	})

	t.Run("dropped when a later re-fire's announcement lands", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			rig, store := refusedRefire(t)
			brokerTakes(rig)
			// THE HOST'S STORE ANSWERS A COLLECTION whose resume then fails:
			// the clock goes with that answer, and the run, handed back for
			// its retry, has not settled, so the refused announcement is still
			// owed.
			store.answer()
			rig.resumer.failWith(errors.New("the model provider did not answer"))
			if err := collectAgain(t, rig); err == nil {
				t.Fatal("the collection whose resume failed was not handed back")
			}
			if delays := rig.retries.delays(); clocked(rig) ||
				!slices.Equal(delays, []time.Duration{answerRetryDelay(1)}) {
				t.Fatalf("the premise: clocked %v with attempts armed after %v; want the clock gone with the "+
					"host's answer and the refused announcement still owed", clocked(rig), delays)
			}
			// THE HOST'S STORE GOES SILENT AGAIN, and a completion the gate
			// refuses meanwhile, no clock running, is kept and re-fired once
			// its condition clears, into a collection that starts the clock
			// again: that re-fire's own announcement afresh lands.
			store.silence("Get")
			rig.resumer.failWith(nil)
			gate := gated(rig)
			gate.refuse(true)
			payload, kept := rig.completion("t1")
			if err := rig.coordinator.OnCompleted(t.Context(), payload, kept); !errors.Is(err, errLeaving) {
				t.Fatalf("OnCompleted = %v, want the gate's refusal", err)
			}
			gate.refuse(false)
			rig.coordinator.Readmit(Draining, "swe")
			synctest.Wait()
			if renewed := renewedOn(rig); len(renewed) != 1 || !clocked(rig) {
				t.Fatalf("the later re-fire announced the completion %d times, clocked %v; want it announced "+
					"afresh once, its own collection having started the clock", len(renewed), clocked(rig))
			}
			// ONE ANNOUNCEMENT IN FLIGHT: the one still owed is that one now.
			if fired := rig.fireRetries(); fired != 0 {
				t.Fatalf("%d attempts were still armed once a later re-fire's announcement landed", fired)
			}
			if renewed := renewedOn(rig); len(renewed) != 1 {
				t.Fatalf("the completion was announced %d times; want once, the later re-fire's announcement "+
					"standing for the one still owed", len(renewed))
			}
		})
	})
}

// announcedOnceItLands holds rig's refused re-fire announcement, the seat now
// not this node's, to one still owed: refused once more, armed again, and
// announced once the broker takes it, with nothing armed after that.
func announcedOnceItLands(t *testing.T, rig *coordRig) {
	t.Helper()
	if fired := rig.fireRetries(); fired != 1 {
		t.Fatalf("%d attempts were armed for the refused announcement once the seat was not this node's; "+
			"want it still owed", fired)
	}
	if renewed := renewedOn(rig); len(renewed) != 0 {
		t.Fatalf("the broker took %d announcements it was to refuse", len(renewed))
	}
	brokerTakes(rig)
	rig.fireRetries()
	renewed := renewedOn(rig)
	if len(renewed) != 1 {
		t.Fatalf("the completion was announced %d times once the broker took it; want once, to the seat's "+
			"control topic, which whoever holds the seat reads", len(renewed))
	}
	if got, ok := renewed[0].Data.(*types.ExecutorFinished); !ok || got.TurnID != "t1" {
		t.Fatalf("published %+v; want t1's completion", renewed[0].Data)
	}
	if fired := rig.fireRetries(); fired != 0 {
		t.Fatalf("%d attempts were still armed once the announcement landed", fired)
	}
}

// linesSaying is every line of logs whose message is msg.
func linesSaying(logs, msg string) []string {
	var said []string
	for line := range strings.SplitSeq(logs, "\n") {
		if strings.Contains(line, "msg="+msg+" ") {
			said = append(said, line)
		}
	}
	return said
}

// restart has the seat's control topic handled from here on by a fresh
// coordinator over the rig's store, broker and box host ([coordRig.restart]),
// made at a delivery ([renewRig.atDelivery]), on the goroutine that hands the
// coordinator that delivery next.
func (r *renewRig) restart(t *testing.T, node string, fence Fence, gate TurnGate) {
	t.Helper()
	r.coordRig.restart(t, r.tee, node, fence, gate)
	r.coordinator.boxes = r.host
}

// restart replaces the rig's coordinator with a fresh one over the same store,
// box host and publisher (q), as node, holding the seat under fence and
// admitting through gate (nil for none): this node after a restart, or the
// seat's next holder, which keeps nothing of what the coordinator before it
// held in memory, its clock on the box's host first. The one before it no
// longer holds the seat.
func (r *coordRig) restart(t *testing.T, q Publisher, node string, fence Fence, gate TurnGate) {
	t.Helper()
	old := r.coordinator
	old.lease = notLeased
	fresh, err := NewCoordinator(CoordinatorOptions{
		FailureKind: func(cause error) string { return "kind of: " + cause.Error() },
		Pushes:      r.pushes, Audience: r.audience, Answers: rigAnswers(),
		Queue: q, Pending: old.pending, Manager: r.manager,
		Resume: r.resumer, Account: r.accountant,
		Now: old.now, After: r.retries.after, Lease: leased(fence), Gate: gate, Node: node,
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(fresh.Stop)
	fresh.boxes = old.boxes
	fresh.endWait, fresh.endPoll = old.endWait, old.endPoll
	r.coordinator = fresh
}

// A JOB NO WAITER ANNOUNCES IS RENEWED BY WHAT ITS ROW SAYS, NOT BY ONE NODE'S
// MEMORY: the clock does not survive a restart or a seat move, so a job
// recorded lost with its host ([LossLaunchNodeStoreSilent]) keeps its
// completion renewed until its round is carried on, and so does one whose
// record says the host's store was silent
// ([LaunchRecord.HostStoreSilentSince]).
//
// Mutation: key the renewal on this node's clock alone, and a carry failing
// after a restart dead-letters and a refusal after a restart or on the next
// holder is acknowledged; write no record of the silence on the release, and
// the refusals are acknowledged the same way.
func TestAJobNoWaiterAnnouncesIsRenewedByItsRow(t *testing.T) {
	t.Parallel()
	// past is how many refusals or failed carries a case goes on for: past one
	// message's whole budget, so one acknowledged, or NAK'd and nothing more,
	// leaves the job unannounced or dead-lettered first.
	past := 3 * (renewBudget + 1)

	t.Run("a carry of a job lost with its host that fails after a restart", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			// THE LOSS IS RECORDED at the bound, and the carry that follows it
			// waits on a watch of the run's log this node's store does not
			// answer, for past a budget; the node restarts at the first
			// delivery after the loss.
			mine := &unansweredWatches{PendingStore: r.coordinator.pending, silent: past}
			r.coordinator.pending = mine
			restarted := false
			r.atDelivery(func(int) {
				if !restarted && r.get("t1").Lost != nil {
					restarted = true
					r.restart(t, "node-b", rigLease, nil)
				}
			})
			r.announce(t)
			if !restarted {
				t.Fatal("the premise: the job was never recorded lost, so nothing restarted")
			}
			r.lostWithItsHostOnTheBroker(t)
			if watched := mine.watched(); watched <= mine.silent {
				t.Fatalf("the premise: the carry watched the run's log %d times; want it handed back past the %d "+
					"watches its store did not answer", watched, mine.silent)
			}
		})
	})

	t.Run("a job lost with its host, whatever its record says of that host's store", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		_, run := unsettledHost(t, rig)
		// THE ROW SAYS THE JOB IS LOST WITH NODE-A because its store did not
		// answer it, and nothing more: this node keeps no clock, and the job's
		// record holds no word of that store. The carry's watch of the run's
		// log goes unanswered by this node's own store.
		if marked, err := rig.pending.MarkLost(t.Context(), "t1", run.LaunchID, JobLoss{
			Cause: LossLaunchNodeStoreSilent, Node: "node-a", At: rig.now,
		}); err != nil || !marked {
			t.Fatalf("the premise: recording the loss = %v, %v", marked, err)
		}
		if got := rig.get("t1"); !got.Launch.HostStoreSilentSince.IsZero() || clocked(rig) {
			t.Fatalf("the premise: the record says the host's store is silent since %v, clocked %v; want neither",
				got.Launch.HostStoreSilentSince, clocked(rig))
		}
		rig.coordinator.pending = &silentStore{PendingStore: rig.coordinator.pending, silent: []string{"WatchLog"}}
		_, ev := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); !errors.Is(err, ErrUnannounced) {
			t.Fatalf("the carry of a job lost with its host that failed = %v; want its delivery handed back as no "+
				"waiter's to announce again", err)
		}
		if got := rig.get("t1"); got.Status != StatusRunning || got.Lost == nil {
			t.Fatalf("the failed carry left the row %s, lost %+v; want it running, still lost", got.Status, got.Lost)
		}
	})

	t.Run("a re-fire while a delivery its row handed back is in flight", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		// THE NODE RESTARTS: the clock on the host is gone with it, and the
		// job's record still says the host's store is silent. Its gate
		// refuses the next delivery, which its row hands back, and the
		// condition then clears into a collection that starts the clock
		// again.
		gate := &turnGate{}
		gate.refuse(true)
		rig.restart(t, rig.queue, "node-b", rigLease, gate.admit)
		_, ev := rig.completion("t1")
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); !errors.Is(err, errLeaving) ||
			!errors.Is(err, ErrUnannounced) {
			t.Fatalf("a refusal after the restart = %v; want it handed back as no waiter's to announce again", err)
		}
		gate.refuse(false)
		rig.coordinator.Readmit(Draining, "swe")
		rig.coordinator.inflight.Wait()
		if !clocked(rig) {
			t.Fatal("the premise: the re-fire's collection did not start the clock on the host")
		}
		if renewed := renewedOn(rig); len(renewed) != 0 {
			t.Fatalf("a re-fire made while the delivery its row handed back was in flight announced the "+
				"completion %d times; want none, that delivery carrying it", len(renewed))
		}
	})

	for _, tc := range []struct {
		name  string
		node  string
		fence Fence
	}{
		{name: "a refusal after a restart, before its first collection", node: "node-b", fence: rigLease},
		{name: "a refusal on the seat's next holder, before its first collection", node: "node-c",
			fence: Fence{Owner: "node-c", Epoch: rigLease.Epoch + 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				r := newRenewRig(t)
				// THE FIRST COLLECTION finds the host's store silent, and
				// hands its claim back; the node restarts, or the seat moves,
				// at the next delivery, and the gate there refuses on a
				// condition nothing signals, for past a budget, before it
				// clears and the host's store answers.
				r.atDelivery(func(n int) {
					if n == 2 {
						gate := &refusingGate{refuse: past, cleared: r.store.answer}
						r.restart(t, tc.node, tc.fence, gate.gate)
					}
				})
				r.announce(t)
				r.resumedOnTheBroker(t)
			})
		})
	}
}

// waiterAnnounces publishes t1's completion as the host's waiter announces it
// on its schedule once its store answers again, and returns its id: a fresh
// announcement, made from a read of the run's row its store answered a second
// after the delivery in hand on the bubble's clock, so after any clock a
// collection started at that delivery. Called only inside a bubble, from a
// case's delivery hook.
//
//fixedwait:bubble
func (r *renewRig) waiterAnnounces(t *testing.T) uuid.UUID {
	t.Helper()
	time.Sleep(time.Second)
	_, ev := r.completion("t1")
	askedRow(ev, time.Now())
	if err := r.broker.Publish(t.Context(), sweControl(), ev); err != nil {
		t.Errorf("the waiter's announcement: %v", err)
	}
	return ev.ID
}

// A WAITER'S ANNOUNCEMENT MADE SINCE THE CLOCK STARTED ENDS IT, AND A RENEWAL
// NEVER DOES ([rowAskedField], [announcedAfter]): it ends this node's clock,
// and the job's record at the release a claim of it makes. A renewal says no
// read.
//
// Mutation: stamp a renewal with a read of its own, and the chain is
// acknowledged at its first refusal; never end the clock at the waiter's
// announcement, and the chains in flight grow by one each; leave the record
// standing at that release, and a restarted node renews a job whose host's
// store answers.
func TestAWaiterAnnouncementMadeSinceTheClockEndsIt(t *testing.T) {
	t.Parallel()

	t.Run("one chain in flight across the waiter's announcements while a refusal lasts", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			// THE FIRST RESUME IS ADMITTED, and its collection starts the
			// clock on the host; the host's store answers again at the
			// second delivery, and its waiter announces the job then and on
			// its schedule after, while the gate refuses past several
			// messages' budgets on a condition nothing signals.
			gate := &refusingGate{admit: 1, refuse: 5 * (renewBudget + 1), cleared: func() {}}
			r.coordinator.gate = gate.gate
			var waiter []uuid.UUID
			r.atDelivery(func(n int) {
				if n == 2 {
					r.store.answer()
				}
				if n == 2 || n == 8 || n == 14 {
					waiter = append(waiter, r.waiterAnnounces(t))
				}
			})
			r.announce(t)
			// RESUMED FIRST: a chain acknowledged with nothing left to carry
			// the completion ends the deliveries before the waiter's later
			// announcements, which is this case's failure and not its premise's.
			resumed := r.resumer.calls()
			if len(resumed) != 1 || resumed[0].Lost != nil || resumed[0].Ended.Text != wholeAccount().Text {
				t.Fatalf("the turn resumed %d times (%+v); want it resumed once on its box's account",
					len(resumed), resumed)
			}
			if len(waiter) != 3 {
				t.Fatalf("the premise: the waiter announced the job %d times; want 3", len(waiter))
			}
			if dead := r.broker.DeadLetters(sweControl(), topics.AgentControlGroup(uuid.MustParse(sweID))); len(dead) != 0 {
				t.Fatalf("%d of the job's announcements were dead-lettered", len(dead))
			}
			seen := r.deliveries()
			for i, d := range seen {
				if d.waiting > 1 {
					t.Fatalf("delivery %d of %d was handed over with %d more announcements waiting; want the "+
						"one chain in flight, and at most the waiter's announcement beside it", i+1, len(seen),
						d.waiting)
				}
			}
			_, times := announcements(seen)
			for _, id := range waiter {
				if times[id] != 1 {
					t.Fatalf("the waiter's announcement %s was delivered %d times; want it acknowledged at its "+
						"first refusal", id, times[id])
				}
			}
		})
	})

	t.Run("the release a claim of the waiter's announcement makes clears the job's record", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		since := rig.get("t1").Launch.HostStoreSilentSince
		if since.IsZero() {
			t.Fatal("the premise: the hand-back recorded no silence of the host's store")
		}
		// THE NODE RESTARTS, keeping no clock, and the host's waiter
		// announces the job from a read its store answered a second after
		// the silence was recorded; the host itself then does not answer
		// the claim's read, which says nothing of its store.
		rig.restart(t, rig.queue, "node-b", rigLease, nil)
		rig.coordinator.boxes = &boxAsks{err: fmt.Errorf("%w: node-a, within a minute", ErrBoxHostSilent)}
		_, ev := rig.completion("t1")
		askedRow(ev, since.Add(time.Second))
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); err != nil {
			t.Fatalf("the waiter's announcement whose read the host did not answer = %v; want it acknowledged, "+
				"its waiter announcing the job again", err)
		}
		if got := rig.get("t1"); got.Status != StatusRunning || !got.Launch.HostStoreSilentSince.IsZero() {
			t.Fatalf("the release left the run %s, its host's store silent since %v; want it running, the "+
				"record cleared", got.Status, got.Launch.HostStoreSilentSince)
		}
	})
}

// AN ANNOUNCEMENT MADE FROM A READ ASKED BEFORE THE CLOCK STARTED ENDS NOTHING,
// HOWEVER LATE IT WAS MADE: the waiter's poll after its read can take
// [pollBound], so a silence found meanwhile starts a clock before the stamp.
// The clock stands, a refusal of it is handed back, and a claim learning
// nothing of the host's store leaves the record.
//
// Mutation: compare the announcement's own stamp with the clock's start, and a
// refusal of it is acknowledged with the store still silent, and a claim the
// host does not answer clears the record.
func TestAnAnnouncementFromAReadBeforeTheClockEndsNothing(t *testing.T) {
	t.Parallel()
	// readBefore makes ev the host's waiter's announcement made a minute after
	// since, from a read of the run's row its store answered a second before.
	readBefore := func(ev *events.Event, since time.Time) {
		ev.Timestamp = since.Add(time.Minute)
		askedRow(ev, since.Add(-time.Second))
	}
	// atTheLimit refuses every resume on a condition that clears by waiting,
	// whose refusal is acknowledged wherever a waiter stands behind it.
	atTheLimit := func(context.Context, string, string, ResumeRoute) (context.Context, func(), error) {
		return nil, nil, &TurnRefused{Refusal: Refusal{Condition: conditionLimit,
			Reason: "every model of the seat's chain is at its usage limit"}}
	}

	t.Run("a refusal of it while the clock runs", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		since := rig.get("t1").Launch.HostStoreSilentSince
		if since.IsZero() || !clocked(rig) {
			t.Fatalf("the premise: clocked %v, the record silent since %v; want both", clocked(rig), since)
		}
		rig.coordinator.gate = atTheLimit
		_, ev := rig.completion("t1")
		readBefore(ev, since)
		err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev)
		if got := controlResult(err); !errors.Is(err, ErrUnannounced) || got.Outcome != queue.OutcomeNak {
			t.Errorf("a refusal of the announcement made from a read before the clock = %v, left %s; want it "+
				"handed back as no waiter's to announce again", err, got.Outcome)
		}
		if !clocked(rig) {
			t.Error("the announcement made from a read before the clock started ended it")
		}
	})

	t.Run("a claim of it the host does not answer, after a restart", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		since := rig.get("t1").Launch.HostStoreSilentSince
		if since.IsZero() {
			t.Fatal("the premise: the hand-back recorded no silence of the host's store")
		}
		rig.restart(t, rig.queue, "node-b", rigLease, nil)
		rig.coordinator.boxes = &boxAsks{err: fmt.Errorf("%w: node-a, within a minute", ErrBoxHostSilent)}
		_, ev := rig.completion("t1")
		readBefore(ev, since)
		if err := rig.coordinator.OnEvent(delivery(t.Context(), ev), ev); !errors.Is(err, ErrUnannounced) {
			t.Errorf("the announcement made from a read before the clock, its host not answering = %v; want "+
				"it handed back as no waiter's to announce again", err)
		}
		if got := rig.get("t1"); got.Status != StatusRunning || !got.Launch.HostStoreSilentSince.Equal(since) {
			t.Errorf("the release left the run %s, its host's store silent since %v; want it running, still "+
				"silent since %v", got.Status, got.Launch.HostStoreSilentSince, since)
		}
	})
}

// countedGets is a run store that counts its reads of a run's row.
type countedGets struct {
	PendingStore
	gets atomic.Int64
}

func (s *countedGets) Get(ctx context.Context, turnID string) (PendingRun, bool, error) {
	s.gets.Add(1)
	return s.PendingStore.Get(ctx, turnID)
}

// A SUCCESSFUL DELIVERY READS NO ROW FOR WHAT THE BOX'S HOST'S STORE SAYS. A
// node that keeps no clock on that store reads the run's row before it lets a
// delivery go, to learn whether the job's record says no waiter announces it
// again; but a delivery whose own collection read the box needs no row for
// that, since the box's host read the row through its store to license the
// read. So a delivery that collects and resumes its job reads the store as
// often handed to the coordinator's event handler as handed to the claim and
// resume alone, whether the box is on this node's host or another's.
//
// Mutation: read the row after every delivery no clock covers, and each
// successful delivery reads the store once more than its claim and resume do.
func TestASuccessfulDeliveryReadsNoRowForItsHostsStore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		remote bool
	}{
		{name: "a box on this node's host"},
		{name: "a box on another node's host", remote: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// gets is how often a successful delivery of a finished job reads
			// the store, handed to OnEvent where viaEvent is set and to
			// OnCompleted otherwise.
			gets := func(viaEvent bool) int64 {
				rig := newCoordRig(t)
				if tc.remote {
					rig.onNode(t, "node-b", hostAnswers{host: rig.hostOn(t, "node-a", rig.pending)}, nil)
					rig.launchedBy("t1", "node-a")
				} else {
					rig.launch("t1")
				}
				rig.runner.Finish(wholeAccount())
				counted := &countedGets{PendingStore: rig.coordinator.pending}
				rig.coordinator.pending = counted
				payload, ev := rig.completion("t1")
				var err error
				if viaEvent {
					err = rig.coordinator.OnEvent(delivery(t.Context(), ev), ev)
				} else {
					err = rig.coordinator.OnCompleted(delivery(t.Context(), ev), payload, ev)
				}
				if err != nil {
					t.Fatalf("the delivery = %v; want the job collected and its turn resumed", err)
				}
				if resumed := rig.resumer.calls(); len(resumed) != 1 || !resumed[0].Success {
					t.Fatalf("the turn resumed %d times (%+v); want it resumed once, collected", len(resumed),
						resumed)
				}
				return counted.gets.Load()
			}
			if claimed, delivered := gets(false), gets(true); delivered != claimed {
				t.Fatalf("a successful delivery read the run's row %d times, and its claim and resume alone %d; "+
					"want no read beyond them", delivered, claimed)
			}
		})
	}
}
