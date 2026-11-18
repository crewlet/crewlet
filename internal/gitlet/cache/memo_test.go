package cache_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE MEMO (memo.go; package doc, "The memo").

// memoKeys is n keys of the case named name.
func memoKeys(name string, n int) []cache.MemoKey {
	keys := make([]cache.MemoKey, n)
	for i := range keys {
		keys[i] = cache.NewMemoKey("case", name, strconv.Itoa(i))
	}
	return keys
}

// memoBody is the i-th answer a case memoizes, and memoTail what its run
// appends to it once it has answered.
func memoBody(i int) []byte { return []byte("the answer of page " + strconv.Itoa(i)) }

const memoTail = "|its total"

// memoize memoizes, as ONE run's batch, the i-th of bodies under keys[i] in
// the copy s holds, each ending with memoTail.
func memoize(t testing.TB, c *cache.Cache, s cache.Synced, keys []cache.MemoKey) {
	t.Helper()
	b := c.Memo(s)
	if b == nil {
		t.Fatal("the cache keeps no memo for the copy")
	}
	for i, k := range keys {
		b.Put(k, memoBody(i))
	}
	b.Place(func(int) []byte { return []byte(memoTail) })
}

// memoEntry is the bytes the memo counts its i-th answer at.
func memoEntry(i int) int64 { return cache.MemoEntryBytes(len(memoBody(i)) + len(memoTail)) }

// memoEntries is what the memo counts its first n answers at.
func memoEntries(n int) int64 {
	var total int64
	for i := range n {
		total += memoEntry(i)
	}
	return total
}

// AN ENTRY IS READ ONLY WHOLE. A read checks an entry's header (its key, body
// length and SHA-256), so an entry cut short, one with a changed byte, and
// another key's answer under this key's name are each a miss: removed from
// the tree and the memo's count, and counted lookups{outcome=corrupt}.
//
// Mutation: read past the digest (readMemo's last case) — the flipped entry is
// served.
func TestAMemoEntryIsReadOnlyWhole(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	keys := memoKeys("whole", 4)
	memoize(t, c, s, keys)
	files := map[cache.MemoKey]string{}
	for i, k := range keys {
		if files[k] = cache.MemoEntryFile(s, k); files[k] == "" {
			t.Fatalf("entry %d is in none of the copy's cohorts", i)
		}
	}
	entry := func(k cache.MemoKey) string { return files[k] }
	if got, ok := c.MemoGet(s, keys[0]); !ok || string(got) != string(memoBody(0))+memoTail {
		t.Fatalf("a whole entry read back %q (%t), want its body and its tail", got, ok)
	}

	info, err := os.Stat(entry(keys[1]))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(entry(keys[1]), info.Size()-1); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(entry(keys[2]))
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 0xff
	if err := os.WriteFile(entry(keys[2]), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	other, err := os.ReadFile(entry(keys[0]))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry(keys[3]), other, 0o600); err != nil {
		t.Fatal(err)
	}
	for i, why := range map[int]string{1: "cut short", 2: "with a byte of its body changed",
		3: "holding another key's answer"} {
		if got, ok := c.MemoGet(s, keys[i]); ok {
			t.Errorf("an entry %s was answered: %q", why, got)
		}
		if statOK(t, entry(keys[i])) {
			t.Errorf("an entry %s is still in the tree after a read refused it", why)
		}
	}
	if got := counter(w.rec, metrics.CodeCacheMemoLookups, "outcome", "corrupt"); got != 3 {
		t.Errorf("lookups{outcome=corrupt} = %d, want the three entries that were not whole", got)
	}
	if st, _ := cache.MemoOf(c, repo); st.Bytes != memoEntry(0) {
		t.Errorf("the memo's record counts %d bytes, want the one whole entry's %d", st.Bytes, memoEntry(0))
	}
}

// THE MEMO IS TRIMMED A COHORT AT A TIME, THE LEAST RECENTLY USED FIRST. A
// sixteenth of the share placed opens a new cohort, so five runs of a
// sixteenth each are five cohorts; a read moves its answer into the newest;
// and the janitor drops the OLDEST cohort whole (one rename), no more than the
// share asks. So the first run's three unread answers go together, and its
// fourth, read after the fifth run, stays.
//
// Mutations: serve a hit where it is (memoUse doing nothing) — the read answer
// goes with its cohort; drop the newest cohort — the latest runs' answers go.
func TestTheMemoIsTrimmedACohortAtATimeTheLeastRecentlyUsedFirst(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	// A share of 18 answers: a cohort opens past every 4.5 KiB placed, so
	// each run of four answers (16 KiB) closes its own.
	share := 18 * memoEntry(0)
	cache.SetMaxBytes(c, cache.MemoShare*share)
	s := w.sync(t, c, repo)
	defer s.Release()
	first := cache.MemoNewest(c)
	keys := memoKeys("cohorts", 20)
	for run := range 5 {
		memoize(t, c, s, keys[4*run:4*run+4])
	}
	if got := cache.MemoNewest(c); got != first+5 {
		t.Fatalf("five runs of a sixteenth of the share each opened the newest cohort at %d, want %d", got, first+5)
	}
	if _, ok := c.MemoGet(s, keys[3]); !ok {
		t.Fatal("the first run's last answer was not memoized")
	}
	cache.JanitorPass(c)

	for i, k := range keys {
		want := i >= 3
		if got := cache.MemoEntryFile(s, k) != ""; got != want {
			t.Errorf("after the trim answer %d (run %d) is kept: %t, want %t", i, i/4, got, want)
		}
	}
	if got := cache.MemoBytes(c); got > share {
		t.Errorf("after the trim the memo holds %d bytes, past its share %d", got, share)
	}
	if statOK(t, cache.MemoCohortDir(s.Dir, first)) {
		t.Errorf("the oldest cohort's directory, %d, is still in the copy after the trim", first)
	}
	trims := w.logs.lines(t, "code_cache_memo_trimmed")
	if len(trims) != 1 || trims[0]["cohort"] != float64(first) {
		t.Errorf("the trim logged %v, want the oldest cohort, %d, dropped alone", trims, first)
	}
}

// A TRIM FALLS DUE ONCE A COHORT, NOT ONCE A MISS. With the memo at its share
// every placement passes it, and the janitor passes after each (as the
// placement's kick has it); a trim drops the oldest cohort, a sixteenth of the
// share, so the next is due only once as much again is placed. 32 placements
// past a full memo of four-answer cohorts are eight trims, not 32.
//
// Mutation: open a cohort at every placement (memoPlaced always opening the
// next) — every placement past the share is a trim.
func TestATrimFallsDueOnceACohort(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	share := 64 * memoEntry(0)
	cache.SetMaxBytes(c, cache.MemoShare*share)
	s := w.sync(t, c, repo)
	defer s.Release()
	keys := memoKeys("due", 96)
	for _, k := range keys[:64] {
		memoize(t, c, s, []cache.MemoKey{k})
	}
	cache.JanitorPass(c)
	if trims := w.logs.lines(t, "code_cache_memo_trimmed"); len(trims) != 0 {
		t.Fatalf("a memo filled to its share exactly was trimmed: %v", trims)
	}
	for i, k := range keys[64:] {
		memoize(t, c, s, []cache.MemoKey{k})
		cache.JanitorPass(c)
		if got := cache.MemoBytes(c); got > share {
			t.Fatalf("after placement %d past the share and a pass the memo holds %d bytes, past its share %d",
				i, got, share)
		}
	}
	perCohort := 64 / cache.MemoCohortsPerShare
	if trims := w.logs.lines(t, "code_cache_memo_trimmed"); len(trims) != 32/perCohort {
		t.Errorf("32 placements past a full memo of %d-answer cohorts were trimmed %d times, want %d", perCohort,
			len(trims), 32/perCohort)
	}
}

// THE TRIM COUNTS ONLY THE COHORTS IT MOVED. It moves the oldest cohort out of
// each copy under that copy's memo lock, which keeps the tree still but not
// the copy: a purge may move the copy's directory, cohort and all, into trash/
// before the trim's move, and the purge already took those bytes out of the
// totals. So code_cache_memo_trimmed reports only the copies the trim dropped
// the cohort from, and their bytes. The case purges one of two copies sharing
// the oldest cohort as the trim is about to move it.
//
// Mutation: count a cohort whose move met its copy's absence (trimCohortOf
// without its discarded case) — the trim reports both copies and both answers.
func TestTheTrimCountsOnlyTheCohortsItMoved(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	kept, purged := w.repo(t, 1), w.repo(t, 2)
	for _, repo := range []string{kept, purged} {
		w.push(t, repo, "refs/heads/main", nil)
	}
	var (
		c       *cache.Cache
		mu      sync.Mutex
		gone    string
		retired bool
	)
	c = w.open(t, func(o *cache.Options) {
		cache.BeforeTrimming(o, func(dir string) {
			mu.Lock()
			defer mu.Unlock()
			if retired || gone == "" || !strings.HasPrefix(dir, gone+string(filepath.Separator)) {
				return
			}
			// The trim is about to move the purged repository's cohort: a
			// purge takes the copy, which nobody holds, there.
			retired = cache.Retire(c, purged)
		})
	})
	// A share of 32 answers: a cohort is opened past every two placed.
	block := memoEntry(0)
	share := 32 * block
	cache.SetMaxBytes(c, cache.MemoShare*share)
	k := w.sync(t, c, kept)
	defer k.Release()
	first := cache.MemoNewest(c)
	memoize(t, c, k, memoKeys("kept", 1))
	p := w.sync(t, c, purged)
	memoize(t, c, p, memoKeys("purged", 1))
	mu.Lock()
	gone = p.Dir
	mu.Unlock()
	p.Release()
	if got := cache.MemoNewest(c); got != first+1 {
		t.Fatalf("the two copies' answers left the newest cohort at %d, want %d: the oldest cohort is not both "+
			"copies'", got, first+1)
	}
	// Two answers a run, a cohort each: 32 answers more take the memo past its
	// share by the two in the oldest cohort, which the trim drops alone.
	fill := memoKeys("fill", 32)
	for i := 0; i < len(fill); i += 2 {
		memoize(t, c, k, fill[i:i+2])
	}
	cache.JanitorPass(c)

	mu.Lock()
	defer mu.Unlock()
	if !retired {
		t.Fatal("the trim never came to move the purged copy's cohort, so no purge met it")
	}
	if statOK(t, gone) {
		t.Errorf("the purged copy's directory %s is still where it was", gone)
	}
	trims := w.logs.lines(t, "code_cache_memo_trimmed")
	if len(trims) != 1 || trims[0]["cohort"] != float64(first) || trims[0]["copies"] != float64(1) ||
		trims[0]["bytes"] != float64(block) {
		t.Errorf("the trim logged %v, want the oldest cohort, %d, dropped from the one copy it moved it out of, "+
			"%d bytes", trims, first, block)
	}
	if lines := w.logs.lines(t, "code_cache_memo_untrimmed"); len(lines) != 0 {
		t.Errorf("code_cache_memo_untrimmed was logged %d times (%v), want never", len(lines), lines)
	}
	if statOK(t, cache.MemoCohortDir(k.Dir, first)) {
		t.Errorf("the oldest cohort's directory, %d, is still in the kept copy after the trim", first)
	}
	if got := cache.MemoBytes(c); got != share {
		t.Errorf("after the trim the node counts its memo at %d bytes, want the kept copy's %d", got, share)
	}
}

