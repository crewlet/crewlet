package atlasdomain

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

// NewRows builds the publisher's read seam over the log spec names, with the
// guards a write needs.
func NewRows(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, spec, guards)
}

// guards answers whether an entry or a value carries a purge marker, which is
// permanent. No write here is a first-writer-wins create, so nothing guards one.
func guards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (deleted, guard bool, err error) {
	if !ObjectKind(subj.Kind).Purgeable() {
		return false, false, nil
	}
	_, deleted, err = heldBy(ctx, tx, markersOf(subj, nil), "")
	return deleted, false, err
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
			SELECT from_position, readmitted_position FROM atlas_evictions
			WHERE node_id = ?`, f.nodeID).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("atlasdomain: read this node's own eviction: %w", err)
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

// Gates answers whether a durable record produced rows on NO node: the two
// gates [Applier.Gated] installs, read from the publisher's side.
type Gates struct{ db store.ReplicatedReader }

// NewGates builds it.
func NewGates(db store.ReplicatedReader) *Gates { return &Gates{db: db} }

// GatedAt reports whether a record at p applies nowhere: THE PURGE MARKER
// FIRST, the purge's own record exempt by its operation id, then the writer's
// eviction window, in one transaction. A marker is a fact about an object no
// readmission lifts, where the window is a fact about one writer. With the
// record's body it reads every marker a claim is held to; without, the subject's.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position, body []byte) (statelog.Reason, bool, error) {

	if g == nil || g.db.IsZero() {
		return "", false, nil
	}
	var reason statelog.Reason
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		reason = ""
		_, held, err := heldBy(ctx, tx, markersOf(subj, body), opID)
		if err != nil {
			return err
		}
		if held {
			reason = statelog.ReasonDeleted
			return nil
		}
		if writer == "" {
			return nil
		}
		evicted, err := evictedAt(ctx, tx, writer, p.Packed())
		if err != nil {
			return err
		}
		if evicted {
			reason = statelog.ReasonEvicted
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return reason, reason != "", nil
}
