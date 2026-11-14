package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
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
// message's budget runs out well inside the box timeout.
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
	// THE ENGINE'S CONTROL HANDLER, for what these cases meet: an error that
	// names no condition is NAK'd and nil is acknowledged (internal/engine's
	// controlOutcome).
	if err := broker.Subscribe(t.Context(), control, group, func(ctx context.Context, ev *events.Event) queue.Result {
		left, _ := queue.DeliveriesLeftFor(ctx, ev.ID)
		r.mu.Lock()
		r.seen = append(r.seen, delivered{id: ev.ID, left: left, waiting: len(broker.Backlog(control, group))})
		n, before := len(r.seen), r.before
		r.mu.Unlock()
		if before != nil {
			before(n)
		}
		if err := rig.coordinator.OnEvent(ctx, ev); err != nil {
			return queue.Nak(err)
		}
		return queue.Ack()
	}); err != nil {
		t.Fatalf("subscribing to the seat's control topic: %v", err)
	}
	return r
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

// A COMPLETION WHOSE BOX'S HOST ALONE FINDS THE STORE SILENT IS RENEWED BEFORE
// ITS DELIVERY RUNS OUT. Only that host can read the box, and its own waiter,
// the only one that announces the job again, reads the run's row through the
// silent store, so the broker's redelivery is all that asks again. Its budget
// is shorter than the box timeout, so NAK'd and nothing more, the completion was
// dead-lettered before the bound that records the job lost with its host, and
// the seat was held behind a run nothing drove again. So node-b announces the
// completion afresh before the delivery in hand is its last, publishes that
// before acknowledging the delivery, and the wait goes on until the host's
// store answers or the bound gives the job up, and past the bound until the
// job's round is carried on: a recorded loss settles nothing while its carry
// can still fail, and its host's waiter announces nothing either. A re-fire,
// which has no delivery in hand, renews at once; and a hand-back this node's
// own store made is NAK'd and nothing more, since the waiter announces that
// job again.
//
// The memory twin hands a NAK'd message back at once, so time moves on the
// host's own answers, a minute each, on the bubble's clock.
//
// Mutation: renew nothing, and the job is never recorded lost, a dead letter
// left; NAK the delivery a renewal replaced, and the topic holds a growing
// pile of announcements; renew on every hand-back, or only on the last
// delivery, and the deliveries per announcement are not the reserve's; renew
// under the delivery's own id, and no fresh announcement is delivered; ack a
// delivery whose renewal the broker refused, and the job is left with none;
// leave a re-fire unrenewed, or a loss this node's store did not take, and
// nothing carries the completion on; drop the clock at the recorded loss, and
// a carry that fails after it runs a budget out with nothing renewing it;
// renew a hand-back this node's own store made, and its delivery is never
// spent.
func TestACompletionWhoseHostAloneFindsTheStoreSilentIsRenewed(t *testing.T) {
	t.Parallel()
	if (renewBudget+1)*readWait >= DefaultBoxTimeout {
		t.Fatalf("the premise: one message's %d deliveries span %v, not inside the %v box timeout",
			renewBudget+1, (renewBudget+1)*readWait, DefaultBoxTimeout)
	}

	t.Run("the job is lost with its host past a budget shorter than the box timeout", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			r.announce(t)
			if waited := time.Since(r.start); waited < DefaultBoxTimeout {
				t.Fatalf("the broker had nothing left to deliver %v in, inside the %v box timeout",
					waited, DefaultBoxTimeout)
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
			if waited := time.Since(r.start); waited < DefaultBoxTimeout {
				t.Fatalf("the broker had nothing left to deliver %v in, inside the %v box timeout",
					waited, DefaultBoxTimeout)
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
			answerAt := DefaultBoxTimeout - 3*time.Minute
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
			recordsAt := DefaultBoxTimeout + (renewBudget+2)*readWait
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
		rig.coordinator.Readmit(conditionDrain, "swe")
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
// RUNS, WHATEVER ITS KIND. The clock is this node's word that the host's
// waiter, the one thing that would announce the job again, reads the run's row
// through a coordination store that does not answer it: the broker's
// redelivery is then all that asks again, and its budget is shorter than the
// box timeout. A claim this node's own store does not answer, a claim refused
// while a release this node owes is out, and a host that does not answer the
// read at all are hand-backs within that same wait, and each one's delivery is
// NAK'd and renewed before its last, never acknowledged: the completion
// outlives one message's budget, and the turn resumes once the host's store
// answers, each renewal leaving one announcement in flight. A re-fire, which has
// no delivery in hand, is renewed at once whatever it hands back while the
// clock runs.
//
// Mutation: renew only the host's own answer that its store did not answer,
// and a claim the store did not answer, a claim refused for a release owed or
// a host that did not answer runs one message's budget out to its dead letter;
// acknowledge a host that did not answer while the clock runs, and nothing
// announces the job again; renew a re-fire only for the host's own answer, and
// a re-fire the host did not answer is left with nothing to carry it.
func TestEveryHandBackIsRenewedWhileTheClockOnItsHostRuns(t *testing.T) {
	t.Parallel()
	// past is how many deliveries a hand-back goes on for: past one message's
	// whole budget, so one NAK'd and nothing more is dead-lettered first.
	past := 2 * (renewBudget + 1)

	resumedOnTheBroker := func(t *testing.T, r *renewRig) {
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

	t.Run("a claim this node's store does not answer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			// THE FIRST CLAIM LANDS, and its collection starts the clock on
			// the host; the claims after it go unanswered past a budget.
			r.coordinator.pending = &unansweredClaims{PendingStore: r.coordinator.pending, before: 1,
				silent: past, answered: r.store.answer}
			r.announce(t)
			resumedOnTheBroker(t, r)
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
			resumedOnTheBroker(t, r)
		})
	})

	t.Run("a host that does not answer the read", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newRenewRig(t)
			// THE FIRST READ IS THE HOST'S ANSWER THAT ITS STORE DID NOT
			// ANSWER, which starts the clock; the host then answers nothing
			// for past one message's budget, a minute a read, inside the box
			// timeout, and comes back with its store answering.
			r.host.BoxReader = &hostReads{BoxReader: r.host.BoxReader, from: 2, silent: renewBudget + 2,
				answered: r.store.answer}
			r.announce(t)
			resumedOnTheBroker(t, r)
		})
	})

	t.Run("a re-fire a host does not answer", func(t *testing.T) {
		t.Parallel()
		rig := newCoordRig(t)
		unsettledHost(t, rig)
		handedBackUnsettled(t, rig, "the host's store did not answer")
		// KEPT FOR A CONDITION, the delivery acknowledged, and re-fired once
		// it clears, into a host that does not answer: no delivery is in
		// hand, and the clock on the host's store still runs.
		gate := gated(rig)
		gate.refuse(true)
		payload, kept := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, kept); !errors.Is(err, errLeaving) {
			t.Fatalf("OnCompleted = %v, want the gate's refusal", err)
		}
		rig.coordinator.boxes = &boxAsks{err: fmt.Errorf("%w: node-a, within a minute", ErrBoxHostSilent)}
		gate.refuse(false)
		rig.coordinator.Readmit(conditionDrain, "swe")
		rig.coordinator.inflight.Wait()
		if renewed := renewedOn(rig); len(renewed) != 1 {
			t.Fatalf("the re-fire a host did not answer, the clock on its store running, announced the completion "+
				"%d times; want it announced afresh once", len(renewed))
		}
		if got := rig.get("t1"); got.Status != StatusRunning {
			t.Fatalf("the re-fire left the run %s; want it handed back to running", got.Status)
		}
	})
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

