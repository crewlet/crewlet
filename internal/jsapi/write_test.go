package jsapi_test

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
)

// clients is a client of each API this package builds, over a connection
// that is never dialled: what is asked of them here is their options.
func clients(t *testing.T) map[string]jetstream.JetStream {
	t.Helper()
	out := map[string]jetstream.JetStream{}
	for name, api := range map[string]jsapi.API{"account": jsapi.Account(), "embedded": jsapi.Embedded()} {
		js, err := api.Client(&nats.Conn{})
		if err != nil {
			t.Fatalf("%s client: %v", name, err)
		}
		out[name] = js
	}
	return out
}

// A WRITE WAITS THE REQUEST TERM, AND ACROSS A LINK THE TIME ITS BYTES TAKE TO
// CROSS IT.
//
// A write is acknowledged only once its last byte is in, so across a link
// (a leaf's, a dial to an external cluster) its first send is given the
// request term AND its bytes at the slowest allowed pace: the transport's
// eight-mebibyte ceiling four minutes beyond a write of nothing, at the object
// store's floor. A connection crossing nothing waits the request term alone.
//
// Mutation: set RequestTerm to anything but the library's own five seconds
// without the client's, and the row crossing no link goes red; leave the
// crossing out of the bound, and the rows across a link do.
func TestAWriteWaitsTheRequestTermAndAcrossALinkItsBytesCrossing(t *testing.T) {
	t.Parallel()
	for name, js := range clients(t) {
		if got := jsapi.WriteBound(js, 0); got != jsapi.RequestTerm {
			t.Errorf("%s: a write crossing no link waits %v, want the request term %v alone",
				name, got, jsapi.RequestTerm)
		}
		empty := jsapi.WriteBound(js, jsapi.Sending(0, objstore.MiBPace))
		if empty <= jsapi.RequestTerm || empty >= jsapi.RequestTerm+objstore.MiBPace/100 {
			t.Errorf("%s: a write of nothing across a link waits %v, want the request term %v "+
				"and only its protocol line's crossing beside it", name, empty, jsapi.RequestTerm)
		}
		// THE CEILING'S EIGHT MEBIBYTES, a pace each: four minutes. To the
		// microsecond, for the floating point the arithmetic is done in.
		ceiling := jsapi.WriteBound(js, jsapi.Sending(8<<20, objstore.MiBPace)) - empty
		if off := ceiling - 8*objstore.MiBPace; off < -time.Microsecond || off > time.Microsecond {
			t.Errorf("%s: eight mebibytes across a link at the floor add %v to a write's wait, "+
				"want %v", name, ceiling, 8*objstore.MiBPace)
		}
	}
}

// A WRITE'S REQUEST CROSSES RAW, where a read's reply crosses base64-encoded.
//
// A publish carries its message as it is behind its protocol line, so its
// request is the message's own bytes, while the reply to a read of the same
// message carries it base64-encoded inside JSON and is a third larger. Counted
// as a read's, a write on a dead link would be held a third longer than its
// bytes need before it reads as an outage. At a pace of a nanosecond a byte —
// a mebibyte in a mebibyte of nanoseconds — the arithmetic is exact, and the
// crossing a message adds is its byte count.
//
// Mutation: count a write's request as base64, as Crossing counts a read's
// reply, and the write adds a third more than its bytes.
func TestAWritesRequestCrossesRawWhereAReadsReplyIsBase64(t *testing.T) {
	t.Parallel()
	const byteANanosecond = time.Duration(1 << 20)
	for _, n := range []int{1, 3 << 20, 8 << 20} {
		if added := jsapi.Sending(n, byteANanosecond) - jsapi.Sending(0, byteANanosecond); added != time.Duration(n) {
			t.Errorf("a message of %d bytes adds %v to its write's crossing, want its %d raw bytes",
				n, added, n)
		}
		wantRead := time.Duration(base64.StdEncoding.EncodedLen(n) - base64.StdEncoding.EncodedLen(0))
		if added := jsapi.Crossing(n, byteANanosecond) - jsapi.Crossing(0, byteANanosecond); added != wantRead {
			t.Errorf("a message of %d bytes adds %v to a read's reply, want its %d base64 bytes",
				n, added, wantRead)
		}
	}
}

// A CALLER'S OWN DEADLINE IS KEPT, and only a write whose caller set none is
// held to the budget.
//
// The budget is for a write whose caller said nothing. A caller that did knows
// what its wait is for, so a shorter deadline is never extended to the budget
// and a longer one never shortened, on every send. Exact in a bubble.
//
// Mutation: hold the write to the budget in place of its caller's deadline,
// and the shorter deadline is extended; hold it to the budget beside its
// caller's, and the longer one is cut short.
func TestAWriteKeepsItsCallersDeadline(t *testing.T) {
	t.Parallel()
	js := clients(t)["embedded"]
	crossing := jsapi.Sending(8<<20, objstore.MiBPace)
	first := jsapi.WriteBound(js, crossing)
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		for _, c := range []struct {
			name   string
			caller time.Duration
			want   []time.Duration
		}{
			{"no deadline", 0, []time.Duration{first, first + crossing}},
			{"a deadline shorter than the budget", time.Second, []time.Duration{time.Second, time.Second}},
			{"a deadline longer than the budget", time.Hour, []time.Duration{time.Hour, time.Hour}},
		} {
			parent, stop := t.Context(), context.CancelFunc(func() {})
			if c.caller > 0 {
				parent, stop = context.WithTimeout(parent, c.caller)
			}
			sends := jsapi.Budget(js, crossing)
			for i, want := range c.want {
				send, done := sends.Next(parent)
				deadline, set := send.Deadline()
				done()
				if !set || deadline.Sub(start) != want {
					t.Errorf("send %d of a write whose caller set %s waits until %v from now "+
						"(set %t), want %v", i+1, c.name, deadline.Sub(start), set, want)
				}
			}
			stop()
		}
	})
}

