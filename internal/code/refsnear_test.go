package code_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/store"
)

// THE POINT IS THE BOUND'S AND THE WINDOW'S ARITHMETIC: a repository is near
// the limit when its live refs and the refs it set within RefExpiry together
// pass MaxRefsPerRepo, and at that sum exactly it is not — the alarm borrows
// both decisions whole and adds no margin of its own.
//
// Mutation: compare with >= and the sum at the bound is near — red.
func TestNearLimitIsTheBoundsArithmetic(t *testing.T) {
	t.Parallel()
	half := code.MaxRefsPerRepo / 2
	for _, c := range []struct {
		live, recent int
		want         bool
	}{
		{0, 0, false},
		{half, half, false},
		{half + 1, half, true},
		{code.MaxRefsPerRepo, 0, false},
		{code.MaxRefsPerRepo, 1, true},
		{60_000, 45_000, true},
		{60_000, 30_000, false},
	} {
		if got := code.NearLimit(c.live, c.recent); got != c.want {
			t.Errorf("NearLimit(%d live, %d recent) = %v, want %v against the bound of %d",
				c.live, c.recent, got, c.want, code.MaxRefsPerRepo)
		}
	}
}

// THE READING COUNTS WHAT THE BOUND AND THE WINDOW SAY, of the active
// repositories alone and of their live refs alone:
//
//   - alpha holds 60 000 live refs, 45 000 of them set within RefExpiry:
//     105 000, past the bound — counted;
//   - bravo holds 60 000, 30 000 of them recent — 90 000, inside it — and
//     20 000 tombstones written within the window beside them, which are
//     deletions, never refs set: counted, they would take it to 110 000;
//   - charlie holds 1 000, every one recent: never a candidate;
//   - delta is ARCHIVED with 51 000, every one recent — 102 000 — and takes no
//     push, so nothing it holds moves towards the bound: not counted;
//   - echo holds 56 000, every one recent: 112 000, counted, and the worst.
//
// Mutation: drop the empty-target test from the count and bravo's tombstones
// count — three, red; drop the status from the candidates and the archived delta
// counts — red; keep the first repository past the point as the worst rather
// than the one with the most, and alpha is named — red.
func TestRefsNearLimitCountsWhatTheBoundAndTheWindowSay(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	now := harnessAt
	recent := store.EncodeTime(now.Add(-time.Hour))
	old := store.EncodeTime(now.Add(-2 * code.RefExpiry))
	seed := func(repo, prefix, target string, count int, at int64) {
		t.Helper()
		n.exec(`WITH RECURSIVE k(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM k WHERE i + 1 < ?)
			INSERT INTO code_refs (repo_id, ref, target, position, at)
			SELECT ?, printf('refs/heads/%s/%06d', ?, i), ?, 1, ? FROM k`, count, repo, prefix, target, at)
	}
	live := func(name string, recentRefs, oldRefs int) string {
		t.Helper()
		repo := n.ensure(name, suiteSettings(), t1).RepoID
		seed(repo, "recent", oidOf(name), recentRefs, recent)
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

	got, err := n.reader.RefsNearLimit(t.Context(), now)
	if err != nil {
		t.Fatalf("read the repositories near the ref bound: %v", err)
	}
	want := code.RefsNear{Repos: 2, Worst: "echo", Live: 56_000, Recent: 56_000}
	if got != want {
		t.Fatalf("the reading is %+v, want %+v: alpha and echo past the bound, echo the most, and neither "+
			"bravo's tombstones nor the archived delta counted", got, want)
	}

	// A WINDOW LATER every ref set an hour ago is past RefExpiry, so no
	// repository sets refs fast enough to pass the bound: the reading clears.
	if got, err := n.reader.RefsNearLimit(t.Context(), now.Add(code.RefExpiry)); err != nil ||
		got != (code.RefsNear{}) {
		t.Fatalf("a window later the reading is (%+v, %v), want none near the bound", got, err)
	}
}
