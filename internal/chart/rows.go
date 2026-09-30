package chart

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE THREE SEAMS A WRITE AUTHORITY IS BUILT FROM, and the one thing each is
// for.
//
// [statelog.NewPublisher] refuses a nil Rows, Fence or Gates by name, which is
// what makes a domain with a half-built write authority a boot failure rather
// than a node that appends records the fleet drops one at a time. This file is
// the chart's three.

// NewRows builds the framework's row seam over this domain's guards.
func NewRows(db *store.DB) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, chartGuards)
}

// chartGuards answers the two object-level facts a first write needs.
//
// THE TOMBSTONE IS PERMANENT WHERE A GUARDING ROW IS NOT: an object below the
// trim floor has no record left on the log to prove it ever existed, and its
// own row is what still says so — while a removal's tombstone outlives the row
// itself and is what makes the removal irreversible.
//
// TWO KINDS HAVE THEM, and they are the two that are objects: a unit and a
// seat. Every other kind here is created by its first record and removed by
// nothing — the structure is a subject rather than a row, a key claim is an
// address, and a barrier writes nothing at all — so answering false for both
// is the correct answer rather than an omission.
func chartGuards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (
	deleted, guard bool, err error) {

	var table, column string
	switch ObjectKind(subj.Kind) {
	case KindUnit:
		table, column = "chart_units", "key"
	case KindSeat:
		table, column = "chart_seats", "handle"
	default:
		return false, false, nil
	}
	var removed, present int
	err = tx.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM chart_removed
			 WHERE object_kind = ? AND object_id = ?),
			(SELECT COUNT(*) FROM `+table+` WHERE `+column+` = ?)`,
		subj.Kind, subj.ID, subj.ID).Scan(&removed, &present)
	if err != nil {
		return false, false, fmt.Errorf("chart: read the guards on %s %s: %w",
			subj.Kind, subj.ID, err)
	}
	return removed > 0, present > 0, nil
}

// ReadScope is the closure a READ is about.
//
// It is the same alphabet a record's own scope resolves into, which is what
// makes the coverage probe one comparison rather than a translation between two
// vocabularies. A read that names nothing is the DOMAIN — not because it
// touches everything, but because a read that cannot say what it is about is
// one every deferred record concerns, and the honest answer to "is this
// complete" is then "no".
func ReadScope(unit, seat string) statelog.ScopeSet {
	switch {
	case seat != "":
		return statelog.ScopeSet{Paths: []string{
			ScopeTerm{Kind: TermSeat, Unit: unit, ID: NormalizeKey(seat)}.Path(),
		}}
	case unit != "":
		return statelog.ScopeSet{Paths: []string{
			ScopeTerm{Kind: TermUnit, ID: NormalizeKey(unit)}.Path(),
		}}
	}
	return statelog.ScopeSet{Paths: []string{RootPath()}}
}

// Fence refuses a write this node must not make.
//
// THE SAME TWO PRICES ITS SIBLINGS PAY, because this domain installs the same
// gate: Evicted runs on every append and is answered from rows this node
// already has, and ClearForZero pays a coordination round trip because being
// wrong there is a LOST UPDATE rather than a duplicate.
type Fence struct {
	db     *store.DB
	nodeID string

	// Floor is the fleet's published trim floor at a generation and Ends
	// the log's two ends, read live, and Committed this node's applier's
	// live checkpoint — the three reads [statelog.ZeroFence] takes beside
	// the eviction. The cursor the floor is compared against is passed per
	// call: the checkpoint the write's own snapshot read
	// ([statelog.Snap.Checkpoint]), never the applier's live position, for
	// the reason the tracker's fence gives. Set by the engine after the
	// runner exists, because a fence built before its applier would compare
	// against a position that does not move.
	Floor     func(ctx context.Context, generation uint32) (uint64, error)
	Ends      func(ctx context.Context) (statelog.LogEnds, error)
	Committed func() statelog.Position
}

// NewFence builds it.
func NewFence(db *store.DB, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction, FROM ITS OWN APPLIED ROWS.
//
// Not from coordination, which is the point: a wedged coordination path is a
// precondition of an eviction being permitted at all, so the source that is
// still fresh in exactly that failure is this node's own replicated table.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f == nil || f.db == nil || f.nodeID == "" {
		return false, nil
	}
	var from, readmitted sql.NullInt64
	// THROUGH THE HANDLE, NOT ITS POOL. `DB.SQL()` answers a NIL pool on a
	// replicated estate that is not open — a legitimate, documented state
	// of that peer, since an adoption closes it between its rename and its
	// reopen — and a statement issued on it panics inside database/sql.
	// [store.DB.Read] answers [store.ErrNoEstate], which the refusal below
	// already handles as the honest "unreadable is not not-evicted".
	err := f.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position
			FROM chart_evictions WHERE node_id = ?`, f.nodeID).
			Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		// UNREADABLE IS NOT "not evicted". A fence that failed open on a
		// store it could not read is a node appending records every
		// other node drops, collecting acknowledgements for writes that
		// happen nowhere.
		return false, fmt.Errorf("chart: read this node's own eviction: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 < from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of ZERO is
// safe from this node.
//
// A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is the opposite of the
// fail-open rule a delivery claim follows: failing open there costs a duplicate
// notification, which is recoverable, and failing open here costs a CLAIM that
// overwrites one another node already made — which is not.
//
// IT IS THE KEY CLAIM THAT NEEDS THIS. A rekey publishes at zero on the new
// key's own subject, because a key that has never been claimed has no anchor
// — and a key whose claim has been TRIMMED AWAY has no anchor either. The two
// are indistinguishable from the log alone, so a node below the floor must not
// guess.
//
// THE CHECK IS [statelog.ZeroFence], the one every sibling's fence runs: this
// supplies the four reads. Written out here it refused an evicted node with
// [statelog.ErrConflict], which a caller reads as a colleague editing and
// retries into, and a node below the floor with prose rather than a reason;
// every refusal is now an [*statelog.Unavailable] naming it.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.ZeroFence{
		Evicted: f.Evicted, Floor: f.Floor, Ends: f.Ends, Committed: f.Committed,
	}.ClearForZero(ctx, cursor)
}

