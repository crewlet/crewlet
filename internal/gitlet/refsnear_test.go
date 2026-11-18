package gitlet_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/store"
)

// A repository is near the limit when its live refs plus the refs it set
// within RefExpiry reach MaxRefsPerRepo, at the sum exactly too (the push
// decide already refuses every create there), and one short is not: the alarm
// adds no margin of its own.
//
// Mutation: compare with > and the sum at the bound is not near — red;
// compare with >= MaxRefsPerRepo-1 and the sum one short is near — red.
func TestNearLimitIsTheBoundsArithmetic(t *testing.T) {
	t.Parallel()
	half := gitlet.MaxRefsPerRepo / 2
	for _, c := range []struct {
		live, recent int
		want         bool
	}{
		{0, 0, false},
		{half, half, true},
		{half, half - 1, false},
		{half + 1, half, true},
		{gitlet.MaxRefsPerRepo, 0, true},
		{gitlet.MaxRefsPerRepo - 1, 0, false},
		{gitlet.MaxRefsPerRepo, 1, true},
		{60_000, 45_000, true},
		{60_000, 30_000, false},
	} {
		if got := gitlet.NearLimit(c.live, c.recent); got != c.want {
			t.Errorf("NearLimit(%d live, %d recent) = %v, want %v against the bound of %d",
				c.live, c.recent, got, c.want, gitlet.MaxRefsPerRepo)
		}
	}
}

// The reading counts active repositories and live refs alone (live+recent):
// alpha 60k+45k counted; bravo 60k+30k not, its 20k recent tombstones being
// deletions; charlie 1k+1k never a candidate; delta archived (51k+51k), takes
// no push, not counted; echo 56k+56k counted and the worst; foxtrot exactly
// MaxRefsPerRepo, none recent, counted at the bound; golf half the bound, all
// recent, counted only because the cut takes half the bound itself.
//
// Mutation: drop the empty-target test and bravo counts; drop the status and
// delta counts; keep the first past the point as worst and alpha is named;
// judge the sum with > and foxtrot and golf drop; cut the candidates with >
// and golf is never read — each red.
func TestRefsNearLimitCountsWhatTheBoundAndTheWindowSay(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	now := harnessAt
	recent := store.EncodeTime(now.Add(-time.Hour))
	old := store.EncodeTime(now.Add(-2 * gitlet.RefExpiry))
	seed := func(repo, prefix, target string, count int, at int64) {
		t.Helper()
		n.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_refs (repo_id, ref, target, position, at)
			SELECT ?, printf('refs/heads/%s/%06d', ?, i), ?, 1, ? FROM k`, count, repo, prefix, target, at)
	}
	live := func(name string, recentRefs, oldRefs int) string {
		t.Helper()
		repo := n.ensure(name, suiteSettings(), t1).RepoID
		// The recursive seed writes one row at a count of zero, so a count of
		// none seeds nothing.
		if recentRefs > 0 {
			seed(repo, "recent", oidOf(name), recentRefs, recent)
		}
		if oldRefs > 0 {
			seed(repo, "old", oidOf(name), oldRefs, old)
		}
		n.exec(`UPDATE code_repos SET refs = refs + ? WHERE id = ?`, recentRefs+oldRefs, repo)
		return repo
	}
	live("alpha", 45_000, 15_000)
	bravo := live("bravo", 30_000, 30_000)
	seed(bravo, "deleted", "", 20_000, recent)
	n.exec(`UPDATE code_repos SET tombstones = tombstones + ? WHERE id = ?`, 20_000, bravo)
	live("charlie", 1_000, 0)
	delta := live("delta", 51_000, 0)
	n.exec(`UPDATE code_repos SET status = 'archived' WHERE id = ?`, delta)
	live("echo", 56_000, 0)
	live("foxtrot", 0, gitlet.MaxRefsPerRepo)
	live("golf", gitlet.MaxRefsPerRepo/2, 0)

	got, err := n.reader.RefsNearLimit(t.Context(), now)
	if err != nil {
		t.Fatalf("read the repositories near the ref bound: %v", err)
	}
	want := gitlet.RefsNear{Repos: 4, Worst: "echo", Live: 56_000, Recent: 56_000}
	if got != want {
		t.Fatalf("the reading is %+v, want %+v: alpha, echo, foxtrot (at the bound) and golf (reaching it "+
			"from half) counted, echo the most, and neither bravo's tombstones nor the archived delta",
			got, want)
	}

	// A WINDOW LATER every ref set an hour ago is past RefExpiry, so no
	// repository sets refs fast enough to reach the bound — but foxtrot still
	// holds it, and every create there is still refused: it alone is counted.
	later, err := n.reader.RefsNearLimit(t.Context(), now.Add(gitlet.RefExpiry))
	if want := (gitlet.RefsNear{Repos: 1, Worst: "foxtrot", Live: gitlet.MaxRefsPerRepo}); err != nil ||
		later != want {
		t.Fatalf("a window later the reading is (%+v, %v), want (%+v, nil): foxtrot alone, at the bound",
			later, err, want)
	}
}
