package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE APPLIER MAINTAINS DERIVED COLUMNS: each repository's changes by state, whatever its
// status, and the repositories in use tallied by visibility and owner, re-derived where the rules move.
var _ statelog.Deriver = (*Applier)(nil)

// DerivationVersion is the rule set this build derives its columns at: 1 is each repository's
// changes by state and their tallies by owner and visibility; 2 adds each repository's pushes by UTC
// day and quarter-hour and by pusher and day (apply_activity.go), each bump re-reading every push row
// at about 20 µs a row (doc.go's measured section). Bumped by any change to any of them.
const DerivationVersion = 2

// DerivationVersion implements [statelog.Deriver].
func (a *Applier) DerivationVersion() int { return DerivationVersion }

// rederivePageRows is how many repositories [Applier.Rederive] reads at once, each page read
// whole and closed before any of its rows is written.
const rederivePageRows = 1000

// Rederive implements [statelog.Deriver]: the counts recounted from code_changes, the tallies
// from the counts, and each repository's push activity from its push rows, through the statements
// the applies keep them with. On rows this build kept it writes nothing.
func (a *Applier) Rederive(ctx context.Context, tx *sql.Tx, opts statelog.ApplyOptions) (int, error) {
	written, activityErr := rederivePushActivity(ctx, tx, opts.MaxVariables)
	if activityErr != nil {
		return 0, activityErr
	}
	wanted := map[tallyKey]ChangeCounts{}
	after := ""
	for {
		page, err := rederivePage(ctx, tx, after)
		if err != nil {
			return 0, err
		}
		for _, s := range page {
			counted, countErr := changesByState(ctx, tx, s.id)
			if countErr != nil {
				return 0, countErr
			}
			if d := counted.plus(s.counts.negated()); !d.zero() {
				if _, err = tx.ExecContext(ctx, changeCountsSQL, d.Draft, d.Open, d.Closed, d.Merged,
					s.id); err != nil {
					return 0, fmt.Errorf("gitlet: re-derive %s's change counts: %w", s.id, err)
				}
				written++
			}
			if s.live && !counted.zero() {
				wanted[s.key] = wanted[s.key].plus(counted)
			}
		}
		if len(page) < rederivePageRows {
			break
		}
		after = page[len(page)-1].id
	}
	held := map[tallyKey]ChangeCounts{}
	if err := eachTally(ctx, tx, func(key tallyKey, c ChangeCounts) error {
		held[key] = c
		return nil
	}); err != nil {
		return 0, err
	}
	keys := make([]tallyKey, 0, len(wanted)+len(held))
	for key := range wanted {
		keys = append(keys, key)
	}
	for key := range held {
		if _, ok := wanted[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, tallyKey.compare)
	for _, key := range keys {
		want, had := wanted[key], held[key]
		moved := false
		if d := want.plus(had.negated()); !d.zero() {
			if err := tally(ctx, tx, key, d); err != nil {
				return 0, err
			}
			moved = true
		}
		if want.zero() {
			pruned, err := untally(ctx, tx, key)
			if err != nil {
				return 0, err
			}
			moved = moved || pruned
		}
		if moved {
			written++
		}
	}
	return written, nil
}

// rederived is one repository as [Applier.Rederive] reads it.
type rederived struct {
	id string
	tallyStanding
}

// rederivePage is the repositories after the id after, by their key, a page read whole.
func rederivePage(ctx context.Context, tx *sql.Tx, after string) ([]rederived, error) {
	rows, err := tx.QueryContext(ctx, rederiveReposSQL, after, rederivePageRows)
	if err != nil {
		return nil, fmt.Errorf("gitlet: read the repositories to re-derive: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var page []rederived
	for rows.Next() {
		var r rederived
		if r.tallyStanding, err = scanStanding(rows, &r.id); err != nil {
			return nil, fmt.Errorf("gitlet: read a repository to re-derive: %w", err)
		}
		page = append(page, r)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("gitlet: read the repositories to re-derive: %w", err)
	}
	return page, nil
}

// changesByState is repoID's changes counted by state, one range of its index.
func changesByState(ctx context.Context, tx *sql.Tx, repoID string) (ChangeCounts, error) {
	rows, err := tx.QueryContext(ctx, changesByStateSQL, repoID)
	if err != nil {
		return ChangeCounts{}, fmt.Errorf("gitlet: count %s's changes by state: %w", repoID, err)
	}
	defer func() { _ = rows.Close() }()
	var c ChangeCounts
	for rows.Next() {
		var (
			state string
			n     int64
		)
		if err = rows.Scan(&state, &n); err != nil {
			return ChangeCounts{}, fmt.Errorf("gitlet: count %s's changes by state: %w", repoID, err)
		}
		c.add(ChangeState(state), n)
	}
	if err = rows.Err(); err != nil {
		return ChangeCounts{}, fmt.Errorf("gitlet: count %s's changes by state: %w", repoID, err)
	}
	return c, nil
}

// tallyRows is the tally row a record that moves a repository's changes writes, and
// tallyMoveRows the two a record moving a repository between owners or visibilities writes.
const (
	tallyRows     = 1
	tallyMoveRows = 2
)

// tallyKey is who may read a repository's changes: its visibility and its owner.
type tallyKey struct {
	visibility Visibility
	owner      Owner
}

// compare orders keys as the tally's primary key does.
func (k tallyKey) compare(o tallyKey) int {
	return cmp.Or(cmp.Compare(k.visibility, o.visibility), cmp.Compare(k.owner.Kind, o.owner.Kind),
		cmp.Compare(k.owner.Ref, o.owner.Ref))
}

// tallyStanding is what one repository puts in the tallies: its key, and its counts
// while it is in use ([RepoStatus.live]).
type tallyStanding struct {
	live   bool
	key    tallyKey
	counts ChangeCounts
}

// scanStanding reads a row of lead, then status, visibility, owner kind, owner and the four
// counts, as what the repository puts in the tallies.
func scanStanding(row interface{ Scan(...any) error }, lead ...any) (tallyStanding, error) {
	var (
		status, visibility, kind, owner string
		draft, open, closed, merged     int64
	)
	if err := row.Scan(append(lead, &status, &visibility, &kind, &owner, &draft, &open, &closed,
		&merged)...); err != nil {
		return tallyStanding{}, err
	}
	return tallyStanding{live: RepoStatus(status).live(), key: tallyKey{visibility: Visibility(visibility),
		owner: Owner{Kind: OwnerKind(kind), Ref: owner}}, counts: stateCounts(draft, open, closed, merged)}, nil
}

// countChanges moves repoID's counts by d and, while it is in use, its key's tally row;
// it answers the tally rows it wrote. A zero d writes nothing. It never prunes: while a
// repository is in use none of its changes is removed, so no move takes a key's total down.
func countChanges(ctx context.Context, tx *sql.Tx, repoID string, d ChangeCounts) (int, error) {
	if d.zero() {
		return 0, nil
	}
	if _, err := tx.ExecContext(ctx, changeCountsSQL, d.Draft, d.Open, d.Closed, d.Merged, repoID); err != nil {
		return 0, fmt.Errorf("gitlet: count %s's changes: %w", repoID, err)
	}
	s, err := tallyStandingIn(ctx, tx, repoID)
	if err != nil || !s.live {
		return 0, err
	}
	if err = tally(ctx, tx, s.key, d); err != nil {
		return 0, err
	}
	return tallyRows, nil
}

// tallyStandingIn is what repoID's row puts in the tallies now: nothing where no row holds it.
func tallyStandingIn(ctx context.Context, tx *sql.Tx, repoID string) (tallyStanding, error) {
	s, err := scanStanding(tx.QueryRowContext(ctx, tallyStandingSQL, repoID))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return tallyStanding{}, nil
	case err != nil:
		return tallyStanding{}, fmt.Errorf("gitlet: read what %s puts in the change tallies: %w", repoID, err)
	}
	return s, nil
}

// retally moves repoID's counts from what before put in the tallies to what its row puts there
// now, deleting a row left at zero; it answers the tally rows it wrote.
func retally(ctx context.Context, tx *sql.Tx, repoID string, before tallyStanding) (int, error) {
	after, err := tallyStandingIn(ctx, tx, repoID)
	if err != nil || before == after {
		return 0, err
	}
	written := 0
	if before.live && !before.counts.zero() {
		if err = tally(ctx, tx, before.key, before.counts.negated()); err != nil {
			return 0, err
		}
		if _, err = untally(ctx, tx, before.key); err != nil {
			return 0, err
		}
		written++
	}
	if after.live && !after.counts.zero() {
		if err = tally(ctx, tx, after.key, after.counts); err != nil {
			return 0, err
		}
		written++
	}
	return written, nil
}

// tally adds d to key's tally row, inserting it where none is.
func tally(ctx context.Context, tx *sql.Tx, key tallyKey, d ChangeCounts) error {
	if _, err := tx.ExecContext(ctx, tallySQL, string(key.visibility), string(key.owner.Kind), key.owner.Ref,
		d.Draft, d.Open, d.Closed, d.Merged); err != nil {
		return fmt.Errorf("gitlet: tally the changes of %s's %s repositories: %w", key.owner.Ref,
			key.visibility, err)
	}
	return nil
}

// untally deletes key's tally row where it counts nothing, and reports whether it did.
func untally(ctx context.Context, tx *sql.Tx, key tallyKey) (bool, error) {
	res, err := tx.ExecContext(ctx, untallySQL, string(key.visibility), string(key.owner.Kind), key.owner.Ref)
	if err != nil {
		return false, fmt.Errorf("gitlet: prune the change tally of %s's %s repositories: %w", key.owner.Ref,
			key.visibility, err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// The counts' statements, each registered in plan_test.go but the upsert, which reads no row.
const (
	changeCountsSQL = `UPDATE code_repos SET draft_changes = draft_changes + ?, open_changes = open_changes + ?,
		closed_changes = closed_changes + ?, merged_changes = merged_changes + ? WHERE id = ?`
	tallyStandingSQL = `SELECT status, visibility, owner_kind, owner, draft_changes, open_changes,
		closed_changes, merged_changes FROM code_repos WHERE id = ?`
	tallySQL = `INSERT INTO code_change_tallies (visibility, owner_kind, owner, draft_changes, open_changes,
			closed_changes, merged_changes) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (visibility, owner_kind, owner) DO UPDATE SET
			draft_changes = draft_changes + excluded.draft_changes,
			open_changes = open_changes + excluded.open_changes,
			closed_changes = closed_changes + excluded.closed_changes,
			merged_changes = merged_changes + excluded.merged_changes`
	untallySQL = `DELETE FROM code_change_tallies WHERE visibility = ? AND owner_kind = ? AND owner = ?
		AND draft_changes = 0 AND open_changes = 0 AND closed_changes = 0 AND merged_changes = 0`
	// rederiveReposSQL is a page of repositories by their key, after ?1: what Rederive recounts.
	rederiveReposSQL = `SELECT id, status, visibility, owner_kind, owner, draft_changes, open_changes,
		closed_changes, merged_changes FROM code_repos WHERE id > ? ORDER BY id LIMIT ?`
	// changesByStateSQL is one repository's changes counted by state, one range of its index.
	changesByStateSQL = `SELECT state, COUNT(*) FROM code_changes INDEXED BY ` + changesRepoStateKey + `
		WHERE repo_id = ? GROUP BY state`
)