// EVERY SEND OF A WRITE IS GIVEN ITS OWN CROSSING, UP TO THE SENDS A WRITE MAY
// MAKE, so the budget's end is fixed when it starts; and a pause between two
// sends is held to the budget as it stands.
//
// Every send carries the whole message across the link again. So the first is
// given the request term and its crossing, each later one a crossing more up to
// WriteSends; a send past those gets the budget as it stands. A pause that
// would outlast the budget ends at its end, with the deadline's error.
//
// Mutation: give every send the first send's budget, and the second and third
// are short of their crossing; add a crossing with no end, and the fourth is
// past the bound; let a pause run past the budget, and it is waited out whole.
func TestEverySendOfAWriteIsGivenItsOwnCrossingUpToTheSendsAWriteMayMake(t *testing.T) {
	t.Parallel()
	js := clients(t)["embedded"]
	crossing := jsapi.Sending(1<<20, objstore.MiBPace)
	bound := jsapi.RequestTerm + jsapi.WriteSends*crossing
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		sends := jsapi.Budget(js, crossing)
		for i := range jsapi.WriteSends + 2 {
			given := min(i+1, jsapi.WriteSends)
			want := jsapi.RequestTerm + time.Duration(given)*crossing
			send, done := sends.Next(t.Context())
			deadline, set := send.Deadline()
			done()
			if !set || deadline.Sub(start) != want {
				t.Fatalf("send %d waits until %v from the budget's start (set %t), want the "+
					"request term and %d crossings, %v", i+1, deadline.Sub(start), set, given, want)
			}
		}
		// THE BUDGET AS IT STANDS: its bound, none of it spent yet. A pause
		// inside it ends when asked to.
		if err := sends.Pause(t.Context(), crossing); err != nil {
			t.Fatalf("a pause of one crossing inside the budget = %v, want it waited out", err)
		}
		if waited := time.Since(start); waited != crossing {
			t.Errorf("a pause of %v ended %v after the budget's start", crossing, waited)
		}
		// One that would outlast it ends at its end, as the deadline.
		err := sends.Pause(t.Context(), time.Hour)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a pause past the budget = %v, want the budget's deadline", err)
		}
		if ended := time.Since(start); ended != bound {
			t.Errorf("a pause past the budget ended %v after its start, want at its bound, %v",
				ended, bound)
		}
	})
}

// A CALL THAT MAKES EVERY SEND ITSELF WAITS THE BUDGET'S WHOLE BOUND, and
// nothing after it moves the bound.
//
// The client re-sends a write nobody heard inside the one context it is
// handed (a KV write's re-sends cannot be turned off), so each call is given
// every crossing at once: the request term and WriteSends crossings from the
// budget's start, whatever was sent before. A second call and a send after it
// wait the same bound. A caller's own deadline is kept.
//
// Mutation: give a call the first send's budget, and a client's re-sends run
// short of their crossings; add the whole bound per call, and the second is
// past it.
func TestACallThatMakesEverySendItselfWaitsTheBudgetsWholeBound(t *testing.T) {
	t.Parallel()
	js := clients(t)["embedded"]
	crossing := jsapi.Sending(8<<20, objstore.MiBPace)
	bound := jsapi.RequestTerm + jsapi.WriteSends*crossing
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		waits := func(ctx context.Context, done context.CancelFunc) time.Duration {
			defer done()
			deadline, set := ctx.Deadline()
			if !set {
				return -1
			}
			return deadline.Sub(start)
		}
		sends := jsapi.Budget(js, crossing)
		for i, got := range []time.Duration{
			waits(sends.Whole(t.Context())),
			waits(sends.Whole(t.Context())),
			waits(sends.Next(t.Context())),
		} {
			if got != bound {
				t.Errorf("call %d of a budget waits until %v from its start, want its whole bound, %v",
					i+1, got, bound)
			}
		}
		// AFTER A SEND OF ITS OWN: the first send has its one crossing, and a
		// call after it the bound, never the bound and the crossing spent.
		sent := jsapi.Budget(js, crossing)
		if got := waits(sent.Next(t.Context())); got != jsapi.RequestTerm+crossing {
			t.Errorf("a budget's first send waits until %v from its start, want the request "+
				"term and one crossing", got)
		}
		if got := waits(sent.Whole(t.Context())); got != bound {
			t.Errorf("a call after a send waits until %v from the budget's start, want its "+
				"whole bound, %v", got, bound)
		}
		caller, stop := context.WithTimeout(t.Context(), time.Second)
		defer stop()
		if got := waits(jsapi.Budget(js, crossing).Whole(caller)); got != time.Second {
			t.Errorf("a call whose caller set a second waits until %v from now, want the "+
				"caller's second", got)
		}
	})
}
