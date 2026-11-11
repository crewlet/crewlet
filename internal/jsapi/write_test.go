package jsapi_test

import (
	"context"
	"encoding/base64"
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
// A write is acknowledged only once its last byte is in, so where a connection
// crosses a link — a leaf's link to its members, a dial to an external
// cluster — a write is given the request term AND its bytes at the link's
// slowest allowed pace: the transport's eight-mebibyte ceiling four minutes
// beyond a write of nothing, at the object store's floor. A connection that
// crosses nothing — a member's own — has no pace, and every write on it waits
// the request term alone, which is what every client this package builds holds
// as its default.
//
// Mutation: build a client without the request term and set RequestTerm to
// anything but the library's own five seconds, or give a write no pace its
// bytes' crossing anyway, and a row in process goes red; leave the crossing out
// of the bound, and the row across a link does.
func TestAWriteWaitsTheRequestTermAndAcrossALinkItsBytesCrossing(t *testing.T) {
	t.Parallel()
	for name, js := range clients(t) {
		for _, n := range []int{0, 1, 1 << 20, 8 << 20} {
			if got := jsapi.WriteBound(js, n, 0); got != jsapi.RequestTerm {
				t.Errorf("%s: a write of %d bytes crossing no link waits %v, want the request "+
					"term %v alone", name, n, got, jsapi.RequestTerm)
			}
		}
		empty := jsapi.WriteBound(js, 0, objstore.MiBPace)
		if empty <= jsapi.RequestTerm || empty >= jsapi.RequestTerm+objstore.MiBPace/100 {
			t.Errorf("%s: a write of nothing across a link waits %v, want the request term %v "+
				"and only its protocol line's crossing beside it", name, empty, jsapi.RequestTerm)
		}
		// THE CEILING'S EIGHT MEBIBYTES, a pace each: four minutes. To the
		// microsecond, for the floating point the arithmetic is done in.
		ceiling := jsapi.WriteBound(js, 8<<20, objstore.MiBPace) - empty
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
// held to the bound.
//
// The bound is what a write is given where nobody said how long it may wait. A
// caller that did knows what its wait is for — a turn's budget, a request's
// reply margin — so a deadline shorter than the bound is never extended to it,
// and a longer one never shortened. In a bubble, where the clock moves only
// when every goroutine waits, each deadline is exact.
//
// Mutation: hold the write to the bound in place of its caller's deadline, and
// the shorter deadline is extended; hold it to the bound beside its caller's,
// and the longer one is cut short.
func TestAWriteKeepsItsCallersDeadline(t *testing.T) {
	t.Parallel()
	js := clients(t)["embedded"]
	const size = 8 << 20
	bound := jsapi.WriteBound(js, size, objstore.MiBPace)
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		for _, c := range []struct {
			name   string
			caller time.Duration
			want   time.Duration
		}{
			{"no deadline", 0, bound},
			{"a deadline shorter than the bound", time.Second, time.Second},
			{"a deadline longer than the bound", time.Hour, time.Hour},
		} {
			parent, stop := t.Context(), context.CancelFunc(func() {})
			if c.caller > 0 {
				parent, stop = context.WithTimeout(parent, c.caller)
			}
			ctx, cancel := jsapi.Writing(parent, js, size, objstore.MiBPace)
			deadline, set := ctx.Deadline()
			if !set || deadline.Sub(start) != c.want {
				t.Errorf("a write whose caller set %s waits until %v from now (set %t), want %v",
					c.name, deadline.Sub(start), set, c.want)
			}
			cancel()
			stop()
		}
	})
}