// A RUN'S STAGED PAGES ARE THE MEMO'S. A page staged and not yet placed counts
// against the memo's share beside its kept pages, so the janitor makes room
// for it from the memo's own answers, never by evicting a repository (a cold
// sync of every pack, a GET each, for bytes the memo was never entitled to).
// The case stages a page that takes the cache one byte past its budget, the
// kept pages within the share and kept plus staged past it, so only the
// share's count of the staged page decides whether the idle copy goes.
//
// Mutations: hold only the kept pages to the share (trimMemo reading memoBytes
// alone) — kept and staged stay past the share; evict for the memo's excess
// too (overBudget reading the total alone) — the idle repository goes.
func TestARunsStagedPagesAreTheMemos(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	idle, hot := w.repo(t, 1), w.repo(t, 2)
	// A mebibyte the compressor cannot shrink, so a sixteenth of a budget
	// holding it holds a dozen of the memo's pages.
	w.push(t, idle, "refs/heads/main", map[string]string{"noise": noise(1, 1<<20)})
	w.push(t, hot, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, idle).Release()
	clk.Add(time.Minute)
	s := w.sync(t, c, hot)
	defer s.Release()

	// m is the most kept pages that fit the share once the budget is one
	// byte short of the staged page, which then takes them past it.
	repos := cache.Total(c)
	body := make([]byte, 4096)
	staged := cache.MemoStagedBytes(len(body))
	entry := memoEntry(0)
	m := int((repos + staged - 1) / (15 * entry))
	keys := memoKeys("kept", m)
	memoize(t, c, s, keys)
	kept := cache.MemoBytes(c)
	if kept != memoEntries(m) {
		t.Fatalf("the memo counts %d bytes, want its %d entries' %d", kept, m, memoEntries(m))
	}
	budget := repos + kept + staged - 1
	share := budget / cache.MemoShare
	if kept > share || kept+staged <= share || staged > share {
		t.Fatalf("the case's figures miss what it is about: kept %d, staged %d, share %d of a budget of %d",
			kept, staged, share, budget)
	}
	b := c.Memo(s)
	b.Put(memoKeys("staged", 1)[0], body)
	if got := cache.MemoStaged(c); got != staged {
		t.Fatalf("the run staged %d bytes, want its page's %d", got, staged)
	}
	cache.SetMaxBytes(c, budget)
	cache.JanitorPass(c)

	repoKept := func(when string) {
		t.Helper()
		if _, ok := cache.Repos(c)[idle]; !ok {
			t.Errorf("%s: the idle repository was evicted for a page the memo's share counts", when)
		}
		if got := cache.MemoBytes(c) + cache.MemoStaged(c); got > share {
			t.Errorf("%s: the memo holds %d bytes, its kept and staged pages together, past its share %d", when,
				got, share)
		}
	}
	repoKept("while the page is staged")
	b.Place(func(int) []byte { return []byte(memoTail) })
	cache.JanitorPass(c)
	repoKept("once the page is placed")
}

// A PAGE PAST THE MEMO'S SHARE IS NEVER STAGED. A page that would take every
// run's staged pages past the share is not staged: the memo could never keep
// it, and staged it would hold room the repositories' budget is for. So ~6 KB
// pages against a share of a few hundred bytes are answered unmemoized,
// uncounted, and evict no repository, while cut or after.
//
// Mutation: stage whatever the share (stageMemo always finding room) — the
// staged pages hold more than the share.
func TestAPagePastTheMemosShareIsNeverStaged(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	idle, hot := w.repo(t, 1), w.repo(t, 2)
	w.push(t, idle, "refs/heads/main", nil)
	w.push(t, hot, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, idle).Release()
	clk.Add(time.Minute)
	s := w.sync(t, c, hot)
	defer s.Release()
	budget := cache.Total(c) + 8<<10
	cache.SetMaxBytes(c, budget)
	share := budget / cache.MemoShare

	b := c.Memo(s)
	keys := memoKeys("past", 4)
	for _, k := range keys {
		b.Put(k, []byte(strings.Repeat("x", 6000)))
	}
	if got := cache.MemoStaged(c); got > share {
		t.Errorf("the run staged %d bytes against a share of %d", got, share)
	}
	cache.JanitorPass(c)
	if _, ok := cache.Repos(c)[idle]; !ok {
		t.Error("the idle repository was evicted while pages past the memo's share were cut")
	}
	b.Place(func(int) []byte { return []byte(memoTail) })
	cache.JanitorPass(c)
	if _, ok := cache.Repos(c)[idle]; !ok {
		t.Error("the idle repository was evicted once pages past the memo's share were placed")
	}
	for _, k := range keys {
		if _, ok := c.MemoGet(s, k); ok {
			t.Errorf("a page past the memo's whole share was memoized: %s", k)
		}
	}
	if got := cache.MemoStaged(c); got != 0 {
		t.Errorf("the memo counts %d bytes staged once the run's batch is placed", got)
	}
}

// WHAT THE MEMO HOLDS PAST ITS SHARE IS NO REPOSITORY'S TO MAKE ROOM FOR. An
// eviction is for the repositories' excess over the budget less the memo's
// share, never the memo's own, which its trim gives back; so an eviction pass
// alone, the cache past its budget by no more than the memo's excess, evicts
// nothing. It covers a page staged between the trim and the eviction.
//
// Mutation: evict while the total is past the budget (overBudget counting the
// memo's excess) — the idle repository is evicted.
func TestTheMemosExcessEvictsNoRepository(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	idle, hot := w.repo(t, 1), w.repo(t, 2)
	w.push(t, idle, "refs/heads/main", nil)
	w.push(t, hot, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, idle).Release()
	clk.Add(time.Minute)
	s := w.sync(t, c, hot)
	defer s.Release()
	memoize(t, c, s, memoKeys("excess", 8))
	budget := cache.Total(c) - 1
	cache.SetMaxBytes(c, budget)
	if excess := cache.MemoBytes(c) - budget/cache.MemoShare; excess < 1 {
		t.Fatalf("the memo holds %d bytes past its share, and the case is about its excess", excess)
	}
	if n := cache.EvictToBudget(c); n != 0 {
		t.Errorf("an eviction pass evicted %d repositories for what the memo holds past its share", n)
	}
	if _, ok := cache.Repos(c)[idle]; !ok {
		t.Error("the idle repository was evicted for the memo's excess")
	}
}

// A PLACEMENT PAST THE MEMO'S SHARE WAKES THE JANITOR. The memo's excess makes
// no eviction due (overBudget), so no release or round kicks a pass for it:
// the placement does, and the trim follows at once, not at a tick (none comes
// here).
//
// Mutation: place without the kick — the memo stays past its share.
func TestAPlacementPastTheShareWakesTheJanitor(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	keys := memoKeys("woken", 8)
	share := memoEntries(len(keys) / 2)
	c := w.open(t, neverTicks)
	cache.SetMaxBytes(c, cache.MemoShare*share)
	s := w.sync(t, c, repo)
	defer s.Release()
	startIdle(t, w, c)
	memoize(t, c, s, keys)
	eventually(t, "the memo trimmed to its share", func() bool {
		return cache.MemoBytes(c) <= share && cache.Running(c) == 0
	})
}

// A PREVIOUS PROCESS'S ANSWERS NOBODY READ SINCE GO FIRST. A boot opens the
// newest cohort past every one the states record and the previous process
// reserved (TestABootOpensItsNewestCohortPastEveryOneItsCacheReserved), so a
// trim takes the cold copy's pre-restart cohort whole before any answer the
// warm copy placed after it, and the cold answer read after the restart stays.
//
// Mutation: open no cohort at the boot (the newest at the last one recorded)
// — the warm copy's answers share the cold ones' cohort, and go with them.
func TestAPreviousProcesssAnswersNobodyReadSinceGoFirst(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	cold, warm := w.repo(t, 1), w.repo(t, 2)
	w.push(t, cold, "refs/heads/main", nil)
	w.push(t, warm, "refs/heads/main", nil)
	c := w.open(t)
	sc := w.sync(t, c, cold)
	coldDir := sc.Dir
	coldKeys := memoKeys("cold", 32)
	memoize(t, c, sc, coldKeys)
	before, _ := cache.MemoOf(c, cold)
	cache.JanitorTick(c) // the copy's state records its memo
	sc.Release()

	c = w.reopen(t, c)
	if st, _ := cache.MemoOf(c, cold); st.Bytes != memoEntries(32) || st.Listed {
		t.Fatalf("the cold copy's memo after the restart is %+v, want its state's %d bytes, not yet listed", st,
			memoEntries(32))
	}
	if got := cache.MemoNewest(c); got <= before.Cohorts[len(before.Cohorts)-1] {
		t.Fatalf("the boot left the newest cohort at %d, not past the %v its states record", got, before.Cohorts)
	}
	sw := w.sync(t, c, warm)
	defer sw.Release()
	keys := memoKeys("warm", 8)
	memoize(t, c, sw, keys)
	sc = w.sync(t, c, cold)
	defer sc.Release()
	if _, ok := c.MemoGet(sc, coldKeys[0]); !ok {
		t.Fatal("the cold copy's answer was not memoized across the restart")
	}
	// A share the warm answers and the one cold answer read fit, and the cold
	// answers nobody read do not.
	cache.SetMaxBytes(c, cache.MemoShare*memoEntries(12))
	cache.JanitorPass(c)

	for _, cohort := range before.Cohorts {
		if statOK(t, cache.MemoCohortDir(coldDir, cohort)) {
			t.Errorf("the cold copy's cohort %d is still in its tree after a trim past the share", cohort)
		}
	}
	for i, k := range coldKeys {
		if got, want := cache.MemoEntryFile(sc, k) != "", i == 0; got != want {
			t.Errorf("the cold copy's answer %d is kept: %t, want %t", i, got, want)
		}
	}
	for i, k := range keys {
		if cache.MemoEntryFile(sw, k) == "" {
			t.Errorf("the warm copy's answer %d was trimmed before the cold ones nobody read", i)
		}
	}
}

