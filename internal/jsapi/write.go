package jsapi

import (
	"context"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// WriteBound is how long the FIRST send of an acknowledged write on js waits
// for its stream's acknowledgement when its caller set no deadline: the request
// term ([RequestTerm]) plus crossing, the time the request takes to cross the
// link its connection crosses ([Sending], at the link's pace); none for a
// connection crossing no link. Later sends get one more crossing each, up to
// [WriteSends] ([Budget]).
//
// The stream acknowledges only once the last byte is in, and a link may be as
// slow as a mebibyte in thirty seconds (internal/objstore's MiBPace). Held to
// the request term alone, a write at the transport's eight-mebibyte ceiling
// failed on any link under 1.6 MiB/s as a broker that does not answer, which
// its caller retries as an outage.
func WriteBound(js jetstream.JetStream, crossing time.Duration) time.Duration {
	return js.Options().DefaultTimeout + crossing
}

// WriteSends is how many sends of one acknowledged write its budget gives a
// crossing: the first, and the re-sends the client makes of a write nobody
// heard (jetstream.DefaultPubRetryAttempts, two), by the client inside the one
// context it is handed (a KV write's, internal/coord/kv) or one send at a time
// by internal/queue/jetstream where the client's retry is turned off.
//
// THE BUDGET'S WHOLE BOUND, fixed when the write starts: the request term and
// WriteSends crossings ([Sends]). A send past these (an attempt after a
// leader's in-flight answer) is made within the budget as it stands, and no
// pause moves it. Counted per send with no end, a write met by an in-flight
// answer on every attempt gained a crossing each time and spent milliseconds of
// it, so its budget ran ahead of the clock and it never ended undecided.
const WriteSends = 1 + jetstream.DefaultPubRetryAttempts

// Sends is the budget of ONE acknowledged write across every send: [Budget]
// starts it, [Sends.Next] is the context each send waits in, [Sends.Pause] a
// wait between two. Each derives from the caller's context and never keeps it.
// A Sends is not shared between goroutines.
//
// The crossing is an allowance per SEND, never per call, and it ends. A write
// is sent again where its answer decided nothing (nobody heard it, or a leader
// refused a compare-and-set mid-flight: internal/jsinflight), and every send
// carries the WHOLE message across again. A budget counted once for the call
// left a large record's second send the request term alone. So the first send
// is given [WriteBound], each later one ONE MORE CROSSING up to [WriteSends];
// a caller's own deadline is never extended nor shortened.
type Sends struct {
	// deadline is the budget as it stands, where the caller set no
	// deadline: the instant the send being made, or the pause after it, has
	// to be answered by.
	deadline time.Time

	// crossing is what each send after the first adds to the budget, up to
	// [WriteSends] sends.
	crossing time.Duration

	// given is how many sends the budget has given a crossing, up to
	// [WriteSends]: none before the first send.
	given int
}

// Budget starts the budget of one acknowledged write on js, each send of which
// takes crossing to cross its connection's link ([Sending]; none where it
// crosses no link). It runs from now and ends at most the request term and
// [WriteSends] crossings later.
//
// The bound counts a write's own bytes at the floor, not what is AHEAD of it
// on the link (another large write, an object store put's window) nor a link
// slower than the floor: either can still read as an outage. The worst case is
// long, about four minutes a send, twelve over [WriteSends], with everything
// the node sends behind it waiting, lease renewals among them
// (crewlet/crewlet#1035, not fixed here). A caller's own deadline is not
// checked by an external client while it waits for the connection's lock.
func Budget(js jetstream.JetStream, crossing time.Duration) *Sends {
	return &Sends{deadline: time.Now().Add(WriteBound(js, crossing)), crossing: crossing}
}

// Next is the context the write's next send waits in, derived from ctx, the
// write's caller's: ctx itself where it carries a deadline, and where it does
// not, ctx held to the budget so far — [WriteBound] from the budget's start
// for the first send, one more crossing for each send after it up to
// [WriteSends] sends, and the budget as it stands for every send past those.
func (s *Sends) Next(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, set := ctx.Deadline(); set {
		return ctx, func() {}
	}
	if s.given > 0 && s.given < WriteSends {
		s.deadline = s.deadline.Add(s.crossing)
	}
	s.given = min(s.given+1, WriteSends)
	return context.WithDeadline(ctx, s.deadline)
}

// Whole is the context a call that may make EVERY send of the write itself
// waits in — the client's own call, which re-sends a write nobody heard
// inside the one context it is handed where no option turns that off (a KV
// write's): ctx itself where it carries a deadline, and where it does not, ctx
// held to the budget's whole bound, the request term and [WriteSends]
// crossings from its start. Every send the budget will give a crossing is
// given it here, so a send after it moves nothing.
func (s *Sends) Whole(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, set := ctx.Deadline(); set {
		return ctx, func() {}
	}
	s.deadline = s.deadline.Add(time.Duration(WriteSends-max(s.given, 1)) * s.crossing)
	s.given = WriteSends
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
