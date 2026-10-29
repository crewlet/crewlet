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
// Every read a domain's read authority serves asks that domain's health first,
// and again after any read that waited ([statelog.ReaderDeps.Health]). Most of
// a health is this node's own and costs nothing to read: its reached
// position, its applier's stop and fault, what it holds deferred, its eviction
// row, whether its stream is the one its rows are keyed to. TWO TERMS ARE NOT:
// the fleet's published trim floors are a leader read of each registered
// domain's key in a coordination bucket, and the log's two ends are a
// stream-info request to that stream's leader, made on a handle whose mutex
// serialises every caller. Read on every read, they put the company's whole
// read rate on the coordination stream's leader, N nodes over, and queued
// every read of a domain on one node behind one stream-info round trip at a
// time — while the stale level promised a read that "takes no broker call at
// all".
//
// So a read is served both from memory ([memo]): the last value read, refreshed
// OFF the read's path, ONE read at a time, once it is older than
// [servingRecheck] — the request gate's own recheck, so a node pays at most one
// read of each domain's floor and one stream-info request per log a second
// however many reads it serves — and every fresh read anything else makes (the
// copy's verdict, admission, the heartbeat's reports) refreshes it too. The
// floors are memoised PER DOMAIN ([floorMemos]), each refreshed by a read of
// that domain's key alone, because one domain's floor that cannot be read is
// that domain's refusal and no other's: memoised together, a row that does not
// decode under one key held every domain's reads to the age of a read that
// never answered. MEASURED
// (BenchmarkAReadsReadiness, one node, the tracker's read authority at the
// stale level): 1.11 ms and 1.01 broker messages a read when both were asked
// per read, 0.12 ms and 0.001 served from memory.
//
// # How old each may be, and why the two bounds differ
//
// What a value that old can get wrong is bounded the way the verdict's is: the
// floors only rise within a generation and a stale log end is lower than the
// truth, so a read may be served a second longer by a node that just fell
// behind the published floor, or report a second's records less lag — never a
// hole, which the stream's own first record decides, and never a position
// past the end that was not one (see [observeWith]). A value nobody could
// refresh AGES, and each term ages at a bound of its own:
//
//   - THE FLOORS at [statelog.FloorCacheStale], four heartbeats, each
//     domain's on its own: its instant is the [statelog.Floor] a health
//     carries, so a domain's reads refuse `floor_unknown` exactly when the
//     alarm of that name says they do, naming that domain.
//   - THE ENDS at [endsStale], the oldest a WORKING refresh leaves them. The
//     lag a `max_lag_seq` or `max_lag_seconds` read is checked against is
//     derived from them, and a lag a minute old is no bound at all: a node
//     thousands of records behind under a partial partition, served as within
//     the caller's few hundred. Past the bound they are unread, and a health
//     whose ends are unread refuses every read — which is what it did when the
//     ends were read per read and the read failed.
//
// FRESH READS STAY WHERE AN UNREAD VALUE IS A LOST UPDATE: the write fence's
// expectation of zero reads the floor and the log's ends itself
// ([stateLog.trimFloor], [stateLog.logEndsOf]), and so does the trim duty. The
// fence's floor read is on the write's path and stays there — it must follow
// the write's own read of its subject, the floor theorem's order — so it is
// the cheapest read that order allows: its own domain's key and no other,
// never this memo, and never a listing of the register.

// endsStale is how old a log's ends may be and still be served to a read: one
// [servingRecheck] before a read starts their refresh, plus the [servingRead]
// that refresh is bounded by — the oldest a working refresh can leave them.
// Ends any older are ends nobody could refresh, which are unread.
//
// NOT [statelog.FloorCacheStale], which is the floor's own bound and is a
// minute for the floor's own reason — four heartbeats before an unread register
// stops counting as a slow one. The ends answer a different question at a
// different rate: a bounded read's lag is derived from them, a log can run on
// by thousands of records in a minute, and a minute-old end served that read a
// lag nobody had measured since — a node that had fallen far behind, served as
// within the bound its caller declared.
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

	// reads counts the reads this memo made itself, which is what a read
	// path costs the broker.
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

