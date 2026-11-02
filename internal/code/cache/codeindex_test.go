package cache_test

import (
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
)

// blobReads records how many blobs each build of a code index read through
// its cat-file.
type blobReads struct {
	mu    sync.Mutex
	reads []int
}

func (r *blobReads) add(n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads = append(r.reads, n)
}

func (r *blobReads) all() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.reads)
}

// indexOf builds s's code index and fails t unless the build answered want.
func indexOf(t *testing.T, c *cache.Cache, s cache.Synced, want cache.IndexOutcome) {
	t.Helper()
	got, err := c.Index(t.Context(), s)
	if err != nil || got != want {
		t.Fatalf("Index = (%q, %v), want %q", got, err, want)
	}
}

// tipPaths is every path the code index of s's copy names, each with
// whether the index skips its blob.
func tipPaths(t *testing.T, c *cache.Cache, s cache.Synced) map[string]bool {
	t.Helper()
	view, ok := c.CodeIndex(s)
	if !ok {
		t.Fatal("the copy keeps no code index")
	}
	out := map[string]bool{}
	for i := range view.Map.Len() {
		out[string(view.Map.Path(i))] = view.Map.Skipped(i)
	}
	return out
}

// A SWAP LINKS THE CODE INDEX (S18). A compaction keeps every object, and the
// index is keyed by blob, so the incarnation a compaction's swap promotes
// holds the old copy's index files as SECOND NAMES of the same inodes, its
// record carried — and the indexer, looking at the same tip, reads no blob:
// a compaction never costs a re-index.
//
// Mutation: drop the index on pack-retired (derived.go's declaration,
// OnPackRetired: Drop) — the next incarnation holds no index, and the
// indexer reads every blob again.
func TestASwapLinksTheCodeIndex(t *testing.T) {
	t.Parallel()
	var reads blobReads
	k := newCompactable(t, false, cache.IndexReadingWith(reads.add))
	indexOf(t, k.c, k.held, cache.IndexBuilt)
	tip, before, ok := cache.CodeIndexFiles(k.c, k.repo)
	if !ok || len(before) < 2 {
		t.Fatalf("the copy keeps the code index %v (%t): the case needs a tip map and a segment", before, ok)
	}
	held := cache.CodeIndexBytes(k.c)
	k.compact(t)
	s := k.w.sync(t, k.c, k.repo)
	if s.Dir == k.held.Dir {
		t.Fatalf("the compaction answered the copy a reader holds, %s: no swap", s.Dir)
	}
	gotTip, after, ok := cache.CodeIndexFiles(k.c, k.repo)
	if !ok || gotTip != tip || len(after) != len(before) {
		t.Fatalf("the next incarnation keeps the code index %v at %s (%t), want the old copy's %v at %s",
			after, gotTip, ok, before, tip)
	}
	for i := range before {
		if !strings.HasPrefix(after[i], s.Dir) {
			t.Errorf("the next incarnation's index file %s is not in it (%s)", after[i], s.Dir)
		}
		if !sameInode(t, before[i], after[i]) {
			t.Errorf("the next incarnation's %s is not the old copy's %s, linked", after[i], before[i])
		}
	}
	if got := cache.CodeIndexBytes(k.c); got != held {
		t.Errorf("the node counts its code indexes at %d bytes after the swap, want the %d it held once", got,
			held)
	}
	indexOf(t, k.c, s, cache.IndexCurrent)
	if got := reads.all(); !slices.Equal(got, []int{3}) {
		t.Errorf("the builds read %v blobs, want the first build's 3 and nothing after the swap", got)
	}
}

// A NEW TIP INDEXES ONLY ITS NEW BLOBS. Every blob a segment holds is known,
// so a build at a later tip reads through its cat-file only the blobs that
// tip brought — whatever the branch holds besides — and the tip map names
// every file of the new tip.
//
// Mutation: treat every blob of the tip as unknown (codeindex.go's known) —
// the second build reads all three.
func TestANewTipIndexesOnlyItsNewBlobs(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	var reads blobReads
	c := w.open(t, cache.IndexReadingWith(reads.add))
	indexOf(t, c, w.sync(t, c, repo), cache.IndexBuilt)
	head, _ := w.push(t, repo, "refs/heads/main", map[string]string{"c.go": "package c\n"})
	s := w.sync(t, c, repo)
	indexOf(t, c, s, cache.IndexBuilt)
	if got := reads.all(); !slices.Equal(got, []int{2, 1}) {
		t.Errorf("the builds read %v blobs, want 2 at the first tip and only the new tip's one", got)
	}
	if tip, _, _ := cache.CodeIndexFiles(c, repo); tip != head {
		t.Errorf("the index answers for %s, want the new tip %s", tip, head)
	}
	paths := tipPaths(t, c, s)
	if want := []string{"a.go", "b.go", "c.go"}; !slices.Equal(slices.Sorted(maps.Keys(paths)), want) {
		t.Errorf("the tip map names %v, want %v", paths, want)
	}
	indexOf(t, c, s, cache.IndexCurrent)
}

