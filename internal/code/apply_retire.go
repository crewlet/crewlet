package code

import (
	"context"
	"database/sql"
	"fmt"
)

// applyRetireSweep removes the repository's retired pack rows at or below the
// record's Through, oldest first, and raises the repository's mark to it.
//
// PURE IN THE RECORD: it reads no clock and removes no row above Through —
// every retirement its decide did not read sits above it, at a later position
// — and a later sweep naming the same Through deletes nothing and leaves the
// mark where it is. At most [RetireSweepRows] rows, the bound its decide cut
// at, so the record stays inside the framework's row budget whatever its
// Through says.
//
// A THROUGH NO WRITER FORMS IS AN ERROR, not a skip: a position on another
// log, or one at or past the record's own — a retirement is a record below
// the sweep that removes it — would raise the mark by a number from no
// history these rows have.
func (a *Applier) applyRetireSweep(ctx context.Context, tx *sql.Tx, at applyContext, p RetireSweep) (int, error) {
	through := p.Through.Packed()
	if p.Through.Stream != at.position.Stream || through <= 0 || through >= at.packed {
		return 0, fmt.Errorf("code: the retire sweep at %s sweeps through %s — a sweep names a "+
			"position on its own log below itself, where a record retired the rows it removes, "+
			"and no writer forms this one", at.position, p.Through)
	}
	res, err := tx.ExecContext(ctx, sweepRetiredSQL, p.RepoID, through, RetireSweepRows)
	if err != nil {
		return 0, fmt.Errorf("code: sweep %s's retired packs at %s: %w", p.RepoID, at.position, err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx, sweptRetiredSQL, through, at.packed, p.RepoID); err != nil {
		return 0, fmt.Errorf("code: raise %s's retired mark at %s: %w", p.RepoID, at.position, err)
	}
	return int(n) + 1, nil
}

// The sweep's writes — the applier's alone, so they live beside its other
// statements — each registered in plan_test.go, removing rows in the order the
// decide's walk reads them (retire.go).
const (
	// sweepRetiredSQL is a sweep's apply: the rowid idiom, so the rows are the
	// ones the walk's order reaches first and no more than its bound.
	sweepRetiredSQL = `DELETE FROM code_packs_retired WHERE rowid IN (SELECT rowid
		FROM code_packs_retired INDEXED BY code_packs_retired_repo_idx
		WHERE repo_id = ? AND retired_position <= ?
		ORDER BY retired_position, pack_id, part LIMIT ?)`
	sweptRetiredSQL = `UPDATE code_repos SET retired_through = MAX(retired_through, ?), version = ?
		WHERE id = ?`
)
