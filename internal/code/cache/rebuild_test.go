package cache_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A REBUILD REPLACES THE DIRECTORY UNDER ITS READERS, never in place: the
// rebuilt repository is the next incarnation, beside the one a reader holds,
// whose git process keeps the files it opened — a path is a name, and renamed
// under it, that process would read another directory's files mid-run. The
// old one goes once its reader lets go.
func TestARebuildReplacesTheDirectoryUnderItsReaders(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	old := w.sync(t, c, repo)
	before := read(t, filepath.Join(old.Dir, "packed-refs"))

	rebuilt, err := c.Rebuild(t.Context(), old, cache.Failure{Reason: cache.RebuildCorrupt})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	defer rebuilt.Release()
	if cache.Incarnation(rebuilt) != 2 || rebuilt.Dir == old.Dir || rebuilt.Kind != cache.SyncFull {
		t.Fatalf("Rebuild answered incarnation %d at %s (%s), want incarnation 2 beside %s, read whole",
			cache.Incarnation(rebuilt), rebuilt.Dir, rebuilt.Kind, old.Dir)
	}
	if got := read(t, filepath.Join(old.Dir, "packed-refs")); got != before {
		t.Error("the rebuild changed the directory a reader holds")
	}
	if _, err := runGit(t, w.pool, old.Dir, "", "cat-file", "-e", tip); err != nil {
		t.Errorf("a reader of the replaced incarnation could not read its tip: %v", err)
	}
	if got := w.image(t, c, repo); got != w.rebuilt(t, repo) {
		t.Errorf("the rebuilt image:\n%s\nwant a fresh cache's:\n%s", got, w.rebuilt(t, repo))
	}

	old.Release()
	cache.JanitorPass(c)
	if exists(old.Dir) || len(names(t, w.path("trash"))) != 0 {
		t.Error("the replaced incarnation outlived its last reader")
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "corrupt"); got != 1 {
		t.Errorf("%s{reason=corrupt} = %d, want 1", metrics.CodeCacheRebuilds, got)
	}
	lines := w.logs.lines(t, "code_cache_rebuilt")
	if len(lines) != 1 || lines[0]["repo"] != repo || lines[0]["reason"] != "corrupt" || lines[0]["incarnation"] != float64(2) {
		t.Errorf("code_cache_rebuilt = %v, want one line naming %s, corrupt, incarnation 2", lines, repo)
	}
}

// MANY FAILURES ON ONE CACHE COST ONE REBUILD: ten requests failing on one
// damaged directory at once share one rebuild — one whole read and one
// download of each pack — and a request that failed on an incarnation already
// replaced is answered from the current one with no rebuild at all.
func TestManyFailuresOnOneCacheCostOneRebuild(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	for range 3 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t)
	failed := w.sync(t, c, repo)
	wholeReads := func() int {
		n := 0
		for _, call := range w.log.firstPages(repo) {
			if call.Since.IsZero() {
				n++
			}
		}
		return n
	}
	before, opens := wholeReads(), w.objects.total()

	// The rebuild's whole read waits until all ten are waiting on it (under
	// the hang detector, and never failing the case from the round's own
	// goroutine: a count short of ten shows in the assertions below).
	var once sync.Once
	w.log.set(func(f *fakeLog) {
		f.hook = func(call fakeCall) error {
			if call.Page == 1 && call.Since.IsZero() {
				once.Do(func() {
					for deadline := time.Now().Add(hangDetector); cache.Waiting(c, repo) < 10 && time.Now().Before(deadline); {
						time.Sleep(time.Millisecond)
					}
				})
			}
			return nil
		}
	})
	answers := make([]cache.Synced, 10)
	errs := make([]error, 10)
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			answers[i], errs[i] = c.Rebuild(context.Background(), failed, cache.Failure{Reason: cache.RebuildMissing})
		}()
	}
	wg.Wait()
	for i := range 10 {
		if errs[i] != nil {
			t.Fatalf("Rebuild %d: %v", i, errs[i])
		}
		defer answers[i].Release()
		if cache.Incarnation(answers[i]) != 2 || answers[i].Dir != answers[0].Dir {
			t.Errorf("Rebuild %d answered incarnation %d at %s, want the one rebuilt incarnation", i,
				cache.Incarnation(answers[i]), answers[i].Dir)
		}
	}
	if got := wholeReads() - before; got != 1 {
		t.Errorf("ten failures cost %d whole reads, want one", got)
	}
	if got := w.objects.total() - opens; got != 3 {
		t.Errorf("ten failures downloaded %d bundles, want each of the 3 packs once", got)
	}

	before, opens = wholeReads(), w.objects.total()
	late, err := c.Rebuild(t.Context(), failed, cache.Failure{Reason: cache.RebuildMissing})
	if err != nil {
		t.Fatalf("a rebuild of an incarnation already replaced: %v", err)
	}
	defer late.Release()
	if cache.Incarnation(late) != 2 || wholeReads() != before || w.objects.total() != opens {
		t.Errorf("a failure on a replaced incarnation rebuilt again: incarnation %d, %d whole reads, %d downloads",
			cache.Incarnation(late), wholeReads()-before, w.objects.total()-opens)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "missing"); got != 1 {
		t.Errorf("%s{reason=missing} = %d, want 1", metrics.CodeCacheRebuilds, got)
	}
	failed.Release()
}