// store records v, read elsewhere and landed at at: a fresh read is as good
// as the memo's own, and keeps the memo from making one. A read that landed
// before the one held replaces nothing.
func (m *memo[T]) store(v T, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if at.Before(m.at) {
		return
	}
	m.held, m.at, m.failed = v, at, nil
}

// last is the last value that answered and how long ago it landed, by the
// memo's own clock, however old — false where nothing ever answered. It is
// what a term nobody could refresh is reported with: the value is past
// serving, and its age is still a fact worth naming.
func (m *memo[T]) last() (T, time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.at.IsZero() {
		var zero T
		return zero, 0, false
	}
	return m.held, m.clock().Sub(m.at), true
}

// serve is the value a read is answered with:
//
//   - one USABLE and younger than [servingRecheck] is answered as held;
//   - one usable and no older than stale is answered as held, and ONE read is
//     started to replace it, which nobody waits for;
//   - otherwise — nothing held, nothing usable, or a value nobody could
//     refresh for the whole stale bound — the read waits for that one read,
//     no longer than ctx, and answers its failure where it did not answer.
//
// stale is the VALUE'S bound, never the memo's: how old the term may be before
// it stops describing what it describes ([statelog.FloorCacheStale] for the
// floors, [endsStale] for the ends). usable says whether a held value describes
// what the caller is asking about at all (a log's ends read at another
// generation do not).
//
// ctx is the LIFE of what is served — the node's own, which a read's
// readiness is asked under ([stateLog.readerFor]) — and never one request's:
// the read it starts is shared by every read after it, so a request that gave
// up must not become every read's failure. THE READ IS NEVER MADE UNDER THE
// LOCK, and it is bounded by [servingRead], so twenty reads behind an
// unreachable broker neither queue one after another nor wait past the bound.
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

// refresh makes the one read in flight and stores what it answered, stamped
// with the instant it LANDED: stamped with the instant it began, a read slower
// than [servingRecheck] would be stale the moment it was stored.
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

// readEnds reads running's log's two ends, noting this node's position first:
// its REACHED position, the one the health it is read for describes
// ([stateLog.observeWith]) — every record through it is on the log, so ends
// read after it are at or past it.
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
// holds none, and a domain's memo is made the first time anything asks for it.
//
// ONE A DOMAIN, because each domain's floor is its own key and its own fault
// ([publishedFloors]): one memo for every domain kept a refresh only when all
// of them answered, so a read of the tracker refused `floor_unknown` once the
// knowledge base's key had failed for [statelog.FloorCacheStale], and a read
// of one domain paid a key read of every other one at each refresh.
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
// ([memo.serve]) — a read of that domain's key alone. How long a floor has
// gone unread is the alarm heartbeat's own observation ([floorWatch]), never
// a tally of these refreshes, whose rate is whatever the read rate is.
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
// # Two failures, two different answers
//
// A FLOOR NOBODY COULD REFRESH IS NOT AN ERROR: it is a health whose floor is
// [statelog.FloorUnknown], stamped with the instant the floor was last read
// (zero where it never was), so the read refuses `floor_unknown` naming how
// long ago — what the alarm of that name says, and what an operator can act
// on. Everything else this health carries was still read.
//
// Anything else that could not be read — the log's ends past [endsStale], the
// eviction row — IS an error, which the read authority refuses
// `broker_unreachable` naming it ([statelog.ReaderDeps.Health]). Before the
// two were told apart, both were folded into a zero health whose zero floor
// was refused first: a stream leader that stopped answering read as a floor
// "never read on this node", and a floor last read two minutes ago read the
// same.
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
		// The age is the memo's, measured on its clock, and the
		// instant handed on is that age back from now — the one clock
		// the read authority ages a floor by.
		health.Floor = statelog.Floor{State: statelog.FloorUnknown}
		if _, age, ok := s.floorMemos.of(name).last(); ok {
			health.Floor.ReadAt = time.Now().Add(-age)
		}
		return health, nil
	}
	return floored(health, name, floors)
}
