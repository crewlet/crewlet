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
// seat it asked ([Channel.Target]) still holds the ask somewhere it will take
// it up from, so its answer is still coming however long the channel is quiet.
//
// The engine implements it, as the only party that knows where an ask can wait
// (an executor's record, days on a person's question; the seat's inbox while a
// run works, the seat is paused or its budget is spent). None has an end the
// idle timeout could be sized to, and a channel closed under any of them drops
// the later answer ([ErrClosed]) and leaves the asker never woken.
//
// (true, nil): owed, spared. (false, nil): nothing will answer, closed. An
// error: spared this tick, since closing cannot be undone.
type Owed interface {
	AnswerOwed(ctx context.Context, ch Channel) (bool, error)
}

// Sweeper is the retention half of a channel's life, a type of its own.
//
// A [Service] addresses: [New] refuses one with no [Directory], since one that
// woke nobody would read as a slow colleague, not a wiring mistake.
//
// A sweep addresses nothing: it closes channels idle past a cutoff whose answer
// is no longer owed and deletes ones closed past another. It has no directory
// to give (one is built from the running company, and demanding it would tie a
// fleet-wide record's retention to this node having applied a configuration).
// One type would make the sweep's construction fail wherever the directory is
// required, silently dropping the channel jobs.
type Sweeper struct {
	keeper

	// owed is asked about every idle channel before it is closed — see
	// [Owed].
	owed Owed
}

// NewSweeper builds the retention half alone.
//
// An [Owed] is required: a sweep that cannot ask closes every idle channel,
// including an ask an executor is still working on, and the later answer is
// dropped with nothing reporting more than an ordinary close. A nil now takes
// [time.Now] in UTC, so the zero-value path is the real one.
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
// It lives here, not in the sweep job: announcing each close is part of
// closing, and a channel reaching this sweep is the only signal an ask went
// unanswered. A close somebody else made is not reported: a party closing its
// own channel mid-sweep comes back carrying the other writer's instant
// ([Store.Close] keeps the first), and it would draw two closes for one channel.
//
// Nothing stops the rest: a spared or failed channel is skipped, and a failed
// announcement does not undo its close. The first error is returned once every
// channel has been attempted.
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
