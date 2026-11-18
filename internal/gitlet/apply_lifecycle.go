package gitlet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// The lifecycle applies besides apply.go's create, settings, delete and
// restore: a rename and the release of a renamed repository's old name, each
// on a NAME's subject, and a live turn's hold, on the repository's.

// applyRename moves a repository to the name its record claimed, the old one
// redirecting until the record's instant plus [RedirectWindow], whole or not
// at all:
//
//  1. the row must still be under From, active or archived, and a tool's,
//     else NOTHING: two renames of one repository on two name subjects both
//     land, and a configuration-managed repository is never a tool's;
//  2. To's claim must be free, or the repository's own redirecting old name
//     (taken back), else the WHOLE record applies as nothing, lest the old
//     name redirect to a name the repository does not hold;
//  3. From redirects until the record's instant plus RedirectWindow;
//  4. the row takes the name, and its idle clock and version move.
func (a *Applier) applyRename(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	p, ok := payload.(Rename)
	if !ok {
		return 0, fmt.Errorf("gitlet: the rename at %s carries a %T", at.position, payload)
	}
	// THE NAME IS THE ADDRESS, as a create's: a record that arbitrated one
	// name and claims another would take a name nobody contended for.
	if token := NameToken(p.To); token != at.record.Subject.ID {
		return 0, fmt.Errorf("gitlet: the rename at %s arbitrated name %s and its payload names %q, whose "+
			"token is %s", at.position, at.record.Subject.ID, p.To, token)
	}
	var name, status, managed string
	err = tx.QueryRowContext(ctx, renameRowSQL, p.RepoID).Scan(&name, &status, &managed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("gitlet: read repository %s for the rename at %s: %w", p.RepoID, at.position, err)
	}
	if name != p.From || ManagedBy(managed) != ManagedTools ||
		!RepoStatus(status).live() {
		return 0, nil
	}
	holder, redirect, taken, err := claimRowIn(ctx, tx, at.record.Subject.ID)
	if err != nil {
		return 0, err
	}
	rows := 0
	switch {
	case !taken:
		if _, err = tx.ExecContext(ctx, `INSERT INTO code_names (token, name, repo_id, redirect_until)
			VALUES (?, ?, ?, NULL)`, at.record.Subject.ID, p.To, p.RepoID); err != nil {
			return 0, fmt.Errorf("gitlet: claim %q at %s: %w", p.To, at.position, err)
		}
	case holder == p.RepoID && redirect.Valid:
		// ITS OWN OLD NAME, taken back: it stops redirecting.
		if _, err = tx.ExecContext(ctx, keepNameSQL, at.record.Subject.ID, p.RepoID); err != nil {
			return 0, fmt.Errorf("gitlet: take %q back at %s: %w", p.To, at.position, err)
		}
	default:
		return 0, nil
	}
	rows++
	res, err := tx.ExecContext(ctx, redirectNameSQL, store.EncodeTime(at.brokerAt.Add(RedirectWindow)),
		NameToken(p.From), p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: redirect %q at %s: %w", p.From, at.position, err)
	}
	n, _ := res.RowsAffected()
	rows += int(n)
	if res, err = tx.ExecContext(ctx, renameRepoSQL, p.To, at.at(), at.packed, p.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: rename %s at %s: %w", p.RepoID, at.position, err)
	}
	n, _ = res.RowsAffected()
	return rows + int(n), nil
}

