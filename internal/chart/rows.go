package chart

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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

	// Cursor is this node's committed position, and Floor the published
	// trim floor. Both are set by the engine after the runner exists,
	// because a fence built before its applier would compare against a
	// position that does not move.
	Cursor func() statelog.Position
	Floor  func(ctx context.Context) (uint64, error)
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
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	if f == nil {
		return nil
	}
	evicted, err := f.Evicted(ctx)
	if err != nil {
		return err
	}
	if evicted {
		return fmt.Errorf("chart: this node is evicted, so a write at an "+
			"expectation of zero would be dropped by every peer: %w",
			statelog.ErrConflict)
	}
	if f.Floor == nil {
		// NO FLOOR SEAM IS A FENCE THAT CANNOT ANSWER, and a fence that
		// cannot answer refuses. A publisher built without one would
		// otherwise publish every claim at zero against a log whose
		// beginning it has never seen.
		return fmt.Errorf("chart: no published trim floor is readable, so this " +
			"node cannot establish that an absent anchor means an unclaimed " +
			"key rather than a claim trimmed beneath it")
	}
	floor, err := f.Floor(ctx)
	if err != nil {
		return fmt.Errorf("chart: read the published trim floor: %w — a floor "+
			"that cannot be read is not a floor that is low, and publishing at "+
			"zero on the guess is a lost update nothing recovers", err)
	}
	// THE FLOOR IS THE FIRST SEQUENCE THE TRIM HAS NOT LICENSED REMOVING,
	// so a node that has consumed through the one before it has consumed
	// everything that may be gone — the same comparison both siblings make,
	// for the same reason.
	if floor > cursor.Seq+1 {
		return fmt.Errorf("chart: the trim may have removed everything below %d "+
			"and this node has consumed through %d, so an absent anchor may be a "+
			"claim trimmed beneath it rather than a key that was never taken: %w",
			floor, cursor.Seq, statelog.ErrUnavailable)
	}
	return nil
}

// Gates reads the same two gates [Applier.Gated] does, from OUTSIDE a record's
// own transaction: it is what a writer asks before it publishes, where the
// applier asks after the broker has committed.
type Gates struct{ db *store.DB }

// NewGates builds it.
func NewGates(db *store.DB) *Gates { return &Gates{db: db} }

// AdoptedAt is when this node's replicated estate last came from a peer.
//
// THE PUBLISHER ASKS BECAUSE AN ADOPTION BREAKS THE RESOLUTION. Resolving an
// ambiguous publish means looking for the record's own effect in this node's
// rows, and rows that arrived in somebody else's snapshot carry effects this
// node never applied — so a write that landed after the adoption's own instant
// is the only one that can be resolved from them.
func (g *Gates) AdoptedAt(ctx context.Context) (time.Time, bool, error) {
	if g == nil || g.db == nil {
		return time.Time{}, false, nil
	}
	return statelog.AdoptedAt(ctx, g.db)
}

// GatedAt reports the gate that dropped a record at p.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position) (statelog.Reason, bool, error) {

	if g == nil || g.db == nil {
		return "", false, nil
	}
	// ONE TRANSACTION FOR BOTH GATES, and through the HANDLE rather than
	// its pool — see [Fence.Evicted] for why a nil pool is reachable here.
	// One transaction rather than two statements is the same rule every
	// multi-statement answer in this package follows: the two gates would
	// otherwise be read at two instants, and a record could be reported
	// ungated by an eviction that had landed between them.
	if writer != "" {
		var from, readmitted sql.NullInt64
		err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `
				SELECT from_position, readmitted_position
				FROM chart_evictions WHERE node_id = ?`, writer).
				Scan(&from, &readmitted)
		})
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return "", false, fmt.Errorf("chart: read the eviction gate for "+
				"node %s: %w", writer, err)
		default:
			at := p.Packed()
			evicted := from.Valid && at > from.Int64
			back := readmitted.Valid && at >= readmitted.Int64
			if evicted && !back {
				return statelog.ReasonEvicted, true, nil
			}
		}
	}
	ref, ok := gatedObject(statelog.Record{Subject: subj})
	if !ok {
		return "", false, nil
	}
	var author sql.NullString
	err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT record_id FROM chart_removed
			WHERE object_kind = ? AND object_id = ?`,
			string(ref.Kind), ref.ID).Scan(&author)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("chart: read the removal gate for %s: %w",
			ref, err)
	}
	if author.Valid && author.String == opID {
		return "", false, nil
	}
	return statelog.ReasonDeleted, true, nil
}