// A REBUILD MOVES THE MEMO WHOLE. A rebuild repairs packs, not answers: the
// replaced copy's memo tree is renamed into the new copy as it is promoted
// (the very directory, never refilled an entry at a time), so the round pays
// nothing per answer. Every entry is answered there as it was, counted once;
// the replaced copy, which a reader still holds, answers none.
//
// Mutations: link the tree an entry at a time — the rebuilt copy's tree is
// another directory; carry nothing — the rebuilt copy answers none.
func TestARebuildMovesTheMemoWhole(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s, err := c.Sync(t.Context(), repo, linearizable)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	keys := memoKeys("rebuild", 64)
	memoize(t, c, s, keys)
	before, _ := cache.MemoOf(c, repo)
	held := cache.MemoBytes(c)
	oldTree, err := os.Stat(cache.MemoTree(s.Dir))
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := cache.RebuildWhole(t.Context(), c, s, cache.Failure{Reason: cache.RebuildCorrupt,
		Began: time.Now()})
	if err != nil {
		t.Fatalf("the rebuild: %v", err)
	}
	defer rebuilt.Release()
	newTree, err := os.Stat(cache.MemoTree(rebuilt.Dir))
	switch {
	case err != nil:
		t.Fatalf("the rebuilt copy holds no memo of the copy it replaced: %v", err)
	case !os.SameFile(oldTree, newTree):
		t.Error("the rebuilt copy's memo is another directory than the replaced copy's: a rebuild moves it whole")
	}
	if statOK(t, cache.MemoTree(s.Dir)) {
		t.Error("the replaced copy still holds a memo tree beside the one the rebuild carried")
	}
	for i, k := range keys {
		if got, ok := c.MemoGet(rebuilt, k); !ok || string(got) != string(memoBody(i))+memoTail {
			t.Errorf("the rebuilt copy answers entry %d %q (%t), want the replaced copy's", i, got, ok)
		}
	}
	if _, ok := c.MemoGet(s, keys[0]); ok {
		t.Error("the replaced copy answered from a memo the rebuild carried away")
	}
	if after, _ := cache.MemoOf(c, repo); after.Bytes != before.Bytes {
		t.Errorf("the rebuilt copy's memo counts %d bytes, want the replaced copy's %d", after.Bytes, before.Bytes)
	}
	if got := cache.MemoBytes(c); got != held {
		t.Errorf("the node counts its memo at %d bytes after the rebuild, want the %d it held once", got, held)
	}
}

// A REBUILD OVER ANOTHER HISTORY CARRIES NO MEMO. An answer is a function of
// object ids only while its history stands, so a rebuild whose round reads
// another history carries none of the memo: not over a log recreated under
// the node (the replaced copy's history marked gone), nor over one reanchored
// at another generation (the rebuild's own round drops the memo).
//
// Mutations: carry a memo whose history is marked gone (carryMemo's
// historyGone) — the recreated case answers it; carry into a copy that
// dropped a memo (carryMemo's generation) — the reanchored one's.
func TestARebuildOverAnotherHistoryCarriesNoMemo(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"recreated", "reanchored"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t)
			w.sync(t, c, repo).Release()
			earlier := w.log.state()
			w.push(t, repo, "refs/heads/main", nil)
			s, err := c.Sync(t.Context(), repo, linearizable)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Release()
			keys := memoKeys(how, 3)
			memoize(t, c, s, keys)
			if how == "recreated" {
				w.log.recreate(earlier)
			} else {
				w.log.set(func(f *fakeLog) { f.gen++ })
			}
			rebuilt, err := cache.RebuildWhole(t.Context(), c, s, cache.Failure{Reason: cache.RebuildCorrupt,
				Began: time.Now()})
			if err != nil {
				t.Fatalf("the rebuild: %v", err)
			}
			defer rebuilt.Release()
			for _, k := range keys {
				if _, ok := c.MemoGet(rebuilt, k); ok {
					t.Errorf("the rebuilt copy answered entry %s of the history it no longer reads", k)
				}
			}
			if st, _ := cache.MemoOf(c, repo); st.Recorded {
				t.Errorf("the rebuilt copy records a memo of %d bytes from the history it no longer reads", st.Bytes)
			}
		})
	}
}

// A RECREATED LOG DROPS THE MEMO. A log recreated, or restored from an older
// copy, is another history, which the memo cannot tell entry by entry: the
// full round converging the copy drops the whole tree and its record before
// the round's tidy (so before any git process runs there), and nothing the old
// history answered is answered again.
//
// Mutation: keep the memo on a history gone (OnHistoryGone Keep) — the round
// tidies a copy still holding the old answers, which are still answered.
func TestARecreatedLogDropsTheMemo(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"recreated", "restored"} {
		t.Run(how, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			var (
				gone   atomic.Bool
				mu     sync.Mutex
				tidied []bool // after the log went back: whether each round's tidied copy held a memo tree
			)
			c := w.open(t, func(o *cache.Options) {
				cache.AfterSweep(o, func(dir string) {
					if gone.Load() {
						mu.Lock()
						tidied = append(tidied, statOK(t, cache.MemoTree(dir)))
						mu.Unlock()
					}
				})
			})
			w.sync(t, c, repo)
			earlier := w.log.state()
			w.push(t, repo, "refs/heads/main", nil)
			s := w.sync(t, c, repo)
			keys := memoKeys(how, 3)
			memoize(t, c, s, keys)

			if how == "recreated" {
				w.log.recreate(earlier)
			} else {
				w.log.restore(earlier)
			}
			gone.Store(true)
			after := w.sync(t, c, repo)
			mu.Lock()
			defer mu.Unlock()
			if len(tidied) == 0 {
				t.Fatal("no full round read the gone history's copy whole")
			}
			for i, held := range tidied {
				if held {
					t.Errorf("round %d tidied a copy still holding the memo of the history it replaced", i)
				}
			}
			for _, k := range keys {
				if _, ok := c.MemoGet(after, k); ok {
					t.Errorf("entry %s of the history the log no longer holds was answered", k)
				}
			}
			if st, _ := cache.MemoOf(c, repo); st.Recorded {
				t.Errorf("the copy records a memo of %d bytes after its history went", st.Bytes)
			}
		})
	}
}

// NOTHING READ OVER A GONE HISTORY IS MEMOIZED AFTER ITS DROP. A batch begun
// before the history went places nothing once the drop has moved the memo's
// generation; and while the old history's packs are in a copy, its memo is
// shut (a read may meet either history's objects). Once they are out it
// memoizes again: in the same copy where the round swept them in place (no
// reader holds it), else in the NEXT incarnation (swap.go): the copy the swap
// leaves keeps them for its readers, so its memo stays shut.
//
// Mutations: let Place ignore the generation — the earlier batch places its
// answer; let Memo ignore the shut memo — the round's window begins a batch;
// reopen the memo of a copy whose strays leave by the swap (apply) — a batch
// begins over the copy the swap is leaving.
func TestNothingReadOverAGoneHistoryIsMemoizedAfterItsDrop(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// held: a reader holds the copy through the round, so the old
		// history's packs leave it by the swap.
		held bool
	}{
		{"in place, from a copy no reader holds", false},
		{"by the swap, from a copy a reader holds", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			faults := &gitletfault.Script{}
			var (
				cc     atomic.Pointer[cache.Cache]
				held   atomic.Pointer[cache.Synced]
				gone   atomic.Bool
				mu     sync.Mutex
				during []bool // whether a batch began in the round's window
			)
			c := w.open(t, func(o *cache.Options) {
				o.Faults = faults
				cache.AfterSweep(o, func(string) {
					if !gone.Load() {
						return
					}
					mu.Lock()
					defer mu.Unlock()
					during = append(during, cc.Load().Memo(*held.Load()) != nil)
				})
			})
			cc.Store(c)
			first := w.sync(t, c, repo)
			earlier := w.log.state()
			w.push(t, repo, "refs/heads/main", nil)
			s := w.sync(t, c, repo)
			held.Store(&s)

			begun := c.Memo(s)
			stale := memoKeys("stale", 1)[0]
			begun.Put(stale, memoBody(0))
			w.log.recreate(earlier)
			var now cache.Synced
			gone.Store(true)
			if !tc.held {
				first.Release()
				s.Release()
				now = w.sync(t, c, repo)
			} else {
				// The swap's window: the old history's pack still in the
				// copy it leaves, nothing renamed yet.
				release, reached := make(chan struct{}), make(chan struct{})
				faults.Arm(gitletfault.SyncSwapBeforeRename, gitletfault.Action{Once: true, Block: release,
					Reached: reached})
				answered := make(chan cache.Synced, 1)
				go func() {
					got, err := c.Sync(context.Background(), repo, linearizable)
					if err != nil {
						t.Errorf("the round after the history went: %v", err)
					}
					answered <- got
				}()
				select {
				case <-reached:
				case <-time.After(hangDetector): //fixedwait:liveness
					t.Fatal("the round never reached the swap's rename")
				}
				if b := c.Memo(s); b != nil {
					t.Error("a batch began over the copy the swap is leaving, which still holds the old history's " +
						"pack")
				}
				close(release)
				now = <-answered
				t.Cleanup(now.Release)
			}
			gone.Store(false)
			begun.Place(func(int) []byte { return []byte(memoTail) })

			if inPlace := now.Dir == s.Dir; inPlace == tc.held {
				t.Fatalf("the round answered %s, the copy the batch began over being %s: in place %t, want %t",
					now.Dir, s.Dir, inPlace, !tc.held)
			}
			mu.Lock()
			if len(during) == 0 {
				t.Error("no full round read the gone history's copy whole")
			}
			for i, began := range during {
				if began {
					t.Errorf("a batch began as round %d tidied, before the old history's packs were out", i)
				}
			}
			mu.Unlock()
			if _, ok := c.MemoGet(now, stale); ok {
				t.Error("an answer a run cut before the history went was memoized after its drop")
			}
			if tc.held && c.Memo(s) != nil {
				t.Error("a batch began over the copy the swap left, which still holds the old history's pack")
			}
			fresh := memoKeys("fresh", 1)
			memoize(t, c, now, fresh)
			if _, ok := c.MemoGet(now, fresh[0]); !ok {
				t.Error("the copy memoized nothing once the old history's packs were out of it")
			}
		})
	}
}

