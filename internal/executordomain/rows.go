package executordomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The publisher's three seams: what it reads, what refuses a write it must not
// make, and what tells a dropped record from a lost race.

// NewRows builds the publisher's read seam over the log spec names.
//
// NO GUARDS, and that is the domain rather than an omission: the guards answer
// a first-writer-wins create ([statelog.PatternCreate]), and nothing on this
// log is created that way. A conversation and a seat are not objects anything
// creates, only subjects records arbitrate on, and a turn's tombstone is read
// by its own records' decides and applies ([readTomb]).
func NewRows(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, spec, nil)
}

// Fence refuses a write this node must not make: Evicted on every append, from
// rows this node already has, and ClearForZero before an expectation of zero,
// which pays a coordination round trip because being wrong there is a lost
// update.
type Fence struct {
	db     store.ReplicatedReader
	nodeID string

	// Floor is the fleet's published trim floor at a generation, Ends the
	// log's two ends read live, and Committed this node's applier's live
	// checkpoint: the reads [statelog.ZeroFence] takes.
	Floor     func(ctx context.Context, generation uint32) (uint64, error)
	Ends      func(ctx context.Context) (statelog.LogEnds, error)
	Committed func() statelog.Position
}

// NewFence builds it.
func NewFence(db store.ReplicatedReader, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction, FROM ITS OWN APPLIED ROWS, which
// are the one source still fresh when coordination is wedged. UNREADABLE IS NOT
// "not evicted": a fence that failed open would collect acknowledgements for
// writes every other node drops.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f == nil || f.db.IsZero() || f.nodeID == "" {
		return false, nil
	}
	var from, readmitted sql.NullInt64
	err := f.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position FROM executor_evictions
			WHERE node_id = ?`, f.nodeID).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("executordomain: read this node's own eviction: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 < from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of zero is
// safe from this node ([statelog.ZeroFence], the check every strict domain's
// fence runs).
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.ZeroFence{
		Evicted: f.Evicted, Floor: f.Floor, Ends: f.Ends, Committed: f.Committed,
	}.ClearForZero(ctx, cursor)
}

// Gates answers whether a durable record produced rows on NO node: the one
// gate [Applier.Gated] installs, a writer's eviction window, read from the
// publisher's side by the rule [statelog.Gates] states.
type Gates struct{ db store.ReplicatedReader }

// NewGates builds it.
func NewGates(db store.ReplicatedReader) *Gates { return &Gates{db: db} }

// GatedAt reports whether a record at p applies nowhere: a record its writer
// published inside that writer's eviction window, whatever its kind — the
// window is a fact about the WRITER. In one transaction through the handle,
// so the answer describes one committed state.
func (g *Gates) GatedAt(ctx context.Context, _ statelog.Subject,
	writer, _ string, p statelog.Position, _ []byte) (statelog.Reason, bool, error) {

	if g == nil || g.db.IsZero() || writer == "" {
		return "", false, nil
	}
	var evicted bool
	if err := g.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		evicted, err = evictedAt(ctx, tx, writer, p.Packed())
		return err
	}); err != nil {
		return "", false, err
	}
	if evicted {
		return statelog.ReasonEvicted, true, nil
	}
	return "", false, nil
}
