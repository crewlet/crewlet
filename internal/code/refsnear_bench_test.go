package code_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/store"
)

// BenchmarkRefsNearLimit times ONE READING of code_refs_near_limit at the
// code host's size (M3-master §11 #7): config.CodeMaxRepos (10 000) active
// repositories walked by name, three of them candidates each holding
// MaxRefsPerRepo live refs, every one set within RefExpiry, and
// MaxTombstonesPerRepo tombstones beside them — so each count reads the
// widest range its key's repo_id prefix has, 200 000 entries.
//
// THE DECISION RULE (M3-boxes §10.5): past one second a reading, the count
// moves to an index of its own — code_refs_live_at_idx, over code_refs'
// (repo_id, at) of the rows with a target — in a migration of its own; under
// it, the primary key serves. Measured with -benchtime 1x -count 1 on the
// runner's two E-cores: 142 ms a reading, a seventh of the rule's second, so
// the primary key serves and no index was added.
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
			SELECT ?, printf('refs/heads/b%06d', i), ?, 1, ? FROM k`, code.MaxRefsPerRepo, repo, oidOf(repo), set)
		exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_refs (repo_id, ref, target, position, at)
			SELECT ?, printf('refs/heads/gone/b%06d', i), '', 1, ? FROM k`, code.MaxTombstonesPerRepo, repo, set)
		exec(`UPDATE code_repos SET refs = ?, tombstones = ? WHERE id = ?`, code.MaxRefsPerRepo,
			code.MaxTombstonesPerRepo, repo)
	}
	for b.Loop() {
		got, err := n.reader.RefsNearLimit(ctx, at)
		if err != nil || got.Repos != 3 {
			b.Fatalf("the reading is (%+v, %v), want the three candidates near the bound", got, err)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Milliseconds())/float64(b.N), "ms/reading")
}
