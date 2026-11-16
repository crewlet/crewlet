package gitlet

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// repoTalliesDrift names the first difference between the repository tallies and the rows, ""
// for none: every tally row against code_repos grouped by visibility, owner, kind and status, no
// zero row, and each kind's sum against code_repo_kinds.
func repoTalliesDrift(ctx context.Context, tx *sql.Tx) (string, error) {
	wanted := repoTallies{}
	rows, err := tx.QueryContext(ctx, `SELECT visibility, owner_kind, owner, kind, status, COUNT(*) FROM code_repos
		GROUP BY 1, 2, 3, 4, 5`)
	if err != nil {
		return "", err
	}
	err = eachRow(rows, func(s scanner) (err error) {
		var visibility, ownerKind, owner, kind, status string
		var n int64
		if err = s.Scan(&visibility, &ownerKind, &owner, &kind, &status, &n); err == nil {
			key := repoTallyKey{tallyKey{Visibility(visibility), Owner{Kind: OwnerKind(ownerKind), Ref: owner}},
				RepoKind(kind)}
			c := wanted[key]
			c.add(RepoStatus(status), n)
			wanted[key] = c
		}
		return err
	})
	if err != nil {
		return "", err
	}
	held := repoTallies{}
	if err = eachRepoTally(ctx, tx, func(key repoTallyKey, c RepoCounts) error {
		held[key] = c
		return nil
	}); err != nil {
		return "", err
	}
	if !maps.Equal(wanted, held) {
		return fmt.Sprintf("the repository tallies hold %s and the repositories %s", repoTallyText(held),
			repoTallyText(wanted)), nil
	}
	kinds := map[RepoKind]int64{}
	if rows, err = tx.QueryContext(ctx, `SELECT kind, repos FROM code_repo_kinds`); err != nil {
		return "", err
	}
	if err = eachRow(rows, func(s scanner) (err error) {
		var kind string
		var n int64
		if err = s.Scan(&kind, &n); err == nil && n != 0 {
			kinds[RepoKind(kind)] = n
		}
		return err
	}); err != nil {
		return "", err
	}
	tallied := map[RepoKind]int64{}
	for key, c := range held {
		tallied[key.kind] += c.sum(RepoStatuses())
	}
	if !maps.Equal(kinds, tallied) {
		return fmt.Sprintf("the repository tallies count %v by kind and code_repo_kinds %v", tallied, kinds), nil
	}
	return "", nil
}

