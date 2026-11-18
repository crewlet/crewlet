package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The publisher's three seams: what it reads, what refuses a write this node
// must not make, and what tells a dropped record from a lost race.

// NewRows builds the publisher's read seam over the log spec names.
func NewRows(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, spec, codeGuards)
}

// codeGuards answers the two facts a first write needs: whether a deletion
// marker holds the subject for ever, and whether a guarding row says the
// object exists below the trim floor, where the broker's claim no longer does.
//
// A REPOSITORY has both: its row, and the marker a purge leaves. A NAME has a
// guarding row and no marker (its release is part of a purge). The COMPANY's
// fence has its row. Every other object is created by its first record and
// removed by nothing, so false for both is the answer, not an omission. A
// CHANGE's and a CHECKS subject included: the framework asks them only of a
// create-only write, theirs are arbitrated or additive, the repository's
// marker drops their records at the apply gate ([gatedRepo]), and its purge
// takes their rows.
func codeGuards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (deleted, guard bool, err error) {
	switch ObjectKind(subj.Kind) {
	case KindRepo:
		var removed, present int
		err = tx.QueryRowContext(ctx, guardRepoSQL, subj.ID, subj.ID).Scan(&removed, &present)
		if err != nil {
			return false, false, fmt.Errorf("gitlet: read the guards on repository %s: %w", subj.ID, err)
		}
		return removed > 0, present > 0, nil
	case KindName:
		var held int
		if err = tx.QueryRowContext(ctx, guardNameSQL, subj.ID).Scan(&held); err != nil {
			return false, false, fmt.Errorf("gitlet: read the guard on name %s: %w", subj.ID, err)
		}
		return false, held > 0, nil
	case KindCompany:
		var held int
		if err = tx.QueryRowContext(ctx, guardCompanySQL, subj.ID).Scan(&held); err != nil {
			return false, false, fmt.Errorf("gitlet: read the guard on the company's %s: %w", subj.ID, err)
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

// Fence refuses a write this node must not make, at two prices: Evicted runs
// on every append from rows this node already has, and ClearForZero pays a
// coordination round trip because being wrong there is a lost update rather
// than a duplicate.
type Fence struct {
	db     store.ReplicatedReader
	nodeID string

	// Floor (the fleet's published trim floor at a generation), Ends (the
	// log's two ends) and Committed (this node's applier's live checkpoint)
	// are [statelog.ZeroFence]'s reads, which the engine supplies.
	Floor     func(ctx context.Context, generation uint32) (uint64, error)
	Ends      func(ctx context.Context) (statelog.LogEnds, error)
	Committed func() statelog.Position
}

// NewFence builds it.
func NewFence(db store.ReplicatedReader, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction FROM ITS OWN APPLIED ROWS, not
// from coordination: a wedged coordination path is a precondition of an
// eviction being permitted at all.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f == nil || f.db.IsZero() || f.nodeID == "" {
		return false, nil
	}
	var from, readmitted sql.NullInt64
	// Through the handle: it answers store.ErrNoEstate for an estate an
	// adoption closed rather than panicking on a closed pool.
	err := f.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, evictionSQL, f.nodeID).Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		// Unreadable is not "not evicted": failing open would collect
		// acknowledgements for writes that happen nowhere.
		return false, fmt.Errorf("gitlet: read this node's own eviction: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 < from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of ZERO is
// safe from this node, by [statelog.ZeroFence]. A read that answers unknown
// refuses.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.ZeroFence{
		Evicted: f.Evicted, Floor: f.Floor, Ends: f.Ends, Committed: f.Committed,
	}.ClearForZero(ctx, cursor)
}

// evictionSQL is a node's eviction window on this log.
const evictionSQL = `SELECT from_position, readmitted_position FROM code_evictions WHERE node_id = ?`

// Gates answers whether a durable record produced rows on NO node: the two
// gates [Applier.Gated] installs, read from the publisher's side. They must
// agree, or a resolution reads every unapplied record as "somebody else won".
type Gates struct{ db store.ReplicatedReader }

// NewGates builds it.
func NewGates(db store.ReplicatedReader) *Gates { return &Gates{db: db} }

// GatedAt reports whether a record at p applies nowhere, and the gate that
// answers for it, by the rule [statelog.Gates] states. ONE TRANSACTION FOR
// BOTH GATES: two reads could pair two instants, and across an adoption two
// files (TestGatedAtReadsBothGatesInOneTransaction). THE DELETION GATE FIRST,
// the answer that stays true (a readmission ends an eviction); the order
// decides only the reason REPORTED, as [Applier.Gated]'s opposite order only
// the one a drop is counted under. Only a change's record has its body read,
// and only its envelope, by the applier's own decoder ([Domain.Envelope]), in
// one pass that builds no payload: its repository is the one its scope names
// ([gatedRepo]). Handed no body (another operation's record), it is answered
// ungated by a marker, and the re-decide that leads to meets the repository
// gone in its own snapshot.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position, body []byte) (statelog.Reason, bool, error) {

	if g == nil || g.db.IsZero() {
		return "", false, nil
	}
	var scope statelog.ScopeSet
	if ObjectKind(subj.Kind) == KindChange && body != nil {
		// An envelope this build cannot read names no scope: answered as
		// one handed no body.
		if env, err := (Domain{}).Envelope(body); err == nil {
			scope = env.Scope
		}
	}
	var reason statelog.Reason
	var gated bool
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		if repoID, ok := gatedRepo(subj, scope); ok {
			var author sql.NullString
			err := tx.QueryRowContext(ctx, markerSQL, repoID).Scan(&author)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("gitlet: read the deletion gate for repository %s: %w", repoID, err)
			case author.Valid && author.String == opID:
				// The record that wrote the marker is not gated by it; it
				// still falls through to the eviction gate.
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
			return fmt.Errorf("gitlet: read the eviction gate for node %s: %w", writer, err)
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

// evictedAt is the eviction window at a packed position, (from, readmitted):
// a record at or below the eviction's position was written while the node was
// still counted, one at or above a readmission by a node taken back. The
// applier and the gate reader both ask it, so they cannot disagree.
func evictedAt(at int64, from, readmitted sql.NullInt64) bool {
	evicted := from.Valid && at > from.Int64
	back := readmitted.Valid && at >= readmitted.Int64
	return evicted && !back
}

// gatedRepo is the repository whose deletion marker gates a record on subj
// with envelope scope scope: a REPOSITORY or CHECKS subject's own id; for a
// CHANGE's subject, the one repository its scope names ([scopedRepo]), since
// [ChangeWriter] names it beside the change and [Applier.applyChange] holds
// the payload to it. A NAME subject names none: its create's apply checks the
// marker itself. [Applier.Gated] and [Gates.GatedAt] both ask it, because
// [statelog.Gates] holds the two to one answer.
//
// THE SCOPE, NEVER THE PAYLOAD: the framework has already decoded the
// envelope, and a change's record can be most of a record's bytes, so the
// payload would cost several JSON passes in every apply for one field.
func gatedRepo(subj statelog.Subject, scope statelog.ScopeSet) (string, bool) {
	switch ObjectKind(subj.Kind) {
	case KindRepo, KindChecks:
		return subj.ID, true
	case KindChange:
		return scopedRepo(scope)
	}
	return "", false
}

// scopedRepo is the one repository scope names by its path ([RepoPath]), and
// false where it names none or more than one; an unreadable scope is the
// domain's whole path ([Scope.UnmarshalJSON]), which names none.
func scopedRepo(scope statelog.ScopeSet) (string, bool) {
	var repoID string
	for _, p := range scope.Paths {
		id, ok := strings.CutPrefix(p, RepoPath(""))
		if !ok || id == "" || strings.Contains(id, statelog.ScopeSeparator) {
			continue
		}
		if repoID != "" {
			return "", false
		}
		repoID = id
	}
	return repoID, repoID != ""
}
