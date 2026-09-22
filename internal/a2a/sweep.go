package a2a

import (
	"context"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// THE RETENTION HALF OF A CHANNEL'S LIFE, and why it is a type of its own.
//
// A [Service] ADDRESSES. An ask names a target, an answer names a recipient,
// and [New] refuses a service with no [Directory] by name — one that could
// open a channel and wake nobody reads as a slow colleague rather than as a
// wiring mistake, which is the worst shape a failure here can take.
//
// A sweep addresses nothing. It closes channels idle past a cutoff and
// deletes ones closed past another, and the node that runs it deliberately
// has no directory to give: a directory is built from the RUNNING COMPANY, so
// demanding one would tie the retention of a fleet-wide record to whether
// this node has applied a configuration.
//
// Both were one type once, and the sweep built it with an empty [Options].
// The day the directory became required that construction started failing,
// the engine logged a warning and dropped both channel jobs — so idle asks
// stayed open for ever and closed ones were never deleted, on a company whose
// every other surface looked healthy. A capability that asks for exactly what
// it needs cannot be handed less, which is what makes that unreachable rather
// than merely fixed.
type Sweeper struct {
	channels Store
	queue    queue.Publisher

	// now is injectable so the suite can pin the clock. See [Options.Now].
	now func() time.Time
}

// NewSweeper builds the retention half alone.
//
// A nil now takes [time.Now] in UTC, so the zero-value path is the real one.
func NewSweeper(channels Store, pub queue.Publisher, now func() time.Time) (*Sweeper, error) {
	if channels == nil {
		return nil, fmt.Errorf("a2a: no channel store")
	}
	if pub == nil {
		return nil, fmt.Errorf("a2a: no publisher")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Sweeper{channels: channels, queue: pub, now: now}, nil
}

// SweepIdle closes every channel idle since before cutoff and announces each
// one, reporting how many it closed.
//
// HERE RATHER THAN IN THE SWEEP JOB, because announcing is the reason
// [Store.CloseIdle] returns the channels it closed rather than a count — and
// the one caller discarded the list, so nothing was published at all. Three
// separate places described a close event the sweep never emitted: that
// method's own contract, the "system" fallback in
// [types.A2AChannelClosed.Summary], and the event-system doc. What an operator
// lost with it is the only signal that an ask went unanswered: the requester's
// turn ended when it asked, so a channel reaching this sweep means some turn
// never finished, and that is exactly the event worth seeing.
//
// The publisher lives in this package for the reason every other A2A publish
// does: what a close means on the wire is this package's decision, and
// internal/maintenance is a scheduler of jobs rather than a second author of
// event payloads.
//
// A FAILED ANNOUNCEMENT DOES NOT UNDO THE CLOSE, and does not stop the rest:
// the channel is already closed in the store, the sweep cannot roll that back,
// and abandoning the remaining channels would leave a batch half-reported with
// no record of where it stopped. The first error is returned once every
// channel has been attempted.
func (s *Sweeper) SweepIdle(ctx context.Context, cutoff time.Time) (int, error) {
	now := s.now()
	closed, err := s.channels.CloseIdle(ctx, cutoff, now)
	if err != nil {
		return 0, err
	}
	var first error
	for _, ch := range closed {
		// No ClosedBy, no TurnID: a swept channel is one NO turn finished,
		// and naming this node's sweep as the closer would read as a
		// participant. See [Closure.ClosedBy].
		if err := s.announceClose(ctx, ch, Closure{ChannelID: ch.ID}, now); err != nil && first == nil {
			first = err
		}
	}
	return len(closed), first
}

// Purge deletes channels closed before cutoff, returning the count.
//
// A passthrough, so the retention sweep drives ONE surface rather than holding
// the store beside the service and choosing between them per job.
func (s *Sweeper) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.channels.Purge(ctx, cutoff)
}

// announceClose publishes the close record for an already-closed channel.
func (s *Sweeper) announceClose(ctx context.Context, ch Channel, c Closure, now time.Time) error {
	ev := events.New(types.A2AChannelClosed{
		ChannelID: ch.ID, ClosedBy: c.ClosedBy,
		Participants: ch.Participants(),
		MessageCount: ch.Messages,
		// THE RECORD'S OWN TWO INSTANTS, never two machines' clocks: a
		// channel is opened on one node and closed on another as a matter
		// of course, so a duration taken across them would be skew.
		DurationMS: float64(ch.Duration(now).Milliseconds()),
		TurnID:     c.TurnID, WorkKey: c.WorkKey,
	}, tracing.TraceOf(ctx))
	ev.Source = c.ClosedBy
	if err := s.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		return fmt.Errorf("a2a: announce close of %s: %w", ch.ID, err)
	}
	return nil
}
