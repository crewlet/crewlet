package cache_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A PINNED REPOSITORY IS NEVER EVICTED, whatever the budget: a reader's git
// process has its directory as its GIT_DIR, and a request it is serving is
// the very use the budget is there to keep warm. Once it lets go, the next
// pass takes it.
func TestAPinnedRepositoryIsNeverEvicted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, func(o *cache.Options) { o.MaxBytes = 1 })
	s := w.sync(t, c, repo)
	cache.JanitorPass(c)
	if _, ok := cache.Repos(c)[repo]; !ok || !exists(s.Dir) {
		t.Fatal("a pinned repository was evicted")
	}
	s.Release()
	cache.JanitorPass(c)
	if _, ok := cache.Repos(c)[repo]; ok || exists(s.Dir) {
		t.Fatal("an unpinned repository over the budget was kept")
	}
	lines := w.logs.lines(t, "code_cache_evicted")
	if len(lines) != 1 || lines[0]["repo"] != repo || lines[0]["reason"] != "budget" {
		t.Errorf("code_cache_evicted = %v, want one line naming %s for the budget", lines, repo)
	}
}

// EVICTION KEEPS THE CACHE UNDER ITS BUDGET, least recently used first: five
// repositories used in a known order, a budget of two and a half of them, and
// one pass evicts the three used longest ago — the third because two and a
// half is not three. What the pass counts is what the gauge says.
func TestEvictionKeepsTheCacheUnderBudget(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	var repos []string
	for n := range 5 {
		repo := w.repo(t, n+1)
		w.push(t, repo, "refs/heads/main", nil)
		repos = append(repos, repo)
	}
	c := w.open(t, clk.on)
	sizes := map[string]int64{}
	for _, repo := range repos {
		clk.Add(time.Minute)
		s := w.sync(t, c, repo)
		sizes[repo] = heldBy(t, s.Dir)
		s.Release()
	}
	cache.SetMaxBytes(c, sizes[repos[3]]+sizes[repos[4]]+sizes[repos[2]]/2)
	cache.JanitorPass(c)

	held := cache.Repos(c)
	for i, repo := range repos {
		if _, ok := held[repo]; ok != (i >= 3) {
			t.Errorf("repository %d (used %d of 5) held = %t after the pass", i+1, i+1, ok)
		}
	}
	if total := cache.Total(c); total > sizes[repos[3]]+sizes[repos[4]]+sizes[repos[2]]/2 {
		t.Errorf("the total after the pass is %d, over the budget", total)
	}
	if got := gauge(t, w.rec, metrics.CodeCacheBytes); got != float64(cache.Total(c)) {
		t.Errorf("%s = %v, want the pass's total %d", metrics.CodeCacheBytes, got, cache.Total(c))
	}
	if got := gauge(t, w.rec, metrics.CodeCacheRepos); got != 2 {
		t.Errorf("%s = %v, want 2", metrics.CodeCacheRepos, got)
	}
	if got := names(t, w.path("trash")); len(got) != 0 {
		t.Errorf("trash/ holds %v after a pass that evicted: its removal is the same pass's", got)
	}
}

// AN IDLE REPOSITORY UNDER THE BUDGET IS KEPT, however long it has been idle:
// it costs disk the budget already bounds, and evicting it would cost a GET a
// push at its next fetch. Brought forward a month later it costs what moved —
// one delta call and the one new pack — and a cache older than the rows'
// tombstone horizon is answered whole, reusing every pack it holds.
func TestAnIdleRepositoryUnderBudgetIsKept(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/dev", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, repo).Release()

	clk.Add(30 * 24 * time.Hour)
	cache.JanitorPass(c)
	if _, ok := cache.Repos(c)[repo]; !ok {
		t.Fatal("a repository under the budget was evicted for being idle")
	}

	_, row := w.push(t, repo, "refs/heads/main", nil)
	calls, opens := len(w.log.firstPages(repo)), w.objects.total()
	s := w.sync(t, c, repo)
	if got := w.log.firstPages(repo)[calls:]; len(got) != 1 || got[0].Since.IsZero() {
		t.Errorf("bringing an idle cache forward asked %v, want one delta", got)
	}
	if got, _, _ := w.objects.counts(); w.objects.total()-opens != 1 || got[row.Object] != 1 {
		t.Errorf("bringing an idle cache forward downloaded %d bundles, want the new one", w.objects.total()-opens)
	}
	s.Release()

	w.log.write(repo, nil, map[string]string{"refs/heads/dev": ""})
	w.log.sweep(repo)
	opens = w.objects.total()
	s = w.sync(t, c, repo)
	if s.Kind != cache.SyncFull {
		t.Errorf("a cache older than the tombstone horizon was answered %s, want %s", s.Kind, cache.SyncFull)
	}
	if got := w.objects.total() - opens; got != 0 {
		t.Errorf("a whole answer to a warm cache downloaded %d bundles, want none", got)
	}
	if got := w.image(t, c, repo); got != w.rebuilt(t, repo) {
		t.Errorf("the image:\n%s\nwant a fresh cache's:\n%s", got, w.rebuilt(t, repo))
	}
}