// THE HELD BYTES COUNT THE MEMO by its running total, never by measuring its
// tree: a maintenance run and a full round's index each measure the derived
// files, and a directory measures as its inode's few KiB however many answers
// it holds. So the record keeps the entries' bytes through both, neither
// stats anything under the tree, the janitor writes the state once the memo
// moved, and a boot's layout counts the memo from it.
//
// Mutation: measure a tree as any owned entry (image.sizes stats it, and
// recordDerived takes the measure) — the record is the directory's size.
func TestTheHeldBytesCountTheMemo(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	for range caseRoundPacks {
		w.push(t, repo, "refs/heads/main", nil)
	}
	var (
		mu     sync.Mutex
		stated []string
	)
	c := w.open(t, clk.on, cache.RoundPacksAt(caseRoundPacks), cache.StatingWith(func(rel string) {
		mu.Lock()
		stated = append(stated, rel)
		mu.Unlock()
	}))
	s := w.sync(t, c, repo)
	dir := s.Dir
	keys := memoKeys("held", 8)
	memoize(t, c, s, keys)
	want := memoEntries(len(keys))
	cache.JanitorPass(c)
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want the repository", ran)
	}
	cache.MarkWhole(c, repo, cache.RebuildShape, 0) // a full round, whose own index measures again
	s.Release()
	w.sync(t, c, repo)
	cache.JanitorTick(c)

	if st, _ := cache.MemoOf(c, repo); st.Bytes != want || !st.Stated {
		t.Errorf("after a maintenance run and a round's index the memo records %d bytes (its state that record: "+
			"%t), want its entries' %d", st.Bytes, st.Stated, want)
	}
	mu.Lock()
	for _, rel := range stated {
		if rel == "crewlet/memo" || strings.HasPrefix(rel, "crewlet/memo/") {
			t.Errorf("a measure stated %s: the memo is counted by its writer, never by a stat", rel)
		}
	}
	mu.Unlock()
	if got := readStateDoc(t, dir).Derived[string(cache.DerivedMemo)].Bytes; got != want {
		t.Errorf("the state records the memo at %d bytes, want its entries' %d", got, want)
	}
	c.Stop(context.Background())
	doc := readStateDoc(t, dir)
	layout, err := cache.Prepare(t.Context(), w.layout.Dir)
	if err != nil {
		t.Fatal(err)
	}
	defer layout.Release()
	if floor := doc.DiskBytes + want; layout.Held < floor {
		t.Errorf("the layout holds %d bytes, less than the copy's packs and its memo's %d", layout.Held, floor)
	}
}

// A COPY'S MEMO IS WRITTEN DOWN ONCE A TICK AT MOST. A state write is two
// syncs (file and directory), so the janitor writes a copy's state for its
// memo's moves on its TICK alone, however many kicked passes run between,
// once per copy whose memo moved.
//
// Mutation: write the moved memos' states on every pass (stateMemos outside
// the ticked branch) — the kicked passes write them.
func TestACopysMemoIsWrittenDownOnceATickAtMost(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	var (
		mu     sync.Mutex
		writes int
	)
	c := w.open(t, func(o *cache.Options) {
		cache.StateWriteFails(o, func(id string, round bool) error {
			if id == repo && !round {
				mu.Lock()
				writes++
				mu.Unlock()
			}
			return nil
		})
	})
	written := func() int {
		mu.Lock()
		defer mu.Unlock()
		return writes
	}
	s := w.sync(t, c, repo)
	defer s.Release()
	keys := memoKeys("ticked", 6)
	for i, k := range keys[:3] {
		memoize(t, c, s, []cache.MemoKey{k})
		if _, ok := c.MemoGet(s, k); !ok {
			t.Fatalf("entry %d was not memoized", i)
		}
		cache.JanitorPass(c)
	}
	if n := written(); n != 0 {
		t.Errorf("the janitor's kicked passes wrote the copy's state %d times for its memo's moves, want none "+
			"before a tick", n)
	}
	cache.JanitorTick(c)
	if n := written(); n != 1 {
		t.Errorf("the tick wrote the copy's state %d times, want once for the memo's moves since its last", n)
	}
	if st, _ := cache.MemoOf(c, repo); !st.Stated || st.Bytes != memoEntries(3) {
		t.Errorf("after the tick the state records the memo as it stands: %t, and the memo holds %d, want its "+
			"entries' %d", st.Stated, st.Bytes, memoEntries(3))
	}
	cache.JanitorTick(c)
	if n := written(); n != 1 {
		t.Errorf("a tick with nothing moved since the last wrote the state again: %d writes", n)
	}
}

// A TICK WRITES A COPY'S STATE ONLY WHILE THE COPY IS DUE ONE, and logs a
// failed write only for a copy still due. The tick picks the copies whose memo
// moved, then writes each holding none of their locks, so a purge, promote or
// swap may retire one meanwhile (into trash/ once unheld) and a round may
// write its state. A copy no longer due is not written, nor its failure
// logged (code_cache_memo_unstated would promise a retry no tick makes); one
// still due is logged once and written at the next tick.
//
// Mutations: log no failure — row 1 logs none; log every failure — rows 2 and
// 3 log one; excuse only a copy in trash/ (judged by discarded alone) — row 3,
// which a reader holds, logs one; write every copy picked (no look as each
// write begins) — rows 4 and 5 write the other copy's state.
func TestATickWritesACopysStateOnlyWhileItIsDue(t *testing.T) {
	t.Parallel()
	refused := errors.New("the disk refused the state")

	t.Run("a copy still due whose write fails is logged and written at the next tick", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		var refuse atomic.Bool
		c := w.open(t, func(o *cache.Options) {
			cache.StateWriteFails(o, func(_ string, round bool) error {
				if !round && refuse.Load() {
					return refused
				}
				return nil
			})
		})
		s := w.sync(t, c, repo)
		memoize(t, c, s, memoKeys("refused", 2))
		refuse.Store(true)
		cache.JanitorTick(c)
		if lines := w.logs.lines(t, "code_cache_memo_unstated"); len(lines) != 1 || lines[0]["repo"] != repo {
			t.Errorf("code_cache_memo_unstated was logged %d times (%v), want once, for %s", len(lines), lines,
				repo)
		}
		if st, _ := cache.MemoOf(c, repo); st.Stated {
			t.Error("the copy's state records its memo though the tick's write of it failed")
		}
		refuse.Store(false)
		cache.JanitorTick(c)
		if st, _ := cache.MemoOf(c, repo); !st.Stated {
			t.Error("the next tick did not write the state of the copy whose write had failed")
		}
		if lines := w.logs.lines(t, "code_cache_memo_unstated"); len(lines) != 1 {
			t.Errorf("code_cache_memo_unstated was logged %d times once the next tick wrote the state, want the "+
				"first failure's once", len(lines))
		}
	})

	t.Run("a copy retired and moved into trash/ under its write is no failure", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		var (
			c       *cache.Cache
			mu      sync.Mutex
			retired bool
		)
		c = w.open(t, func(o *cache.Options) {
			cache.StateWriteFails(o, func(_ string, round bool) error {
				if round {
					return nil
				}
				mu.Lock()
				defer mu.Unlock()
				if !retired {
					// A purge takes the copy, which nobody holds, as the tick
					// writes its state: its directory goes into trash/.
					retired = cache.Retire(c, repo)
				}
				return nil
			})
		})
		s := w.sync(t, c, repo)
		dir := s.Dir
		memoize(t, c, s, memoKeys("purged", 2))
		s.Release()
		cache.JanitorTick(c)
		mu.Lock()
		defer mu.Unlock()
		if !retired {
			t.Fatal("the tick never wrote the copy's state, so no purge met its write")
		}
		if statOK(t, dir) {
			t.Errorf("the purged copy's directory %s is still where it was", dir)
		}
		if lines := w.logs.lines(t, "code_cache_memo_unstated"); len(lines) != 0 {
			t.Errorf("code_cache_memo_unstated was logged %d times (%v) for a copy gone under its write, want never",
				len(lines), lines)
		}
		if got := cache.MemoBytes(c); got != 0 {
			t.Errorf("the node counts its memo at %d bytes once the one copy holding a memo is gone, want 0", got)
		}
	})

	t.Run("a copy retired under its write while a reader holds it is no failure", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		var (
			c       *cache.Cache
			mu      sync.Mutex
			retired bool
		)
		c = w.open(t, func(o *cache.Options) {
			cache.StateWriteFails(o, func(_ string, round bool) error {
				if round {
					return nil
				}
				mu.Lock()
				defer mu.Unlock()
				if retired {
					return nil
				}
				// A rebuild replaces the copy as the tick writes its state,
				// a reader still holding it, and the disk refuses the write.
				retired = cache.Retire(c, repo)
				return refused
			})
		})
		s := w.sync(t, c, repo)
		memoize(t, c, s, memoKeys("replaced", 2))
		cache.JanitorTick(c)
		mu.Lock()
		defer mu.Unlock()
		if !retired {
			t.Fatal("the tick never wrote the copy's state, so no rebuild met its write")
		}
		if !statOK(t, s.Dir) {
			t.Errorf("the replaced copy's directory %s went while a reader holds it", s.Dir)
		}
		if lines := w.logs.lines(t, "code_cache_memo_unstated"); len(lines) != 0 {
			t.Errorf("code_cache_memo_unstated was logged %d times (%v) for a copy retired under its write, want "+
				"never", len(lines), lines)
		}
	})

	// two is a world of two repositories whose memos moved since their states
	// were written; as the tick writes its first state, meet acts on the OTHER
	// repository's copy. It answers the world, the cache, the repositories,
	// and what the tick did: the repositories whose state it wrote, in order,
	// and the one meet met.
	two := func(t *testing.T, meet func(c *cache.Cache, other string)) (*syncWorld, *cache.Cache, [2]string,
		func() (written []string, met string)) {
		t.Helper()
		w := newSyncWorld(t, onMemory)
		repos := [2]string{w.repo(t, 1), w.repo(t, 2)}
		for _, repo := range repos {
			w.push(t, repo, "refs/heads/main", nil)
		}
		var (
			c       *cache.Cache
			mu      sync.Mutex
			written []string
			met     string
		)
		c = w.open(t, func(o *cache.Options) {
			cache.StateWriteFails(o, func(id string, round bool) error {
				if round {
					return nil
				}
				mu.Lock()
				written = append(written, id)
				first := len(written) == 1
				if first {
					met = repos[0]
					if id == met {
						met = repos[1]
					}
				}
				other := met
				mu.Unlock()
				if first {
					meet(c, other)
				}
				return nil
			})
		})
		for _, repo := range repos {
			s := w.sync(t, c, repo)
			memoize(t, c, s, memoKeys(repo, 2))
			s.Release()
		}
		return w, c, repos, func() ([]string, string) {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(written), met
		}
	}

	t.Run("a copy retired since the tick picked it is not written", func(t *testing.T) {
		t.Parallel()
		w, c, _, did := two(t, func(c *cache.Cache, other string) {
			// A purge takes the other copy, which nobody holds, as the tick
			// writes this one's state.
			if !cache.Retire(c, other) {
				t.Errorf("the cache held no copy of %s to purge", other)
			}
		})
		cache.JanitorTick(c)
		if written, purged := did(); len(written) != 1 || written[0] == purged {
			t.Errorf("the tick wrote the states of %v, %s purged once the tick had picked it, want the other "+
				"copy's alone", written, purged)
		}
		if lines := w.logs.lines(t, "code_cache_memo_unstated"); len(lines) != 0 {
			t.Errorf("code_cache_memo_unstated was logged %d times (%v), want never", len(lines), lines)
		}
	})

	t.Run("a copy another writer stated since the tick picked it is not written", func(t *testing.T) {
		t.Parallel()
		w, c, repos, did := two(t, func(c *cache.Cache, other string) {
			// A round writes the other copy's state as the tick writes this
			// one's.
			if err := cache.Persist(context.Background(), c, other, true); err != nil {
				t.Errorf("the round's write of %s's state: %v", other, err)
			}
		})
		cache.JanitorTick(c)
		if written, stated := did(); len(written) != 1 || written[0] == stated {
			t.Errorf("the tick wrote the states of %v, %s's written by a round once the tick had picked it, want "+
				"the other copy's alone", written, stated)
		}
		for _, repo := range repos {
			if st, _ := cache.MemoOf(c, repo); !st.Stated {
				t.Errorf("after the tick %s's state does not record its memo", repo)
			}
		}
		if lines := w.logs.lines(t, "code_cache_memo_unstated"); len(lines) != 0 {
			t.Errorf("code_cache_memo_unstated was logged %d times (%v), want never", len(lines), lines)
		}
	})
}

