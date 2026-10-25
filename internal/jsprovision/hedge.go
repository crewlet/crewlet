package jsprovision

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
)

// ReadWith asks one metadata READ — a stream's, a consumer's or a bucket's
// info, a message by sequence or by subject — until something answers it, for
// as long as ctx allows, at t's cadences: the request presumed lost after
// t.ReadTerm and another sent t.ReAsk later, WITH EVERY REQUEST ALREADY SENT
// STILL OUTSTANDING ([hedge]). The first answer from any of them is the
// answer; [Unanswered] is the only condition that is not one.
//
// again, when non-nil, is called each time a request is presumed lost, with
// the number of requests sent so far, so a caller can say which object is not
// being answered. When ctx ends first, what is returned is what the silence
// said — [nats.ErrNoResponders] or [nats.ErrTimeout] where any request heard
// one, since each names why nobody replied — and otherwise ctx's own error.
//
// A WRITE IS NEVER ASKED THIS WAY: its reply is HELD until the object it made
// has a leader ([AskTerm]), and a second create sent while the first is held
// is a second write, so it asks through [Timing.Ask].
func ReadWith[T any](ctx context.Context, t Timing, one func(context.Context) (T, error),
	again func(asks int)) (T, error) {

	if err := t.unset(); err != nil {
		var zero T
		return zero, err
	}
	return hedge(ctx, t, one, again)
}

// Read is [ReadWith] at the production cadences, which no topology changes.
func Read[T any](ctx context.Context, one func(context.Context) (T, error), again func(asks int)) (T, error) {
	return ReadWith(ctx, Clustered(false).Timing(), one, again)
}

// hedge asks one metadata read until something answers it, keeping every
// request it has sent OUTSTANDING while it sends the next.
//
// # Why a request is never abandoned
//
// Because a request nobody has answered yet is in one of two states this side
// cannot tell apart, and only one of them is dead. The vendored server queues
// every routed JetStream API request for a pool of workers
// (server/jetstream_api.go, apiDispatch: jsAPIRoutedInfoReqs for an info
// request, jsAPIRoutedReqs for the rest) and decides it only when a worker
// takes it: a request put to a group with no leader yet is then DROPPED
// without a reply, and one behind a backed-up queue is answered LATE — the
// leader is only slow. Abandoned at [ReadTerm] and asked again, the dropped
// one is replaced, which is what the term is for; but so is the late one, and
// the server goes on processing it — and its replacement joins the same queue
// a whole term behind it. Once the leader's queue answers slower than the
// term, no read is ever taken at all: every log read on that node — the usage
// reading every append and every barrier is admitted through, the
// last-sequence read that discriminates every rejected write — fails at its
// caller's budget at once, a cliff rather than a slowdown. Kept outstanding,
// the late reply is taken when it comes, and a dropped one still costs only a
// term and a [ReAsk] before its replacement is on its way.
//
// IT ASKS THE SERVER NOTHING MORE THAN ABANDONING DID: one request every
// t.ReadTerm + t.ReAsk, and an abandoned request was processed all the same.
// What it keeps beside each is a reply this side still listens for.
//
// Every request runs on one context, derived from ctx and cancelled as hedge
// returns, which waits for each request to end and hears every reply: no
// request outlives the call. one may therefore be running more than once at a
// time, and returns its answer rather than writing it anywhere.
//
// AN ANSWER OUTRANKS THE CALLER'S ENDING. When ctx ends, every request still
// out is cancelled and heard out, and one that the object answered — a
// handle, a not-found, a refusal — is what comes back, ahead of the silence or
// the deadline: a caller whose context was already over is still told what
// the one request it made was answered, as it was when a request ran on the
// caller's own goroutine.
func hedge[T any](ctx context.Context, t Timing, one func(context.Context) (T, error),
	again func(asks int)) (T, error) {

	type reply struct {
		ask int
		v   T
		err error
	}
	asking, stop := context.WithCancel(ctx)
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
			v, err := one(asking)
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