// A REBUILD IS NOT REPEATED WHERE IT CANNOT HELP: a rebuild re-downloads the
// whole repository, and one that did not end a failure will not end it from
// the same rows — every byte verified on the way in makes the same directory,
// and the damage is the object store's or the rows', which verify names. So
// a rebuild's product that fails again is refused for as long as it holds
// what that rebuild read, however long ago that was; once its rows have
// moved, it waits RebuildCooldown from the last attempt, twice as long after
// the next, and so on. Every refusal is counted, and logged at most once a
// cooldown, so whether the window is ever reached is measured rather than
// guessed.
func TestARebuildIsNotRepeatedWhereItCannotHelp(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	first := w.sync(t, c, repo)
	began := clk.Now()
	second, err := c.Rebuild(t.Context(), first, cache.Failure{Reason: cache.RebuildCorrupt})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	defer second.Release()
	first.Release()

	refused := func(when string, s cache.Synced, rule string) {
		t.Helper()
		_, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildCorrupt})
		if !errors.Is(err, cache.ErrRebuildRefused) || !strings.Contains(err.Error(), "repo-1") ||
			!strings.Contains(err.Error(), "crewlet code verify repo-1") {
			t.Fatalf("a rebuild %s = %v, want ErrRebuildRefused naming repo-1 and crewlet code verify", when, err)
		}
		if got := counter(w.rec, metrics.CodeCacheRebuildRefusals, "rule", rule); got == 0 {
			t.Errorf("a rebuild %s was refused and %s{rule=%s} did not count it", when, metrics.CodeCacheRebuildRefusals, rule)
		}
	}
	rebuilt := func(when string, s cache.Synced, n int) cache.Synced {
		t.Helper()
		got, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildCorrupt})
		if err != nil {
			t.Fatalf("a rebuild %s: %v", when, err)
		}
		if cache.Incarnation(got) != n {
			t.Fatalf("a rebuild %s answered incarnation %d, want %d", when, cache.Incarnation(got), n)
		}
		return got
	}
	move := func() {
		t.Helper()
		w.push(t, repo, "refs/heads/main", nil)
		w.sync(t, c, repo).Release()
	}

	clk.Set(began.Add(cache.RebuildCooldown + time.Minute))
	refused("of what the last rebuild read, past the cooldown", second, "unchanged")

	move()
	again := clk.Now()
	third := rebuilt("once its rows moved, past the cooldown", second, 3)
	defer third.Release()

	move()
	clk.Set(again.Add(2*cache.RebuildCooldown - time.Minute))
	refused("of the second in a row, inside twice the cooldown", third, "cooldown")
	clk.Set(again.Add(2 * cache.RebuildCooldown))
	defer rebuilt("of the second in a row, at twice the cooldown", third, 4).Release()

	if got := w.logs.lines(t, "code_cache_rebuild_refused"); len(got) != 2 || got[0]["rule"] != "unchanged" ||
		got[1]["rule"] != "cooldown" || got[0]["repo"] != repo {
		t.Errorf("code_cache_rebuild_refused = %v, want one line a refusal a cooldown apart, naming %s", got, repo)
	}
	for _, n := range []int{1, 2, 3, 4, 8, 9, 30} {
		got, ceiling := cache.RebuildWindow(n), cache.RebuildBackoffCap
		if got > ceiling || (n > 1 && got < min(2*cache.RebuildWindow(n-1), ceiling)) {
			t.Errorf("RebuildWindow(%d) = %v: want twice the one before, at most %v", n, got, ceiling)
		}
	}
}