// A REPOSITORY LARGER THAN THE BUDGET IS SERVED: a request is waiting for it,
// so its sync succeeds and says how far over it is; held, a pass keeps it, and
// released, the next pass takes it.
func TestARepositoryLargerThanTheBudgetIsServed(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, func(o *cache.Options) { o.MaxBytes = 100 })
	s := w.sync(t, c, repo)
	lines := w.logs.lines(t, "code_cache_over_budget")
	if len(lines) != 1 || lines[0]["budget"] != float64(100) || lines[0]["bytes"] != float64(heldBy(t, s.Dir)) {
		t.Errorf("code_cache_over_budget = %v, want one line naming %d bytes against 100", lines, heldBy(t, s.Dir))
	}
	cache.JanitorPass(c)
	if _, ok := cache.Repos(c)[repo]; !ok {
		t.Fatal("a repository a request holds was evicted")
	}
	s.Release()
	cache.JanitorPass(c)
	if _, ok := cache.Repos(c)[repo]; ok {
		t.Fatal("a repository over the budget nobody holds was kept")
	}
}

// TRASH IS REMOVED OFF THE REQUEST PATH: the release of the last pin on a
// retired incarnation — one a rebuild replaced — returns with the directory
// renamed into trash/, not removed; the janitor removes it.
func TestTrashIsRemovedOffTheRequestPath(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	old := w.sync(t, c, repo)
	rebuilt, err := c.Rebuild(t.Context(), old, cache.RebuildCorrupt)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	defer rebuilt.Release()
	old.Release()
	if exists(old.Dir) {
		t.Fatal("a retired incarnation stayed in repos/ after its last reader let go")
	}
	moved := trashed(t, w, filepath.Base(old.Dir))
	if len(moved) != 1 {
		t.Fatalf("trash/ holds %v, want the released incarnation", names(t, w.path("trash")))
	}
	if !exists(filepath.Join(w.path("trash"), moved[0], "objects")) {
		t.Error("the released incarnation was removed on the request's path; trash/ is the janitor's to empty")
	}
	cache.JanitorPass(c)
	if got := names(t, w.path("trash")); len(got) != 0 {
		t.Errorf("trash/ after a pass holds %v", got)
	}
}

// trashed is every entry of trash/ that is base moved there.
func trashed(t *testing.T, w *syncWorld, base string) []string {
	t.Helper()
	var out []string
	for _, name := range names(t, w.path("trash")) {
		if strings.HasSuffix(name, "-"+base) {
			out = append(out, name)
		}
	}
	return out
}

// AN EVICTED REPOSITORY SYNCS COLD: nothing of it is left on the node, so its
// next sync reads it whole and downloads every pack again, into a first
// incarnation.
func TestAnEvictedRepositorySyncsCold(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, a := w.push(t, repo, "refs/heads/main", nil)
	_, b := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	w.sync(t, c, repo).Release()
	cache.SetMaxBytes(c, 1)
	cache.JanitorPass(c)
	if _, ok := cache.Repos(c)[repo]; ok {
		t.Fatal("nothing was evicted")
	}
	cache.SetMaxBytes(c, 1<<30)

	pages := len(w.log.firstPages(repo))
	s := w.sync(t, c, repo)
	if got := w.log.firstPages(repo)[pages:]; len(got) != 1 || !got[0].Since.IsZero() {
		t.Errorf("the sync after an eviction asked %v, want one whole read", got)
	}
	opens, _, _ := w.objects.counts()
	if opens[a.Object] != 2 || opens[b.Object] != 2 {
		t.Errorf("after an eviction the packs were downloaded %d and %d times in all, want twice each",
			opens[a.Object], opens[b.Object])
	}
	if got := cache.Incarnation(s); got != 1 {
		t.Errorf("the repository came back as incarnation %d, want 1", got)
	}
	if got := w.image(t, c, repo); got != w.rebuilt(t, repo) {
		t.Errorf("the image:\n%s\nwant a fresh cache's:\n%s", got, w.rebuilt(t, repo))
	}
}

// A ROUND THAT GROWS THE CACHE PAST ITS BUDGET KICKS THE JANITOR: a burst of
// cold syncs would otherwise overshoot the budget by whatever arrives before
// the next tick. The janitor here never ticks, and its first pass has ended
// before the round, so only the round's kick can evict the repository used
// longest ago.
func TestAGrowingRoundKicksTheJanitor(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	a, b := w.repo(t, 1), w.repo(t, 2)
	w.push(t, a, "refs/heads/main", nil)
	w.push(t, b, "refs/heads/main", nil)
	c := w.open(t, clk.on, neverTicks)
	sa := w.sync(t, c, a)
	cache.SetMaxBytes(c, heldBy(t, sa.Dir))
	sa.Release()
	startIdle(t, w, c)

	clk.Add(time.Minute)
	sb := w.sync(t, c, b)
	defer sb.Release()
	eventually(t, "the least recently used repository is evicted", func() bool {
		_, ok := cache.Repos(c)[a]
		return !ok
	})
	if _, ok := cache.Repos(c)[b]; !ok {
		t.Error("the repository a reader holds was evicted")
	}
}

