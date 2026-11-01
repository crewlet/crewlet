package engine

import (
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// # Where a wake feed's group starts when it has to be created again
//
// A wake feed's durable group is the fleet's position on its log, and while
// it exists every open resumes from it. When it is gone and its log is not (an
// operator's delete, a broker that lost its consumer state), the next open
// creates it, and created with nothing to go on it starts at the log's first
// retained record: a week of a company's chat or tracker records derived into
// wakes again, which the feed's dedupe layers, remembering minutes, let
// through, so agents were woken again for work they had already done.
//
// So every node consuming a group records the group's acknowledgement floor in
// the fleet's coordination store as it advances ([coord.FeedMark]), and a group
// created again starts at the record after it. A group that exists is opened
// where it stands whatever the mark says.

// feedMarkEvery is how often a node consuming a group records its floor.
//
// TEN SECONDS: what a recreated group replays is what the fleet acknowledged
// since the mark was last written, and ten seconds of a company's records is
// a handful of wakes, which the claim (five minutes) and the deterministic wake
// id collapse. Each write is one broker read and, when the floor moved, one
// compare-and-set, per node per group, which is nothing beside the feed's own
// traffic.
const feedMarkEvery = 10 * time.Second

// feedMarkWriteBudget bounds one mark's read and write, so a slow store holds
// up the next mark rather than the feed.
const feedMarkWriteBudget = 5 * time.Second

// feedGeneration is the log's generation as this node's applier has committed
// it, and false while it has committed nothing: a node that has applied
// nothing of the log cannot say which number space a floor belongs to.
func feedGeneration(applied *statelog.Runner) (uint32, bool) {
	if applied == nil {
		return 0, false
	}
	at := applied.Committed()
	if at.IsZero() {
		return 0, false
	}
	return at.Generation, true
}

// feedPosition is one wake feed group's place in the fleet's mark register:
// what a group created again starts after ([feedPosition.start]) and what each
// open records as it goes ([feedMarker]).
type feedPosition struct {
	log    *jetstream.DomainLog
	marks  coord.FeedMarkRegister
	stream string
	group  string

	// generation is the log's generation as this node knows it, and false
	// while it knows none ([feedGeneration]).
	generation func() (uint32, bool)
}

// position is the feed's place for group, or false where no register is
// wired.
func (f domainFeed) position(group string) (feedPosition, bool) {
	if f.marks == nil {
		return feedPosition{}, false
	}
	applied := f.applied
	return feedPosition{log: f.log, marks: f.marks, stream: f.stream, group: group,
		generation: func() (uint32, bool) { return feedGeneration(applied) }}, true
}

// start is where a group created by this open should start: the record after
// the fleet's recorded floor, or zero (the log's first retained record) when
// there is no mark this node can use. It never fails an open: a mark that
// cannot be read is the old behaviour, said in a line.
func (p feedPosition) start(ctx context.Context) uint64 {
	gen, known := p.generation()
	if !known {
		return 0
	}
	mark, found, err := p.marks.FeedMark(ctx, p.stream, p.group)
	switch {
	case err != nil:
		log.WarnContext(ctx, "changefeed_mark_unread", "stream", p.stream, "group", p.group,
			"error", err.Error(),
			"detail", "if the group has to be created it starts at the log's first retained "+
				"record, and the wakes it derives again are collapsed only within the feed's "+
				"dedupe windows")
		return 0
	case !found || mark.Generation != gen:
		return 0
	}
	// A MARK PAST THE LOG'S END is from a longer log than this one, a
	// stream recreated under the same generation, and starting from it
	// would skip every record this log takes until it got there.
	if _, last, err := p.log.Bounds(ctx); err == nil && mark.Floor > last {
		log.WarnContext(ctx, "changefeed_mark_past_log", "stream", p.stream, "group", p.group,
			"mark", mark.Floor, "last", last,
			"detail", "the recorded floor is past the log's last record, so it is not this "+
				"log's; a group created now starts at the log's first retained record")
		return 0
	}
	return mark.Floor + 1
}

// feedMarker records one open group's floor on [feedMarkEvery] until the group
// is stopped, and once more as it stops.
type feedMarker struct {
	at feedPosition

	stop chan struct{}
	once sync.Once
	done sync.WaitGroup

	// last is the floor this marker last wrote, so a quiet log costs a
	// broker read per tick and no write.
	last uint64
}

// startFeedMarker starts recording the group's floor.
func startFeedMarker(ctx context.Context, at feedPosition) *feedMarker {
	m := &feedMarker{at: at, stop: make(chan struct{})}
	m.done.Add(1)
	go m.run(context.WithoutCancel(ctx), ctx.Done())
	return m
}

// run ticks until the group stops or the open's context ends, and writes a last
// mark on the way out, so a clean stop leaves the floor it stopped at.
func (m *feedMarker) run(ctx context.Context, ended <-chan struct{}) {
	defer m.done.Done()
	ticker := time.NewTicker(feedMarkEvery)
	defer ticker.Stop()
	for {
		select {
		case <-m.stop:
			m.mark(ctx)
			return
		case <-ended:
			m.mark(ctx)
			return
		case <-ticker.C:
			m.mark(ctx)
		}
	}
}

// mark reads the group's floor and raises the fleet's mark to it if it moved.
func (m *feedMarker) mark(ctx context.Context) {
	at := m.at
	gen, known := at.generation()
	if !known {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, feedMarkWriteBudget)
	defer cancel()
	floor, exists, err := at.log.GroupAckFloor(ctx, at.group)
	switch {
	case err != nil:
		log.WarnContext(ctx, "changefeed_mark_failed", "stream", at.stream, "group", at.group,
			"error", err.Error(), "detail", "the group's floor could not be read; the mark "+
				"stays where it was and is tried again on the next tick")
		return
	case !exists || floor == 0 || floor <= m.last:
		return
	}
	if _, err := at.marks.RaiseFeedMark(ctx, coord.FeedMark{Stream: at.stream, Group: at.group,
		Generation: gen, Floor: floor}); err != nil {
		log.WarnContext(ctx, "changefeed_mark_failed", "stream", at.stream, "group", at.group,
			"floor", floor, "error", err.Error(), "detail", "the mark stays where it was and "+
				"is tried again on the next tick")
		return
	}
	m.last = floor
}

// close stops the marker and waits for its last mark. Nil-safe.
func (m *feedMarker) close() {
	if m == nil {
		return
	}
	m.once.Do(func() { close(m.stop) })
	m.done.Wait()
}