// A RENEWAL THE BROKER DOES NOT TAKE IS SAID BY WHAT IT LEAVES. On a delivery,
// the delivery is handed back to make the renewal again when it comes back, and
// its line says so (sandbox_completion_renew_failed). A re-fire has no delivery
// to come back on: its own line says the completion waits for the host's store
// (sandbox_completion_refire_unsettled), and nothing says a delivery was handed
// back, since a reader told so waits for a retry that never comes.
//
// Not parallel: it captures the process-wide logger ([captureLogs]).
//
// Mutation: say the delivery's line wherever a renewal is refused, and a
// re-fire's refusal says it handed back a delivery it never had; say it
// nowhere, and a renewal refused on a delivery is said by nothing.
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

	t.Run("a re-fire says it waits for the host's store, and no more", func(t *testing.T) {
		logs := captureLogs(t)
		rig := newCoordRig(t)
		gate := gated(rig)
		gate.refuse(true)
		unsettledHost(t, rig)
		payload, kept := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, kept); !errors.Is(err, errLeaving) {
			t.Fatalf("OnCompleted = %v, want the gate's refusal", err)
		}
		gate.refuse(false)
		rig.queue.mu.Lock()
		rig.queue.err = errBrokerRefused
		rig.queue.mu.Unlock()
		rig.coordinator.Readmit(conditionDrain, "swe")
		rig.coordinator.inflight.Wait()

		if failed := linesSaying(logs.String(), "sandbox_completion_renew_failed"); len(failed) != 0 {
			t.Fatalf("a re-fire's refused renewal said a delivery was handed back to make it again: %q", failed)
		}
		refire := linesSaying(logs.String(), "sandbox_completion_refire_unsettled")
		if len(refire) != 1 || !strings.Contains(refire[0], "level=WARN") ||
			!strings.Contains(refire[0], errBrokerRefused.Error()) {
			t.Fatalf("the re-fire's refused renewal was said as %q; want it once, at warning, with the "+
				"broker's refusal", refire)
		}
	})
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