// WHAT THE MAINTAINER WRITES ENDS WHAT A REBUILD READ: a rebuild's product is
// refused another only while it holds exactly what that rebuild read, and the
// maintainer's index, bitmap and commit-graph are files no rebuild wrote — one
// of them damaged as it was written is damage a rebuild repairs. So once a run
// has written into the product, the next rebuild of it waits only its line's
// window, however long nobody has pushed.
func TestWhatTheMaintainerWritesEndsWhatARebuildRead(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	first := w.sync(t, c, repo)
	began := clk.Now()
	second, err := c.Rebuild(t.Context(), first, cache.Failure{Reason: cache.RebuildCorrupt})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	defer second.Release()
	first.Release()

	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v over the rebuilt repository, want it", ran)
	}
	clk.Set(began.Add(cache.RebuildCooldown + time.Minute))
	third, err := c.Rebuild(t.Context(), second, cache.Failure{Reason: cache.RebuildCorrupt})
	if err != nil {
		t.Fatalf("a rebuild of a product the maintainer has written into, past the cooldown: %v", err)
	}
	defer third.Release()
	if cache.Incarnation(third) != 3 {
		t.Errorf("the rebuild answered incarnation %d, want 3", cache.Incarnation(third))
	}
}

// A LINE OF REBUILDS ENDS WHEN THE ASKING STOPS: the cache never sees a
// request succeed, so a day with no rebuild asked of a repository is the sign
// the last one ended its failure, and the next ask is new damage — its window
// RebuildCooldown, not the one earlier incidents grew. Three incidents a quiet
// day apart, each repaired, then an attempt the estate fails transiently: the
// next is refused inside the cooldown and allowed at it, where a line that
// counted every rebuild the process ever made would wait eight times as long.
func TestALineOfRebuildsEndsWhenTheAskingStops(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	for n := 2; n <= 4; n++ {
		next, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildCorrupt})
		if err != nil {
			t.Fatalf("the rebuild of incident %d, a quiet day after the last: %v", n-1, err)
		}
		if cache.Incarnation(next) != n {
			t.Fatalf("the rebuild of incident %d answered incarnation %d, want %d", n-1, cache.Incarnation(next), n)
		}
		s.Release()
		s = next
		clk.Add(cache.RebuildQuiet)
	}
	defer func() { s.Release() }()

	unreachable := errors.New("the estate is unreachable")
	w.log.set(func(f *fakeLog) { f.hook = func(fakeCall) error { return unreachable } })
	failedAt := clk.Now()
	if _, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildMissing}); !errors.Is(err, unreachable) {
		t.Fatalf("a rebuild whose whole read failed = %v, want the read's error", err)
	}
	w.log.set(func(f *fakeLog) { f.hook = nil })

	clk.Set(failedAt.Add(cache.RebuildCooldown - time.Minute))
	if _, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildMissing}); !errors.Is(err, cache.ErrRebuildRefused) {
		t.Fatalf("a rebuild inside the cooldown of a failed first attempt = %v, want ErrRebuildRefused", err)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuildRefusals, "rule", "cooldown"); got != 1 {
		t.Errorf("%s{rule=cooldown} = %d, want the one refusal", metrics.CodeCacheRebuildRefusals, got)
	}
	clk.Set(failedAt.Add(cache.RebuildCooldown))
	again, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildMissing})
	if err != nil {
		t.Fatalf("a rebuild a cooldown after a failed first attempt at new damage: %v", err)
	}
	defer again.Release()
	if cache.Incarnation(again) != 5 {
		t.Errorf("the rebuild answered incarnation %d, want 5", cache.Incarnation(again))
	}
}

