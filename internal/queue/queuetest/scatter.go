package queuetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
)

// runScatter covers the ephemeral request/scatter verbs — the fan-out a query
// uses, and the one pair here that must leave nothing behind.
//
// Every other verb in this contract is durable on purpose. These two are the
// opposite on purpose, and the difference is not a performance note: a backend
// that implemented them over its durable path would pass most of the cases
// below and fail the ones that ask what happens to a request nobody served.
func (s *suite) runScatter(t *testing.T) {
	ctx := t.Context()

	t.Run("every_server_sees_every_request", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, echo("a"))
		serve(ctx, t, q, subject, echo("b"))

		replies := ask(ctx, t, q, subject, []byte("q"), 2)
		if got := replyTexts(replies); !sameSet(got, []string{"a:q", "b:q"}) {
			t.Fatalf("two answerers on one subject replied %v — a scatter that "+
				"hands each request to ONE member divides the work twice and "+
				"covers a fraction of it", got)
		}
	})

	t.Run("reaching_want_returns_without_waiting_out_the_deadline", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, echo("fast"))
		// A SECOND ANSWERER THAT NEVER ANSWERS. Without it the ask
		// would end because everyone replied, which is a different
		// reason and would make this case pass on a backend that
		// always waits out its deadline.
		serve(ctx, t, q, subject, func(ctx context.Context, _ []byte) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})

		deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		started := time.Now()
		replies, err := q.Ask(deadline, subject, []byte("q"), 1)
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		if len(replies) != 1 {
			t.Fatalf("asked for 1 reply and got %d", len(replies))
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("an ask satisfied by its first reply took %s of a 10s "+
				"deadline — a fan-out that waits for the slowest answerer it "+
				"did not need is a fan-out with no latency benefit", elapsed)
		}
	})

	t.Run("ask_each_hands_a_reply_over_as_it_arrives", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, echo("fast"))
		// AN ANSWERER THAT NEVER ANSWERS, so the ask runs its whole
		// deadline: what this case asks is whether the fast reply was
		// handed over before that, not whether the ask ended early.
		serve(ctx, t, q, subject, func(ctx context.Context, _ []byte) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		started := time.Now()
		var (
			got     []string
			arrived time.Duration
		)
		err := q.AskEach(deadline, subject, []byte("q"), func(reply []byte) bool {
			if got = append(got, string(reply)); len(got) == 1 {
				arrived = time.Since(started)
			}
			return true
		})
		if err != nil {
			t.Fatalf("ask each: %v", err)
		}
		if !sameSet(got, []string{"fast:q"}) {
			t.Fatalf("ask each handed over %v, want the one answer that came", got)
		}
		if arrived > time.Second {
			t.Fatalf("the first reply was handed over %s into a 2s ask: AskEach "+
				"that collects for the end is Ask under another name", arrived)
		}
	})

	t.Run("ask_each_ends_when_each_says_so", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, echo("first"))
		serve(ctx, t, q, subject, func(ctx context.Context, _ []byte) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})
		deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		started := time.Now()
		calls := 0
		if err := q.AskEach(deadline, subject, []byte("q"), func([]byte) bool {
			calls++
			return false
		}); err != nil {
			t.Fatalf("ask each: %v", err)
		}
		if calls != 1 {
			t.Fatalf("each was called %d times after answering false the first time", calls)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("an ask its caller ended took %s of a 10s deadline", elapsed)
		}
	})

	t.Run("a_deadline_returns_what_arrived_and_is_not_an_error", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, echo("here"))
		serve(ctx, t, q, subject, func(ctx context.Context, _ []byte) ([]byte, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		})

		deadline, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		defer cancel()
		replies, err := q.Ask(deadline, subject, []byte("q"), 2)
		if err != nil {
			t.Fatalf("a scatter that lost an answerer to its deadline returned "+
				"an error (%v) — the caller is the one that knows what a "+
				"missing answer costs it, and an error forces every caller to "+
				"unwrap one to find out how many it got", err)
		}
		if got := replyTexts(replies); !sameSet(got, []string{"here:q"}) {
			t.Fatalf("a scatter past its deadline returned %v, want the one "+
				"answer that arrived", got)
		}
	})

	t.Run("an_answerer_that_ignores_its_context_does_not_hold_the_asker", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, echo("here"))
		// AN ANSWERER THAT NEVER RETURNS AT ALL, which is the case a
		// deadline exists for: one that merely watches its own context
		// leaves when the asker does, so a collection loop that never
		// looks at its deadline still ends and the case passes.
		released := make(chan struct{})
		serve(ctx, t, q, subject, func(context.Context, []byte) ([]byte, error) {
			<-released
			return []byte("late"), nil
		})
		// RELEASED BEFORE THE WITHDRAWAL, which waits for the answers in
		// flight: cleanups run last-registered first, so this one is
		// registered after the serve whose stop it has to precede.
		t.Cleanup(func() { close(released) })

		deadline, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		defer cancel()
		done := make(chan [][]byte, 1)
		go func() {
			replies, err := q.Ask(deadline, subject, []byte("q"), 2)
			if err != nil {
				t.Errorf("ask: %v", err)
			}
			done <- replies
		}()
		select {
		case replies := <-done:
			if got := replyTexts(replies); !sameSet(got, []string{"here:q"}) {
				t.Fatalf("the ask returned %v, want the one answer that "+
					"arrived before its deadline", got)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("an answerer that never returned held the asker past its " +
				"own deadline — the deadline is the asker's and only the " +
				"asker can enforce it, because only the asker knows it")
		}
	})

	t.Run("a_request_sent_as_its_deadline_passed_is_not_a_failed_ask", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, echo("here"))

		// A DEADLINE ALREADY PAST WHEN THE REQUEST GOES OUT: the backend
		// takes the request — on the real one it is in the connection's
		// outbound buffer, and goes out with it — and then its wait for the
		// broker to confirm it ends at once. An error says the ask could
		// not be made at all, which a caller reads as "nobody received it":
		// a write routed through it told a person it was NOT made, and a
		// note to a running turn that nothing was sent, when either may
		// have reached its server. Whatever arrived, which is nothing, is
		// the answer, as every deadline's is.
		past, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
		defer cancel()
		if _, err := q.Ask(past, subject, []byte("q"), 1); err != nil {
			t.Fatalf("an ask whose deadline passed as it was sent answered %v — "+
				"the request may have reached its server, so it is not one "+
				"that could not be made", err)
		}
	})

	t.Run("an_answerers_error_is_a_missing_answer_not_a_failed_ask", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, echo("good"))
		serve(ctx, t, q, subject, func(context.Context, []byte) ([]byte, error) {
			return nil, errors.New("this answerer is broken")
		})

		deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		replies, err := q.Ask(deadline, subject, []byte("q"), 2)
		if err != nil {
			t.Fatalf("one broken answerer failed the whole ask: %v", err)
		}
		if got := replyTexts(replies); !sameSet(got, []string{"good:q"}) {
			t.Fatalf("a scatter with one failing answerer returned %v, want "+
				"only the working one's answer", got)
		}
	})

	t.Run("a_request_nobody_serves_is_answered_by_nobody", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		deadline, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		defer cancel()
		replies, err := q.Ask(deadline, ns(t)+".unserved", []byte("q"), 1)
		if err != nil {
			t.Fatalf("asking a subject nobody serves is not an error: %v", err)
		}
		if len(replies) != 0 {
			t.Fatalf("a subject nobody serves answered %d times", len(replies))
		}
	})

	t.Run("nothing_is_retained_for_a_later_server", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"

		// ASKED BEFORE ANYBODY SERVES. On a durable transport this
		// request would be waiting in a stream; here it must be gone.
		early, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		defer cancel()
		if _, err := q.Ask(early, subject, []byte("early"), 1); err != nil {
			t.Fatalf("ask: %v", err)
		}

		seen := make(chan []byte, 4)
		serve(ctx, t, q, subject, func(_ context.Context, req []byte) ([]byte, error) {
			seen <- req
			return req, nil
		})
		late, cancelLate := context.WithTimeout(ctx, 750*time.Millisecond)
		defer cancelLate()
		if _, err := q.Ask(late, subject, []byte("late"), 1); err != nil {
			t.Fatalf("ask: %v", err)
		}

		var got []string
		for len(seen) > 0 {
			got = append(got, string(<-seen))
		}
		if !sameSet(got, []string{"late"}) {
			t.Fatalf("a server that registered after an ask received %v — a "+
				"scatter that retains its requests would replay a query whose "+
				"asker left minutes ago, and there is nobody to give the "+
				"answer to", got)
		}
	})

	t.Run("an_unsubscribed_server_stops_answering", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, echo("stays"))
		stop, err := q.Serve(ctx, subject, queue.ConcurrentAnswers, echo("goes"))
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		// ASSIGNED rather than redeclared: a shadowing `:=` here is the
		// shape govet cannot tell from the bug it is on for, and the
		// Serve error above has already been read.
		if err = stop(ctx); err != nil {
			t.Fatalf("stop serving: %v", err)
		}

		deadline, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		defer cancel()
		replies, err := q.Ask(deadline, subject, []byte("q"), 2)
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		if got := replyTexts(replies); !sameSet(got, []string{"stays:q"}) {
			t.Fatalf("after one answerer withdrew the scatter returned %v", got)
		}
	})

	t.Run("the_request_and_the_reply_are_byte_identical", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		// Bytes a text transport would mangle: a NUL, invalid UTF-8, and
		// a newline. The payload here is somebody's own encoding and the
		// queue is not entitled to an opinion about it.
		payload := []byte{0x00, 0xff, 0xfe, '\n', 'x'}
		serve(ctx, t, q, subject, func(_ context.Context, req []byte) ([]byte, error) {
			return append([]byte{0x00}, req...), nil
		})
		replies := ask(ctx, t, q, subject, payload, 1)
		want := append([]byte{0x00}, payload...)
		if len(replies) != 1 || string(replies[0]) != string(want) {
			t.Fatalf("a scatter round trip returned %q, want %q", replies, want)
		}
	})

	t.Run("an_oversized_request_is_refused_as_too_large", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, echo("a"))
		deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		_, err := q.Ask(deadline, subject, make([]byte, queue.MaxPayloadBytes+1), 1)
		if !errors.Is(err, queue.ErrTooLarge) {
			t.Fatalf("an oversized request was refused with %v, want %v — a "+
				"producer must be able to tell the one failure it must not "+
				"retry from a transport that was merely down",
				err, queue.ErrTooLarge)
		}
	})

	t.Run("one_slow_answer_does_not_hold_the_next_request", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		// THE FIRST REQUEST IS HELD until the case ends; every later one
		// is answered at once. An answerer run one request at a time
		// cannot answer the second while it holds the first, which is a
		// registration that makes every asker in the fleet wait for the
		// sum of everybody else's latency.
		held := make(chan struct{})
		holding := make(chan struct{})
		var first sync.Once
		serve(ctx, t, q, subject, func(_ context.Context, req []byte) ([]byte, error) {
			hold := false
			first.Do(func() { hold = true })
			if hold {
				close(holding)
				<-held
			}
			return req, nil
		})
		t.Cleanup(func() { close(held) })

		slow, cancelSlow := context.WithTimeout(ctx, 10*time.Second)
		defer cancelSlow()
		go func() { _, _ = q.Ask(slow, subject, []byte("slow"), 1) }()
		// UNTIL THE HELD REQUEST IS IN, so the second is not simply
		// answered first.
		select {
		case <-holding:
		case <-time.After(5 * time.Second):
			t.Fatal("the first request never reached the answerer")
		}

		deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		started := time.Now()
		replies, err := q.Ask(deadline, subject, []byte("next"), 1)
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		if got := replyTexts(replies); !sameSet(got, []string{"next"}) {
			t.Fatalf("a request behind a held answer got %v after %s — one "+
				"registration answering one request at a time", got,
				time.Since(started))
		}
	})

	// THE BOUND IS THE REGISTRATION'S OWN, and so are its slots: a
	// registration answers up to the bound it states, a request past it
	// waits for one of ITS slots, and another registration beside it is
	// answered at once. That is what lets a registration whose answers wait
	// on a queue of their own — a push proposal behind its repository's
	// earlier pushes, held for its asker's whole attempt — sit beside one
	// whose answers do not: on one gate, sixty-four such waits hold every
	// request the process answers.
	t.Run("a_registration_answers_up_to_its_own_bound_and_holds_no_other", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		full := ns(t) + ".full"
		const bound = 2
		entered := make(chan string, bound+1)
		release := make(chan struct{})
		stop, err := q.Serve(ctx, full, bound, func(ctx context.Context, req []byte) ([]byte, error) {
			entered <- string(req)
			select {
			case <-release:
				return req, nil
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		})
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		var once sync.Once
		releaseAll := func() { once.Do(func() { close(release) }) }
		t.Cleanup(func() {
			// RELEASED BEFORE THE WITHDRAWAL, which waits for the
			// answers in flight.
			releaseAll()
			bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if err := stop(bounded); err != nil {
				t.Errorf("stop serving: %v", err)
			}
		})

		asked, cancelAsked := context.WithTimeout(ctx, 20*time.Second)
		defer cancelAsked()
		answered := make(chan string, bound+1)
		for i := range bound + 1 {
			go func() {
				replies, err := q.Ask(asked, full, fmt.Appendf(nil, "r%d", i), 1)
				if err == nil && len(replies) == 1 {
					answered <- string(replies[0])
				}
			}()
		}
		for range bound {
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatalf("fewer than %d requests reached a registration bounded at %d", bound, bound)
			}
		}
		// THE ONE PAST THE BOUND WAITS for a slot of this registration.
		select {
		case req := <-entered:
			t.Fatalf("%s reached the answerer with %d already in flight on a registration "+
				"bounded at %d — the bound is the registration's to state, and one that "+
				"ignores it gives an answerer that waits on a queue of its own as many "+
				"waiting answers as it is sent", req, bound, bound)
		case <-time.After(300 * time.Millisecond):
		}

		// ANOTHER REGISTRATION, beside the full one, is answered at once.
		other := ns(t) + ".other"
		serve(ctx, t, q, other, echo("other"))
		if got := replyTexts(ask(ctx, t, q, other, []byte("q"), 1)); !sameSet(got, []string{"other:q"}) {
			t.Fatalf("a request to another registration got %v while this one's slots were "+
				"full — a registration's slots are its own, and one that shares them holds "+
				"every other answerer's requests behind its own", got)
		}

		releaseAll()
		var got []string
		for range bound + 1 {
			select {
			case reply := <-answered:
				got = append(got, reply)
			case <-time.After(10 * time.Second):
				t.Fatalf("after its slots were released the registration answered %v of %d "+
					"requests — the one past the bound waits for a slot, it is never dropped",
					got, bound+1)
			}
		}
		if !sameSet(got, []string{"r0", "r1", "r2"}) {
			t.Fatalf("the registration answered %v, want every request it was sent", got)
		}
	})

	t.Run("a_bound_below_one_is_refused", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		for _, bound := range []int{0, -1} {
			stop, err := q.Serve(ctx, ns(t)+".none", bound, echo("never"))
			if stop != nil {
				_ = stop(ctx)
			}
			if !errors.Is(err, queue.ErrAnswerBound) {
				t.Fatalf("a registration bounded at %d was answered %v, want %v — one that may "+
					"answer nothing holds every request it is sent until its askers give up",
					bound, err, queue.ErrAnswerBound)
			}
		}
	})

	t.Run("a_withdrawal_waits_for_the_answers_in_flight", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		entered := make(chan struct{})
		release := make(chan struct{})
		var finished atomic.Bool
		stop, err := q.Serve(ctx, subject, queue.ConcurrentAnswers, func(context.Context, []byte) ([]byte, error) {
			close(entered)
			// IGNORES ITS CONTEXT, which is what makes the wait visible:
			// one that watched it would return the moment the withdrawal
			// cancelled it and the case could not tell a wait from none.
			<-release
			finished.Store(true)
			return []byte("done"), nil
		})
		if err != nil {
			t.Fatalf("serve: %v", err)
		}
		asked, cancelAsk := context.WithTimeout(ctx, 10*time.Second)
		defer cancelAsk()
		go func() { _, _ = q.Ask(asked, subject, []byte("q"), 1) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("the answerer was never called")
		}
		go func() {
			time.Sleep(300 * time.Millisecond)
			close(release)
		}()
		bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		if err := stop(bounded); err != nil {
			t.Fatalf("stop serving: %v", err)
		}
		if !finished.Load() {
			t.Fatal("the withdrawal returned with an answer still running — a " +
				"caller that closes what the answerer reads straight after " +
				"withdrawing would close it under that answer")
		}
	})

	// A REPLY OVER THE CEILING IS A MISSING ANSWER, on every backend. The
	// real broker's client refuses to publish it, so the asker hears
	// nothing; a twin that delivered it would certify an answerer whose
	// large replies are lost in production — which is exactly the reply a
	// fleet's history read sends when a node holds a long turn.
	t.Run("an_oversized_reply_is_a_missing_answer", func(t *testing.T) {
		t.Parallel()
		q := s.start(ctx, t)
		subject := ns(t) + ".ask"
		serve(ctx, t, q, subject, func(context.Context, []byte) ([]byte, error) {
			return make([]byte, queue.MaxPayloadBytes+1), nil
		})
		serve(ctx, t, q, subject, echo("small"))
		deadline, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		replies, err := q.Ask(deadline, subject, []byte("q"), 2)
		if err != nil {
			t.Fatalf("ask: %v", err)
		}
		if got := replyTexts(replies); !sameSet(got, []string{"small:q"}) {
			t.Fatalf("the scatter returned %d replies (%v), want only the one "+
				"inside the ceiling", len(replies), got)
		}
	})

	if s.caps.Peer != nil {
		t.Run("a_scatter_reaches_a_peer_process", func(t *testing.T) {
			t.Parallel()
			q := s.start(ctx, t)
			peer := startQueue(ctx, t, s.caps.Peer(t, q))

			// ASKED THE INSTANT Serve RETURNS, and repeatedly on fresh
			// subjects. A backend whose Serve returns before the broker
			// knows about the subscription loses this race sometimes
			// rather than always — which is the worst shape a missing
			// registration has, because it certifies clean on a warm
			// broker and fails in CI. Repeating narrows the window this
			// can hide in; it does not close it, and that is stated
			// rather than hoped.
			for round := range scatterPeerRounds {
				subject := ns(t) + ".ask"
				serve(ctx, t, peer, subject, echo("peer"))

				deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
				replies, err := q.Ask(deadline, subject, []byte("q"), 1)
				cancel()
				if err != nil {
					t.Fatalf("round %d: ask: %v", round, err)
				}
				if got := replyTexts(replies); !sameSet(got, []string{"peer:q"}) {
					t.Fatalf("round %d: an ask on one node reached %v — either "+
						"a fan-out never leaves the node it started on, or "+
						"Serve returned before the broker had registered the "+
						"answerer and the asker raced past it", round, got)
				}
			}
		})
	}
}

