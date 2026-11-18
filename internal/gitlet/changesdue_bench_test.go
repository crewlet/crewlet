package gitlet_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// benchRechecks is the rechecks a repository's turn asks at the most: the
// home's MergeabilityPerPass (internal/gitlet/home), which this package's
// suites do not import.
const benchRechecks = 64

// BenchmarkChangesDue times the `changes_due` reads a home makes, at the scale
// changesdue.go's head states its figures at: 1 000 repositories of 10 live
// changes, nothing due (the steady state a backstop pass walks whole), beside
// ONE of 10 000 live changes behind a moved target, every one due a recheck:
//
//   - repos: a backstop pass, every page: every live change checked against its
//     facts', mergeability's and conversation's keys, answering the busy one;
//   - changes: the busy repository's turn, every page, no rechecks asked: none due;
//   - rechecks: that turn's first page asking benchRechecks, oldest merge first.
//
// A node pays repos once a pass plus changes or rechecks per repository it homes
// with work; the fleet pays repos once a Gitlet node a Backstop (internal/gitlet/home).
func BenchmarkChangesDue(b *testing.B) {
	n := newNode(b)
	ctx := context.Background()
	const (
		quietRepos, quietChanges = 1_000, 10
		busyChanges              = 10_000
		// tip is the position of every target's tip; a held merge is of it,
		// a moved target's merge of the earlier one.
		tip, earlier = 100, 50
	)
	// seed is a repository of changes live changes, each merged into its
	// target at the tip position mergedTip — held where that is the tip's —
	// recorded at its index's instant, the oldest first.
	seed := func(changes int, mergedTip int64) string {
		id := uuid.Must(uuid.NewV7()).String()
		if err := n.db.Tx(ctx, func(tx *sql.Tx) error {
			for _, stmt := range []struct {
				sql  string
				args []any
			}{
				{`INSERT INTO code_repos (id, name, status, managed_by, owner_kind, owner, default_branch,
					created_at) VALUES (?, ?, 'active', 'config', 'unit', 'ENG', 'main', 0)`, []any{id, "r-" + id}},
				{`INSERT INTO code_refs (repo_id, ref, target, position, at) VALUES (?, 'refs/heads/main', ?, ?,
					0)`, []any{id, oidOf("tip-" + id), tip}},
				{`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
					INSERT INTO code_changes (id, repo_id, number, title, body_bytes, state, source_ref,
						target_ref, head_oid, revision, owner, owner_kind, opened_at, opened_position, version,
						position)
					SELECT printf('%s-c%06d', ?, i), ?, i + 1, 'a change', 0, 'open',
						printf('refs/heads/c%06d', i), 'refs/heads/main', printf('%040d', i), 1, 'seat', 'agent',
						0, i + 1, 1, i + 1 FROM k`, []any{changes, id, id}},
				{`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
					INSERT INTO code_conversations (change_id, repo_id, comments, text_bytes, version, position,
						readiness_version)
					SELECT printf('%s-c%06d', ?, i), ?, 0, 0, 1, 1, 1 FROM k`, []any{changes, id, id}},
				{`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
					INSERT INTO code_revision_facts (change_id, n, target_ref, target_oid, target_position,
						base_tree, head_tree, touched_count, node, owners_digest, human_paths_digest, bytes, position,
						classes, touched)
					SELECT printf('%s-c%06d', ?, i), 1, 'refs/heads/main', '', ?, '', '', 1, 'node-a', '', '', 0, 1,
						'', '' FROM k`, []any{changes, id, tip}},
				{`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
					INSERT INTO code_mergeability (change_id, revision, target_ref, target_oid, target_position,
						state, conflicts_count, node, bytes, position, at, change_version, readiness_version,
						checks_position, ready, waits_person, conflicts)
					SELECT printf('%s-c%06d', ?, i), 1, 'refs/heads/main', '', ?, 'clean', 0, 'node-a', 0, 1,
						i, 1, 1, 0, 0, 0, '' FROM k`, []any{changes, id, mergedTip}},
			} {
				if _, err := tx.ExecContext(ctx, stmt.sql, stmt.args...); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			b.Fatalf("seed a repository of %d live changes: %v", changes, err)
		}
		return id
	}
	for range quietRepos {
		seed(quietChanges, tip)
	}
	busy := seed(busyChanges, earlier)
	// THE INSTANT A RECHECK IS DUE BY: every merge the seed recorded is older.
	due := harnessAt

	read := func(q gitlet.ReadQuery) gitlet.ReadAnswer {
		a, err := n.reader.Read(ctx, q)
		if err != nil {
			b.Fatalf("read %+v: %v", q.ChangesDue, err)
		}
		return a
	}
	b.Run("repos", func(b *testing.B) {
		for b.Loop() {
			var repos []string
			q := gitlet.ReadQuery{Kind: gitlet.ReadChangesDue, Fresh: stale,
				ChangesDue: &gitlet.ChangesDueQuery{RecheckBefore: due}}
			for {
				a := read(q)
				repos = append(repos, a.DueRepos...)
				if a.Next == "" {
					break
				}
				q.Cursor = a.Next
			}
			if len(repos) != 1 || repos[0] != busy {
				b.Fatalf("the pass listed %v, want the busy repository %s alone", repos, busy)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Microseconds())/1e3/float64(b.N), "ms/pass")
	})
	b.Run("changes", func(b *testing.B) {
		for b.Loop() {
			q := gitlet.ReadQuery{Kind: gitlet.ReadChangesDue, Fresh: stale,
				ChangesDue: &gitlet.ChangesDueQuery{RepoID: busy, RecheckBefore: due}}
			for {
				a := read(q)
				if len(a.DueChanges) != 0 {
					b.Fatalf("the busy repository listed %d due changes, want none: every merge is held",
						len(a.DueChanges))
				}
				if a.Next == "" {
					break
				}
				q.Cursor = a.Next
			}
		}
		b.ReportMetric(float64(b.Elapsed().Microseconds())/1e3/float64(b.N), "ms/turn")
	})
	b.Run(fmt.Sprintf("rechecks=%d", benchRechecks), func(b *testing.B) {
		for b.Loop() {
			a := read(gitlet.ReadQuery{Kind: gitlet.ReadChangesDue, Fresh: stale,
				ChangesDue: &gitlet.ChangesDueQuery{RepoID: busy, RecheckBefore: due, Rechecks: benchRechecks}})
			if len(a.Rechecks) != benchRechecks || a.Next != "" {
				b.Fatalf("the busy repository's first page named %d rechecks (next %q), want %d on one page",
					len(a.Rechecks), a.Next, benchRechecks)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Microseconds())/1e3/float64(b.N), "ms/turn")
	})
}