// DAMAGE NO REBUILD ENDS IS RETRIED ONCE A DAY, HOWEVER OFTEN IT IS ASKED
// ABOUT: a product failing again holds exactly what its rebuild read, and is
// refused while it does — a request every few minutes, or every few hours,
// is the failure going on, never a quiet that ends the line. But not for
// ever: the store can come to hold what it lacked and a directory be damaged
// on its own disk, so a day after the attempt — the one download a day the
// backoff's cap prices — it is rebuilt again.
func TestDamageNoRebuildEndsIsRetriedOnceADay(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	first := w.sync(t, c, repo)
	began := clk.Now()
	second, err := c.Rebuild(t.Context(), first, cache.Failure{Reason: cache.RebuildCorrupt})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	defer second.Release()
	first.Release()

	for _, after := range []time.Duration{
		time.Minute, cache.RebuildCooldown + 2*time.Minute, 6 * time.Hour, 12 * time.Hour, 18 * time.Hour,
		cache.RebuildBackoffCap - time.Minute,
	} {
		clk.Set(began.Add(after))
		_, err := c.Rebuild(t.Context(), second, cache.Failure{Reason: cache.RebuildCorrupt})
		if !errors.Is(err, cache.ErrRebuildRefused) {
			t.Fatalf("a rebuild of what the last one read, asked %v after it = %v, want ErrRebuildRefused", after, err)
		}
	}
	if got := counter(w.rec, metrics.CodeCacheRebuildRefusals, "rule", "unchanged"); got != 6 {
		t.Errorf("%s{rule=unchanged} = %d, want all 6 refusals", metrics.CodeCacheRebuildRefusals, got)
	}
	retried := began.Add(cache.RebuildBackoffCap)
	clk.Set(retried)
	third, err := c.Rebuild(t.Context(), second, cache.Failure{Reason: cache.RebuildCorrupt})
	if err != nil {
		t.Fatalf("a rebuild of what the last one read, a day after it: %v", err)
	}
	defer third.Release()
	if cache.Incarnation(third) != 3 {
		t.Errorf("the rebuild a day on answered incarnation %d, want 3", cache.Incarnation(third))
	}

	// The refusals were asks: the line went on through them, so the day's
	// retry is the SECOND attempt in a row, and once a push has moved its
	// product the next waits twice the cooldown, not once.
	w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo).Release()
	clk.Set(retried.Add(cache.RebuildCooldown + 5*time.Minute))
	if _, err := c.Rebuild(t.Context(), third, cache.Failure{Reason: cache.RebuildCorrupt}); !errors.Is(err, cache.ErrRebuildRefused) {
		t.Fatalf("a rebuild of the second in a row, inside twice the cooldown = %v, want ErrRebuildRefused", err)
	}
	clk.Set(retried.Add(2 * cache.RebuildCooldown))
	fourth, err := c.Rebuild(t.Context(), third, cache.Failure{Reason: cache.RebuildCorrupt})
	if err != nil {
		t.Fatalf("a rebuild of the second in a row, at twice the cooldown: %v", err)
	}
	defer fourth.Release()
}

// A REFUSED REBUILD IS LOGGED ONCE A COOLDOWN: every request failing on a
// damaged repository asks again, and one line a request would bury the one
// that says what to do; every refusal is still counted.
func TestARefusedRebuildIsLoggedOnceACooldown(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	first := w.sync(t, c, repo)
	second, err := c.Rebuild(t.Context(), first, cache.Failure{Reason: cache.RebuildMissing})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	defer second.Release()
	first.Release()
	for range 5 {
		if _, err := c.Rebuild(t.Context(), second, cache.Failure{Reason: cache.RebuildMissing}); !errors.Is(err, cache.ErrRebuildRefused) {
			t.Fatalf("a rebuild of what the last one read = %v, want ErrRebuildRefused", err)
		}
	}
	clk.Add(cache.RebuildCooldown)
	if _, err := c.Rebuild(t.Context(), second, cache.Failure{Reason: cache.RebuildMissing}); !errors.Is(err, cache.ErrRebuildRefused) {
		t.Fatalf("a rebuild of what the last one read = %v, want ErrRebuildRefused", err)
	}
	if got := len(w.logs.lines(t, "code_cache_rebuild_refused")); got != 2 {
		t.Errorf("six refusals over a cooldown logged %d lines, want 2", got)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuildRefusals, "reason", "missing"); got != 6 {
		t.Errorf("%s{reason=missing} = %d, want all 6", metrics.CodeCacheRebuildRefusals, got)
	}
}

