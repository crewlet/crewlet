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
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE MEMO (memo.go): answers by object id kept beside the copy they were cut
// from, each entry whole or not served, held to a sixteenth of the budget
// before any repository goes, counted by its running total — never by a
// measure of its directory — kept by a rebuild and dropped by a history gone,
// with nothing read over the history it replaced placed after the drop.

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

// AN ENTRY IS READ ONLY WHOLE. Every read holds an entry to its header — the
// key it answers, its body's length and the SHA-256 of what it holds — so an
// entry cut short, one whose bytes changed, and another key's answer under
// this key's name are each a miss, removed from the tree and from what the
// memo counts, and counted lookups{outcome=corrupt}; a whole one is answered,
// body and tail.
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
// sixteenth of the share placed in the node's newest cohort opens another, so
// five runs' pages of four answers each, each run a sixteenth of the share, are
// five cohorts; a read of an answer in an older cohort moves it into the
// newest; and the janitor, with the memo past its share, drops the OLDEST
// cohort whole — one rename of its directory — and no more than the memo's
// share asks. So the first run's three answers nobody read since go together,
// and its fourth, read after the fifth run, stays with every later run's.
//
// Mutations: serve a hit where it is (memoUse doing nothing) — the first
// run's read answer goes with its cohort; drop the newest cohort rather than
// the oldest — the latest runs' answers go.
func TestTheMemoIsTrimmedACohortAtATimeTheLeastRecentlyUsedFirst(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	// A share of 18 answers: a cohort is opened past every 4.5 KiB
	// placed, so each run of four answers, 16 KiB, closes its own.
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

// A TRIM FALLS DUE ONCE A COHORT, NOT ONCE A MISS. With the memo at its share,
// every placement takes it past it, and the janitor passes after each, as the
// placement's kick has it pass: a trim drops the oldest cohort, a sixteenth of
// the share, so the memo is then that far under its share and the next trim
// is due once as much again has been placed. So 32 placements of an answer
// past a full memo, a cohort being four answers, are eight trims — where
// trimming to the share exactly trimmed at every one of the 32.
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
// each copy that holds it, under that copy's memo lock, which keeps the tree
// still but not the copy: a purge may retire a copy the trim has found holding
// the cohort and move its directory into trash/ whole — the cohort with it —
// before the trim's own move. That cohort left with its copy, whose bytes the
// purge took out of the totals, so the trim counts neither it nor its bytes:
// code_cache_memo_trimmed reports the copies the trim dropped the cohort from
// and what that gave back.
//
// The case puts one answer of each of two copies in the node's oldest cohort,
// fills one of them past the memo's share in cohorts of their own, and purges
// the other as the trim is about to move its cohort.
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
			// The trim has found the purged repository's copy holding the
			// cohort and is about to move it: a purge takes the copy, which
			// nobody holds, there.
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

// A RUN'S STAGED PAGES ARE THE MEMO'S. A page a read's run has staged and not
// yet placed counts against the memo's share beside the pages it keeps, so the
// janitor makes room for it from the memo's own answers — never by evicting a
// repository, which would cost a cold sync of every pack it holds, a GET each
// from the object store, for bytes the memo was never entitled to.
//
// The case stages one page that takes the cache one byte past its budget,
// with the memo's kept pages within their share and those and the staged page
// together past it: so only the share's count of the staged page decides
// whether a repository goes. The idle copy is the only one an eviction may
// take; the copy the run reads stays pinned.
//
// Mutation: hold only the kept pages to the share (trimMemo reading
// memoBytes alone) — the pass leaves the kept and staged pages together past
// the share; and with the eviction counting the memo's excess too (overBudget
// reading the total alone), the idle repository is evicted for the page.
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

	// The kept pages, m of them at a block each, and one staged page of a
	// block's body: the most m whose pages fit the share once the budget is
	// one byte short of the staged page — which then takes them past it.
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

// A PAGE PAST THE MEMO'S SHARE IS NEVER STAGED. A run's page that would take
// every run's staged pages past the memo's share is not staged at all: the
// memo could never keep it, and staged it would hold room the repositories'
// budget is for. So four pages of some 6 KB against a share of a few hundred
// bytes are answered unmemoized, none of their bytes is counted, and no
// repository goes for them, while they are cut or after.
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
// eviction is for what the repositories hold past their room — the budget
// less what the memo may hold — never for the memo's own excess, which its
// trim gives back: so an eviction pass on its own, with the memo past its
// share and the cache past its budget by no more than that, evicts nothing.
// It closes what the trim before it cannot: a page staged between the trim
// and the eviction.
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

// A PLACEMENT PAST THE MEMO'S SHARE WAKES THE JANITOR. No eviction is due for
// the memo's excess (overBudget), so no release or round kicks a pass for it:
// the placement that takes the memo past its share does, and the trim follows
// at once rather than at the janitor's tick, which never comes here.
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
// node's newest cohort past every one a state records, so what a previous
// process left is older than anything placed or read since: the trim that the
// memo past its share asks takes the cold copy's answers — the whole cohort
// they share, one rename — before any the warm copy's run placed after the
// restart, and the cold answer a read moved on after the restart stays.
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

// A REBUILD MOVES THE MEMO WHOLE. A rebuild repairs packs, not answers, and
// the memo of the copy it replaces MOVES into the new one as that is promoted:
// one rename of its tree, whatever it holds — so the tree in the rebuilt copy
// is the very directory the replaced copy held, never a new one filled an
// entry at a time, and the round that holds the repository pays nothing an
// answer for it. Every entry is answered there as it was and counted once;
// the replaced copy, which a reader still holds, answers none from then on.
//
// Mutations: link the tree an entry at a time, as a seeding would — the
// rebuilt copy's tree is another directory; carry nothing — the rebuilt copy
// answers none.
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
// object ids only while the history it was cut from stands, so a rebuild
// whose round reads another history carries nothing of the replaced copy's
// memo: not over a log recreated under the node, which marks the replaced
// copy's history gone, nor over a log reanchored at another generation, whose
// copy the rebuild's own round finds gone and drops a memo of.
//
// Mutations: carry a memo whose history is marked gone (carryMemo's
// historyGone) — the recreated case's rebuilt copy answers it; carry into a
// copy that dropped a memo (carryMemo's generation) — the reanchored one's.
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

// A RECREATED LOG DROPS THE MEMO. A log recreated under the cache, or restored
// from an older copy, is another history, which the memo cannot tell entry by
// entry: the full round that converges the copy to it drops the whole tree —
// before any git process of the round runs there, which the round's tidy
// precedes — and its record, so nothing the old history answered is answered
// again.
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
// before the history went — its run read the copy as the old history left it
// — places nothing once the drop has moved the memo's generation; and while
// the old history's packs are still in a copy, its memo is shut: no batch
// begins, since a read then may meet objects of either history. Once they are
// out, answers are memoized again — in the same copy, where the round that
// dropped the memo swept them IN PLACE (a copy no reader holds), and in the
// copy's NEXT incarnation where a reader holds it and they leave by the SWAP
// (swap.go): the copy the swap leaves keeps them for its readers, and its memo
// stays shut, through the swap's own window and after.
//
// Mutations: let Place ignore the generation — the batch from before the drop
// places its answer in place; let Memo ignore the shut memo — the round's own
// window begins a batch; reopen the memo of a copy whose strays leave by the
// swap (apply) — a batch begins over the copy the swap is leaving.
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
			faults := &codefault.Script{}
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
				// The swap's own window: the copy it leaves is staged out of,
				// the old history's pack still in it, and nothing renamed yet.
				release, reached := make(chan struct{}), make(chan struct{})
				faults.Arm(codefault.SyncSwapBeforeRename, codefault.Action{Once: true, Block: release,
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
				case <-time.After(hangDetector):
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

// THE HELD BYTES COUNT THE MEMO, by its running total and never by a measure
// of its tree: a maintenance run and a full round's index each measure the
// derived files and record what they measure, and a tree measured as a
// directory is its inode's few KiB however many answers it holds — written
// into the record, a memo of gigabytes would have counted as nothing. So the
// record keeps the entries' bytes through both, nothing under the tree is
// stated by either, the janitor writes the state once the memo moved, and a
// boot's layout counts the memo from it.
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

// A COPY'S MEMO IS WRITTEN DOWN ONCE A TICK AT MOST. A state written is two
// syncs, the file and its directory, so the janitor writes a copy's state for
// its memo's moves on its TICK alone — however many passes kicks run between
// two ticks, and however often the memo moves — and the tick writes each copy
// whose memo moved once, the state then recording what the memo holds.
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

// A TICK WRITES A COPY'S STATE ONLY WHILE THE COPY IS DUE ONE, and reports a
// failed write only for a copy still due. The tick picks every copy whose memo
// moved since its state was written, then writes them one after another, two
// syncs each, holding no lock of theirs: a purge, a rebuild's promote or a swap
// may retire a copy meanwhile — and move its directory into trash/ under the
// write once nobody holds it — and a round may write its state. A copy no
// longer due is not written. A write that fails for one is nothing lost and
// nothing the next tick takes up, so it is not logged: code_cache_memo_unstated
// tells an operator the node tries again at its next tick. A copy still due
// whose write fails is logged once, and the next tick writes it.
//
// Mutations: log no failure of the tick's writes — the first row logs none;
// log every failure (no look at the copy once its write failed) — the second
// and third rows log one; excuse a failure only for a copy moved into trash/
// (judged by discarded alone) — the third row, whose copy a reader still
// holds, logs one; write every copy the tick picked (no look as each write
// begins) — the fourth and fifth rows write the other copy's state.
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

	// two is a world of two repositories whose copies' memos moved since their
	// states were written, its cache opened with a knob on the tick's state
	// writes: the first of them meets the OTHER repository's copy with meet —
	// what befalls that copy as the tick writes this one's state, whichever of
	// the two the tick picked first. It answers the world, the cache, the
	// repositories, and what the tick did: every repository whose state it
	// wrote, in order, and the one meet met.
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

// THE JANITOR LISTS EVERY COPY'S TREE ONCE A PROCESS, behind the serving, and
// makes the copy's record what its tree holds — reading the names of its
// cohorts, never their answers. A crash keeps the state a tick last wrote, so
// a boot may find in a tree a cohort no state records, placed after that
// state, which the listing measures and counts and opens the node's newest
// past; a cohort recorded whose directory is gone, which it forgets; and a
// tree of another layout — answers in the tree itself, as a build that kept
// the memo in one directory left them — which is no memo of this build's and
// goes whole, unread. What the listing records, the tick writes down.
//
// The memo's lock keeps the tree still while it is listed, but not the copy:
// a purge retires it and moves its directory into trash/ whole, with no memo
// lock taken, between the listing that names a cohort and the measure that
// opens it. That cohort is nothing to count and no failure, so nothing is
// logged — code_cache_memo_unlisted says the copy is listed again at the next
// pass, of a copy that is gone. A cohort still there that the measure cannot
// read is logged: here one replaced by a file, the one such failure a case
// can make whoever runs it. And a listing the pass's end cuts short — the
// engine stopping — failed at nothing: it logs nothing and leaves the copy to
// the next pass, which lists it.
//
// Mutations: pass over a cohort no state records (listMemo measuring nothing)
// — the first row's record lacks it; keep a recorded cohort the tree no longer
// holds — the second row's record keeps it; leave a tree of another layout's
// names be — the third row's tree is still there; log a cohort gone from
// under the measure (listMemo without its fs.ErrNotExist case) — the fourth
// row logs it; excuse every error of a cohort's measure — the fifth row logs
// nothing; log a measure the pass's end cut short (listMemo without its
// ended-context case) — the sixth row logs it.
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
				"pass unlisted", st)
		}
	})

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
}

