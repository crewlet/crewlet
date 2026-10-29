package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The publisher's three seams: what it reads, what refuses a write this node
// must not make, and what tells a dropped record from a lost race.

// NewRows builds the publisher's read seam over the log spec names.
func NewRows(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, spec, codeGuards)
}

// codeGuards answers the two object-level facts a first write needs: whether
// a deletion marker holds the subject for ever, and whether a guarding row
// says the object exists below the trim floor, where the broker's own claim
// no longer does.
//
// A REPOSITORY has both: its row, and the marker a purge leaves. A NAME has a
// guarding row and no marker — releasing a name is part of a purge, not a
// destruction of its own. The COMPANY's fence has its row. Every other object
// is created by its first record and removed by nothing, so false for both is
// the answer rather than an omission. A CHANGE's subject is no exception: the
// framework asks both only of a create-only write, and every write on a
// change's subject is arbitrated; its repository's marker drops its records at
// the apply gate ([gatedRepo]) and its rows go with the repository's purge. A
// repository's CHECKS subject is none either: its writes are additive, which
// the framework asks neither of, and the repository's marker drops them at the
// same gate.
func codeGuards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (deleted, guard bool, err error) {
	switch ObjectKind(subj.Kind) {
	case KindRepo:
		var removed, present int
		err = tx.QueryRowContext(ctx, guardRepoSQL, subj.ID, subj.ID).Scan(&removed, &present)
		if err != nil {
			return false, false, fmt.Errorf("code: read the guards on repository %s: %w", subj.ID, err)
		}
		return removed > 0, present > 0, nil
	case KindName:
		var held int
		if err = tx.QueryRowContext(ctx, guardNameSQL, subj.ID).Scan(&held); err != nil {
			return false, false, fmt.Errorf("code: read the guard on name %s: %w", subj.ID, err)
		}
		return false, held > 0, nil
	case KindCompany:
		var held int
		if err = tx.QueryRowContext(ctx, guardCompanySQL, subj.ID).Scan(&held); err != nil {
			return false, false, fmt.Errorf("code: read the guard on the company's %s: %w", subj.ID, err)
		}
		return false, held > 0, nil
	}
	return false, false, nil
}

// The guards' statements, each one point read on a primary key.
const (
	guardRepoSQL = `SELECT
		(SELECT COUNT(*) FROM code_deletions WHERE repo_id = ?),
		(SELECT COUNT(*) FROM code_repos WHERE id = ?)`
	guardNameSQL    = `SELECT COUNT(*) FROM code_names WHERE token = ?`
	guardCompanySQL = `SELECT COUNT(*) FROM code_activation WHERE id = ?`
)

// Fence refuses a write this node must not make — the tracker's and the
// knowledge base's two prices: Evicted runs on every append and is answered
// from rows this node already has, and ClearForZero pays a coordination round
// trip because being wrong there is a lost update rather than a duplicate.
type Fence struct {
	db     store.ReplicatedReader
	nodeID string

	// Floor is the fleet's published trim floor at a generation and Ends
	// the log's two ends, read live; Committed is this node's applier's live
	// checkpoint — [statelog.ZeroFence]'s four reads, which the engine
	// supplies.
	Floor     func(ctx context.Context, generation uint32) (uint64, error)
	Ends      func(ctx context.Context) (statelog.LogEnds, error)
	Committed func() statelog.Position
}

// NewFence builds it.
func NewFence(db store.ReplicatedReader, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction, FROM ITS OWN APPLIED ROWS — not
// from coordination, because a wedged coordination path is a precondition of
// an eviction being permitted at all.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f == nil || f.db.IsZero() || f.nodeID == "" {
		return false, nil
	}
	var from, readmitted sql.NullInt64
	// THROUGH THE HANDLE, which answers store.ErrNoEstate for an estate an
	// adoption has closed rather than panicking on a closed pool.
	err := f.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, evictionSQL, f.nodeID).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		// UNREADABLE IS NOT "not evicted": a fence that failed open would
		// collect acknowledgements for writes that happen nowhere.
		return false, fmt.Errorf("code: read this node's own eviction: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 < from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of ZERO is
// safe from this node — [statelog.ZeroFence], which this supplies the reads
// for. A read that answers unknown refuses.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.ZeroFence{
		Evicted: f.Evicted, Floor: f.Floor, Ends: f.Ends, Committed: f.Committed,
	}.ClearForZero(ctx, cursor)
}