// THE JANITOR LISTS EVERY COPY'S TREE ONCE A PROCESS, behind the serving, by
// its cohorts' names alone (package doc, "The memo"). It counts a cohort a
// crash left unrecorded, forgets a recorded one gone, and drops a tree of
// another layout (answers outside any cohort) whole, unread; the tick writes
// down what it records. A cohort or entry gone under the measure (a purge
// takes no memo lock; something outside the engine) is no failure; one still
// named that cannot be read is logged (code_cache_memo_unlisted), and the
// copy keeps its state's count until a listing measures it whole, lest its
// bytes sit on disk in no count. Passing over recorded cohorts is sound only
// because the boot opens past every cohort the previous process reserved.
// Each row's mutation is above it.
func TestTheJanitorListsEveryCopysTreeOnce(t *testing.T) {
	t.Parallel()
	block := memoEntry(0)
	// crashed is a copy whose state a tick wrote over one run's four answers,
	// then two runs more — the second in a cohort of its own — and a restart
	// that kept that state, the restarted cache opened with mutate; before
	// runs before the restart.
	crashed := func(t *testing.T, before func(dir string, first uint64),
		mutate ...func(*cache.Options)) (*cache.Cache, string, string, uint64, *syncWorld) {
		t.Helper()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		c := w.open(t)
		s := w.sync(t, c, repo)
		dir, first := s.Dir, cache.MemoNewest(c)
		keys := memoKeys("crashed", 9)
		memoize(t, c, s, keys[:4])
		cache.JanitorTick(c) // the state records the first run's cohort
		// A share whose sixteenth is an answer: each run from here closes
		// the cohort it was placed in.
		cache.SetMaxBytes(c, cache.MemoShare*cache.MemoCohortsPerShare*block)
		memoize(t, c, s, keys[4:6])
		memoize(t, c, s, keys[6:9])
		s.Release()
		if before != nil {
			before(dir, first)
		}
		return w.reopen(t, c, mutate...), repo, dir, first, w
	}

	// Mutation: pass over a cohort no state records (listMemo measuring
	// nothing) — the record lacks it.
	t.Run("a cohort no state records is counted", func(t *testing.T) {
		t.Parallel()
		c, repo, dir, first, _ := crashed(t, nil)
		if st, _ := cache.MemoOf(c, repo); st.Listed || st.Bytes != 4*block {
			t.Fatalf("the boot's record is %+v, want the tick's four answers, not yet listed", st)
		}
		cache.JanitorPass(c)
		// The second run's answers, in the cohort the state records, are the
		// tick's writes a crash costs the count; the third run's cohort, which
		// no state records, is counted whole.
		st, _ := cache.MemoOf(c, repo)
		if !st.Listed || !slices.Equal(st.Cohorts, []uint64{first, first + 1}) ||
			!slices.Equal(st.Held, []int64{4 * block, 3 * block}) {
			t.Errorf("after the listing the record is %+v, want cohorts %d and %d holding %d and %d bytes", st,
				first, first+1, 4*block, 3*block)
		}
		if got := cache.MemoBytes(c); got != 7*block {
			t.Errorf("the node counts its memo at %d bytes after the listing, want %d", got, 7*block)
		}
		if got := cache.MemoNewest(c); got <= first+1 {
			t.Errorf("the node's newest cohort is %d after the listing found cohort %d, want one past it", got,
				first+1)
		}
		cache.JanitorTick(c)
		want := []cohortDoc{{N: first, Bytes: 4 * block}, {N: first + 1, Bytes: 3 * block}}
		if got := readStateDoc(t, dir).Derived[string(cache.DerivedMemo)].Cohorts; !slices.Equal(got, want) {
			t.Errorf("the tick wrote the memo's cohorts as %v, want %v", got, want)
		}
	})

	// Mutation: keep a recorded cohort the tree no longer holds.
	t.Run("a recorded cohort gone is forgotten", func(t *testing.T) {
		t.Parallel()
		c, repo, _, first, _ := crashed(t, func(dir string, first uint64) {
			if err := os.RemoveAll(cache.MemoCohortDir(dir, first)); err != nil {
				t.Fatal(err)
			}
		})
		cache.JanitorPass(c)
		st, _ := cache.MemoOf(c, repo)
		if !slices.Equal(st.Cohorts, []uint64{first + 1}) || !slices.Equal(st.Held, []int64{3 * block}) {
			t.Errorf("after the listing the record is %+v, want cohort %d alone, holding %d bytes", st, first+1,
				3*block)
		}
	})

	// Mutation: leave a tree of another layout's names be.
	t.Run("a tree of another layout goes whole", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		c := w.open(t)
		s := w.sync(t, c, repo)
		dir := s.Dir
		s.Release()
		write(t, filepath.Join(cache.MemoTree(dir), memoKeys("flat", 1)[0].String()),
			"an answer a build of one directory kept")
		c = w.reopen(t, c)
		cache.JanitorPass(c)
		if statOK(t, cache.MemoTree(dir)) {
			t.Error("a memo tree holding an answer outside any cohort is still in the copy after its listing")
		}
		if st, _ := cache.MemoOf(c, repo); st.Recorded {
			t.Errorf("the copy records a memo of %+v from a tree of another layout", st)
		}
		if lines := w.logs.lines(t, "code_cache_memo_foreign"); len(lines) != 1 {
			t.Errorf("code_cache_memo_foreign was logged %d times, want once for the tree", len(lines))
		}
	})

	// Mutation: log a cohort gone from under the measure (listMemo without its
	// fs.ErrNotExist case).
	t.Run("a cohort gone with its copy under the listing is no failure", func(t *testing.T) {
		t.Parallel()
		var (
			c         *cache.Cache
			w         *syncWorld
			repo, dir string
			first     uint64
			retired   bool
		)
		c, repo, dir, first, w = crashed(t, nil, func(o *cache.Options) {
			cache.BeforeMeasuring(o, func(d string) {
				// The listing has named the cohort no state records and is
				// about to measure it: a purge takes the copy there.
				if d == cache.MemoCohortDir(dir, first+1) {
					retired = cache.Retire(c, repo)
				}
			})
		})
		cache.JanitorPass(c)
		if !retired {
			t.Fatal("the listing never came to measure the cohort no state records, so no purge met it")
		}
		if statOK(t, dir) {
			t.Errorf("the purged copy's directory %s is still where it was", dir)
		}
		if lines := w.logs.lines(t, "code_cache_memo_unlisted"); len(lines) != 0 {
			t.Errorf("code_cache_memo_unlisted was logged %d times (%v) for a cohort gone with its copy, want never",
				len(lines), lines)
		}
		if got := cache.MemoBytes(c); got != 0 {
			t.Errorf("the node counts its memo at %d bytes once the one copy holding a memo is gone, want 0", got)
		}
	})

	// The cohort becomes a file: the one read failure a case can make
	// whoever runs it. Mutation: excuse every error of a cohort's measure.
	t.Run("a cohort the measure cannot read is logged", func(t *testing.T) {
		t.Parallel()
		var (
			c         *cache.Cache
			w         *syncWorld
			repo, dir string
			first     uint64
			replaced  bool
		)
		c, repo, dir, first, w = crashed(t, nil, func(o *cache.Options) {
			cache.BeforeMeasuring(o, func(d string) {
				cohort := cache.MemoCohortDir(dir, first+1)
				if d != cohort {
					return
				}
				if err := os.RemoveAll(cohort); err != nil {
					t.Error(err)
					return
				}
				if err := os.WriteFile(cohort, []byte("no cohort"), 0o600); err != nil {
					t.Error(err)
					return
				}
				replaced = true
			})
		})
		cache.JanitorPass(c)
		if !replaced {
			t.Fatal("the listing never came to measure the cohort no state records")
		}
		lines := w.logs.lines(t, "code_cache_memo_unlisted")
		if len(lines) != 1 || lines[0]["cohort"] != float64(first+1) {
			t.Errorf("code_cache_memo_unlisted was logged %d times (%v), want once, for cohort %d", len(lines),
				lines, first+1)
		}
		if st, _ := cache.MemoOf(c, repo); st.Listed {
			t.Errorf("the copy's record is %+v after a cohort of it could not be read, want it left to the next "+
				"tick unlisted", st)
		}
	})

	// The engine stopping fails at nothing: the next pass lists the copy.
	// Mutation: log a measure the pass's end cut short (listMemo without its
	// ended-context case).
	t.Run("a listing the pass's end cuts short is no failure", func(t *testing.T) {
		t.Parallel()
		ctx, stop := context.WithCancel(t.Context())
		defer stop()
		var (
			c         *cache.Cache
			w         *syncWorld
			repo, dir string
			first     uint64
			stopped   bool
		)
		c, repo, dir, first, w = crashed(t, nil, func(o *cache.Options) {
			cache.BeforeMeasuring(o, func(d string) {
				// The engine stops as the listing is about to measure the
				// cohort no state records.
				if d == cache.MemoCohortDir(dir, first+1) && !stopped {
					stop()
					stopped = true
				}
			})
		})
		cache.JanitorPassUnder(ctx, c)
		if !stopped {
			t.Fatal("the listing never came to measure the cohort no state records")
		}
		if lines := w.logs.lines(t, "code_cache_memo_unlisted"); len(lines) != 0 {
			t.Errorf("code_cache_memo_unlisted was logged %d times (%v) for a listing the pass's end cut short, "+
				"want never", len(lines), lines)
		}
		if st, _ := cache.MemoOf(c, repo); st.Listed {
			t.Errorf("the copy's record is %+v after its listing was cut short, want it left to the next pass "+
				"unlisted", st)
		}
		cache.JanitorPass(c)
		if st, _ := cache.MemoOf(c, repo); !st.Listed || !slices.Equal(st.Cohorts, []uint64{first, first + 1}) {
			t.Errorf("after the next pass the copy's record is %+v, want it listed with cohorts %d and %d", st,
				first, first+1)
		}
	})

	// Mutation: take an entry gone by its stat for the measure's failure
	// (measureCohort without its fs.ErrNotExist case) — the record lacks the
	// cohort.
	t.Run("an entry gone by its stat counts nothing", func(t *testing.T) {
		t.Parallel()
		var (
			c         *cache.Cache
			w         *syncWorld
			repo, dir string
			first     uint64
			removed   bool
		)
		c, repo, dir, first, w = crashed(t, nil, func(o *cache.Options) {
			cache.BeforeStatingMemo(o, func(entry string) error {
				// The cohort's listing has named its entries, and something
				// outside the engine removes the first before the measure
				// stats it.
				if removed || filepath.Dir(entry) != cache.MemoCohortDir(dir, first+1) {
					return nil
				}
				if err := os.Remove(entry); err != nil {
					t.Error(err)
					return nil
				}
				removed = true
				return nil
			})
		})
		cache.JanitorPass(c)
		if !removed {
			t.Fatal("the measure never came to stat an entry of the cohort no state records")
		}
		if lines := w.logs.lines(t, "code_cache_memo_unlisted"); len(lines) != 0 {
			t.Errorf("code_cache_memo_unlisted was logged %d times (%v) for an entry gone by its stat, want never",
				len(lines), lines)
		}
		st, _ := cache.MemoOf(c, repo)
		if !st.Listed || !slices.Equal(st.Cohorts, []uint64{first, first + 1}) ||
			!slices.Equal(st.Held, []int64{4 * block, 2 * block}) {
			t.Errorf("after the listing the record is %+v, want cohorts %d and %d holding %d and %d bytes, the "+
				"second its two entries left", st, first, first+1, 4*block, 2*block)
		}
	})

	// Mutation: pass over an entry whose stat fails (measureCohort counting
	// only what it stats) — nothing is logged and the copy is listed.
	t.Run("an entry the measure cannot stat is logged", func(t *testing.T) {
		t.Parallel()
		var (
			c         *cache.Cache
			w         *syncWorld
			repo, dir string
			first     uint64
			replaced  bool
		)
		c, repo, dir, first, w = crashed(t, nil, func(o *cache.Options) {
			cache.BeforeStatingMemo(o, func(entry string) error {
				// The cohort's listing has named its entries, and the cohort
				// becomes a file before the measure stats the first, so the
				// stat answers ENOTDIR: an entry still named that cannot be
				// stat'ed, as EIO from a failing disk or EACCES from a lost
				// search permission leaves one.
				cohort := cache.MemoCohortDir(dir, first+1)
				if replaced || filepath.Dir(entry) != cohort {
					return nil
				}
				if err := os.RemoveAll(cohort); err != nil {
					t.Error(err)
					return nil
				}
				if err := os.WriteFile(cohort, []byte("no cohort"), 0o600); err != nil {
					t.Error(err)
					return nil
				}
				replaced = true
				return nil
			})
		})
		cache.JanitorPass(c)
		if !replaced {
			t.Fatal("the measure never came to stat an entry of the cohort no state records")
		}
		lines := w.logs.lines(t, "code_cache_memo_unlisted")
		if len(lines) != 1 || lines[0]["cohort"] != float64(first+1) {
			t.Errorf("code_cache_memo_unlisted was logged %d times (%v), want once, for cohort %d", len(lines),
				lines, first+1)
		}
		if st, _ := cache.MemoOf(c, repo); st.Listed || st.Bytes != 4*block {
			t.Errorf("the copy's record is %+v after an entry of a cohort could not be stat'ed, want it left to "+
				"the next tick unlisted, holding the %d bytes its state recorded", st, 4*block)
		}
		if got := cache.MemoBytes(c); got != 4*block {
			t.Errorf("the node counts its memo at %d bytes, want the %d the copy's state recorded", got, 4*block)
		}
	})

	// Mutation, for this row and the next: open the boot's newest past the
	// states alone (openMemo ignoring the reservation) — the placement and
	// hit land in the crash's cohort, which the record then counts short.
	t.Run("a placement and a hit before the listing leave the crash's cohort to it", func(t *testing.T) {
		t.Parallel()
		c, repo, dir, first, w := crashed(t, nil)
		if got := cache.MemoNewest(c); got <= first+2 {
			t.Fatalf("the boot opened cohort %d, want one past the %d the crashed process had opened", got,
				first+2)
		}
		s := w.sync(t, c, repo)
		defer s.Release()
		// A run places its answers, and a read finds one the state recorded
		// and moves it into the newest, before the janitor's first pass.
		memoize(t, c, s, memoKeys("after", 2))
		if _, ok := c.MemoGet(s, memoKeys("crashed", 9)[0]); !ok {
			t.Fatal("an answer the state recorded was not served after the restart")
		}
		cache.JanitorPass(c)
		st, _ := cache.MemoOf(c, repo)
		cohorts, held := crashCounted(t, dir, first, block)
		if !st.Listed || !slices.Equal(st.Cohorts, cohorts) || !slices.Equal(st.Held, held) {
			t.Errorf("after the listing the record is %+v, want cohorts %v holding %v: the crash's cohort %d "+
				"counted whole", st, cohorts, held, first+1)
		}
	})

	// Under that mutation the placement puts the crash's cohort in the record
	// and the state, and the tick lists the copy.
	t.Run("a placement between two failed listings leaves the crash's cohort out of every state", func(t *testing.T) {
		t.Parallel()
		var (
			c         *cache.Cache
			w         *syncWorld
			repo, dir string
			first     uint64
			failing   atomic.Bool
		)
		failing.Store(true)
		c, repo, dir, first, w = crashed(t, nil, func(o *cache.Options) {
			cache.BeforeStatingMemo(o, func(entry string) error {
				// The disk fails every stat of the crash's cohort until the
				// case fixes it.
				if failing.Load() && filepath.Dir(entry) == cache.MemoCohortDir(dir, first+1) {
					return syscall.EIO
				}
				return nil
			})
		})
		cache.JanitorPass(c)
		s := w.sync(t, c, repo)
		defer s.Release()
		memoize(t, c, s, memoKeys("after", 2))
		cache.JanitorTick(c) // listed again, and the copy's state written
		if lines := w.logs.lines(t, "code_cache_memo_unlisted"); len(lines) != 2 {
			t.Errorf("code_cache_memo_unlisted was logged %d times (%v) over two failed listings, want twice",
				len(lines), lines)
		}
		st, _ := cache.MemoOf(c, repo)
		if st.Listed || slices.Contains(st.Cohorts, first+1) {
			t.Errorf("the copy's record is %+v while the crash's cohort %d cannot be measured, want it unlisted "+
				"and naming no such cohort", st, first+1)
		}
		for _, ch := range readStateDoc(t, dir).Derived[string(cache.DerivedMemo)].Cohorts {
			if ch.N == first+1 {
				t.Errorf("the tick wrote the crash's cohort %d into the copy's state at %d bytes, which no "+
					"measure counted", ch.N, ch.Bytes)
			}
		}
		failing.Store(false)
		cache.JanitorTick(c)
		st, _ = cache.MemoOf(c, repo)
		cohorts, held := crashCounted(t, dir, first, block)
		if !st.Listed || !slices.Equal(st.Cohorts, cohorts) || !slices.Equal(st.Held, held) {
			t.Errorf("once the disk is fixed the record is %+v, want cohorts %v holding %v: the crash's cohort %d "+
				"counted whole", st, cohorts, held, first+1)
		}
	})
}