// repoTallyText is repository tallies as a sorted line: each key's importing, active, archived,
// deleted and purging, for a failure's sentence.
func repoTallyText(t repoTallies) string {
	keys := slices.SortedFunc(maps.Keys(t), repoTallyKey.compare)
	parts := make([]string, len(keys))
	for i, k := range keys {
		c := t[k]
		parts[i] = fmt.Sprintf("%s/%s/%s/%s=%d/%d/%d/%d/%d", k.visibility, k.owner.Kind, k.owner.Ref, k.kind,
			c.Importing, c.Active, c.Archived, c.Deleted, c.Purging)
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// repoTallies is the estate's repository tally rows as repoTallyText writes them.
func (e *estate) repoTallies() string {
	e.t.Helper()
	held := repoTallies{}
	if err := e.db.Reader().Read(context.Background(), func(tx *sql.Tx) error {
		return eachRepoTally(context.Background(), tx, func(key repoTallyKey, c RepoCounts) error {
			held[key] = c
			return nil
		})
	}); err != nil {
		e.t.Fatalf("read the repository tallies: %v", err)
	}
	return repoTallyText(held)
}

// EACH RECORD MOVES THE REPOSITORY TALLIES: every record that makes, moves or removes a
// repository leaves the tally rows the case states, those the rows imply, and reports the rows
// its tally moves wrote beside its own.
// Mutation: drop the retally call from a create, an import's end, a purge part or the purge,
// count a status move as two rows, or skip the prune.
func TestEachRecordMovesTheRepositoryTallies(t *testing.T) {
	t.Parallel()
	const repo = "repo-c"
	created := func(t *testing.T, e *estate) { e.apply(create(t, repo, "c")) }
	importCreate := func(t *testing.T) built {
		return rec(t, NameSubject("c"), OpCreate, Scope{NamePath(NameToken("c")), RepoPath(repo)},
			CreateRepo{RepoID: repo, Name: "c", Settings: settingsFor(""),
				Import: &ImportSource{URL: "https://example.com/c.git"}})
	}
	importing := func(t *testing.T, e *estate) { e.apply(importCreate(t)) }
	deleteOf := func(t *testing.T) built {
		return ccRecord(t, repo, OpDelete, LifecycleRecord{RepoID: repo, ConfigEpoch: 1})
	}
	deleted := func(t *testing.T, e *estate) { created(t, e); e.apply(deleteOf(t)) }
	purging := func(t *testing.T, e *estate) { deleted(t, e); e.apply(partOf(t, repo, 1)) }
	purgeOf := func(t *testing.T) built {
		return rec(t, RepoSubject(repo), OpPurge, Scope{NamePath(NameToken("c")), RepoPath(repo)},
			Purge{RepoID: repo, Name: "c", Reason: "test"})
	}
	archived := func(t *testing.T, e *estate) {
		created(t, e)
		s := settingsFor("")
		s.Archived = true
		e.apply(ccSettings(t, repo, s))
	}
	restored := func(t *testing.T, owner Owner) built {
		back := ccOwned(owner, VisibilityCompany, 2)
		return ccRecord(t, repo, OpRestore, LifecycleRecord{RepoID: repo, ConfigEpoch: 2, Settings: &back})
	}
	const project = "company/unit/ENG/project="
	for _, tc := range []struct {
		name    string
		given   func(t *testing.T, e *estate)
		records func(t *testing.T) []built
		tallies string
		// rows is what the last record reports, the tally rows with its own; -1 where its own
		// rows are another test's (the settings records' bound, limits_test.go).
		rows int
	}{
		// A create's row, its name claim, its kind's count and its one tally row.
		{"a create", func(*testing.T, *estate) {}, func(t *testing.T) []built { return []built{create(t, repo, "c")} },
			"[" + project + "0/1/0/0/0]", 4},
		{"a create that imports", func(*testing.T, *estate) {}, func(t *testing.T) []built {
			return []built{importCreate(t)}
		}, "[" + project + "1/0/0/0/0]", 4},
		{"a scratch repository beside a project", created, func(t *testing.T) []built {
			s := settingsFor("")
			s.Kind, s.Owner = RepoScratch, Owner{Kind: OwnerSeat, Ref: "dev"}
			return []built{rec(t, NameSubject("dev.notes"), OpCreate,
				Scope{NamePath(NameToken("dev.notes")), RepoPath("repo-s")},
				CreateRepo{RepoID: "repo-s", Name: "dev.notes", Settings: s})}
		}, "[company/seat/dev/scratch=0/1/0/0/0 " + project + "0/1/0/0/0]", 4},
		// The import's end moves its row and its one tally row.
		{"an import's end", importing, func(t *testing.T) []built {
			return []built{ccRecord(t, repo, OpImportDone, ImportDone{RepoID: repo, Node: "git-a"})}
		}, "[" + project + "0/1/0/0/0]", 2},
		{"an archive", created, func(t *testing.T) []built {
			s := settingsFor("")
			s.Archived = true
			return []built{ccSettings(t, repo, s)}
		}, "[" + project + "0/0/1/0/0]", -1},
		{"an unarchive", archived, func(t *testing.T) []built {
			return []built{ccSettings(t, repo, settingsFor(""))}
		}, "[" + project + "0/1/0/0/0]", -1},
		{"a settings change to another owner", created, func(t *testing.T) []built {
			return []built{ccSettings(t, repo, ccOwned(ccOPS, VisibilityCompany, 1))}
		}, "[company/unit/OPS/project=0/1/0/0/0]", -1},
		{"a visibility change", created, func(t *testing.T) []built {
			return []built{ccSettings(t, repo, ccOwned(ccENG, VisibilityUnit, 1))}
		}, "[unit/unit/ENG/project=0/1/0/0/0]", -1},
		{"a pull mirror set on an active repository", created, func(t *testing.T) []built {
			s := settingsFor("")
			s.MirrorFrom = &MirrorSource{URL: "https://example.com/c.git"}
			return []built{ccSettings(t, repo, s)}
		}, "[" + project + "1/0/0/0/0]", -1},
		{"a delete", created, func(t *testing.T) []built { return []built{deleteOf(t)} },
			"[" + project + "0/0/0/1/0]", -1},
		{"a deleted repository's owner moved", deleted, func(t *testing.T) []built {
			return []built{ccSettings(t, repo, ccOwned(ccOPS, VisibilityCompany, 1))}
		}, "[company/unit/OPS/project=0/0/0/1/0]", -1},
		{"a restore under a third owner", func(t *testing.T, e *estate) {
			deleted(t, e)
			e.apply(ccSettings(t, repo, ccOwned(ccOPS, VisibilityCompany, 1)))
		}, func(t *testing.T) []built { return []built{restored(t, ccQA)} },
			"[company/unit/QA/project=0/1/0/0/0]", -1},
		{"a restore of a repository whose import never finished", func(t *testing.T, e *estate) {
			importing(t, e)
			e.apply(deleteOf(t))
		}, func(t *testing.T) []built { return []built{restored(t, ccENG)} }, "[" + project + "1/0/0/0/0]", -1},
		// A first part of an empty repository writes its counters' row and its one tally row.
		{"the first purge part", deleted, func(t *testing.T) []built { return []built{partOf(t, repo, 1)} },
			"[" + project + "0/0/0/0/1]", 2},
		{"a later purge part", purging, func(t *testing.T) []built { return []built{partOf(t, repo, 2)} },
			"[" + project + "0/0/0/0/1]", 1},
		// The purge's name claim, row, kind's count, tally row and marker.
		{"the purge", purging, func(t *testing.T) []built { return []built{purgeOf(t)} }, "[]", 5},
		{"the purge beside another repository of the key", func(t *testing.T, e *estate) {
			purging(t, e)
			e.apply(create(t, "repo-d", "d"))
		}, func(t *testing.T) []built { return []built{purgeOf(t)} }, "[" + project + "0/1/0/0/0]", 5},
		{"a redelivered create", created, func(t *testing.T) []built { return []built{create(t, repo, "c")} },
			"[" + project + "0/1/0/0/0]", -1},
		{"a redelivered purge", purging, func(t *testing.T) []built {
			p := purgeOf(t)
			return []built{p, p}
		}, "[]", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := openApplyEstate(t)
			tc.given(t, e)
			e.changeCountsMatchRows()
			records := tc.records(t)
			e.apply(records[:len(records)-1]...)
			rows := e.applyCounting(records[len(records)-1])
			if tc.rows >= 0 && rows != tc.rows {
				t.Errorf("the record reported %d rows, want %d", rows, tc.rows)
			}
			if got := e.repoTallies(); got != tc.tallies {
				t.Errorf("the repository tallies hold %s, want %s", got, tc.tallies)
			}
			e.changeCountsMatchRows()
		})
	}
}

// THE REPOSITORY TALLIES ARE RE-DERIVED: on tallies the applies kept the re-derivation writes
// nothing, and it rebuilds them whole after they are lost, and a corrupted row alone.
// Mutation: let Rederive skip rederiveRepoTallies, or tally only the repositories in use.
func TestTheRepositoryTalliesAreReDerived(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-a", "a"), create(t, "repo-b", "b"), create(t, "repo-c", "c"),
		ccSettings(t, "repo-b", ccOwned(ccOPS, VisibilityUnit, 1)),
		ccRecord(t, "repo-c", OpDelete, LifecycleRecord{RepoID: "repo-c", ConfigEpoch: 1}))
	kept := e.repoTallies()
	if n := e.rederive(); n != 0 {
		t.Errorf("re-deriving the tallies the applies kept wrote %d rows", n)
	}
	e.exec(`DELETE FROM code_repo_tallies`)
	if n := e.rederive(); n != 2 {
		t.Errorf("re-deriving lost tallies wrote %d rows, want one a key, 2", n)
	}
	if got := e.repoTallies(); got != kept {
		t.Errorf("the re-derived tallies hold %s, and the applies kept %s", got, kept)
	}
	e.changeCountsMatchRows()
	e.exec(`UPDATE code_repo_tallies SET deleted_repos = 7 WHERE owner = 'ENG'`)
	if n := e.rederive(); n != 1 {
		t.Errorf("re-deriving one corrupted row wrote %d rows, want 1", n)
	}
	e.exec(`INSERT INTO code_repo_tallies VALUES ('company', 'unit', 'QA', 'project', 0, 1, 0, 0, 0)`)
	if n := e.rederive(); n != 1 || e.repoTallies() != kept {
		t.Errorf("re-deriving a row no repository stands for wrote %d rows and left %s, want 1 and %s", n,
			e.repoTallies(), kept)
	}
	e.changeCountsMatchRows()
}

