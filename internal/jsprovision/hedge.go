package jsprovision

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// ReadWith asks one metadata READ (a stream's, consumer's or bucket's info, a
// message by sequence or subject) until something answers, for as long as ctx
// allows: a request presumed lost after t.ReadTerm, another sent t.ReAsk later,
// WITH EVERY REQUEST STILL OUTSTANDING ([hedge]). The first answer wins;
// [Unanswered] is the only condition that is not one. again, when non-nil, is
// called per request presumed lost with the number sent. When ctx ends first
// the silence's error returns ([nats.ErrNoResponders] or [nats.ErrTimeout]), else ctx's.
//
// A WRITE IS NEVER ASKED THIS WAY when its reply is held or its repeat is a
// second fact: a create asks through [Timing.Ask] ([AskTerm]). The one write
// asked as a read is internal/learning/memsync's hydration barriers, publishes
// to a subject keeping one message, so a second copy replaces the first.
func ReadWith[T any](ctx context.Context, t Timing, one func(context.Context) (T, error),
	again func(asks int)) (T, error) {

	// Never past t.Lookup, whatever the caller set: past it a group in any
	// state answers a read (a leader, or JSClusterNotAvailError;
	// [LookupBudget]), so a read still unanswered asks a broker it cannot
	// reach. A deadline-less read would hold the trim tick's loop, the one that
	// evaluates the node's alarms. A shorter caller deadline is kept. Across a
	// link the timing follows ([Timing.Following]) the ceiling holds past it by
	// Beyond at most, asking nothing more and waiting for requests out
	// ([Timing.Ceiling]). Its ending is SILENCE, never the caller's ending.
	// A read that was ONE request until hedged keeps that request's bound
	// instead ([ReadBoundedWith]).
	return ReadBoundedWith(ctx, t, t.Lookup, one, again)
}

// Read is [ReadWith] at the production cadences, which no topology changes.
func Read[T any](ctx context.Context, one func(context.Context) (T, error), again func(asks int)) (T, error) {
	return ReadWith(ctx, Clustered(false).Timing(), one, again)
}

// ReadBoundedWith is [ReadWith] for a read that was ONE request until hedged: a
// caller with no deadline of its own is held to bound, that request's client
// DefaultTimeout (jetstream.JetStreamOptions), before the read begins and never
// past it ([hedge]); a caller's own deadline is kept, under the ceiling. Held
// to the t.Lookup ceiling instead, a deadline-less duty tick, boot step or
// heartbeat sized at five seconds would wait six times as long for nothing.
// Inside five seconds a read is asked at 0, 2 and 4 s, so a request dropped
// across an election is re-asked at 2 s, where the new leader answers (2.0 to
// 2.2 s measured); an election past about 4 s is not ridden out. The bound
// ending is SILENCE ([Unanswered]); a bound of zero or less, a deadline already
// past, is refused by name.
func ReadBoundedWith[T any](ctx context.Context, t Timing, bound time.Duration,
	one func(context.Context) (T, error), again func(asks int)) (T, error) {

	// Across a link the timing follows ([Timing.Following]), bound and ceiling
	// hold past their spans by Beyond at most while the link moves: a reply
	// queued behind megabytes is the broker having answered. NOTHING MORE IS
	// ASKED past the span, only waited for: past the ceiling every request sent
	// inside it is answered or was dropped past recovering, and another would
	// queue behind the bytes the first waits on. No link: time alone.
	if err := t.unset(); err != nil {
		var zero T
		return zero, err
	}
	if bound <= 0 {
		var zero T
		return zero, fmt.Errorf("jsprovision: a read's bound is %v — it is its client's "+
			"DefaultTimeout, which every JetStream client holds", bound)
	}
	asking := t.Lookup
	if _, set := ctx.Deadline(); !set {
		var cancel context.CancelFunc
		ctx, cancel = t.Ceiling(ctx, bound)
		defer cancel()
		asking = min(asking, bound)
	}
	ctx, cancel := t.Ceiling(ctx, t.Lookup)
	defer cancel()
	return hedge(ctx, t, asking, one, again)
}

// ReadBounded is [ReadBoundedWith] at the production cadences, which no
// topology changes.
func ReadBounded[T any](ctx context.Context, bound time.Duration, one func(context.Context) (T, error),
	again func(asks int)) (T, error) {

	return ReadBoundedWith(ctx, Clustered(false).Timing(), bound, one, again)
}

