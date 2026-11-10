package queuetest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
)

// runBroadcast covers the ephemeral fan-out verbs, Broadcast and Listen: the
// pair presence travels on, and like the scatter pair they must leave nothing
// behind.
//
// The two halves a backend could get wrong without any other case noticing are
// the ones asked here: a message nobody was listening for must be gone for a
// listener that arrives a moment later (a backend over its durable path would
// replay it), and every listener must hear every message (a backend over a
// queue group would hand each to one).
func (s *suite) runBroadcast(t *testing.T) {
	ctx := t.Context()

	t.Run("every_listener_hears_every_message", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".room"
		a, b := newHeard(), newHeard()
		listen(ctx, t, q, subject, a.record)
		listen(ctx, t, q, subject, b.record)
		broadcast(ctx, t, q, subject, []byte("one"))
		broadcast(ctx, t, q, subject, []byte("two"))
		a.waitFor(t, "first listener", subject+"=one", subject+"=two")
		b.waitFor(t, "second listener", subject+"=one", subject+"=two")
	})

	// Mutation: let a listener's matching run wider than its pattern, and
	// the one-token pattern hears a stray subject ahead of the fence.
	t.Run("a_pattern_hears_the_subjects_it_matches_and_no_other", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		base := ns(t)
		one, all := newHeard(), newHeard()
		listen(ctx, t, q, base+".*.presence", one.record)
		listen(ctx, t, q, base+".>", all.record)
		broadcast(ctx, t, q, base+".a.presence", []byte("x"))
		broadcast(ctx, t, q, base+".a.b.presence", []byte("y"))
		broadcast(ctx, t, q, base+".elsewhere", []byte("z"))
		// FENCED BY A LATER BROADCAST THE ONE-TOKEN PATTERN DOES MATCH: one
		// process's broadcasts reach a listener in the order sent, so a
		// subject the pattern should not have matched is heard before the
		// fence or never, however late its delivery runs.
		fence := base + ".fence.presence"
		broadcast(ctx, t, q, fence, []byte("f"))
		one.waitFor(t, "the one-token pattern", fence+"=f")
		all.waitFor(t, "the trailing pattern", base+".a.presence=x",
			base+".a.b.presence=y", base+".elsewhere=z", fence+"=f")
		if got := one.all(); !slices.Equal(got, []string{base + ".a.presence=x", fence + "=f"}) {
			t.Fatalf("a one-token pattern heard %v up to a later broadcast it matches, want "+
				"only the subject it matches before it: two backends with their own matchers "+
				"disagree about who is in a room", got)
		}
		// AND THE ORDER THE FENCE RIDES, asserted where it is ridden: a
		// backend that reordered one sender's broadcasts could land a stray
		// match after the fence, and the check above would pass on it.
		want := []string{base + ".a.presence=x", base + ".a.b.presence=y",
			base + ".elsewhere=z", fence + "=f"}
		if got := all.all(); !slices.Equal(got, want) {
			t.Fatalf("a listener heard one sender's broadcasts as %v, want the order they "+
				"were sent in, %v: a later broadcast is no fence for one that can overtake it",
				got, want)
		}
	})

	// Mutation: keep a broadcast nobody heard and hand it to the next
	// listener that registers, and the late listener hears it ahead of
	// "now".
	t.Run("a_message_nobody_listened_for_is_never_heard", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".room"
		// BROADCAST BEFORE ANYBODY LISTENS. On a durable transport this
		// message would be waiting; here it must already be gone.
		broadcast(ctx, t, q, subject, []byte("early"))
		late := newHeard()
		listen(ctx, t, q, subject, late.record)
		// "now" IS THE FENCE. It is sent after "early" by the same process,
		// so a listener that has heard it has heard "early" already or never
		// will: a broadcast kept for a later listener — a durable path,
		// which is the failure this case names — is handed over in the order
		// it was sent, ahead of what followed it.
		broadcast(ctx, t, q, subject, []byte("now"))
		late.waitFor(t, "the late listener", subject+"=now")
		if got := late.all(); !slices.Equal(got, []string{subject + "=now"}) {
			t.Fatalf("a listener that arrived after a broadcast heard %v; a broadcast "+
				"retained for a later listener is a heartbeat replayed after its moment", got)
		}
	})

	// Mutation: leave the withdrawn registration standing, and the barrier
	// carries the broadcast to it before the case looks.
	t.Run("a_withdrawn_listener_hears_nothing_more", func(t *testing.T) {
		t.Parallel()
		barrier := s.needListenBarrier(t)
		q := s.start(ctx, t)
		subject := ns(t) + ".room"
		stays, goes := newHeard(), newHeard()
		listen(ctx, t, q, subject, stays.record)
		stop, err := q.Listen(ctx, subject, goes.record)
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		if err = stop(ctx); err != nil {
			t.Fatalf("stop listening: %v", err)
		}
		broadcast(ctx, t, q, subject, []byte("after"))
		stays.waitFor(t, "the remaining listener", subject+"=after")
		// READ BEHIND A BARRIER, NOT A FENCE: a withdrawn listener hears a
		// later broadcast no more than this one, so its silence after one
		// is the same on a withdrawal that left it standing and whose
		// handler had not run yet. A barrier through every listener this
		// client still holds is what such a registration would have
		// carried the broadcast past.
		barrier(ctx, q)
		if got := goes.all(); len(got) != 0 {
			t.Fatalf("a listener that withdrew went on hearing %v", got)
		}
	})

	// A WITHDRAWAL WAITS FOR THE HANDLER RUNNING, bounded by the context it
	// is given, and the bound is what makes the wait readable. With a
	// handler held, a withdrawal under a bound that has already ended either
	// waited for it — and gave up at once, answering that bound's own error
	// — or did not, and answered success, or an error that is not the
	// bound's. Its answer is the signal, read with the handler still held;
	// no pause stands in for one. And a bound that outlasts the handler is
	// answered with success once the handler has returned.
	//
	// Mutation: return from the withdrawal without waiting on the gate, or
	// wait on a context other than its bound, and the first half fails.
	t.Run("a_withdrawal_waits_for_the_handler_running", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		held := holdListener(ctx, t, q, ns(t)+".room")
		ended, cancel := context.WithTimeout(ctx, 0)
		defer cancel()
		err := withdraw(ended, t, held, "a withdrawal under a bound that had ended", nil)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("a withdrawal under a bound that had ended, with a handler still "+
				"running, answered %v, want the bound's own %v: a withdrawal that returns "+
				"without waiting says the handler is done, and a caller that tears down "+
				"what the handler reads straight after withdrawing tears it down under "+
				"that handler", err, context.DeadlineExceeded)
		}
		if held.finished.Load() {
			t.Fatal("the held handler had finished before the case released it: the " +
				"withdrawal above was answered with no handler running")
		}
		held.release()

		waited := holdListener(ctx, t, q, ns(t)+".room")
		if err := withdraw(ctx, t, waited, "a withdrawal whose bound outlasted the handler",
			waited.release); err != nil {
			t.Fatalf("a withdrawal whose bound outlasted the handler answered %v once the "+
				"handler had returned, want success", err)
		}
		if !waited.finished.Load() {
			t.Fatal("the withdrawal returned with a handler still running; a caller " +
				"that tears down what the handler reads straight after withdrawing " +
				"would tear it down under that handler")
		}
	})

	// ONE MESSAGE AT A TIME, of which this case decides the half every
	// backend can show. That no delivery runs beside another is an absence
	// a case here could read only behind a quiet window — this one slept a
	// millisecond in each handler for an overlap to show, and a second
	// delivery the runner made late passed it with the bug present — so
	// each backend's half is decided where it can be exact: the twin's in a
	// bubble (memory's TestAHandlerNeverRunsBesideItself), JetStream's by
	// where its code sits (jetstream's
	// TestAHandlerRunsInlineOnTheGoroutineThatTookItsDelivery). What is
	// left here is a positive: a listener handed a burst one message at a
	// time hears every message of it once, in the order it was sent. It is
	// read once the last is heard, a wait for deliveries that must happen,
	// so it needs waitFor's liveness bound and no other.
	//
	// Mutation: lose one broadcast of the burst for its listener, or hand it
	// over twice, and the listener's record is not the burst.
	t.Run("one_listener_hears_one_message_at_a_time", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".room"
		h := newHeard()
		listen(ctx, t, q, subject, h.record)
		var want []string
		for i := range 20 {
			body := fmt.Sprintf("m%02d", i)
			want = append(want, subject+"="+body)
			broadcast(ctx, t, q, subject, []byte(body))
		}
		h.waitFor(t, "the listener", want...)
		if got := h.all(); !slices.Equal(got, want) {
			t.Fatalf("one listener heard a burst of one sender's broadcasts as %v, want each "+
				"once, in the order sent, %v", got, want)
		}
	})

	t.Run("a_panicking_listener_loses_its_message_and_nothing_else", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".room"
		var panics atomic.Int32
		h := newHeard()
		listen(ctx, t, q, subject, func(subject string, payload []byte) {
			if string(payload) == "bad" {
				panics.Add(1)
				panic("this listener is broken")
			}
			h.record(subject, payload)
		})
		other := newHeard()
		listen(ctx, t, q, subject, other.record)
		broadcast(ctx, t, q, subject, []byte("bad"))
		broadcast(ctx, t, q, subject, []byte("good"))
		h.waitFor(t, "the listener that panicked once", subject+"=good")
		other.waitFor(t, "a neighbouring listener", subject+"=bad", subject+"=good")
		if panics.Load() != 1 {
			t.Fatalf("the panicking handler ran %d times, want 1", panics.Load())
		}
	})

	t.Run("the_bytes_arrive_exactly_as_sent", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".room"
		payload := []byte{0x00, 0xff, 0xfe, '\n', 'x'}
		got := make(chan []byte, 1)
		listen(ctx, t, q, subject, func(_ string, p []byte) { got <- p })
		broadcast(ctx, t, q, subject, payload)
		select {
		case p := <-got:
			if string(p) != string(payload) {
				t.Fatalf("a broadcast arrived as %q, want %q", p, payload)
			}
		case <-time.After(5 * time.Second): //fixedwait:liveness
			t.Fatal("the broadcast never arrived")
		}
	})

	t.Run("an_oversized_broadcast_is_refused_as_too_large", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		err := q.Broadcast(ctx, ns(t)+".room", make([]byte, queue.MaxPayloadBytes+1))
		if !errors.Is(err, queue.ErrTooLarge) {
			t.Fatalf("an oversized broadcast was refused with %v, want %v", err,
				queue.ErrTooLarge)
		}
	})

	t.Run("a_pattern_is_not_a_subject_to_broadcast_on", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		base := ns(t)
		for _, subject := range []string{"", base + ".*", base + ".>", base + "..x", base + ".a b"} {
			if err := q.Broadcast(ctx, subject, []byte("x")); err == nil {
				t.Errorf("a broadcast on %q was accepted; a broker refuses a publish to "+
					"a pattern after the call has returned, so the sender is told nothing", subject)
			}
		}
	})

	if s.caps.Peer != nil {
		t.Run("a_broadcast_reaches_a_listener_in_a_peer_process", func(t *testing.T) {
			t.Parallel()
			q := s.start(ctx, t)
			peer := s.startQueue(ctx, t, s.caps.Peer(t, q))
			// BROADCAST THE INSTANT Listen RETURNS, on fresh subjects, for
			// the reason the scatter's peer arm repeats: a Listen that
			// returns before its server holds the interest loses this race
			// sometimes rather than always.
			for round := range scatterPeerRounds {
				subject := fmt.Sprintf("%s.r%d", ns(t), round)
				h := newHeard()
				listen(ctx, t, peer, subject, h.record)
				broadcast(ctx, t, q, subject, []byte("hello"))
				h.waitFor(t, fmt.Sprintf("round %d: the peer's listener", round),
					subject+"=hello")
			}
		})
	}
}