// A REPOSITORY OVER THE BUDGET GOES ONCE ITS LAST READER LETS GO: the release
// that unpins it kicks the janitor, rather than leaving it to the next tick —
// which this janitor never takes. The pass the round's own growth kicked has
// ended, keeping it, before the reader lets go.
func TestARepositoryOverTheBudgetGoesWithItsLastReader(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, neverTicks, func(o *cache.Options) { o.MaxBytes = 1 })
	startIdle(t, w, c)

	sentinel := w.path("trash", "sentinel")
	write(t, sentinel, "removed by the pass the round's growth kicks")
	s := w.sync(t, c, repo)
	eventually(t, "the pass the round kicked has ended", func() bool {
		return !exists(sentinel) && cache.Running(c) == 0
	})
	if _, ok := cache.Repos(c)[repo]; !ok {
		t.Fatal("a repository a reader holds was evicted")
	}
	s.Release()
	eventually(t, "the released repository over the budget is evicted", func() bool {
		_, ok := cache.Repos(c)[repo]
		return !ok
	})
}

// neverTicks is an Options mutation whose janitor and maintainer never tick:
// whatever either does, a kick asked for.
func neverTicks(o *cache.Options) {
	cache.TicksFrom(o, make(chan time.Time))
	cache.MaintainerTicksFrom(o, make(chan time.Time))
}

// startIdle starts c and waits until the janitor's first pass — the one Start
// runs at once, over what the boot moved into trash/ — has ended.
func startIdle(t *testing.T, w *syncWorld, c *cache.Cache) {
	t.Helper()
	c.Start(t.Context())
	eventually(t, "the janitor's first pass", func() bool {
		return len(names(t, w.path("trash"))) == 0 && cache.Running(c) == 0
	})
}

// WHAT SCRATCH/ HOLDS COUNTS AGAINST THE BUDGET: a push's scratch repository
// is disk the cache uses, and only the disk knows how much — so the janitor
// measures it, and both its total and the gauge include it.
func TestTheJanitorCountsWhatScratchHolds(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	c := w.open(t)
	cache.JanitorPass(c)
	before := cache.Total(c)
	write(t, w.path("scratch", "push.git", "objects", "pack", "pack-1.pack"), strings.Repeat("p", 4096))
	cache.JanitorPass(c)
	if got := cache.Total(c) - before; got != 4096 {
		t.Errorf("the total grew by %d with 4096 bytes in scratch/", got)
	}
	if got := gauge(t, w.rec, metrics.CodeCacheBytes); got != float64(cache.Total(c)) {
		t.Errorf("%s = %v, want the total %d", metrics.CodeCacheBytes, got, cache.Total(c))
	}
	if err := os.RemoveAll(w.path("scratch", "push.git")); err != nil {
		t.Fatal(err)
	}
	cache.JanitorPass(c)
	if got := cache.Total(c); got != before {
		t.Errorf("the total is %d once scratch/ is empty again, want %d", got, before)
	}
}

// A FILE IN SCRATCH/ WITH ANOTHER NAME IS COUNTED ONCE: a push's
// advertisement links the cache's packed-refs into its view, and an adopted
// push's pack is linked from its scratch into the incarnation before the
// scratch goes — the same bytes on the disk once, already in the total
// through the incarnation. Counted twice, every adopted push would evict as
// many bytes again of the repositories used least recently, each a cold
// sync later.
func TestAFileScratchSharesWithTheCacheIsCountedOnce(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, row := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	defer s.Release()
	cache.JanitorPass(c)
	before := cache.Total(c)
	view := w.path("scratch", "view.git")
	pack := w.path("scratch", "push.git", "objects", "pack")
	for _, d := range []string{view, pack} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for from, to := range map[string]string{
		filepath.Join(s.Dir, "packed-refs"):                                      filepath.Join(view, "packed-refs"),
		filepath.Join(s.Dir, "objects", "pack", "pack-"+row.GitChecksum+".pack"): filepath.Join(pack, "pack-"+row.GitChecksum+".pack"),
	} {
		if err := os.Link(from, to); err != nil {
			t.Fatal(err)
		}
	}
	cache.JanitorPass(c)
	if got := cache.Total(c); got != before {
		t.Errorf("the total moved from %d to %d with links to the incarnation's own files in scratch/", before, got)
	}
}