// crashCounted is what the crashed copy's record (crashed) must say once its
// tree is listed: each cohort at what its directory holds, but the first,
// counted as the pre-crash state said: short by the two answers placed in it
// after, which is what a crash costs the count.
func crashCounted(t *testing.T, dir string, first uint64, block int64) ([]uint64, []int64) {
	t.Helper()
	cohorts, held := cohortsOnDisk(t, dir, block)
	if len(cohorts) == 0 || cohorts[0] != first {
		t.Fatalf("the crashed copy's tree holds cohorts %v, want its state's %d the first", cohorts, first)
	}
	held[0] -= 2 * block
	return cohorts, held
}

// A BOOT OPENS ITS NEWEST COHORT PAST EVERY ONE ITS CACHE RESERVED. A cohort's
// number is durable in the cache's root before the node's newest moves to it,
// so a boot opens past every cohort a previous process may have placed in (and
// past the states'), reserving its own before any placement. A root holding
// copies and no readable reservation is logged and the boot opens past the
// states; a fresh cache logs nothing; a root that takes no write opens no cache.
//
// Mutations: open past the states alone (openMemo ignoring the reservation) —
// row 1 opens at the state's; reserve nothing at the boot — every row's
// reservation is the previous process's; log no unread reservation — rows 2
// and 3 log nothing; log a fresh cache's absence — the fresh case logs; open
// a cache whose reservation could not be written — the last case opens.
func TestABootOpensItsNewestCohortPastEveryOneItsCacheReserved(t *testing.T) {
	t.Parallel()
	block := memoEntry(0)
	// opened is a stopped cache whose copy's state records cohort first,
	// while its process went on to open two cohorts past it, each by a run
	// that filled the one before, with no tick since.
	opened := func(t *testing.T) (*syncWorld, uint64) {
		t.Helper()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		c := w.open(t)
		s := w.sync(t, c, repo)
		first := cache.MemoNewest(c)
		keys := memoKeys("opened", 3)
		memoize(t, c, s, keys[:1])
		cache.JanitorTick(c) // the state records cohort first
		// A share whose sixteenth is an answer: each run from here closes
		// the cohort it was placed in.
		cache.SetMaxBytes(c, cache.MemoShare*cache.MemoCohortsPerShare*block)
		memoize(t, c, s, keys[1:2])
		memoize(t, c, s, keys[2:3])
		if got := cache.MemoNewest(c); got != first+2 {
			t.Fatalf("the process's newest cohort is %d after two runs that each filled one, want %d", got, first+2)
		}
		s.Release()
		c.Stop(context.Background())
		return w, first
	}
	for _, tc := range []struct {
		name   string
		before func(t *testing.T, file string)
		// past is how far past first the cohort is that the boot opens one
		// past.
		past   uint64
		logged int
	}{
		{"past the newest its previous process reserved", nil, 2, 0},
		{"past the states where the reservation is gone", func(t *testing.T, file string) {
			if err := os.Remove(file); err != nil {
				t.Fatal(err)
			}
		}, 0, 1},
		{"past the states where the reservation is damaged", func(t *testing.T, file string) {
			write(t, file, "seventeen\n")
		}, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w, first := opened(t)
			file := cache.MemoCohortsFile(w.layout.Dir)
			if tc.before != nil {
				tc.before(t, file)
			}
			c := w.open(t)
			want := first + tc.past + 1
			if got := cache.MemoNewest(c); got != want {
				t.Errorf("the boot opened cohort %d, want %d", got, want)
			}
			if lines := w.logs.lines(t, "code_cache_memo_cohorts_unread"); len(lines) != tc.logged {
				t.Errorf("code_cache_memo_cohorts_unread was logged %d times (%v), want %d", len(lines), lines,
					tc.logged)
			}
			if got := read(t, file); got != cohortLine(want) {
				t.Errorf("the root reserves %q after the boot, want the cohort it opened, %q", got, cohortLine(want))
			}
		})
	}

	t.Run("a cache no process has opened", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		c := w.open(t)
		if got := cache.MemoNewest(c); got != 1 {
			t.Errorf("a fresh cache opened cohort %d, want the first, 1", got)
		}
		if lines := w.logs.lines(t, "code_cache_memo_cohorts_unread"); len(lines) != 0 {
			t.Errorf("code_cache_memo_cohorts_unread was logged %d times (%v) for a fresh cache, want never",
				len(lines), lines)
		}
		if got := read(t, cache.MemoCohortsFile(w.layout.Dir)); got != cohortLine(1) {
			t.Errorf("a fresh cache's root reserves %q, want %q", got, cohortLine(1))
		}
	})

	t.Run("a root that takes no write opens no cache", func(t *testing.T) {
		t.Parallel()
		w, _ := opened(t)
		file := cache.MemoCohortsFile(w.layout.Dir)
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		// A directory holding something at the reservation's name: no file
		// is renamed over it.
		write(t, filepath.Join(file, "held"), "not a reservation")
		w.reboot(t)
		c, err := cache.Open(t.Context(), w.options(t, w.wire))
		if c != nil {
			c.Stop(context.Background())
		}
		if err == nil || !strings.Contains(err.Error(), file) {
			t.Errorf("Open over a root whose reservation could not be written answered %v, want an error naming %s",
				err, file)
		}
	})
}

