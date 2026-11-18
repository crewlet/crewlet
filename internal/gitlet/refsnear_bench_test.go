package gitlet_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/store"
)

// BenchmarkRefsNearLimit times one reading of code_refs_near_limit at
// Gitlet's size: config.CodeMaxRepos (10 000) active repositories, three of
// them candidates each holding MaxRefsPerRepo live refs set within RefExpiry
// and MaxTombstonesPerRepo tombstones, so each count reads its repo_id
// prefix's widest range, 200 000 entries.
//
// The rule: past one second a reading, the count gets an index of its own
// (code_refs_live_at_idx, over (repo_id, at) of the rows with a target);
// under it the primary key serves. It reads in 142 ms (-benchtime 1x on two
// E-cores), so there is no such index.
func BenchmarkRefsNearLimit(b *testing.B) {
	n := newNode(b)
	ctx := context.Background()
	at := time.Date(2031, 4, 2, 3, 14, 0, 0, time.UTC)
	set := store.EncodeTime(at.Add(-time.Hour))
	exec := n.exec
	exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
		INSERT INTO code_repos (id, name, status, managed_by, owner_kind, owner, default_branch, created_at)
		SELECT printf('repo-%05d', i), printf('r-%05d', i), 'active', 'config', 'unit', 'ENG', 'main', 0 FROM k`,
		10_000)
	for _, repo := range []string{"repo-00100", "repo-05000", "repo-09900"} {
		exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_refs (repo_id, ref, target, position, at)
			SELECT ?, printf('refs/heads/b%06d', i), ?, 1, ? FROM k`, gitlet.MaxRefsPerRepo, repo, oidOf(repo), set)
		exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_refs (repo_id, ref, target, position, at)
			SELECT ?, printf('refs/heads/gone/b%06d', i), '', 1, ? FROM k`, gitlet.MaxTombstonesPerRepo, repo, set)
		exec(`UPDATE code_repos SET refs = ?, tombstones = ? WHERE id = ?`, gitlet.MaxRefsPerRepo,
			gitlet.MaxTombstonesPerRepo, repo)
	}
	for b.Loop() {
		got, err := n.reader.RefsNearLimit(ctx, at)
		if err != nil || got.Repos != 3 {
			b.Fatalf("the reading is (%+v, %v), want the three candidates near the bound", got, err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/reading")
}