// Gates answers whether a durable record produced rows on NO node.
//
// THE SAME TWO GATES [Applier.Gated] INSTALLS, read from the publisher's side.
// The two must agree: a resolution that looked for a gate the applier never
// installs would read every unapplied record as "somebody else won".
type Gates struct{ db *store.DB }

// NewGates builds it.
func NewGates(db *store.DB) *Gates { return &Gates{db: db} }

// GatedAt reports whether a record at p applies nowhere, and the gate that
// answers for it, by the rule [statelog.Gates] states — which every sibling's
// reader keeps too, and statelogtest.RunGates certifies for each.
//
// The record's body is not read: a content record is published on its
// object's own subject, which names what the removal gate is keyed on.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position, _ []byte) (statelog.Reason, bool, error) {

	if g == nil || g.db == nil {
		return "", false, nil
	}
	var reason statelog.Reason
	var gated bool
	// ONE TRANSACTION FOR BOTH GATES, on ONE load of the replicated peer,
	// and through the HANDLE rather than its pool — see [Fence.Evicted] for
	// why a nil pool is reachable here. Two reads would be two instants, and
	// possibly two FILES: every `Replicated()` reloads the peer pointer an
	// adoption swaps, and the applier commits between any two reads — so a
	// removal committing between them made one half of the answer describe
	// the estate before it and the other the estate after it.
	err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		// THE DELETION GATE FIRST, by the rule [statelog.Gates] states: a
		// removal's tombstone holds the object for every writer for ever,
		// where an eviction is one writer's and a readmission ends it, so
		// `deleted` is the answer that stays true. A record both gates drop
		// is dropped either way, so the order decides only which one is
		// REPORTED.
		if ref, ok := gatedObject(statelog.Record{Subject: subj}); ok {
			var author sql.NullString
			err := tx.QueryRowContext(ctx, `
				SELECT record_id FROM chart_removed
				WHERE object_kind = ? AND object_id = ?`,
				string(ref.Kind), ref.ID).Scan(&author)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("chart: read the removal gate for %s: %w", ref, err)
			case author.Valid && author.String == opID:
				// THE RECORD THAT REMOVED THE OBJECT IS NOT GATED BY ITS
				// OWN TOMBSTONE, by its own id: without the exception a
				// removal whose acknowledgement was lost resolves as
				// "applied nowhere", and its caller is told the removal
				// did not happen when it did. It falls through to the
				// eviction gate, because this says only that THIS gate
				// did not drop the record.
			default:
				reason, gated = statelog.ReasonDeleted, true
				return nil
			}
		}
		if writer == "" {
			return nil
		}
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position
			FROM chart_evictions WHERE node_id = ?`, writer).
			Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("chart: read the eviction gate for node %s: %w",
				writer, err)
		}
		// THE APPLIER'S WINDOW, spelled as [Applier.Gated] spells it.
		at := p.Packed()
		evicted := from.Valid && at > from.Int64
		back := readmitted.Valid && at >= readmitted.Int64
		if evicted && !back {
			reason, gated = statelog.ReasonEvicted, true
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return reason, gated, nil
}