// applyReleaseName frees a renamed repository's old name — only where the row
// still points at the record's repository, still redirects, and its redirect
// ended at or before the record's OWN broker instant: a release decided early,
// on a clock ahead of the broker's, releases nothing, alike on every node, and
// the next pass decides it again.
func (a *Applier) applyReleaseName(ctx context.Context, tx *sql.Tx, at applyContext) (int, error) {
	payload, err := DecodeMutation(at.record)
	if err != nil {
		return 0, err
	}
	p, ok := payload.(ReleaseName)
	if !ok {
		return 0, fmt.Errorf("gitlet: the release at %s carries a %T", at.position, payload)
	}
	if token := NameToken(p.Name); token != at.record.Subject.ID {
		return 0, fmt.Errorf("gitlet: the release at %s arbitrated name %s and its payload names %q, whose "+
			"token is %s", at.position, at.record.Subject.ID, p.Name, token)
	}
	res, err := tx.ExecContext(ctx, releaseRedirectSQL, at.record.Subject.ID, p.RepoID, at.at())
	if err != nil {
		return 0, fmt.Errorf("gitlet: release %q at %s: %w", p.Name, at.position, err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// applyHold applies a live turn's hold, and moves no idle clock or last push:
// a hold is not activity, so the scratch expiry's idle clock stays the
// repository's pushes, renames, settings changes and restores.
//
//   - On the scratch repository: an active scratch row's version moves to the
//     hold's position, so an expiry that listed it before refuses it
//     ([Store.ExpireScratch]); any other row, nothing.
//   - On a seat's branch: the ref's held_position (what a deletion listed
//     before it is ordered by) and the repository's version (the replay
//     guard's), active or archived; never the ref's `at`, `position` (a cache's
//     delta cursor), seat or target. A gone, tombstoned or seat-less ref, or a
//     deleted or purging repository, moves nothing (the decide refused each).
func (a *Applier) applyHold(ctx context.Context, tx *sql.Tx, at applyContext, p Hold) (int, error) {
	if p.Ref == "" {
		res, err := tx.ExecContext(ctx, holdSQL, at.packed, p.RepoID)
		if err != nil {
			return 0, fmt.Errorf("gitlet: record turn %s's hold on %s at %s: %w", p.TurnID, p.RepoID, at.position,
				err)
		}
		n, _ := res.RowsAffected()
		return int(n), nil
	}
	held, err := tx.ExecContext(ctx, refHeldSQL, at.packed, p.RepoID, p.Ref)
	if err != nil {
		return 0, fmt.Errorf("gitlet: record turn %s's hold on %s of %s at %s: %w", p.TurnID, p.Ref, p.RepoID,
			at.position, err)
	}
	moved, err := tx.ExecContext(ctx, refHoldVersionSQL, at.packed, p.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: move %s's version to turn %s's hold on %s at %s: %w", p.RepoID, p.TurnID, p.Ref,
			at.position, err)
	}
	refs, _ := held.RowsAffected()
	repos, _ := moved.RowsAffected()
	return int(refs + repos), nil
}

// The statements of the lifecycle's applies, each registered in plan_test.go.
const (
	// renameRowSQL is what a rename's apply holds the row to.
	renameRowSQL = `SELECT name, status, managed_by FROM code_repos WHERE id = ?`
	// keepNameSQL takes a repository's own redirecting old name back.
	keepNameSQL = `UPDATE code_names SET redirect_until = NULL WHERE token = ? AND repo_id = ?`
	// redirectNameSQL leaves the old name redirecting, until the given
	// instant, as the store encodes one.
	redirectNameSQL = `UPDATE code_names SET redirect_until = ? WHERE token = ? AND repo_id = ?`
	// renameRepoSQL gives the row its new name, and moves its idle clock and
	// its version.
	renameRepoSQL = `UPDATE code_repos SET name = ?, last_activity_at = ?, version = ? WHERE id = ?`
	// releaseRedirectSQL frees an old name whose redirect ended by ?3.
	releaseRedirectSQL = `DELETE FROM code_names WHERE token = ? AND repo_id = ?
		AND redirect_until IS NOT NULL AND redirect_until <= ?`
	// holdSQL is a scratch repository's hold's whole apply.
	holdSQL = `UPDATE code_repos SET version = ? WHERE id = ? AND kind = 'scratch' AND status = 'active'`
	// refHeldSQL and refHoldVersionSQL are a seat branch's hold's apply: the
	// ref's mark, by code_refs' key, on a live row a seat set; and the
	// repository's version, active or archived.
	refHeldSQL = `UPDATE code_refs INDEXED BY ` + refsKey + ` SET held_position = ?
		WHERE repo_id = ? AND ref = ? AND target != '' AND seat != ''`
	refHoldVersionSQL = `UPDATE code_repos SET version = ? WHERE id = ? AND status IN ('active', 'archived')`
)
