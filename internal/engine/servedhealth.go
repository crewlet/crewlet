package engine

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A READ'S READINESS, SERVED FROM MEMORY.
//
// A domain's health is asked on every read ([statelog.ReaderDeps.Health]).
// Two terms are remote: the published floors (a leader read of the domain's
// coordination key) and the log's ends (a stream-info request on a handle
// whose mutex serialises callers). Read per read, they put the company's read
// rate on one leader and queue a domain's reads behind one round trip, so both
// are served from a [memo] (BenchmarkAReadsReadiness). Staleness is safe:
// floors only rise within a generation and a stale end is low, so a read is
// served a second too long or reports a second's less lag, never a hole or a
// position past the end ([observeWith]). The write fence and the trim duty read
// both fresh, where an unread value is a lost update ([stateLog.trimFloor]).

// endsStale is how old a log's ends may be and still be served: one
// [servingRecheck] before a read starts their refresh plus the [servingRead]
// bounding it, the oldest a working refresh leaves them. Older ends are unread,
// and a health with unread ends refuses every read. Not
// [statelog.FloorCacheStale] (a minute, four heartbeats): a bounded read's lag
// is derived from the ends, and a minute-old end could serve a node thousands
// of records behind as within its caller's bound.
const endsStale = servingRecheck + servingRead

// memo is one remote value as reads are served it: the last read that
// answered, when it landed, and the one read in flight to replace it. The zero
// value holds nothing.
type memo[T any] struct {
	mu   sync.Mutex
	held T
	// at is when held's read landed, zero while nothing is held.
	at time.Time
	// failed is why the last read did not answer, nil after one that did.
	failed error
	// reading is closed when the read in flight lands, nil while none is.
	reading chan struct{}

	// reads counts the reads this memo made itself: a read path's broker cost.
	reads atomic.Int64

	// now is the clock, for the cases that age a value; nil is the wall's.
	now func() time.Time
}

// clock is now, from [memo.now] or the wall clock.
func (m *memo[T]) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// store records v, read elsewhere and landed at at, so the memo need not make
// a read of its own. A read that landed before the one held replaces nothing.
func (m *memo[T]) store(v T, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if at.Before(m.at) {
		return
	}
	m.held, m.at, m.failed = v, at, nil
}

// last is the last value that answered and its age by the memo's clock,
// however old; false where nothing ever answered. A term nobody could refresh
// is reported with it: past serving, its age is still worth naming.
func (m *memo[T]) last() (T, time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.at.IsZero() {
		var zero T
		return zero, 0, false
	}
	return m.held, m.clock().Sub(m.at), true
}