// hedge asks one metadata read ([ReadWith]) until something answers it,
// keeping every request sent OUTSTANDING while it sends the next. A request is
// never abandoned: an unanswered request is either dropped or answered LATE, and this side
// cannot tell which. The vendored server queues routed API requests for
// workers (server/jetstream_api.go, apiDispatch): a request to a group with no
// leader is DROPPED, one behind a backed-up queue is merely slow. Abandoning at
// [ReadTerm] replaces the dropped one but also queues a duplicate a term behind
// the late one; once the queue answers slower than the term no read is ever
// taken, a cliff rather than a slowdown. Kept outstanding, a late reply is
// taken when it comes and a dropped one costs a term and a [ReAsk].
func hedge[T any](ctx context.Context, t Timing, asking time.Duration,
	one func(context.Context) (T, error), again func(asks int)) (T, error) {

	type reply struct {
		ask int
		v   T
		err error
	}
	// One request every t.ReadTerm + t.ReAsk while nobody answers. While a
	// leader's queue answers slower, each waiting read adds load at that
	// cadence, bounded by its bound and the ceiling, not by the hedge. The
	// first re-ask recovers a request dropped across an election (the new
	// leader answers at ~2 s); later ones only recover longer elections and
	// are more work for a merely slow leader. NOTHING PAST ctx's DEADLINE: a
	// re-ask due after it is not sent, though ctx cannot say so until the
	// runtime runs its timer ([overdue]), which a stalled process runs late.
	// NOTHING PAST asking when above zero: requests out are waited for under
	// ctx and no re-ask is sent. Zero means as long as ctx allows.
	askedUntil := time.Now().Add(asking)
	// Every request runs on one context cancelled as hedge returns, which waits
	// for each and hears every reply: no request outlives the call, so one may
	// run more than once at a time and returns its answer, never writing it.
	// AN ANSWER OUTRANKS THE CALLER'S ENDING: when ctx ends, requests still out
	// are cancelled and heard out, and an answer the object gave (handle,
	// not-found, refusal) returns ahead of the silence or the deadline.
	requests, stop := context.WithCancel(ctx)
	defer stop()
	var running sync.WaitGroup
	// UNBUFFERED, and every request's reply is received — by the loop, or
	// by settled below as hedge returns — so a request never ends without
	// being heard and never blocks once hedge has returned.
	replies := make(chan reply)
	asks := 0
	send := func() {
		asks++
		ask := asks
		running.Go(func() {
			v, err := one(requests)
			replies <- reply{ask: ask, v: v, err: err}
		})
	}
	// settled cancels every request still out and hears each of them end,
	// answering with the first the object answered, if any did.
	settled := func() (reply, bool) {
		stop()
		ended := make(chan struct{})
		go func() {
			running.Wait()
			close(ended)
		}()
		var first reply
		found := false
		for {
			select {
			case r := <-replies:
				if !found && answered(r.err) {
					first, found = r, true
				}
			case <-ended:
				return first, found
			}
		}
	}

	// silence is what the requests nobody answered said, naming the reason
	// where any of them did ([namedSilence]).
	var silence error
	term := time.NewTimer(t.ReadTerm)
	defer term.Stop()
	// reAsk is armed once the newest request is presumed lost, and sends
	// the next.
	var reAsk <-chan time.Time
	lost := func() {
		if again != nil {
			again(asks)
		}
		reAsk = time.After(t.ReAsk)
	}
	send()
	for {
		select {
		case r := <-replies:
			if !Unanswered(ctx, r.err) {
				// ANY ANSWER, from any request — the object, a
				// not-found, a refusal — or ctx's own ending, which
				// says nothing about the object: then the silence
				// that named something is the honest answer.
				if late, ok := settled(); ok && !answered(r.err) {
					return late.v, late.err
				}
				return r.v, namedSilence(silence, r.err)
			}
			silence = namedSilence(silence, r.err)
			// THE NEWEST REQUEST WAS TOLD NOTHING before its term —
			// `no responders` comes back in microseconds — so it is
			// lost now, not a term from now. An older one's silence
			// changes nothing: the newest is still in its term.
			if r.ask == asks && reAsk == nil {
				term.Stop()
				lost()
			}
		case <-term.C:
			lost()
		case <-reAsk:
			reAsk = nil
			if overdue(ctx) {
				// THE DEADLINE PASSED DURING THE PAUSE, and ctx
				// cannot say so until the runtime runs the timer
				// that ends it: a request sent now is asked past
				// the bound its caller set ([overdue]). Nothing
				// more is asked, and ctx's ending, which is due,
				// ends the read below — or an answer to a request
				// already out comes first, and is taken.
				continue
			}
			if asking > 0 && !time.Now().Before(askedUntil) {
				// PAST THE SPAN IT IS ASKED FOR, while a moving link
				// holds ctx open ([ReadBoundedWith]): the requests
				// already out are waited for, and none is added.
				continue
			}
			send()
			term.Reset(t.ReadTerm)
		case <-ctx.Done():
			if late, ok := settled(); ok {
				return late.v, late.err
			}
			var zero T
			return zero, namedSilence(silence, ctx.Err())
		}
	}
}

// answered reports a reply the object gave — a value, a not-found, a refusal —
// rather than a request nobody replied to or one cut off by its context.
func answered(err error) bool {
	return err == nil || (!silent(err) && !errors.Is(err, context.Canceled))
}

// silent reports an error that is no answer from the object: a request
// nobody replied to, or a deadline that ended the asking.
func silent(err error) bool {
	return errors.Is(err, nats.ErrTimeout) || errors.Is(err, nats.ErrNoResponders) ||
		errors.Is(err, context.DeadlineExceeded)
}