// AN INDEX BUILD MERGES AS IT WRITES, never once at its end
// (codeindex.MergeNewest). Four files of disjoint text at a segment a file
// write four equal segments; merged as each is written they settle as one
// of three and the fourth — the fourth is less than half the three, so it
// stays — where one merge pass after the fourth folds all four into one
// from the end, rewriting what it merged at every step: k²/2 segments'
// writes for k, which a repository of a gigabyte at 64 MiB a segment
// paid nineteen times over.
//
// Mutation: merge only after the last segment (flush returns nil rather
// than merging) — the index holds one segment.
func TestAnIndexBuildMergesAsItWritesNotOnceAtTheEnd(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	files := map[string]string{}
	for i, word := range []string{"alpha", "bravo", "charlie", "delta"} {
		files[word+".txt"] = strings.Repeat(word+" "+strings.Repeat(string(rune('k'+i)), 3)+"\n", 8)
	}
	w.push(t, repo, "refs/heads/main", files)
	c := w.open(t, cache.IndexSegmentBytesWith(1))
	s := w.sync(t, c, repo)
	indexOf(t, c, s, cache.IndexBuilt)
	view, ok := c.CodeIndex(s)
	if !ok {
		t.Fatal("the built index cannot be read")
	}
	if got := len(view.Segments); got != 2 {
		t.Fatalf("the index holds %d segments, want 2: three merged as they were written, and the fourth", got)
	}
	if a, b := view.Segments[0].Len(), view.Segments[1].Len(); a != 3 || b != 1 {
		t.Errorf("the segments hold %d and %d blobs, want 3 and 1", a, b)
	}
}

// THE INDEX SKIPS BINARIES AND LARGE FILES (M6-D12), by the one rule git grep
// -I keeps too: a file past codeindex.MaxFileBytes is never read — its size,
// from ls-tree -l, is enough — and a binary one is read and recorded skipped,
// so neither is searched and neither is read again at a later tip.
//
// Mutation: read every blob whatever its size (codeindex.go's segments) — the
// build reads three; or record no skip for a binary — the map says it is
// searched.
func TestTheIndexSkipsBinariesAndLargeFiles(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", map[string]string{
		"text.go":  "package text\n",
		"blob.bin": "\x00\x01\x02 not text\n",
		"big.txt":  strings.Repeat("a line of a large file\n", (1<<20)/20),
	})
	var reads blobReads
	c := w.open(t, cache.IndexReadingWith(reads.add))
	s := w.sync(t, c, repo)
	indexOf(t, c, s, cache.IndexBuilt)
	if got := reads.all(); !slices.Equal(got, []int{2}) {
		t.Errorf("the build read %v blobs, want the text and the binary, never the file past 1 MiB", got)
	}
	want := map[string]bool{"text.go": false, "blob.bin": true, "big.txt": true}
	if got := tipPaths(t, c, s); !maps.Equal(got, want) {
		t.Errorf("the tip map's skips are %v, want %v", got, want)
	}
}

// THE SECOND HOLDER FILLS ITS COPY (M6-D12). A `search: indexed`
// repository's index is kept by the first code.IndexHolders of its git order,
// and a holder that holds no copy fills one for it — deliberately, beside the
// read path's rule that only the home fills — while a node past the holders
// fills nothing, and a holder the order no longer names drops what it kept.
//
// Mutation: hold to the first node of the order alone (codeindex.go's
// indexHeld) — the second holder keeps no index; or skip the drop — the node
// the order moved past keeps its index.
func TestTheSecondHolderFillsItsCopy(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	head, _ := w.push(t, repo, "refs/heads/main", map[string]string{"a.go": "package a\n"})
	var mu sync.Mutex
	order := []string{"git-1", "git-2", "git-3"}
	placed := func(node string) func(*cache.Options) {
		return func(o *cache.Options) {
			o.Placement = &cache.Placement{Node: node, Order: func(id string) ([]string, error) {
				if id != repo {
					return nil, errors.New("no such repository")
				}
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(order), nil
			}}
		}
	}
	// One directory, booted as each node in turn: the third first, so the
	// second starts from what the third left, which is nothing.
	third := w.open(t, placed("git-3"))
	cache.IndexPass(t.Context(), third)
	if _, _, ok := cache.CodeIndexFiles(third, repo); ok {
		t.Error("a node past the first two of the order keeps an index")
	}
	if third.Holds(repo) {
		t.Error("a node past the first two of the order filled a copy for an index it does not keep")
	}
	second := w.reopen(t, third, placed("git-2"))
	if second.Holds(repo) {
		t.Fatal("the second holder boots holding a copy: the case needs one it fills")
	}
	cache.IndexPass(t.Context(), second)
	if tip, _, ok := cache.CodeIndexFiles(second, repo); !ok || tip != head {
		t.Errorf("the second holder keeps the index at %s (%t), want it filled and built at %s", tip, ok, head)
	}

	mu.Lock()
	order = []string{"git-1", "git-3", "git-2"}
	mu.Unlock()
	cache.IndexPass(t.Context(), second)
	if _, _, ok := cache.CodeIndexFiles(second, repo); ok {
		t.Error("a node the order moved past the holders still keeps its index")
	}
	if got := cache.CodeIndexBytes(second); got != 0 {
		t.Errorf("the node counts %d bytes of code indexes after it dropped its one", got)
	}
}