// A COHORT OPENS ONLY ONCE ITS NUMBER IS DURABLE. The placement that fills the
// newest cohort reserves the next in the cache's root before opening it, so a
// crash after leaves a reservation the boot opens past. A refused reservation
// opens nothing: it is logged, the newest stays open, and the next is tried
// once another sixteenth of the share is placed, not at every placement.
//
// Mutations: open on a refusal (memoPlaced moving the newest anyway) — the
// refused reservation opens one; open unreserved (memoPlaced reserving
// nothing) — the boot after opens past nothing the process opened; retry at
// every placement (memoPlaced keeping the fill it failed at) — the short run
// logs again.
func TestACohortOpensOnlyOnceItsNumberIsDurable(t *testing.T) {
	t.Parallel()
	block := memoEntry(0)
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	first := cache.MemoNewest(c)
	file := cache.MemoCohortsFile(w.layout.Dir)
	if got := read(t, file); got != cohortLine(first) {
		t.Fatalf("the root reserves %q after the boot, want the cohort it opened, %q", got, cohortLine(first))
	}
	// A share whose sixteenth is four answers.
	cache.SetMaxBytes(c, cache.MemoShare*cache.MemoCohortsPerShare*4*block)
	// The root refuses the next reservation: a directory holds its name.
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(file, "held"), "not a reservation")

	memoize(t, c, s, memoKeys("refused", 4))
	if got := cache.MemoNewest(c); got != first {
		t.Errorf("the newest cohort is %d after a run filled cohort %d and the root refused the next, want it "+
			"still %d", got, first, first)
	}
	lines := w.logs.lines(t, "code_cache_memo_unreserved")
	if len(lines) != 1 || lines[0]["cohort"] != float64(first+1) {
		t.Errorf("code_cache_memo_unreserved was logged %d times (%v), want once, for cohort %d", len(lines),
			lines, first+1)
	}
	memoize(t, c, s, memoKeys("short", 2))
	if lines := w.logs.lines(t, "code_cache_memo_unreserved"); len(lines) != 1 {
		t.Errorf("code_cache_memo_unreserved was logged %d times (%v) after half a sixteenth more, want the "+
			"first alone: the next is tried a sixteenth after the last", len(lines), lines)
	}

	if err := os.RemoveAll(file); err != nil {
		t.Fatal(err)
	}
	memoize(t, c, s, memoKeys("taken", 2))
	if got := cache.MemoNewest(c); got != first+1 {
		t.Errorf("the newest cohort is %d once the root took the reservation, want %d", got, first+1)
	}
	if got := read(t, file); got != cohortLine(first+1) {
		t.Errorf("the root reserves %q, want the cohort opened, %q", got, cohortLine(first+1))
	}
	s.Release()
	c = w.reopen(t, c)
	if got := cache.MemoNewest(c); got != first+2 {
		t.Errorf("the boot after opened cohort %d, want %d: one past the %d its previous process reserved", got,
			first+2, first+1)
	}
}

// A LISTING THAT FAILED IS TRIED AGAIN AT THE TICK, NOT AT EVERY PASS. Kicked
// passes come back to back on a busy node, and a listing holds the copy's memo
// lock, which a read's placement waits on: so a failed copy waits for the
// tick, and a kicked pass neither measures nor logs it. A copy never listed is
// listed at any pass.
//
// Mutations: list a failed copy at every pass (listMemos ignoring
// memoUnlisted) — the kicked pass measures and logs again; never list it again
// (listMemos passing it over on a tick too) — the tick measures nothing.
func TestAFailedListingIsTriedAgainAtTheTick(t *testing.T) {
	t.Parallel()
	block := memoEntry(0)
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	dir := s.Dir
	memoize(t, c, s, memoKeys("listed", 2))
	cache.JanitorTick(c) // the state records the answers' cohort
	s.Release()
	// The cohort the state records renamed to one no state does, as a crash
	// leaves one: the listing forgets the first and has the second's two
	// entries to stat.
	recorded := cache.MemoNewest(c)
	unrecorded := recorded + 7
	if err := os.Rename(cache.MemoCohortDir(dir, recorded), cache.MemoCohortDir(dir, unrecorded)); err != nil {
		t.Fatal(err)
	}
	var (
		stats   atomic.Int64
		failing atomic.Bool
	)
	failing.Store(true)
	c = w.reopen(t, c, func(o *cache.Options) {
		cache.BeforeStatingMemo(o, func(string) error {
			stats.Add(1)
			if failing.Load() {
				return syscall.EIO
			}
			return nil
		})
	})
	unlisted := func() int { return len(w.logs.lines(t, "code_cache_memo_unlisted")) }

	cache.JanitorPass(c) // the boot's pass: the copy was never listed
	if stats.Load() == 0 || unlisted() != 1 {
		t.Fatalf("the first pass stat'ed %d entries and logged code_cache_memo_unlisted %d times, want the "+
			"listing tried and logged once", stats.Load(), unlisted())
	}
	before := stats.Load()
	cache.JanitorPass(c)
	if got := stats.Load(); got != before || unlisted() != 1 {
		t.Errorf("a kicked pass after the failed listing stat'ed %d entries more and logged "+
			"code_cache_memo_unlisted %d times, want neither measured nor logged again", got-before, unlisted())
	}
	cache.JanitorTick(c)
	if got := stats.Load(); got == before || unlisted() != 2 {
		t.Errorf("the tick after the failed listing stat'ed %d entries more and logged code_cache_memo_unlisted "+
			"%d times, want the listing tried again and logged a second time", got-before, unlisted())
	}
	failing.Store(false)
	cache.JanitorPass(c)
	if st, _ := cache.MemoOf(c, repo); st.Listed {
		t.Errorf("a kicked pass listed the copy (%+v) whose last listing failed, want it left to the tick", st)
	}
	cache.JanitorTick(c)
	st, _ := cache.MemoOf(c, repo)
	if !st.Listed || !slices.Equal(st.Cohorts, []uint64{unrecorded}) || !slices.Equal(st.Held, []int64{2 * block}) {
		t.Errorf("once the disk is fixed the tick's listing recorded %+v, want cohort %d alone, at %d bytes", st,
			unrecorded, 2*block)
	}
}