// listen registers a listener and withdraws it at the end of the test.
func listen(ctx context.Context, t *testing.T, q queue.EventQueue, pattern string, h queue.ListenFunc) {
	t.Helper()
	stop, err := q.Listen(ctx, pattern, h)
	if err != nil {
		t.Fatalf("listen %s: %v", pattern, err)
	}
	t.Cleanup(func() {
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := stop(bounded); err != nil {
			t.Errorf("stop listening on %s: %v", pattern, err)
		}
	})
}

// broadcast sends one message and fails the test on an error.
func broadcast(ctx context.Context, t *testing.T, q queue.EventQueue, subject string, payload []byte) {
	t.Helper()
	if err := q.Broadcast(ctx, subject, payload); err != nil {
		t.Fatalf("broadcast %s: %v", subject, err)
	}
}

// heard records what one listener heard, as "subject=payload".
type heard struct {
	mu   sync.Mutex
	got  []string
	more chan struct{}
}

func newHeard() *heard { return &heard{more: make(chan struct{}, 1)} }

func (h *heard) record(subject string, payload []byte) {
	h.mu.Lock()
	h.got = append(h.got, subject+"="+string(payload))
	h.mu.Unlock()
	select {
	case h.more <- struct{}{}:
	default:
	}
}

func (h *heard) all() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.got)
}