// THE CODE INDEXES ARE TRIMMED BEFORE A REPOSITORY. What the node's indexes
// hold past their share of the budget (cache.CodeIndexShare) is theirs to
// give back, whole and least recently used first: one dropped is built again
// from one tip's blobs, where a repository evicted is a cold sync of every
// pack.
//
// Mutation: trim nothing (codeindex.go's trimCodeIndexes answers at once) —
// the index stays past its share.
func TestTheCodeIndexesAreTrimmedToTheirShare(t *testing.T) {
	t.Parallel()
	roomy := newSyncWorld(t, onMemory)
	repo := roomy.repo(t, 1)
	roomy.push(t, repo, "refs/heads/main", map[string]string{"a.go": "package a\n"})
	c := roomy.open(t)
	indexOf(t, c, roomy.sync(t, c, repo), cache.IndexBuilt)
	if cache.TrimCodeIndexes(t.Context(), c) {
		t.Fatal("the trim dropped an index well within its share")
	}
	// A budget whose share is 64 bytes: past it, any index is — a tip map's
	// trailer alone is 32.
	w := newSyncWorld(t, onMemory)
	repo = w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", map[string]string{"a.go": "package a\n"})
	small := w.open(t, func(o *cache.Options) { o.MaxBytes = 64 * cache.CodeIndexShare })
	indexOf(t, small, w.sync(t, small, repo), cache.IndexBuilt)
	if got := cache.CodeIndexBytes(small); got <= 64 {
		t.Fatalf("the index holds %d bytes: the case needs one past its 64-byte share", got)
	}
	if !cache.TrimCodeIndexes(t.Context(), small) {
		t.Fatal("the trim dropped nothing of an index past its share")
	}
	if _, _, ok := cache.CodeIndexFiles(small, repo); ok || cache.CodeIndexBytes(small) != 0 {
		t.Errorf("the copy still keeps its index (%t), counted at %d bytes", ok, cache.CodeIndexBytes(small))
	}
}

// ONLY THE FIRST OF TWO HOLDERS SENDS A SEARCH ON. A search of the default
// branch that a holder's index does not answer for yet may be sent on to the
// next node (code.UnreadyIndexBuilding) only by the FIRST of the
// repository's index holders, and only where the order names a second: the
// second holder has no holder after it, and the only holder of a one-node
// fleet none at all, so either refusing would leave the search unanswered
// until its indexer caught up. A node past the holders is no holder.
//
// Mutation: let every holder pass a search on (indexHolder's holders[0]) —
// the second holder passes; let the only holder pass (indexHolder's
// len(holders) > 1) — the one-node fleet's holder passes.
func TestOnlyTheFirstOfTwoHoldersSendsASearchOn(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", map[string]string{"a.go": "package a\n"})
	var mu sync.Mutex
	var order []string
	c := w.open(t, func(o *cache.Options) {
		o.Placement = &cache.Placement{Node: "git-1", Order: func(string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(order), nil
		}}
	})
	s := w.sync(t, c, repo)
	for _, tc := range []struct {
		order            []string
		holder, passesOn bool
	}{
		{[]string{"git-1", "git-2", "git-3"}, true, true},
		{[]string{"git-2", "git-1", "git-3"}, true, false},
		{[]string{"git-1"}, true, false},
		{[]string{"git-2", "git-3", "git-1"}, false, false},
	} {
		mu.Lock()
		order = tc.order
		mu.Unlock()
		got, _ := c.IndexTip(s)
		if got.Holder != tc.holder || got.PassesOn != tc.passesOn {
			t.Errorf("git-1 in the order %v is a holder %t and passes a search on %t, want %t and %t", tc.order,
				got.Holder, got.PassesOn, tc.holder, tc.passesOn)
		}
	}
}
