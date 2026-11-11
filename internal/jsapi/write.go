package jsapi

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// WriteBound is how long the FIRST send of an acknowledged write on js waits
// for its stream's acknowledgement when its caller set no deadline: the term
// every request of js waits (its DefaultTimeout, [RequestTerm] on every client
// [API.Client] builds), and crossing, the time the write's request takes to
// cross the link its connection crosses ([Sending] of what it carries, at the
// link's pace). A connection that crosses no link — a member's own, in its own
// process — has no crossing, and its writes keep the request term alone. A
// write sent again is given one more crossing for each send ([Budget]).
//
// # Why a write across a link is given its bytes
//
// The stream acknowledges a message only once its last byte is in, and a
// connection that crosses a link — a leaf's link to the fleet's members, a
// dial to an external cluster — carries it at the link's pace, which the
// engine allows to be as slow as a mebibyte in thirty seconds
// (internal/objstore's MiBPace, the pace a read of a large message across the
// same link is already given: [Leader.Carrying]). Held to the request term
// alone, a write at the transport's eight-mebibyte ceiling could not be
// acknowledged on any link slower than 1.6 mebibytes a second, about fifty
// times that floor, and failed as a broker that does not answer — which its
// caller retries as an outage, so a keeper retried for ever, as unavailable,
// an entry each of whose copies the link went on carrying
// (crewlet/crewlet#1005).
//
// # What it covers, and what it does not
//
// A WRITE IS GIVEN ITS OWN BYTES AT THE FLOOR, and nothing more, so a link
// that carries it alone at the floor or faster never fails it. Two things it
// does not count still can: a link slower than the floor, which is the
// slowest link the engine is sized for, fails it as it fails an object
// store's transfer; and a link carrying something AHEAD of it — another large
// write, an object store put's window — delivers its bytes only after that
// traffic's, which no bound here counts, so a write waiting its turn on a busy
// link can still read as an outage.
//
// SO THE WORST CASE IS LONG, AND NOT ONLY FOR THE WRITE: a write at the
// ceiling across a link at the floor waits the request term and about four
// minutes more before it reads as an outage, and for those four minutes
// everything the node sends behind it on that link waits too — its lease
// renewals among them, which at the shipped lease lapse long before its last
// byte is in. That is a defect of its own, crewlet/crewlet#1035, which this
// bound does not fix: it stops a slow write being abandoned as an outage, and
// does nothing about what the write holds up. A caller that cannot wait that
// long says so with a deadline of its own, which a budget never extends.
func WriteBound(js jetstream.JetStream, crossing time.Duration) time.Duration {
	return js.Options().DefaultTimeout + crossing
}

// Sends is the budget of ONE acknowledged write across every time it is
// sent: [Budget] starts it, [Sends.Next] is the context each send waits in,
// and [Sends.Pause] a wait between two sends. Each is handed the caller's own
// context, which it derives from and never keeps. A write's sends are made one
// at a time, and its Sends is not shared between goroutines.
//
// # The crossing is an allowance per SEND, never per call
//
// A write is sent again where its answer decided nothing — nobody heard it,
// and it is sent again as the client would have (internal/queue/jetstream);
// or a stream's leader refused a compare-and-set without comparing anything,
// because another write to the subject was still in flight
// (internal/jsinflight) — and every send carries the WHOLE message across the
// link again, acknowledged only once its last byte is in. A budget counted
// once for the call left the second send of a large message the request term
// alone, which carries about a sixth of a mebibyte at the floor: a record
// larger than that, met by one in-flight answer, read as no answer while the
// link was still carrying it at a pace the engine allows. So where the caller
// set no deadline the first send is given [WriteBound] — the request term and
// its crossing — and every later send ONE MORE CROSSING, and a pause between
// two is held to the budget as it stands. A caller's own deadline is every
// send's as it is: never extended, and never shortened either.
type Sends struct {
	// deadline is the budget as it stands, where the caller set no
	// deadline: the instant the send being made, or the pause after it, has
	// to be answered by.
	deadline time.Time

	// crossing is what each send after the first adds to the budget.
	crossing time.Duration

	// sent is whether the first send has been made.
	sent bool
}

// Budget starts the budget of one acknowledged write on js, each send of which
// takes crossing to cross the link its connection crosses: [Sending] of what
// the send carries at the link's pace, or that times the sends one call of the
// client may make inside the one context it is handed — and none where the
// connection crosses no link. The budget runs from now.
func Budget(js jetstream.JetStream, crossing time.Duration) *Sends {
	return &Sends{deadline: time.Now().Add(WriteBound(js, crossing)), crossing: crossing}
}

// Next is the context the write's next send waits in, derived from ctx, the
// write's caller's: ctx itself where it carries a deadline, and where it does
// not, ctx held to the budget so far — [WriteBound] from the budget's start
// for the first send, and one more crossing for every send after it.
func (s *Sends) Next(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, set := ctx.Deadline(); set {
		return ctx, func() {}
	}
	if s.sent {
		s.deadline = s.deadline.Add(s.crossing)
	}
	s.sent = true
	return context.WithDeadline(ctx, s.deadline)
}

// Pause waits d inside ctx, the write's caller's, and, where it carries no
// deadline, inside the budget as it stands — the wait between two sends — and
// answers nil once d has passed, or why it could not wait it out: the budget
// ending first, or the caller going away, as that context's error.
func (s *Sends) Pause(ctx context.Context, d time.Duration) error {
	if _, set := ctx.Deadline(); !set {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, s.deadline)
		defer stop()
	}
	pause := time.NewTimer(d)
	defer pause.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-pause.C:
		return nil
	}
}
