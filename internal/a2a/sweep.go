package a2a

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

// keeper is what both halves of a channel's life share: the store a channel is
// kept in, the queue a change to it is announced on, and the clock.
//
// ONE VALUE EMBEDDED IN BOTH, rather than one half embedded in the other. The
// [Service] used to embed the whole [Sweeper], which was harmless while the two
// needed the same three things; the sweep now also needs [Owed], which the
// service has no use for, and a service built with a sweeper inside would have
// to be handed one — a capability asked of every caller of [New] for a sweep
// none of them runs.
type keeper struct {
	channels Store
	queue    queue.Publisher

	// now is injectable so the suite can pin the clock. See [Options.Now].
	now func() time.Time
}

// newKeeper refuses what neither half can work without. A nil now takes
// [time.Now] in UTC, so the zero-value path is the real one.
func newKeeper(channels Store, pub queue.Publisher, now func() time.Time) (keeper, error) {
	if channels == nil {
		return keeper{}, fmt.Errorf("a2a: no channel store")
	}
	if pub == nil {
		return keeper{}, fmt.Errorf("a2a: no publisher")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return keeper{channels: channels, queue: pub, now: now}, nil
}

// Owed is what the sweep asks before it closes an idle channel: whether the
// seat the channel asked ([Channel.Target]) still holds the ask somewhere it
// will take it up from, so that its answer is still coming however long the
// channel has been quiet.
//
// DECLARED HERE, by its one caller, and implemented by the engine, which is the
// only party that knows where an ask can wait: on the record of a coding run
// whose turn took it up, which can wait days on a person's question; or in the
// seat's inbox, held while one of its runs works — a run has no clock bound —
// or while a person has paused the seat, or while its budget window is spent.
// None of those has an end the sweep's idle timeout could be sized to, and a
// channel closed under any of them drops the answer that arrives later
// ([ErrClosed]) and leaves the asker, whose turn ended when it asked, never
// woken.
//
// THREE ANSWERS. (true, nil): the answer is owed, and the channel is spared.
// (false, nil): nothing will answer it — the turn crashed, or the node that ran
// it died, past every bound the timeout is sized to — and it is closed, as
// every idle channel used to be. An error: the sweep cannot tell, and spares the
// channel for this tick, because closing is the step nothing can undo and the
// next tick asks again.
type Owed interface {
	AnswerOwed(ctx context.Context, ch Channel) (bool, error)
}

// Sweeper is THE RETENTION HALF OF A CHANNEL'S LIFE, and a type of its own.
//
// A [Service] ADDRESSES. An ask names a target, an answer names a recipient,
// and [New] refuses a service with no [Directory] by name — one that could
// open a channel and wake nobody reads as a slow colleague rather than as a
// wiring mistake, which is the worst shape a failure here can take.
//
// A sweep addresses nothing. It closes channels idle past a cutoff whose answer
// is no longer owed, and deletes ones closed past another, and the node that
// runs it deliberately has no directory to give: a directory is built from the
// RUNNING COMPANY, so demanding one would tie the retention of a fleet-wide
// record to whether this node has applied a configuration.
//
// Both were one type once, and the sweep built it with an empty [Options].
// The day the directory became required that construction started failing,
// the engine logged a warning and dropped both channel jobs — so idle asks
// stayed open for ever and closed ones were never deleted, on a company whose
// every other surface looked healthy. A capability that asks for exactly what
// it needs cannot be handed less, which is what makes that unreachable rather
// than merely fixed.
type Sweeper struct {
	keeper

	// owed is asked about every idle channel before it is closed — see
	// [Owed].
	owed Owed
}

// NewSweeper builds the retention half alone.
//
// AN [Owed] IS REQUIRED, for the reason the service requires a directory: a
// sweep that cannot ask closes every channel idle past its timeout, among them
// the ask a coding run is still working on, and the answer that arrives later
// is dropped with nothing reporting it as more than an ordinary closed channel.
// A nil now takes [time.Now] in UTC, so the zero-value path is the real one.
func NewSweeper(channels Store, pub queue.Publisher, owed Owed, now func() time.Time) (*Sweeper, error) {
	k, err := newKeeper(channels, pub, now)
	if err != nil {
		return nil, err
	}
	if owed == nil {
		return nil, fmt.Errorf("a2a: no judge of whether an idle channel's answer is still " +
			"owed, so the sweep would close the ask a colleague's executor is still working on")
	}
	return &Sweeper{keeper: k, owed: owed}, nil
}

// SweepIdle closes every channel idle since before cutoff whose answer is no
// longer owed ([Owed]), announces each one, and reports how many it closed.
//
// HERE RATHER THAN IN THE SWEEP JOB, because announcing each close is as much
// the sweep's work as closing: the job used to close through the store and
// discard the list of what it closed, so nothing was published at all. Three
// separate places described a close event the sweep
// never emitted: that method's own contract, the "system" fallback in
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
// A CLOSE SOMEBODY ELSE MADE IS NOT REPORTED. The listing is a snapshot, and a
// party closing its own channel mid-sweep is ordinary: the close then comes
// back carrying the OTHER writer's instant ([Store.Close] keeps the first),
// and reporting it would draw two closes on a dashboard for one channel. The
// instant is the discriminator, read once for the whole sweep.
//
// NOTHING STOPS THE REST: a channel the judge could not answer for is spared
// and the next one asked; a close that failed is skipped; an announcement that
// failed does not undo its close, which the store already holds. Abandoning the
// remaining channels would leave a batch half-swept with no record of where it
// stopped, so the first error is returned once every channel has been
// attempted.
func (s *Sweeper) SweepIdle(ctx context.Context, cutoff time.Time) (int, error) {
	idle, err := s.channels.Idle(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	now := s.now()
	var first error
	keep := func(err error) {
		if first == nil {
			first = err
		}
	}
	closed := 0
	for _, ch := range idle {
		owed, err := s.owed.AnswerOwed(ctx, ch)
		if err != nil {
			keep(fmt.Errorf("a2a: whether channel %s's answer is still owed: %w", ch.ID, err))
			continue
		}
		if owed {
			// Not an anomaly, so not above debug: a parked run's ask is
			// spared on every tick for as long as the person it waits on
			// takes.
			log.DebugContext(ctx, "a2a_channel_spared", "channel_id", ch.ID,
				"requester", ch.Requester, "target", ch.Target,
				"detail", "idle past the sweep's timeout, and its seat still owes its answer")
			continue
		}
		got, err := s.channels.Close(ctx, ch.ID, now)
		if errors.Is(err, ErrNoChannel) {
			// Purged between the listing and the close: nothing is left
			// to close or to announce.
			continue
		}
		if err != nil {
			keep(fmt.Errorf("a2a: close idle channel %s: %w", ch.ID, err))
			continue
		}
		if !got.ClosedAt.Equal(now) {
			continue
		}
		closed++
		// No ClosedBy, no TurnID: a swept channel is one NO turn finished,
		// and naming this node's sweep as the closer would read as a
		// participant. See [Closure.ClosedBy].
		if err := s.announceClose(ctx, got, Closure{ChannelID: got.ID}, now); err != nil {
			keep(err)
		}
	}
	return closed, first
}

// Purge deletes channels closed before cutoff, returning the count.
//
// A passthrough, so the retention sweep drives ONE surface rather than holding
// the store beside the sweeper and choosing between them per job.
func (s *Sweeper) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.channels.Purge(ctx, cutoff)
}

// announceClose publishes the close record for an already-closed channel.
func (k keeper) announceClose(ctx context.Context, ch Channel, c Closure, now time.Time) error {
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
	if err := k.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		return fmt.Errorf("a2a: announce close of %s: %w", ch.ID, err)
	}
	return nil
}