// waitFor waits until every wanted message has been heard, in any order: a
// broadcast is unordered across publishers, and these cases assert who hears
// what, never in which order.
func (h *heard) waitFor(t *testing.T, who string, want ...string) {
	t.Helper()
	deadline := time.After(10 * time.Second) //fixedwait:liveness
	for {
		got := h.all()
		missing := false
		for _, w := range want {
			if !slices.Contains(got, w) {
				missing = true
				break
			}
		}
		if !missing {
			return
		}
		select {
		case <-h.more:
		case <-deadline:
			t.Fatalf("%s heard %v, want %v; a listener that misses a broadcast "+
				"shows a room without somebody in it", who, got, want)
		}
	}
}

// holdListener makes q a listener on subject whose handler is held inside the
// first broadcast it is handed until the returned handler's release, which
// every exit of the case calls too, and returns once that handler is running.
func holdListener(ctx context.Context, t *testing.T, q queue.EventQueue, subject string) heldHandler {
	t.Helper()
	entered, release := make(chan struct{}), make(chan struct{})
	var finished atomic.Bool
	var enter, free sync.Once
	stop, err := q.Listen(ctx, subject, func(string, []byte) {
		enter.Do(func() { close(entered) })
		<-release
		finished.Store(true)
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h := heldHandler{stop: stop, finished: &finished,
		release: func() { free.Do(func() { close(release) }) }}
	// RELEASED ON EVERY EXIT, the failing ones included, and registered
	// after the queue's Stop so it runs first: a stop waits for a handler
	// still running.
	t.Cleanup(h.release)
	broadcast(ctx, t, q, subject, []byte("held"))
	select {
	case <-entered:
	case <-time.After(5 * time.Second): //fixedwait:liveness
		t.Fatal("the listener was never handed the message")
	}
	return h
}
