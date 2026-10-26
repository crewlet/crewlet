package cache_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
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
	entry := func(k cache.MemoKey) string { return filepath.Join(cache.MemoTree(s.Dir), k.String()) }
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

// THE MEMO IS TRIMMED TO ITS SHARE BEFORE ANY REPOSITORY GOES. Over the budget
// with the memo past a sixteenth of it, the janitor takes the memo's entries
// — the least recently used first — until it holds its share, and evicts no
// repository the trim brought the cache under the budget for: an answer costs
// one git process to compute again, a repository a cold sync of every pack.
//
// Neither copy is pinned when the janitor passes, so either is one an eviction
// may take: only the order of the pass keeps them.
//
// Mutation: evict to the budget before trimming the memo (janitorPass) — the
// repository used least recently is evicted.
func TestTheMemoIsTrimmedToItsShareBeforeAnyRepository(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	old, hot := w.repo(t, 1), w.repo(t, 2)
	w.push(t, old, "refs/heads/main", nil)
	w.push(t, hot, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, old).Release()
	clk.Add(time.Minute)
	s := w.sync(t, c, hot)
	keys := memoKeys("trim", 64)
	memoize(t, c, s, keys)
	for _, k := range keys {
		clk.Add(time.Second)
		if _, ok := c.MemoGet(s, k); !ok {
			t.Fatalf("entry %s was not memoized", k)
		}
	}
	memo := cache.MemoBytes(c)
	if memo != memoEntries(len(keys)) {
		t.Fatalf("the memo counts %d bytes, want its entries' %d", memo, memoEntries(len(keys)))
	}
	s.Release()
	// Just past the budget, so trimming the memo to its share brings the
	// cache under it and no eviction is due.
	budget := cache.Total(c) - 1
	cache.SetMaxBytes(c, budget)
	cache.JanitorPass(c)

	repos := cache.Repos(c)
	for _, repo := range []string{old, hot} {
		if _, ok := repos[repo]; !ok {
			t.Errorf("repository %s was evicted while the memo held more than its share", repo)
		}
	}
	if got, share := cache.MemoBytes(c), budget/cache.MemoShare; got > share {
		t.Errorf("after the janitor's pass the memo holds %d bytes, past its share %d", got, share)
	}
	// What it kept is what was used last: the survivors are a run of the
	// newest uses.
	tree := cache.MemoTree(s.Dir)
	kept := -1
	for i, k := range keys {
		on := statOK(t, filepath.Join(tree, k.String()))
		switch {
		case on && kept < 0:
			kept = i
		case !on && kept >= 0:
			t.Errorf("entry %d was trimmed and an entry used before it (%d) was kept", i, kept)
		}
	}
	if kept <= 0 {
		t.Errorf("the trim kept entries from %d on, want it to have taken the oldest uses", kept)
	}
}