// serve is the value a read is answered with: a usable one younger than
// [servingRecheck] as held; a usable one no older than stale as held, starting
// ONE read to replace it that nobody waits for; otherwise it waits for that
// read, no longer than ctx, and answers its failure.
//
// stale is the value's bound, not the memo's ([statelog.FloorCacheStale],
// [endsStale]); usable says whether a held value describes what is asked (ends
// read at another generation do not). ctx is the node's life
// ([stateLog.readerFor]), never one request's: the read is shared, so a request
// that gave up must not fail every read. The read is never made under the lock
// and is bounded by [servingRead], so reads behind an unreachable broker
// neither queue nor wait past it.
func (m *memo[T]) serve(ctx context.Context, stale time.Duration, usable func(T) bool,
	read func(context.Context) (T, error)) (T, error) {

	var zero T
	m.mu.Lock()
	held, have := m.held, !m.at.IsZero() && usable(m.held)
	age := m.clock().Sub(m.at)
	if have && age < servingRecheck {
		m.mu.Unlock()
		return held, nil
	}
	reading := m.reading
	if reading == nil {
		reading = make(chan struct{})
		m.reading = reading
		m.reads.Add(1)
		go m.refresh(ctx, read, reading)
	}
	m.mu.Unlock()
	if have && age <= stale {
		return held, nil
	}
	select {
	case <-reading:
	case <-ctx.Done():
		return zero, ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch {
	case !m.at.IsZero() && usable(m.held) && m.clock().Sub(m.at) <= stale:
		return m.held, nil
	case m.failed != nil:
		return zero, m.failed
	}
	return zero, errUnusableRead
}

// errUnusableRead is a read that answered with a value that still does not
// describe what was asked — a log's ends read while a reanchor moved its
// generation.
var errUnusableRead = errors.New("engine: the value read does not describe this " +
	"node's current position — its log moved generation while it was read")

// refresh makes the one read in flight and stores its answer stamped when it
// LANDED: stamped when it began, a read slower than [servingRecheck] would be
// stale the moment it was stored.
func (m *memo[T]) refresh(ctx context.Context, read func(context.Context) (T, error),
	done chan struct{}) {

	defer close(done)
	bounded, cancel := context.WithTimeout(ctx, servingRead)
	defer cancel()
	v, err := read(bounded)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reading = nil
	if err != nil {
		m.failed = err
		return
	}
	if landed := m.clock(); !landed.Before(m.at) {
		m.held, m.at, m.failed = v, landed, nil
	}
}

// streamEnds is one read of a log's two ends, and the position this node's rows
// were current through just BEFORE it — so the ends are at least as recent as
// that position.
type streamEnds struct {
	first, last uint64
	before      statelog.Position
}

// readEnds reads running's log's two ends, noting this node's REACHED position
// first, the one its health describes ([stateLog.observeWith]): every record
// through it is on the log, so ends read after it are at or past it.
func readEnds(ctx context.Context, running *runningLog) (streamEnds, error) {
	before := running.runner.Reached()
	first, last, err := running.log.Bounds(ctx)
	if err != nil {
		return streamEnds{}, err
	}
	return streamEnds{first: first, last: last, before: before}, nil
}

// freshEnds is running's log's ends read now — and stored for the reads after
// it ([runningLog.ends]).
func freshEnds(ctx context.Context, running *runningLog, _ statelog.Position) (streamEnds, error) {
	ends, err := readEnds(ctx, running)
	if err == nil {
		running.ends.store(ends, time.Now())
	}
	return ends, err
}

// servedEnds is running's log's ends as a read is served them ([memo.serve]):
// held, where they were read on the generation of the stream this node is at
// and no longer ago than [endsStale].
func servedEnds(ctx context.Context, running *runningLog, at statelog.Position) (streamEnds, error) {
	return running.ends.serve(ctx, endsStale, func(e streamEnds) bool {
		return e.before.Stream == at.Stream && e.before.Generation == at.Generation
	}, func(ctx context.Context) (streamEnds, error) {
		return readEnds(ctx, running)
	})
}

// floorMemos is one [memo] per domain of the fleet's published floors, each
// holding the last read of that domain's key that answered. The zero value
// holds none; a domain's memo is made the first time it is asked for.
//
// One a domain, because each floor is its own key and its own fault
// ([publishedFloors]): a shared memo refused every domain's reads
// `floor_unknown` once any one key failed, and read every key at each refresh.
type floorMemos struct {
	mu sync.Mutex
	by map[string]*memo[publishedFloors]
}

// of is domain's memo.
func (m *floorMemos) of(domain string) *memo[publishedFloors] {
	m.mu.Lock()
	defer m.mu.Unlock()
	held, ok := m.by[domain]
	if !ok {
		if m.by == nil {
			m.by = map[string]*memo[publishedFloors]{}
		}
		held = &memo[publishedFloors]{}
		m.by[domain] = held
	}
	return held
}

// servedFloor is domain's published floor as a read is served it
// ([memo.serve]), a read of that domain's key alone, unread past
// [statelog.FloorCacheStale] so a read refuses `floor_unknown` exactly when the
// alarm of that name says. How long a floor has gone unread is the alarm
// heartbeat's own observation ([floorWatch]), never a tally of these
// refreshes, whose rate is the read rate.
func (s *stateLog) servedFloor(ctx context.Context, domain string) (publishedFloors, error) {
	return s.floorMemos.of(domain).serve(ctx, statelog.FloorCacheStale,
		func(publishedFloors) bool { return true },
		func(ctx context.Context) (publishedFloors, error) {
			read := readFloorsOf(ctx, s.fleet, []string{domain})
			_, _, err := read.row(domain)
			return read, err
		})
}

// servedHealth is one domain's health as its read authority is served it:
// every term this node holds read now, and the published floors and the log's
// ends from memory.
//
// A floor nobody could refresh is not an error: the health carries
// [statelog.FloorUnknown] stamped with when the floor was last read (zero if
// never), so the read refuses `floor_unknown` naming how long ago, as the
// alarm does, with every other term still read. Anything else unread (ends
// past [endsStale], the eviction row) is an error, refused
// `broker_unreachable` ([statelog.ReaderDeps.Health]): folded into a zero
// floor, a silent stream leader would read as a floor never read.
func (s *stateLog) servedHealth(ctx context.Context, running *runningLog) (statelog.Health, error) {
	name := running.domain.Name()
	floors, unread := s.servedFloor(ctx, name)
	health, err := s.observeWith(ctx, running, func(ctx context.Context,
		at statelog.Position) (streamEnds, error) {
		return servedEnds(ctx, running, at)
	})
	if err != nil {
		return health, err
	}
	if unread != nil {
		// The age is on the memo's clock; hand on that age back from
		// now, the clock the read authority ages a floor by.
		health.Floor = statelog.Floor{State: statelog.FloorUnknown}
		if _, age, ok := s.floorMemos.of(name).last(); ok {
			health.Floor.ReadAt = time.Now().Add(-age)
		}
		return health, nil
	}
	return floored(health, name, floors)
}
