package tracker

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The two seams that answer "should this node be writing at all", and the one
// thing they have in common: both are asked BEFORE an append, and both are
// wrong in a way that is silent.

// Fence refuses a write this node must not make.
//
// # Why it reads THREE sources and not one
//
// An eviction is permitted only while the target's presence lease has LAPSED —
// at least forty-five seconds of coordination silence — so by construction the
// evicted node is the node whose coordination path is not answering. A fence
// that asked coordination would be asking the one source the situation has
// already broken.
//
// So it asks, cheapest first: a cached answer from the loop that reads the
// tombstones, then coordination itself, then THIS NODE'S OWN APPLIED EVICTION
// ROWS — which are written by its own applier from its own log and are the only
// source still fresh when everything else is wedged. Being wrong here is not a
// refused write: it is a stream of records every node drops while this one
// collects acknowledgements for them.
type Fence struct {
	db     *store.DB
	nodeID string

	// Cached is the coordination-derived answer, refreshed on the same
	// loop that reads the tombstones. It is an OPTIMISATION: it can be
	// stale, and the durable row below is what makes staleness safe.
	Cached func() (bool, bool)

	// Floor is the published trim floor, and Cursor this node's own
	// committed position. Both are needed by the one check that costs a
	// round trip.
	Floor  func(ctx context.Context) (uint64, error)
	Cursor func() statelog.Position
}

// NewFence builds the write fence for one node.
func NewFence(db *store.DB, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction.
//
// ON EVERY APPEND, and therefore answered from what this node already knows:
// the cached answer when it has one, and otherwise the durable row its own
// applier wrote. A coordination round trip here would put one on the hot path
// of every write in the company.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f.Cached != nil {
		if evicted, known := f.Cached(); known {
			return evicted, nil
		}
	}
	var from, readmitted sql.NullInt64
	err := f.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position FROM tracker_evictions
			WHERE node_id = ? AND log_stream = ?`,
			f.nodeID, trackerStream).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		// UNKNOWN IS NOT "NOT EVICTED". A store this node cannot read is
		// a node that cannot establish it may write, and publishing
		// anyway is how an evicted node collects acknowledgements for
		// records every peer drops.
		return false, fmt.Errorf("tracker: read this node's own eviction row: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 <= from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of ZERO is
// safe from this node.
//
// # Why this one pays for a fresh answer
//
// An expectation of zero says "this subject holds nothing". It is formed when
// the anchor is absent, and an anchor is absent for two very different
// reasons: the subject was never written, or it was written and the record has
// been TRIMMED away beneath this node. In the second case publishing at zero
// overwrites a mutation nothing can recover.
//
// What makes the first case safe is the floor: if the published trim floor is
// at or below this node's own cursor, then nothing was trimmed that this node
// has not already consumed, so an absent anchor really does mean an empty
// subject. That reading has to be FRESH — a cached floor is a floor that moved
// — and A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is a deliberate
// departure from the fail-open rule the delivery claim uses: failing open
// there is a duplicate delivery and is recoverable, failing open here is a
// lost update and is not.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	evicted, err := f.Evicted(ctx)
	if err != nil {
		return err
	}
	if evicted {
		return fmt.Errorf("tracker: this node is evicted, so a write at an "+
			"expectation of zero would be dropped by every peer: %w",
			statelog.ErrConflict)
	}
	if f.Floor == nil {
		return fmt.Errorf("tracker: no published trim floor is readable, so " +
			"this node cannot establish that an absent anchor means an empty " +
			"subject rather than a record trimmed beneath it")
	}
	floor, err := f.Floor(ctx)
	if err != nil {
		return fmt.Errorf("tracker: read the published trim floor: %w — a floor "+
			"that cannot be read is not a floor that is low, and publishing at "+
			"zero on the guess is a lost update nothing recovers", err)
	}
	if floor > cursor.Seq {
		return fmt.Errorf("tracker: the trim floor is at %d and this node has "+
			"consumed through %d, so an absent anchor may be a record trimmed "+
			"beneath it rather than a subject that was never written: %w",
			floor, cursor.Seq, statelog.ErrUnavailable)
	}
	return nil
}

// Gates answers whether a durable record produced rows on NO node.
//
// Without it the resolution rule reads "the operation ledger is empty, so
// somebody else won": the writer re-decides, republishes, is dropped by the
// same gate again, and burns its whole round budget to a conflict a model
// reads as a colleague editing the same object.
type Gates struct {
	db *store.DB
}

// NewGates builds the gate reader for one node.
func NewGates(db *store.DB) *Gates { return &Gates{db: db} }

// GatedAt reports the gate that dropped a record at a position.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject, writer string,
	p statelog.Position) (statelog.Reason, bool, error) {

	var reason statelog.Reason
	var gated bool
	err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		// THE DELETION GATE FIRST, because it is permanent where an
		// eviction can be reversed: a caller told "evicted" retries after
		// a readmission, and a caller told "deleted" never should.
		if ObjectKind(subj.Kind) == KindTask {
			var purged int
			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM tracker_deletions WHERE task_id = ?`,
				subj.ID).Scan(&purged); err != nil {
				return fmt.Errorf("tracker: read the deletion gate: %w", err)
			}
			if purged > 0 {
				reason, gated = statelog.ReasonDeleted, true
				return nil
			}
		}
		if writer == "" {
			return nil
		}
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position FROM tracker_evictions
			WHERE node_id = ? AND log_stream = ?`,
			writer, p.Stream).Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("tracker: read the eviction gate: %w", err)
		}
		at := p.Packed()
		if from.Valid && at > from.Int64 &&
			(!readmitted.Valid || at < readmitted.Int64) {
			reason, gated = statelog.ReasonEvicted, true
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return reason, gated, nil
}

// AdoptedAt is when this node's adoption of a donated snapshot completed.
//
// The operation ledger is this node's own and is SCRUBBED from every donated
// snapshot, so an op id minted before this instant cannot be answered for here
// at all — and reading its absence as "somebody else won" would re-decide
// against a row that moved because of this very write.
func (g *Gates) AdoptedAt(ctx context.Context) (time.Time, bool, error) {
	var completed sql.NullInt64
	// THE NODE'S OWN ESTATE, not the replicated one: an adoption record is
	// a fact about THIS machine's history, and a donated snapshot must not
	// carry the recipient's own.
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx,
			`SELECT completed_at FROM statelog_adoption WHERE id = 'current'`).
			Scan(&completed)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return time.Time{}, false, nil
	case err != nil:
		return time.Time{}, false, fmt.Errorf("tracker: read this node's "+
			"adoption record: %w", err)
	case !completed.Valid:
		// AN INCOMPLETE ADOPTION IS NOT AN ABSENT ONE. A node mid-join
		// has a scrubbed ledger and no instant to compare against, so
		// the honest answer is that it cannot answer for any op id.
		return time.Time{}, false, nil
	}
	return store.DecodeTime(completed.Int64), true, nil
}