// A COLD INCARNATION'S MEMO GOES WHOLE. A copy whose memo this process has
// neither read nor written since it opened — nobody asked of it since the
// boot — is trimmed by moving its tree into trash/ in one rename, before an
// entry of a memo this process uses is touched.
//
// Mutation: trim entries before the cold memos (skip trimColdMemos) — the
// warm copy's entries are taken.
func TestAColdIncarnationsMemoGoesWhole(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	cold, warm := w.repo(t, 1), w.repo(t, 2)
	w.push(t, cold, "refs/heads/main", nil)
	w.push(t, warm, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	sc := w.sync(t, c, cold)
	coldDir := sc.Dir
	memoize(t, c, sc, memoKeys("cold", 32))
	cache.JanitorPass(c) // the copy's state records its memo
	sc.Release()

	c = w.reopen(t, c, clk.on)
	clk.Add(time.Minute)
	sw := w.sync(t, c, warm)
	keys := memoKeys("warm", 8)
	memoize(t, c, sw, keys)
	if st, ok := cache.MemoOf(c, cold); !ok || st.Indexed || st.Bytes != memoEntries(32) {
		t.Fatalf("the cold copy's memo after the restart is %+v (held %t), want its state's %d bytes, untouched",
			st, ok, memoEntries(32))
	}
	// A share the warm entries fit and both memos together do not.
	cache.SetMaxBytes(c, 16*(memoEntries(len(keys))+memoEntries(32)/2))
	cache.JanitorPass(c)

	if statOK(t, cache.MemoTree(coldDir)) {
		t.Error("the cold copy's memo tree is still in its incarnation after a trim past the share")
	}
	if st, _ := cache.MemoOf(c, cold); st.Recorded {
		t.Errorf("the cold copy still records a memo of %d bytes", st.Bytes)
	}
	for _, k := range keys {
		if _, ok := c.MemoGet(sw, k); !ok {
			t.Errorf("the warm copy's entry %s was trimmed while a cold memo could go whole", k)
		}
	}
}

// A REBUILD KEEPS THE MEMO. A rebuild repairs packs, not answers: every entry
// of the copy it replaces is linked into the new one — the same file — with
// what the old copy recorded of it, and is answered there as it was.
//
// Mutation: seed nothing (seedMemo returns at once) — the rebuilt copy answers
// none.
func TestARebuildKeepsTheMemo(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s, err := c.Sync(t.Context(), repo, linearizable)
	if err != nil {
		t.Fatal(err)
	}
	keys := memoKeys("rebuild", 3)
	memoize(t, c, s, keys)
	before, _ := cache.MemoOf(c, repo)
	oldEntry := filepath.Join(cache.MemoTree(s.Dir), keys[0].String())
	oldInfo, err := os.Stat(oldEntry)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := cache.RebuildWhole(t.Context(), c, s, cache.Failure{Reason: cache.RebuildCorrupt,
		Began: time.Now()})
	if err != nil {
		s.Release()
		t.Fatalf("the rebuild: %v", err)
	}
	defer rebuilt.Release()
	newInfo, err := os.Stat(filepath.Join(cache.MemoTree(rebuilt.Dir), keys[0].String()))
	switch {
	case err != nil:
		t.Errorf("the rebuilt copy holds no entry of the copy it replaced: %v", err)
	case !os.SameFile(oldInfo, newInfo):
		t.Error("the rebuilt copy's entry is another file than the replaced copy's: a rebuild links it")
	}
	s.Release()
	for i, k := range keys {
		if got, ok := c.MemoGet(rebuilt, k); !ok || string(got) != string(memoBody(i))+memoTail {
			t.Errorf("the rebuilt copy answers entry %d %q (%t), want the replaced copy's", i, got, ok)
		}
	}
	if after, _ := cache.MemoOf(c, repo); after.Bytes != before.Bytes {
		t.Errorf("the rebuilt copy's memo counts %d bytes, want the replaced copy's %d", after.Bytes, before.Bytes)
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
// the round that dropped it has not yet swept the old history's packs, the
// copy's memo is shut: no batch begins, since a read then may meet objects of
// either history. Once the round has swept them, answers are memoized again.
//
// Mutations: let Place ignore the generation — the batch from before the drop
// places its answer; let Memo ignore the shut memo — the round's own window
// begins a batch.
func TestNothingReadOverAGoneHistoryIsMemoizedAfterItsDrop(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	var (
		cc     atomic.Pointer[cache.Cache]
		held   atomic.Pointer[cache.Synced]
		gone   atomic.Bool
		mu     sync.Mutex
		during []bool // whether a batch began in the round's window
	)
	c := w.open(t, func(o *cache.Options) {
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
	w.sync(t, c, repo)
	earlier := w.log.state()
	w.push(t, repo, "refs/heads/main", nil)
	s := w.sync(t, c, repo)
	held.Store(&s)

	begun := c.Memo(s)
	stale := memoKeys("stale", 1)[0]
	begun.Put(stale, memoBody(0))
	w.log.recreate(earlier)
	gone.Store(true)
	w.sync(t, c, repo)
	gone.Store(false)
	begun.Place(func(int) []byte { return []byte(memoTail) })

	mu.Lock()
	if len(during) == 0 {
		t.Error("no full round read the gone history's copy whole")
	}
	for i, began := range during {
		if began {
			t.Errorf("a batch began as round %d tidied, before it had swept the old history's packs", i)
		}
	}
	mu.Unlock()
	if _, ok := c.MemoGet(s, stale); ok {
		t.Error("an answer a run cut before the history went was memoized after its drop")
	}
	fresh := memoKeys("fresh", 1)
	memoize(t, c, s, fresh)
	if _, ok := c.MemoGet(s, fresh[0]); !ok {
		t.Error("the copy memoized nothing once its round had swept the old history")
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
	cache.JanitorPass(c)

	if st, _ := cache.MemoOf(c, repo); st.Bytes != want || st.Stated != want {
		t.Errorf("after a maintenance run and a round's index the memo records %d bytes (its state %d), want its "+
			"entries' %d", st.Bytes, st.Stated, want)
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