// scatterPeerRounds is how many times the peer arm asks.
//
// TWENTY. Each round is one publish and one reply on a warm in-process broker
// — microseconds — so the arm stays cheap, and twenty independent chances is
// what turns a registration race that loses occasionally into one this suite
// is likely to see.
const scatterPeerRounds = 20

// serve registers an answerer and unregisters it at the end of the test.
func serve(ctx context.Context, t *testing.T, q queue.EventQueue, subject string, h queue.AnswerFunc) {
	t.Helper()
	stop, err := q.Serve(ctx, subject, queue.ConcurrentAnswers, h)
	if err != nil {
		t.Fatalf("serve %s: %v", subject, err)
	}
	t.Cleanup(func() {
		// BOUNDED, because a withdrawal waits for the answers in flight
		// and a case that forgot to release one must fail rather than
		// hang the suite.
		bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := stop(bounded); err != nil {
			t.Errorf("stop serving %s: %v", subject, err)
		}
	})
}

// ask scatters with a bounded deadline and fails the test on an error.
func ask(ctx context.Context, t *testing.T, q queue.EventQueue, subject string, req []byte, want int) [][]byte {
	t.Helper()
	deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	replies, err := q.Ask(deadline, subject, req, want)
	if err != nil {
		t.Fatalf("ask %s: %v", subject, err)
	}
	return replies
}

// echo answers "<name>:<request>", so a reply names which answerer produced it.
func echo(name string) queue.AnswerFunc {
	return func(_ context.Context, req []byte) ([]byte, error) {
		return fmt.Appendf(nil, "%s:%s", name, req), nil
	}
}

// labelsOf renders replies as strings for comparison.
func replyTexts(replies [][]byte) []string {
	out := make([]string, 0, len(replies))
	for _, r := range replies {
		out = append(out, string(r))
	}
	return out
}

// sameSet compares two collections ignoring order, because a scatter's
// replies arrive in whatever order the answerers finished.
func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	left := map[string]int{}
	for _, g := range got {
		left[g]++
	}
	for _, w := range want {
		left[w]--
		if left[w] < 0 {
			return false
		}
	}
	return true
}

// ns is a subject namespace unique to one subtest, so parallel cases on a
// shared broker never answer each other's asks.
var nsSeq struct {
	sync.Mutex
	n int
}

func ns(t *testing.T) string {
	t.Helper()
	nsSeq.Lock()
	nsSeq.n++
	n := nsSeq.n
	nsSeq.Unlock()
	return fmt.Sprintf("crewlet.scatter%d", n)
}
