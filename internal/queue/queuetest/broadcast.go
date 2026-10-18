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
		all.waitFor(t, "the trailing pattern", base+".a.presence=x",
			base+".a.b.presence=y", base+".elsewhere=z")
		// SETTLED BY THE TRAILING LISTENER: every message has been handed
		// out once it heard all three, so the one-token pattern's set is
		// final and an extra arrival would be a match it should not make.
		settleBroadcasts()
		if got := one.all(); !slices.Equal(got, []string{base + ".a.presence=x"}) {
			t.Fatalf("a one-token pattern heard %v, want only the subject it matches: "+
				"two backends with their own matchers disagree about who is in a room", got)
		}
	})

	t.Run("a_message_nobody_listened_for_is_never_heard", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".room"
		// BROADCAST BEFORE ANYBODY LISTENS. On a durable transport this
		// message would be waiting; here it must already be gone.
		broadcast(ctx, t, q, subject, []byte("early"))
		late := newHeard()
		listen(ctx, t, q, subject, late.record)
		broadcast(ctx, t, q, subject, []byte("now"))
		late.waitFor(t, "the late listener", subject+"=now")
		settleBroadcasts()
		if got := late.all(); !slices.Equal(got, []string{subject + "=now"}) {
			t.Fatalf("a listener that arrived after a broadcast heard %v; a broadcast "+
				"retained for a later listener is a heartbeat replayed after its moment", got)
		}
	})

	t.Run("a_withdrawn_listener_hears_nothing_more", func(t *testing.T) {
		t.Parallel()
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
		settleBroadcasts()
		if got := goes.all(); len(got) != 0 {
			t.Fatalf("a listener that withdrew went on hearing %v", got)
		}
	})

	t.Run("a_withdrawal_waits_for_the_handler_running", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".room"
		entered := make(chan struct{})
		release := make(chan struct{})
		var finished atomic.Bool
		var once sync.Once
		stop, err := q.Listen(ctx, subject, func(string, []byte) {
			once.Do(func() { close(entered) })
			<-release
			finished.Store(true)
		})
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		broadcast(ctx, t, q, subject, []byte("held"))
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the listener was never handed the message")
		}
		go func() {
			time.Sleep(300 * time.Millisecond)
			close(release)
		}()
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := stop(bounded); err != nil {
			t.Fatalf("stop listening: %v", err)
		}
		if !finished.Load() {
			t.Fatal("the withdrawal returned with a handler still running; a caller " +
				"that tears down what the handler reads straight after withdrawing " +
				"would tear it down under that handler")
		}
	})

	t.Run("one_listener_hears_one_message_at_a_time", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".room"
		var inside, overlapped atomic.Int32
		h := newHeard()
		listen(ctx, t, q, subject, func(subject string, payload []byte) {
			if inside.Add(1) > 1 {
				overlapped.Add(1)
			}
			time.Sleep(time.Millisecond)
			inside.Add(-1)
			h.record(subject, payload)
		})
		var want []string
		for i := range 20 {
			body := fmt.Sprintf("m%02d", i)
			want = append(want, subject+"="+body)
			broadcast(ctx, t, q, subject, []byte(body))
		}
		h.waitFor(t, "the listener", want...)
		if n := overlapped.Load(); n > 0 {
			t.Fatalf("one listener's handler ran %d time(s) beside itself; a handler "+
				"that takes its own lock would be raced by the backend", n)
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
		case <-time.After(5 * time.Second):
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
			peer := startQueue(ctx, t, s.caps.Peer(t, q))
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
	deadline := time.After(10 * time.Second)
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

// broadcastSettle is how long a case waits for a message that must NOT arrive.
//
// HALF A SECOND, the scatter cases' own bound for an answer that must not come:
// a broadcast on a warm in-process broker arrives in microseconds, so a message
// still in flight after this is one the backend held rather than one it was
// slow to deliver.
const broadcastSettle = 500 * time.Millisecond

// settleBroadcasts waits out [broadcastSettle], so a message that should never
// arrive has had its chance to.
func settleBroadcasts() { time.Sleep(broadcastSettle) }
