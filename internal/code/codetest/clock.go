package codetest

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Clock is a fleet's clock as a case moves it on: the instant its data nodes'
// lifecycle stores decide by (code.StoreOptions.Now), and the broker's own
// instant on every record those nodes apply from the moment it was moved.
//
// WHY BOTH, AND WHY THE BROKER'S AT ALL: what a month does to a repository is
// judged twice, alike on every node — the node that decides a release reads
// its own clock, and the record it publishes frees the name only where the
// record's BROKER instant is past the redirect's end, so a release decided on
// a clock ahead of the broker's frees nothing. A case that moved the deciding
// clock alone would publish a release every node applies as nothing; one that
// waited out code.RedirectWindow would wait thirty days. So the clock moves
// the two together, as a month passing moves them, and nothing else: the git
// nodes, the client's git and the retention pass's own instant (which a case
// hands Fleet.Retain) keep theirs.
//
// A MOVE IS A FACT ABOUT RECORDS, never about when they are applied: every
// record past the furthest one the node had applied when the clock moved
// reads the move, every record at or before it reads none — so a node that
// replays its log after a restart reads each record's instant as it read it
// the first time. A case levels the fleet (Fleet.Level) before it moves the
// clock, so no record appended before the move is still unapplied.
//
// The zero value is the wall clock, moved by nothing.
type Clock struct {
	mu sync.Mutex
	// ahead is how far the clock has been moved in all.
	ahead time.Duration
	// applied is the furthest record the wrapped applier has applied, by its
	// packed position.
	applied int64
	// moves are the moves, each from the position past which it is read, in
	// the order they were made.
	moves []clockMove
}

// clockMove is one move of the clock: every record past after reads ahead.
type clockMove struct {
	after int64
	ahead time.Duration
}

// Now is the clock's instant, in UTC.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().UTC().Add(c.ahead)
}

// Elapse moves the clock on by d: the lifecycle stores decide d later from
// now on, and every record appended from now on is applied as though the
// broker had stamped it d later than it did.
func (c *Clock) Elapse(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ahead += d
	c.moves = append(c.moves, clockMove{after: c.applied, ahead: c.ahead})
}

// aheadAt is how far the broker's instant on the record at packed reads ahead
// of the broker's own, and notes the record as applied.
func (c *Clock) aheadAt(packed int64) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var ahead time.Duration
	for _, m := range c.moves {
		if packed > m.after {
			ahead = m.ahead
		}
	}
	c.applied = max(c.applied, packed)
	return ahead
}

// wrap is a's applier reading every record's broker instant through the
// clock: the instant its rows are stamped with, and the one its gates count a
// rejection at.
func (c *Clock) wrap(a statelog.Applier) statelog.Applier { return clockedApplier{Applier: a, c: c} }

// clockedApplier is an applier reading the broker's instant through a Clock.
type clockedApplier struct {
	statelog.Applier
	c *Clock
}

func (a clockedApplier) Gated(ctx context.Context, tx *sql.Tx, rec statelog.Record) (statelog.Reason, bool,
	error) {
	rec.StoredAt = rec.StoredAt.Add(a.c.aheadAt(rec.Position.Packed()))
	return a.Applier.Gated(ctx, tx, rec)
}

func (a clockedApplier) Apply(ctx context.Context, tx *sql.Tx, rec statelog.Record,
	opts statelog.ApplyOptions) (int, error) {
	ahead := a.c.aheadAt(rec.Position.Packed())
	rec.StoredAt, opts.StoredAt = rec.StoredAt.Add(ahead), opts.StoredAt.Add(ahead)
	return a.Applier.Apply(ctx, tx, rec, opts)
}
