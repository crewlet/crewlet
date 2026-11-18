package gitlettest

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Clock is a fleet's clock as a case moves it: the instant its data nodes'
// lifecycle stores decide by (gitlet.StoreOptions.Now) and the broker's
// instant on every record they apply after a move; its zero value is the wall
// clock. Both move together, as a month passing moves them: a release frees a
// name only where its record's broker instant is past the redirect's end, so
// moving the deciding clock alone frees nothing. The Gitlet nodes, the
// client's git and the instant a case hands Fleet.Retain keep theirs.
//
// A move is a fact about records, not about when they apply: only records past
// the furthest one applied at the move read it, so a replay after a restart
// reads each instant as before. A case levels the fleet (Fleet.Level) before
// it moves the clock, so no record appended earlier is still unapplied.
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