// A PUSH REWRITES THE PUSHED INDEX ALONE: the counters a push record writes move its
// repository's entry of code_repos_pushed_idx, and neither the kind's nor the name's index.
// Mutation: key the kind index on last_push_at, or drop last_push_at from the pushed index.
func TestAPushRewritesThePushedIndexAlone(t *testing.T) {
	t.Parallel()
	e := openApplyEstate(t)
	e.apply(create(t, "repo-c", "c"))
	ctx := context.Background()
	if err := e.db.Tx(ctx, func(tx *sql.Tx) error {
		roots := indexRootsOf(ctx, t, tx, "code_repos")
		got := rewrites(t, program(ctx, t, tx, pushCountersSQL,
			[]any{1, 1, 0, 0, 1, 10, 1, 1, false, 5, false, 5, 2, 0, "repo-c"}), roots)
		if !slices.Contains(got, pushedReposKey) || slices.Contains(got, kindReposKey) ||
			slices.Contains(got, "code_repos_name_idx") {
			t.Errorf("a push's counters rewrite %q of code_repos' indexes, want %s and neither %s nor the name's",
				got, pushedReposKey, kindReposKey)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// REPOSITORY COUNTS COUNT EVERY STATUS AND KIND: each status moves exactly its own field and,
// where in use, Live; counts plus their negation are nothing; every kind has its field.
// Mutation: a status or a kind added and not counted.
func TestRepoCountsCountEveryStatusAndKind(t *testing.T) {
	t.Parallel()
	for _, s := range RepoStatuses() {
		var c RepoCounts
		c.add(s, 1)
		if c.sum(RepoStatuses()) != 1 || c.in(s) != 1 || (c.Live == 1) != s.live() || c.zero() {
			t.Errorf("one repository %s counts %+v, want exactly its own status, and live only where it is", s, c)
		}
		if got := c.plus(c.negated()); got != (RepoCounts{}) {
			t.Errorf("%+v and its negation sum to %+v", c, got)
		}
	}
	var none RepoCounts
	if none.add("later", 1); none != (RepoCounts{}) {
		t.Errorf("a status this build does not know counted %+v", none)
	}
	var k RepoKindCounts
	fields := map[*RepoCounts]bool{}
	for _, kind := range RepoKinds() {
		of := k.of(kind)
		if of == nil || fields[of] {
			t.Errorf("the kind %s has no field of its own in RepoKindCounts", kind)
		}
		fields[of] = true
	}
	if n := reflect.TypeFor[RepoKindCounts]().NumField(); n != len(fields) || k.of("later") != nil {
		t.Errorf("RepoKindCounts holds %d fields and the kinds name %d, or a kind this build does not know "+
			"names one", n, len(fields))
	}
}