// THE NODE KEEPS AT MOST MaxMemoCohorts COHORTS, whatever they hold. A cohort
// is opened only by placements, and the trim drops one once the memo holds its
// share, so a memo the disk keeps holds some seventeen; but what thins old
// cohorts without the memo filling — damaged answers removed, answers a read
// moves on — would leave ever more of them, each one more name every miss
// opens. So the janitor's tick drops the oldest while the node keeps more
// than the bound, though the memo is far within its share. Here each run
// places an answer in a cohort of its own and the answer before it is read
// damaged and removed, so the memo holds one answer across forty cohorts.
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

// A STATE WRITTEN BEHIND THE SERVING KEEPS THE COPY'S LAST USE. A boot reads a
// copy's last use from its state's modification time, and the janitor writes
// a copy's state at its tick once its memo moved — a trim that took the
// copy's oldest cohort among the moves, which is no use of the copy: so the
// state it writes carries the copy's last use, never the moment of the write,
// and a copy nobody read since comes back from a restart no more recently
// used than it was — not the one read last, which the budget would keep while
// it evicted the copies seats do read.
//
// Mutation: write the state as it is staged (writeState's stamp dropped) — the
// copy comes back from the restart used at the moment the trim's state was
// written.
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

// A RUN'S EXTENSION OF A PAGE IS THE PAGE'S. What a run appends to a page it
// staged before its end (MemoBatch.Extend) — a diff stat's page's files'
// counts, once the stat has counted past it — is part of the entry the tail
// completes: read back whole, the page, the extension and the tail in that
// order, counted in the memo's record as one entry; and an extension that,
// with the tail, would pass what a run may append to a page gives that page
// back unmemoized, its staged bytes with it.
//
// Mutation: append the extension and leave it out of the entry's digest
// (Extend's sum left alone) — the entry fails its digest and is no answer.
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