// A FAILED LISTING STILL WAITS FOR THE TICK AFTER A SWAP: the swap moves the
// same tree whole into the next incarnation (carryMemo), so that incarnation
// too is listed again only at the tick.
//
// Mutation: carry memoListed alone (carryMemo leaving memoUnlisted false) —
// the first kicked pass after the swap measures and logs again.
func TestAFailedListingWaitsForTheTickAcrossASwap(t *testing.T) {
	t.Parallel()
	k := newCompactable(t, false)
	w, repo := k.w, k.repo
	memoize(t, k.c, k.held, memoKeys("unlisted", 2))
	cache.JanitorTick(k.c) // the state records the answers' cohort
	dir := k.held.Dir
	k.held.Release()
	// The cohort the state records renamed to one no state does, as a crash
	// leaves one, so the boot's listing has entries to stat.
	recorded := cache.MemoNewest(k.c)
	if err := os.Rename(cache.MemoCohortDir(dir, recorded), cache.MemoCohortDir(dir, recorded+7)); err != nil {
		t.Fatal(err)
	}
	var stats atomic.Int64
	c := w.reopen(t, k.c, k.clk.on, func(o *cache.Options) {
		cache.BeforeStatingMemo(o, func(string) error {
			stats.Add(1)
			return syscall.EIO
		})
	})
	unlisted := func() int { return len(w.logs.lines(t, "code_cache_memo_unlisted")) }

	cache.JanitorPass(c) // the boot's pass: the copy was never listed
	if stats.Load() == 0 || unlisted() != 1 {
		t.Fatalf("the boot's pass stat'ed %d entries and logged code_cache_memo_unlisted %d times, want the "+
			"listing tried and logged once", stats.Load(), unlisted())
	}
	held := w.sync(t, c, repo) // a reader holds the copy, so the compaction swaps it
	defer held.Release()
	k.compact(t)
	s := w.sync(t, c, repo)
	defer s.Release()
	if s.Dir == held.Dir {
		t.Fatalf("the compaction answered the copy a reader holds, %s: no swap", s.Dir)
	}
	if lines := w.logs.lines(t, "code_cache_memo_carried"); len(lines) != 1 {
		t.Fatalf("code_cache_memo_carried = %v, want the swap's one line: the case needs the tree carried", lines)
	}
	if st, _ := cache.MemoOf(c, repo); st.Listed {
		t.Fatalf("the next incarnation's memo is listed (%+v), want it unlisted, as the tree the swap carried was",
			st)
	}
	before, logged := stats.Load(), unlisted()
	cache.JanitorPass(c)
	if got := stats.Load(); got != before || unlisted() != logged {
		t.Errorf("a kicked pass after the swap stat'ed %d entries more and logged code_cache_memo_unlisted %d "+
			"times more, want the carried tree left to the tick, as the copy the swap left was", got-before,
			unlisted()-logged)
	}
	cache.JanitorTick(c)
	if got := stats.Load(); got == before || unlisted() != logged+1 {
		t.Errorf("the tick after the swap stat'ed %d entries more and logged code_cache_memo_unlisted %d times "+
			"more, want the carried tree listed again and logged once more", got-before, unlisted()-logged)
	}
}

// cohortLine is the reservation of cohort n as the cache's root holds it.
func cohortLine(n uint64) string { return strconv.FormatUint(n, 10) + "\n" }

// cohortsOnDisk is what the memo tree of the incarnation at dir holds, its
// cohorts in order and each one's entries at block bytes apiece, every answer
// a case places taking one block.
func cohortsOnDisk(t *testing.T, dir string, block int64) ([]uint64, []int64) {
	t.Helper()
	var cohorts []uint64
	for _, name := range names(t, cache.MemoTree(dir)) {
		n, err := strconv.ParseUint(name, 10, 64)
		if err != nil {
			t.Fatalf("the memo tree of %s holds %q, which no cohort is", dir, name)
		}
		cohorts = append(cohorts, n)
	}
	slices.Sort(cohorts)
	held := make([]int64, len(cohorts))
	for i, n := range cohorts {
		held[i] = int64(len(names(t, cache.MemoCohortDir(dir, n)))) * block
	}
	return cohorts, held
}

// THE NODE KEEPS AT MOST MaxMemoCohorts COHORTS, whatever they hold. Cohorts
// open only by placement and the trim drops one per sixteenth, so a full memo
// holds some seventeen; but what thins old cohorts without filling the memo
// (damaged answers removed, answers a read moves on) would leave ever more,
// each one more name every miss opens. So the tick drops the oldest past the
// bound even far within the share. Here the memo holds one answer across
// forty cohorts.
//
// Mutation: drop for the share alone (trimMemo ignoring maxMemoCohorts) — the
// tick leaves all forty.
func TestTheNodeKeepsAtMostMaxMemoCohorts(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	// A share whose sixteenth is an answer: each run closes its cohort.
	cache.SetMaxBytes(c, cache.MemoShare*cache.MemoCohortsPerShare*memoEntry(0))
	s := w.sync(t, c, repo)
	defer s.Release()
	keys := memoKeys("thinned", 40)
	for i, k := range keys {
		memoize(t, c, s, []cache.MemoKey{k})
		if i == 0 {
			continue
		}
		write(t, cache.MemoEntryFile(s, keys[i-1]), "damaged")
		if _, ok := c.MemoGet(s, keys[i-1]); ok {
			t.Fatal("a damaged answer was served")
		}
	}
	if st, _ := cache.MemoOf(c, repo); len(st.Cohorts) != len(keys) {
		t.Fatalf("the memo holds %d cohorts before the tick, want one for each of its %d runs", len(st.Cohorts),
			len(keys))
	}
	cache.JanitorTick(c)
	st, _ := cache.MemoOf(c, repo)
	if span := cache.MemoNewest(c) - st.Cohorts[0]; span > cache.MaxMemoCohorts {
		t.Errorf("after the tick the node keeps cohorts %v, the newest %d: a span of %d past the %d it keeps",
			st.Cohorts, cache.MemoNewest(c), span, cache.MaxMemoCohorts)
	}
	if cache.MemoEntryFile(s, keys[len(keys)-1]) == "" {
		t.Error("the tick dropped the one answer the memo held, in its newest cohort")
	}
}

// A STATE WRITTEN BEHIND THE SERVING KEEPS THE COPY'S LAST USE. A boot reads
// a copy's last use from its state's mtime, and the tick writes the state once
// the memo moved, a trim among the moves, which is no use of the copy. So the
// write stamps the last use, never its own moment; else an unread copy would
// come back from a restart as the most recently used, and the budget would
// keep it while evicting copies seats do read.
//
// Mutation: write the state as it is staged (writeState's stamp dropped) — the
// copy comes back from the restart used at the moment of the trim's write.
func TestAStateWrittenBehindTheServingKeepsTheLastUse(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	dir := s.Dir
	memoize(t, c, s, memoKeys("used", 4))
	s.Release()
	used := cache.LastUsed(c, repo)
	cache.JanitorTick(c) // the use stamped, and the memo's state written
	clk.Add(time.Hour)
	// A share of one answer, so the trim takes the copy's one cohort.
	cache.SetMaxBytes(c, cache.MemoShare*memoEntry(0))
	cache.JanitorPass(c)
	if st, _ := cache.MemoOf(c, repo); st.Stated || st.Bytes != 0 {
		t.Fatalf("after the trim the memo is %+v, want its cohort gone and its state behind it", st)
	}
	cache.JanitorTick(c)
	info, err := os.Stat(filepath.Join(dir, "crewlet-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(used) {
		t.Errorf("the state the trim's tick wrote is stamped %v, want the copy's last use %v", info.ModTime(), used)
	}
	again := w.reopen(t, c, clk.on)
	if got := cache.LastUsed(again, repo); !got.Equal(used) {
		t.Errorf("after a restart the copy was last used %v, want %v: the trim's state is no use of it", got, used)
	}
}

// A RUN'S EXTENSION OF A PAGE IS THE PAGE'S. What a run appends to a staged
// page (MemoBatch.Extend; a diff stat's file counts) is part of the entry:
// read back as page, extension, tail, and counted as one entry. An extension
// that with the tail would pass memoMaxTail gives the page back unmemoized,
// its staged bytes with it.
//
// Mutation: leave the extension out of the entry's digest (Extend's sum left
// alone) — the entry fails its digest and is no answer.
func TestARunsExtensionOfAPageIsThePages(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	defer s.Release()
	keys := memoKeys("extended", 2)
	const counts = "|its files' counts"
	b := c.Memo(s)
	first, second := b.Put(keys[0], memoBody(0)), b.Put(keys[1], memoBody(1))
	b.Extend(first, []byte(counts))
	b.Extend(second, make([]byte, cache.MemoMaxTail))
	b.Place(func(int) []byte { return []byte(memoTail) })

	if got, ok := c.MemoGet(s, keys[0]); !ok || string(got) != string(memoBody(0))+counts+memoTail {
		t.Errorf("the extended page read back %q (%t), want its body, its extension and its tail", got, ok)
	}
	if _, ok := c.MemoGet(s, keys[1]); ok {
		t.Error("a page extended past what a run appends was memoized")
	}
	if got, want := cache.MemoBytes(c), cache.MemoEntryBytes(len(memoBody(0))+len(counts)+len(memoTail)); got != want {
		t.Errorf("the memo counts %d bytes, want the one extended page's %d", got, want)
	}
	if got := cache.MemoStaged(c); got != 0 {
		t.Errorf("the memo counts %d bytes staged once the batch is placed", got)
	}
}

// A PROBE IS COUNTED APART FROM A LOOKUP: MemoProbe counts probe_hit and
// probe_miss, a damaged entry corrupt, and its hit moves the entry to the newest
// cohort as a lookup's does.
// Mutation: MemoProbe counting as MemoGet does, or skipping memoUse.
func TestAProbeIsCountedApartFromALookup(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	share := 18 * memoEntry(0)
	cache.SetMaxBytes(c, cache.MemoShare*share)
	s := w.sync(t, c, repo)
	defer s.Release()
	keys := memoKeys("probed", 8)
	memoize(t, c, s, keys[:4])
	memoize(t, c, s, keys[4:])
	newest := cache.MemoCohortDir(s.Dir, cache.MemoNewest(c)) + string(os.PathSeparator)
	if strings.HasPrefix(cache.MemoEntryFile(s, keys[0]), newest) {
		t.Fatal("the premise: the first run's answers are in an older cohort than the newest")
	}
	if got, ok := c.MemoProbe(s, keys[0]); !ok || string(got) != string(memoBody(0))+memoTail {
		t.Fatalf("a probe of a whole entry read back %q (%t), want its body and its tail", got, ok)
	}
	if !strings.HasPrefix(cache.MemoEntryFile(s, keys[0]), newest) {
		t.Error("a probe's hit left the entry in its older cohort, where the trim takes it first")
	}
	if _, ok := c.MemoProbe(s, cache.NewMemoKey("case", "probed", "absent")); ok {
		t.Error("a probe of a key nothing was kept under was answered")
	}
	damaged := cache.MemoEntryFile(s, keys[1])
	if err := os.Truncate(damaged, 1); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.MemoProbe(s, keys[1]); ok || statOK(t, damaged) {
		t.Error("a probe answered a damaged entry, or left it in the tree")
	}
	for outcome, want := range map[string]uint64{"probe_hit": 1, "probe_miss": 1, "corrupt": 1, "hit": 0, "miss": 0} {
		if got := counter(w.rec, metrics.CodeCacheMemoLookups, "outcome", outcome); got != want {
			t.Errorf("lookups{outcome=%s} = %d after the probes, want %d", outcome, got, want)
		}
	}
}
