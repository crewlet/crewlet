package gitlet

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
)

// THE REPOSITORY TALLIES count every repository by who may read it (visibility and owner), its
// kind and its status, so a count of repositories by kind and status is one scan of a few rows
// judged a key at a time. Kept through retally by every record that writes a repository's row,
// status, owner or visibility, and re-derived from code_repos by [Applier.Rederive].

// repoTallyRows is the tally row a record moving a repository's status within its key writes,
// and repoTallyMoveRows the two a record moving it between owners, visibilities or out of a row writes.
const (
	repoTallyRows     = 1
	repoTallyMoveRows = 2
)

// repoTallyKey is a repository tally row's key: who may read it, and its kind.
type repoTallyKey struct {
	tallyKey
	kind RepoKind
}

// compare orders keys as the repository tally's primary key does.
func (k repoTallyKey) compare(o repoTallyKey) int {
	return cmp.Or(k.tallyKey.compare(o.tallyKey), cmp.Compare(k.kind, o.kind))
}

// repoTallies is repository counts by tally key.
type repoTallies map[repoTallyKey]RepoCounts

// add counts in the repository s stands for, nothing where no row holds it.
func (t repoTallies) add(s tallyStanding) {
	if s.status == "" {
		return
	}
	key := repoTallyKey{s.key, s.kind}
	c := t[key]
	c.add(s.status, 1)
	t[key] = c
}

// retallyRepos moves one repository from what before counted it as to what after does: one row
// where only its status moved, else one out and one in, pruning a row left at zero.
func retallyRepos(ctx context.Context, tx *sql.Tx, before, after tallyStanding) (int, error) {
	from, to := repoTallyKey{before.key, before.kind}, repoTallyKey{after.key, after.kind}
	if from == to && before.status == after.status {
		return 0, nil
	}
	if before.status != "" && after.status != "" && from == to {
		var d RepoCounts
		d.add(before.status, -1)
		d.add(after.status, 1)
		if err := repoTally(ctx, tx, from, d); err != nil {
			return 0, err
		}
		return repoTallyRows, nil
	}
	written := 0
	if before.status != "" {
		var d RepoCounts
		d.add(before.status, -1)
		if err := repoTally(ctx, tx, from, d); err != nil {
			return 0, err
		}
		if err := unrepoTally(ctx, tx, from); err != nil {
			return 0, err
		}
		written++
	}
	if after.status != "" {
		var d RepoCounts
		d.add(after.status, 1)
		if err := repoTally(ctx, tx, to, d); err != nil {
			return 0, err
		}
		written++
	}
	return written, nil
}

// rederiveRepoTallies writes the difference between wanted and the tally rows held, in key order,
// pruning a row wanted holds none of; it answers the rows it wrote.
func rederiveRepoTallies(ctx context.Context, tx *sql.Tx, wanted repoTallies) (int, error) {
	held := repoTallies{}
	if err := eachRepoTally(ctx, tx, func(key repoTallyKey, c RepoCounts) error {
		held[key] = c
		return nil
	}); err != nil {
		return 0, err
	}
	keys := slices.Collect(maps.Keys(wanted))
	for key := range held {
		if _, ok := wanted[key]; !ok {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, repoTallyKey.compare)
	written := 0
	for _, key := range keys {
		want, had := wanted[key], held[key]
		if want == had {
			continue
		}
		if err := repoTally(ctx, tx, key, want.plus(had.negated())); err != nil {
			return 0, err
		}
		if want.zero() {
			if err := unrepoTally(ctx, tx, key); err != nil {
				return 0, err
			}
		}
		written++
	}
	return written, nil
}

// repoTally adds d to key's tally row, inserting it where none is.
func repoTally(ctx context.Context, tx *sql.Tx, key repoTallyKey, d RepoCounts) error {
	if _, err := tx.ExecContext(ctx, repoTallySQL, string(key.visibility), string(key.owner.Kind), key.owner.Ref,
		string(key.kind), d.Importing, d.Active, d.Archived, d.Deleted, d.Purging); err != nil {
		return fmt.Errorf("gitlet: tally %s's %s %s repositories: %w", key.owner.Ref, key.visibility, key.kind, err)
	}
	return nil
}

// unrepoTally deletes key's tally row where it counts nothing.
func unrepoTally(ctx context.Context, tx *sql.Tx, key repoTallyKey) error {
	if _, err := tx.ExecContext(ctx, unrepoTallySQL, string(key.visibility), string(key.owner.Kind), key.owner.Ref,
		string(key.kind)); err != nil {
		return fmt.Errorf("gitlet: prune the tally of %s's %s %s repositories: %w", key.owner.Ref, key.visibility,
			key.kind, err)
	}
	return nil
}

// eachRepoTally hands fn every repository tally row, read whole first.
func eachRepoTally(ctx context.Context, tx *sql.Tx, fn func(repoTallyKey, RepoCounts) error) error {
	type tallied struct {
		key    repoTallyKey
		counts RepoCounts
	}
	rows, err := tx.QueryContext(ctx, repoTalliesSQL)
	if err != nil {
		return fmt.Errorf("gitlet: read the repository tallies: %w", err)
	}
	var all []tallied
	err = eachRow(rows, func(s scanner) (err error) {
		var (
			visibility, ownerKind, owner, kind          string
			importing, active, archived, deleted, purge int64
		)
		if err = s.Scan(&visibility, &ownerKind, &owner, &kind, &importing, &active, &archived, &deleted,
			&purge); err == nil {
			all = append(all, tallied{key: repoTallyKey{tallyKey{visibility: Visibility(visibility),
				owner: Owner{Kind: OwnerKind(ownerKind), Ref: owner}}, RepoKind(kind)},
				counts: statusCounts(importing, active, archived, deleted, purge)})
		}
		return err
	})
	if err != nil {
		return fmt.Errorf("gitlet: read the repository tallies: %w", err)
	}
	for _, t := range all {
		if err = fn(t.key, t.counts); err != nil {
			return err
		}
	}
	return nil
}

// The repository tallies' statements, each registered in plan_test.go but the upsert, which
// reads no row.
const (
	repoTallySQL = `INSERT INTO code_repo_tallies (visibility, owner_kind, owner, kind, importing_repos,
			active_repos, archived_repos, deleted_repos, purging_repos) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (visibility, owner_kind, owner, kind) DO UPDATE SET
			importing_repos = importing_repos + excluded.importing_repos,
			active_repos = active_repos + excluded.active_repos,
			archived_repos = archived_repos + excluded.archived_repos,
			deleted_repos = deleted_repos + excluded.deleted_repos,
			purging_repos = purging_repos + excluded.purging_repos`
	unrepoTallySQL = `DELETE FROM code_repo_tallies WHERE visibility = ? AND owner_kind = ? AND owner = ?
		AND kind = ? AND importing_repos = 0 AND active_repos = 0 AND archived_repos = 0
		AND deleted_repos = 0 AND purging_repos = 0`
	// repoTalliesSQL is every repository tally row, read whole: the scan's bound is scanOK's.
	repoTalliesSQL = `SELECT visibility, owner_kind, owner, kind, importing_repos, active_repos,
		archived_repos, deleted_repos, purging_repos FROM code_repo_tallies`
)
