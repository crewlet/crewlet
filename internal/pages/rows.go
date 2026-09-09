package pages

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The publisher's three seams: what it reads, what refuses a write it must not
// make, and what tells a dropped record from a lost race.

// NewRows builds the publisher's read seam.
func NewRows(db *store.DB) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, pageGuards)
}

// pageGuards answers the two object-level facts a first write needs.
//
// THE DELETION MARKER IS PERMANENT WHERE A GUARDING ROW IS NOT: a page below
// the trim floor has no record left on the log to prove it existed, and its own
// row is what still says so — while a purge's marker outlives the row itself
// and is what makes the removal irreversible.
//
// TWO KINDS HAVE THEM, not one. A PAGE has both, on the tracker's terms. A
// TITLE has a guarding row and no marker: the claim row is what still says an
// address is taken once the create that took it has been trimmed away, which is
// the whole reason a create publishes at an expectation of zero and the
// guarding row is what protects the retry-at-zero branch. Releasing a title is
// a rename, not a destruction, so there is nothing permanent to mark.
//
// Every other object here is created by its first record and removed by
// nothing, so answering false for both is the correct answer rather than an
// omission.
func pageGuards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (
	deleted, guard bool, err error) {

	switch ObjectKind(subj.Kind) {
	case KindPage:
		var removed, present int
		err = tx.QueryRowContext(ctx, `
			SELECT
				(SELECT COUNT(*) FROM pages_deletions WHERE page_id = ?),
				(SELECT COUNT(*) FROM pages_heads WHERE id = ?)`,
			subj.ID, subj.ID).Scan(&removed, &present)
		if err != nil {
			return false, false, fmt.Errorf("pages: read the guards on page "+
				"%s: %w", subj.ID, err)
		}
		return removed > 0, present > 0, nil
	case KindTitle:
		container, token, splitErr := SplitTitleID(subj.ID)
		if splitErr != nil {
			return false, false, splitErr
		}
		var held int
		err = tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pages_titles WHERE container = ? AND title_token = ?`,
			container, token).Scan(&held)
		if err != nil {
			return false, false, fmt.Errorf("pages: read the guard on the "+
				"address %s/%s: %w", container, token, err)
		}
		return false, held > 0, nil
	}
	return false, false, nil
}

// ReadScope is the closure a READ is about.
//
// It is the same alphabet a record's own scope resolves into, which is what
// makes the coverage probe one comparison rather than a translation between two
// vocabularies. A query with no container is the DOMAIN — not because it
// touches everything, but because a read that cannot say what it is about is
// one every deferred record concerns, and the honest answer to "is this
// complete" is then "no".
func ReadScope(container, pageID string) statelog.ScopeSet {
	switch {
	case pageID != "":
		return statelog.ScopeSet{Paths: []string{
			ScopeTerm{Kind: TermObject, Container: container, ID: pageID}.Path(),
		}}
	case container != "":
		return statelog.ScopeSet{Paths: []string{
			ScopeTerm{Kind: TermContainer, ID: container}.Path(),
		}}
	}
	return statelog.ScopeSet{Paths: []string{ScopeTerm{Kind: TermDomain}.Path()}}
}

// Fence refuses a write this node must not make.
//
// THE SAME TWO PRICES THE TRACKER'S PAYS, because this domain installs the
// same gate: Evicted runs on every append and is answered from rows this node
// already has, and ClearForZero pays a coordination round trip because being
// wrong there is a lost update rather than a duplicate.
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
	err := f.db.Replicated().SQL().QueryRowContext(ctx, `
		SELECT from_position, readmitted_position
		FROM pages_evictions WHERE node_id = ?`, f.nodeID).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		// UNREADABLE IS NOT "not evicted". A fence that failed open on a
		// store it could not read is a node appending records every
		// other node drops, collecting acknowledgements for writes that
		// happen nowhere.
		return false, fmt.Errorf("pages: read this node's own eviction: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 < from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of ZERO is
// safe from this node.
//
// A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is a deliberate departure from
// the fail-open rule a delivery claim uses: failing open there is a duplicate
// delivery, which is recoverable; failing open here is a lost update, which is
// not.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	evicted, err := f.Evicted(ctx)
	if err != nil {
		return err
	}
	if evicted {
		return fmt.Errorf("pages: this node is evicted, so a write at an "+
			"expectation of zero would be dropped by every peer: %w",
			statelog.ErrConflict)
	}
	if f.Floor == nil {
		return fmt.Errorf("pages: no published trim floor is readable, so this " +
			"node cannot establish that an absent anchor means an unclaimed " +
			"address rather than a claim trimmed beneath it")
	}
	floor, err := f.Floor(ctx)
	if err != nil {
		return fmt.Errorf("pages: read the published trim floor: %w — a floor "+
			"that cannot be read is not a floor that is low, and publishing at "+
			"zero on the guess is a lost update nothing recovers", err)
	}
	if floor > cursor.Seq {
		return fmt.Errorf("pages: the trim floor is at %d and this node has "+
			"consumed through %d, so an absent anchor may be a claim trimmed "+
			"beneath it rather than an address that was never taken: %w",
			floor, cursor.Seq, statelog.ErrUnavailable)
	}
	return nil
}

// Gates answers whether a durable record produced rows on NO node.
//
// THE SAME TWO GATES [Applier.Gated] INSTALLS, read from the publisher's side.
// The two must agree: a resolution that looked for a gate the applier never
// installs would read every unapplied record as "somebody else won".
type Gates struct{ db *store.DB }

// NewGates builds it.
func NewGates(db *store.DB) *Gates { return &Gates{db: db} }

// GatedAt reports the gate that dropped a record at p.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position) (statelog.Reason, bool, error) {

	if g == nil || g.db == nil {
		return "", false, nil
	}
	db := g.db.Replicated().SQL()
	if writer != "" {
		var from, readmitted sql.NullInt64
		err := db.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position
			FROM pages_evictions WHERE node_id = ?`, writer).Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return "", false, fmt.Errorf("pages: read the eviction gate for "+
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
	if ObjectKind(subj.Kind) != KindPage {
		return "", false, nil
	}
	var author sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT purge_record_id FROM pages_deletions WHERE page_id = ?`,
		subj.ID).Scan(&author)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("pages: read the deletion gate for page "+
			"%s: %w", subj.ID, err)
	}
	// THE RECORD THAT WROTE THE MARKER IS NOT GATED BY IT. A purge whose
	// acknowledgement was lost would otherwise resolve as "applied
	// nowhere", and its caller would be told the destruction it asked for
	// did not happen — when it did.
	if author.Valid && author.String == opID {
		return "", false, nil
	}
	return statelog.ReasonDeleted, true, nil
}

// AdoptedAt is when this node's adoption of a donated snapshot completed.
//
// It qualifies a read of the OPERATION LEDGER, which travels SCRUBBED inside a
// snapshot: an op id minted before this instant cannot be answered for here at
// all, and reading its absence as "somebody else won" would re-decide against a
// row that moved because of this very write.
func (g *Gates) AdoptedAt(ctx context.Context) (time.Time, bool, error) {
	if g == nil || g.db == nil {
		return time.Time{}, false, nil
	}
	return statelog.AdoptedAt(ctx, g.db)
}