// A FAILED REBUILD KEEPS THE OLD CACHE: the incarnation it was building goes
// into trash/ and the one it would have replaced is still current and still
// serving — and the attempt started the cooldown, success or not.
func TestAFailedRebuildKeepsTheOldCache(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, lost := w.push(t, repo, "refs/heads/main", nil)
	tip, _ := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	failed := w.sync(t, c, repo)
	defer failed.Release()
	if err := w.store.Delete(t.Context(), lost.Object); err != nil {
		t.Fatal(err)
	}

	_, err := c.Rebuild(t.Context(), failed, cache.Failure{Reason: cache.RebuildMissing})
	var missing *cache.ErrPackMissing
	if !errors.As(err, &missing) || missing.Pack.PackID != lost.PackID {
		t.Fatalf("a rebuild the store cannot complete = %v, want *ErrPackMissing naming %s", err, lost.PackID)
	}
	if got := cache.Repos(c)[repo]; got != 1 {
		t.Errorf("after a failed rebuild the current incarnation is %d, want the old one", got)
	}
	s, err := c.Sync(t.Context(), repo, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil || cache.Incarnation(s) != 1 || s.Kind != cache.SyncNone {
		t.Fatalf("a stale sync after a failed rebuild = incarnation %d (%s), %v; want the old one at once",
			cache.Incarnation(s), s.Kind, err)
	}
	defer s.Release()
	if _, err := runGit(t, w.pool, s.Dir, "", "cat-file", "-e", tip); err != nil {
		t.Errorf("the old incarnation no longer serves its tip: %v", err)
	}
	if got := trashed(t, w, repo+".2.git"); len(got) != 1 {
		t.Errorf("trash/ holds %v, want the partial incarnation 2", names(t, w.path("trash")))
	}
	if _, err = c.Rebuild(t.Context(), failed, cache.Failure{Reason: cache.RebuildMissing}); !errors.Is(err, cache.ErrRebuildRefused) {
		t.Errorf("a rebuild straight after a failed one = %v, want ErrRebuildRefused", err)
	}
}

// A FAILED REBUILD ANSWERS ONLY ITS OWN CALLERS: a sync queued before the
// rebuild was asked for rides the rebuilt round when it succeeds — a whole
// read at the log's head answers anything — but when the rebuild fails, the
// incarnation it would have replaced is still current and still serving, and
// the sync is answered from it by an ordinary round rather than told of
// somebody else's failed repair.
func TestAFailedRebuildAnswersOnlyItsOwnCallers(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	_, lost := w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	failed := w.sync(t, c, repo)
	defer failed.Release()
	if err := w.store.Delete(t.Context(), lost.Object); err != nil {
		t.Fatal(err)
	}

	// A round held at its first read, so the sync after it queues.
	holding, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	w.log.set(func(f *fakeLog) {
		f.hook = func(fakeCall) error {
			once.Do(func() {
				close(holding)
				<-release
			})
			return nil
		}
	})
	type answer struct {
		s   cache.Synced
		err error
	}
	running, queued, rebuilt := make(chan answer, 1), make(chan answer, 1), make(chan answer, 1)
	go func() {
		s, err := c.Sync(context.Background(), repo, linearizable)
		running <- answer{s, err}
	}()
	<-holding
	go func() {
		s, err := c.Sync(context.Background(), repo, linearizable)
		queued <- answer{s, err}
	}()
	eventually(t, "the second sync queues behind the held round", func() bool { return cache.Waiting(c, repo) == 2 })
	go func() {
		s, err := c.Rebuild(context.Background(), failed, cache.Failure{Reason: cache.RebuildMissing})
		rebuilt <- answer{s, err}
	}()
	eventually(t, "the rebuild joins the queued round", func() bool { return cache.Waiting(c, repo) == 3 })
	close(release)

	for _, ch := range []chan answer{running, queued, rebuilt} {
		select {
		case a := <-ch:
			if a.err == nil {
				defer a.s.Release()
			}
			ch <- a
		case <-time.After(hangDetector):
			t.Fatalf("a caller has had no answer in %v", hangDetector)
		}
	}
	r := <-rebuilt
	var missing *cache.ErrPackMissing
	if !errors.As(r.err, &missing) || missing.Pack.PackID != lost.PackID {
		t.Errorf("the rebuild = %v, want *ErrPackMissing naming %s", r.err, lost.PackID)
	}
	for name, ch := range map[string]chan answer{"held": running, "queued": queued} {
		a := <-ch
		if a.err != nil || cache.Incarnation(a.s) != 1 {
			t.Errorf("the %s sync = incarnation %d, %v; want the incarnation the failed rebuild left serving",
				name, incarnationOf(a.s), a.err)
		}
	}
}

// incarnationOf is s's incarnation, or zero for a sync that failed.
func incarnationOf(s cache.Synced) int {
	if s.Dir == "" {
		return 0
	}
	return cache.Incarnation(s)
}

// A REBUILD'S REASON IS THE HOST'S TO GIVE: what its git process was
// classified as, corrupt or missing. The log's own reasons are counted by
// the rounds that see them, and a Synced the cache did not hand out names no
// incarnation.
func TestARebuildReasonIsTheHostsToGive(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	for _, reason := range []cache.RebuildReason{cache.RebuildStream, cache.RebuildRewound, cache.RebuildVerify, "x", ""} {
		if _, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: reason}); !errors.Is(err, cache.ErrInvalid) {
			t.Errorf("Rebuild(%q) = %v, want ErrInvalid", reason, err)
		}
	}
	if _, err := c.Rebuild(t.Context(), cache.Synced{}, cache.Failure{Reason: cache.RebuildCorrupt}); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("Rebuild of a zero Synced = %v, want ErrInvalid", err)
	}
	// Another cache's Synced names an entry and an incarnation this cache
	// does not hold: rebuilt through them, the repository would be read
	// whole into the other cache's directory and counted here.
	other := newSyncWorld(t, onMemory)
	otherRepo := other.repo(t, 1)
	other.push(t, otherRepo, "refs/heads/main", nil)
	oc := other.open(t)
	foreign := other.sync(t, oc, otherRepo)
	if _, err := c.Rebuild(t.Context(), foreign, cache.Failure{Reason: cache.RebuildCorrupt}); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("Rebuild of another cache's Synced = %v, want ErrInvalid", err)
	}
	if got := cache.Repos(oc)[otherRepo]; got != 1 {
		t.Errorf("a refused rebuild left the other cache's incarnation %d current, want 1", got)
	}
	for name, rec := range map[string]*metrics.Recorder{"this cache": w.rec, "the other cache": other.rec} {
		for _, reason := range cache.RebuildReasons {
			if got := counter(rec, metrics.CodeCacheRebuilds, "reason", string(reason)); got != 0 {
				t.Errorf("%s counted %d rebuilds under %s, want none", name, got, reason)
			}
		}
	}
	if got := cache.Repos(c)[repo]; got != 1 {
		t.Errorf("a refused rebuild left incarnation %d current, want 1", got)
	}
	for _, reason := range cache.RebuildReasons {
		if !reason.Valid() {
			t.Errorf("%q is listed and not valid", reason)
		}
	}
	for _, reason := range []cache.RebuildReason{"", "x", "Corrupt"} {
		if reason.Valid() {
			t.Errorf("%q is valid", reason)
		}
	}
	c.Stop(context.Background())
	if _, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildCorrupt}); !errors.Is(err, cache.ErrStopped) {
		t.Errorf("Rebuild after Stop = %v, want ErrStopped", err)
	}
	s.Release()
}

