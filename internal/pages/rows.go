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

	// Floor is the fleet's published trim floor at a generation and First
	// the log's own first surviving sequence — the two bounds
	// [Fence.ClearForZero] takes the higher of, for the reason the
	// tracker's fence gives. The cursor they are compared against is
	// passed per call, and the floor is read at that cursor's generation,
	// because the cursor has to be the checkpoint the write's own snapshot
	// read ([statelog.Snap.Checkpoint]) — see the tracker's fence for why
	// neither may be the applier's live position.
	Floor func(ctx context.Context, generation uint32) (uint64, error)
	First func(ctx context.Context) (uint64, error)
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
		FROM pages_evictions WHERE node_id = ?`, f.nodeID).Scan(&from, &readmitted)
	})
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
	if f.Floor == nil || f.First == nil {
		return fmt.Errorf("pages: this fence reads no published trim floor or " +
			"no first sequence of the log, so this node cannot establish that " +
			"an absent anchor means an unclaimed address rather than a claim " +
			"trimmed beneath it")
	}
	floor, err := f.Floor(ctx, cursor.Generation)
	if err != nil {
		return fmt.Errorf("pages: read the published trim floor: %w — a floor "+
			"that cannot be read is not a floor that is low, and publishing at "+
			"zero on the guess is a lost update nothing recovers", err)
	}
	first, err := f.First(ctx)
	if err != nil {
		return fmt.Errorf("pages: read the log's first surviving sequence: %w — "+
			"a log that cannot be read is not one that has lost nothing, and "+
			"publishing at zero on the guess is a lost update nothing recovers", err)
	}
	// EVERYTHING BELOW THE HIGHER OF THE TWO MAY BE GONE, so a node that
	// has consumed through the one before it has consumed everything that
	// may be gone — see the tracker's fence, which makes the same
	// comparison for the same reason, through the same
	// [statelog.Replayable].
	if held := max(floor, first); !statelog.Replayable(cursor.Seq, held) {
		return fmt.Errorf("pages: records below %d may have been removed from "+
			"the log (published floor %d, first surviving sequence %d) and this "+
			"node has consumed through %d, so an absent anchor may be a claim "+
			"trimmed beneath it rather than an address that was never taken: %w",
			held, floor, first, cursor.Seq, statelog.ErrUnavailable)
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

// GatedAt reports whether a record at p applies nowhere, and the gate that
// answers for it, by the rule [statelog.Gates] states — which the tracker's
// reader keeps too, and statelogtest.RunGates certifies for both.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position) (statelog.Reason, bool, error) {

	if g == nil || g.db == nil {
		return "", false, nil
	}
	var reason statelog.Reason
	var gated bool
	// ONE TRANSACTION FOR BOTH GATES, on ONE load of the replicated peer,
	// and through the HANDLE rather than its pool — see [Fence.Evicted] for
	// why a nil pool is reachable here. The tracker's reader has the same
	// shape, for the same reason.
	//
	// This used to say so and then make two calls, each its own
	// `Replicated().Read` — so the two halves of one answer were read at
	// two instants, and possibly from two FILES: every `Replicated()`
	// reloads the peer pointer, and an adoption swaps that pointer (close,
	// rename the donated file into place, reopen) while readers run. The
	// applier also commits between any two reads, and the deletion gate is
	// position-independent, so a purge of this page committing between
	// the two calls made the first half describe the estate before it and
	// the second half the estate after it. Whether such a pair still named
	// the gate the rule names then rested on an argument
	// about how each table can move between two instants — a marker only
	// ever appears; an eviction window over an applied position is only
	// ever rewritten by a re-eviction — that nothing here made and nothing
	// enforced. One snapshot is one state the applier committed, so the
	// answer is right whenever the rule is right for a single state, which
	// is the only thing [Applier.Gated]'s own reasoning establishes.
	err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		// THE DELETION GATE FIRST, by the rule [statelog.Gates] states: the
		// marker holds the page for every writer for ever, where an
		// eviction is one writer's and a readmission ends it, so `deleted`
		// is the answer that stays true. A record both gates drop is
		// dropped either way, so the order decides only which one is
		// REPORTED — and [Applier.Gated]'s opposite order decides only
		// which one a drop is COUNTED under.
		if ObjectKind(subj.Kind) == KindPage {
			var author sql.NullString
			err := tx.QueryRowContext(ctx,
				`SELECT purge_record_id FROM pages_deletions WHERE page_id = ?`,
				subj.ID).Scan(&author)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("pages: read the deletion gate for page "+
					"%s: %w", subj.ID, err)
			case author.Valid && author.String == opID:
				// THE RECORD THAT WROTE THE MARKER IS NOT GATED BY IT,
				// by its own id. Without the exception a purge whose
				// acknowledgement was lost resolves as "applied
				// nowhere", and its caller is told the destruction it
				// asked for did not happen — when it did. It falls
				// through to the eviction gate rather than out of the
				// read: the exception says THIS gate did not drop the
				// record, which is no answer about the other one.
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
			FROM pages_evictions WHERE node_id = ?`, writer).Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("pages: read the eviction gate for node %s: %w",
				writer, err)
		}
		// THE APPLIER'S WINDOW, spelled as [Applier.Gated] spells it: the
		// two must agree, and the same two comparisons are what makes that
		// readable rather than derived.
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

// AdoptedAt is the instant before which this node's operation ledger cannot
// vouch for an operation: when its latest adoption of a donated snapshot
// completed, or began where one did not complete — see [statelog.AdoptedAt].
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