// evictionSQL is a node's eviction window on this log.
const evictionSQL = `SELECT from_position, readmitted_position FROM code_evictions WHERE node_id = ?`

// Gates answers whether a durable record produced rows on NO node — the same
// two gates [Applier.Gated] installs, read from the publisher's side. The two
// must agree: a resolution that looked for a gate the applier never installs
// would read every unapplied record as "somebody else won".
type Gates struct{ db store.ReplicatedReader }

// NewGates builds it.
func NewGates(db store.ReplicatedReader) *Gates { return &Gates{db: db} }

// GatedAt reports whether a record at p applies nowhere, and the gate that
// answers for it, by the rule [statelog.Gates] states.
//
// ONE TRANSACTION FOR BOTH GATES, on one resolution of the estate and through
// the handle: two reads would be two instants and possibly two files, since an
// adoption replaces the estate's file while readers run.
//
// THE DELETION GATE FIRST: a marker holds the repository for every writer for
// ever, where an eviction is one writer's and a readmission ends it, so
// `deleted` is the answer that stays true. A record both gates drop is dropped
// either way, so this order decides only which reason is REPORTED, and
// [Applier.Gated]'s opposite order only which one a drop is COUNTED under.
//
// THE BODY IS READ ONLY FOR A CHANGE'S SUBJECT, by the rule the applier's own
// gate asks ([gatedRepo]): a repository subject names its repository and a
// name subject's create checks the marker in its own apply, but a change's
// subject names its change, and the repository it is about is in its payload.
// Handed no body — the record there is another operation's — a change's record
// is answered from the subject alone, ungated by a marker, and the re-decide
// that leads to meets the repository gone in its own snapshot.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position, body []byte) (statelog.Reason, bool, error) {

	if g == nil || g.db.IsZero() {
		return "", false, nil
	}
	var reason statelog.Reason
	var gated bool
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		if repoID, ok := gatedRepo(subj, body); ok {
			var author sql.NullString
			err := tx.QueryRowContext(ctx, markerSQL, repoID).Scan(&author)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("code: read the deletion gate for repository %s: %w", repoID, err)
			case author.Valid && author.String == opID:
				// THE RECORD THAT WROTE THE MARKER IS NOT GATED BY IT, by
				// its own id — and the read falls through to the eviction
				// gate, since the exception answers nothing about that one.
			default:
				reason, gated = statelog.ReasonDeleted, true
				return nil
			}
		}
		if writer == "" {
			return nil
		}
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, evictionSQL, writer).Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("code: read the eviction gate for node %s: %w", writer, err)
		}
		if evictedAt(p.Packed(), from, readmitted) {
			reason, gated = statelog.ReasonEvicted, true
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return reason, gated, nil
}

// markerSQL is the operation that wrote a repository's deletion marker.
const markerSQL = `SELECT purge_record_id FROM code_deletions WHERE repo_id = ?`

// evictedAt is the eviction window at a packed position, HALF-OPEN AT BOTH
// ENDS: a record at or below the eviction's own position was written while the
// node was still counted, and one at or above a readmission by a node the
// fleet has taken back. ONE spelling, which the applier and the gate reader
// both ask.
func evictedAt(at int64, from, readmitted sql.NullInt64) bool {
	evicted := from.Valid && at > from.Int64
	back := readmitted.Valid && at >= readmitted.Int64
	return evicted && !back
}

// gatedRepo is the repository whose deletion marker gates a record on subj
// with the body given: a repository subject names its own, and so does a
// repository's CHECKS subject, keyed by the repository's id; a CHANGE's subject
// names its change, and the repository is the one its payload names
// ([changeRepoOf]) — none where no body is handed, or one this build cannot
// decode. A NAME subject names none — its create's payload names the
// repository, which the gate reader is not handed — so the create's apply
// checks the marker itself. ONE FUNCTION, asked by [Applier.Gated] and
// [Gates.GatedAt] alike, because [statelog.Gates] holds the two to one answer.
func gatedRepo(subj statelog.Subject, body []byte) (string, bool) {
	switch ObjectKind(subj.Kind) {
	case KindRepo, KindChecks:
		return subj.ID, true
	case KindChange:
		if body == nil {
			return "", false
		}
		return changeRepoOf(body)
	}
	return "", false
}