// DAMAGE REASON READS WHAT GIT NAMED: the parts of the repository a run's
// trace2 reports named (gitproc.Damage), never its words. Damage in derived
// files ALONE — a multi-pack-index, a commit-graph, a chunk-format file of
// either — is RebuildDerived, dropped in place, and its Drops name which:
// the index, the graph, or both for a chunk-format message, which names
// neither file. Derived damage beside a pack's is the pack's (corrupt), an
// object git could not find is missing — a bare sentinel, the peel's own,
// included — the pool's refusal of the shape is shape, and anything else,
// `not our ref` among it, is no repair.
//
// Mutations: Derived() true when ANY part is derived (the index-and-pack row
// reads derived); Drops() of PartChunk the index alone (the chunk row keeps
// the graph); Began not carried (the began row reads zero).
func TestDamageReasonReadsWhatGitNamed(t *testing.T) {
	t.Parallel()
	exit := func(kind error, damage *gitproc.Damage) error {
		res := gitproc.Result{Exit: 128}
		if damage != nil {
			res.Damage = damage
		}
		return &gitproc.ExitError{Command: "cat-file", Kind: kind, Result: res}
	}
	corrupt := func(parts gitproc.Part) error {
		return exit(gitproc.ErrCorrupt, &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: parts})
	}
	began := time.Now()
	both := gitproc.PartMultiPackIndex | gitproc.PartCommitGraph
	for _, tc := range []struct {
		name   string
		err    error
		reason cache.RebuildReason // "" for no repair
		drops  gitproc.Part
		began  time.Time
	}{
		{"a multi-pack-index", corrupt(gitproc.PartMultiPackIndex), cache.RebuildDerived, gitproc.PartMultiPackIndex, time.Time{}},
		{"a commit-graph", corrupt(gitproc.PartCommitGraph), cache.RebuildDerived, gitproc.PartCommitGraph, time.Time{}},
		{"a chunk-format file", corrupt(gitproc.PartChunk), cache.RebuildDerived, both, time.Time{}},
		{"an index and a graph", corrupt(both), cache.RebuildDerived, both, time.Time{}},
		{"a began run's index", exit(gitproc.ErrCorrupt, &gitproc.Damage{Kind: gitproc.ErrCorrupt,
			Parts: gitproc.PartMultiPackIndex, Began: began}), cache.RebuildDerived, gitproc.PartMultiPackIndex, began},
		{"a began run's pack", exit(gitproc.ErrCorrupt, &gitproc.Damage{Kind: gitproc.ErrCorrupt,
			Parts: gitproc.PartPack, Began: began}), cache.RebuildCorrupt, 0, began},
		{"an index and a pack", corrupt(gitproc.PartMultiPackIndex | gitproc.PartPack), cache.RebuildCorrupt, 0, time.Time{}},
		{"a pack", corrupt(gitproc.PartPack), cache.RebuildCorrupt, 0, time.Time{}},
		{"an object", exit(gitproc.ErrMissingObject,
			&gitproc.Damage{Kind: gitproc.ErrMissingObject, Parts: gitproc.PartObject}), cache.RebuildMissing, 0, time.Time{}},
		{"the peel's own absent object", fmt.Errorf("cache: peel: %w", gitproc.ErrMissingObject),
			cache.RebuildMissing, 0, time.Time{}},
		{"a shape", fmt.Errorf("git cat-file: %w",
			&gitproc.ShapeError{Rule: gitproc.ShapeConfig, Dir: "/c/repos/r.1.git"}), cache.RebuildShape, 0, time.Time{}},
		{"not our ref", exit(gitproc.ErrNotOurRef, nil), "", 0, time.Time{}},
		{"a failure naming nothing", exit(gitproc.ErrFailed, nil), "", 0, time.Time{}},
		{"no git failure at all", errors.New("cache: the disk refused a write"), "", 0, time.Time{}},
	} {
		f, ok := cache.DamageReason(tc.err)
		if ok != (tc.reason != "") || f.Reason != tc.reason {
			t.Errorf("%s: DamageReason = %q, %v; want %q", tc.name, f.Reason, ok, tc.reason)
		}
		if f.Drops != tc.drops {
			t.Errorf("%s: Drops = %v, want %v", tc.name, f.Drops, tc.drops)
		}
		if !f.Began.Equal(tc.began) {
			t.Errorf("%s: Began = %v, want %v", tc.name, f.Began, tc.began)
		}
	}
}
