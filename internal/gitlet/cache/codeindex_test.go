package cache_test

import (
	"context"
	"errors"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet/cache"
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
	view, ok := c.CodeIndex(t.Context(), s)
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
	view, ok := c.CodeIndex(t.Context(), s)
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

// A BUILD READS WHAT IT PLACED THROUGH MAPPINGS IT GIVES BACK: an index built
// as eight one-file segments, merged as each is written, opens every segment
// it placed over a mapping of the placed file — never the bytes it wrote,
// held in the heap until it committed — and holds a mapping only of the
// segments it still reads: a merge's two inputs go as it is placed, so the
// build holds no more mappings than segments, and none once it is over.
//
// Mutations: keep a merge's inputs mapped until the build ends, and the build
// holds a mapping a segment it ever placed, red; drop nothing at the build's
// end, and its mappings outlive it, red.
func TestABuildReadsWhatItPlacedThroughMappingsItGivesBack(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	files := map[string]string{}
	for i := range 8 {
		word := string(rune('a' + i))
		files[word+".txt"] = strings.Repeat(word+" "+strings.Repeat(word, 3)+"\n", 8)
	}
	w.push(t, repo, "refs/heads/main", files)
	var mu sync.Mutex
	var placed []int
	c := w.open(t, cache.IndexSegmentBytesWith(1), cache.IndexPlacedWith(func(mapped int) {
		mu.Lock()
		defer mu.Unlock()
		placed = append(placed, mapped)
	}))
	s := w.sync(t, c, repo)
	before, _ := cache.MappedRuns(c)
	indexOf(t, c, s, cache.IndexBuilt)
	after, _ := cache.MappedRuns(c)
	mu.Lock()
	defer mu.Unlock()
	if len(placed) != len(files) {
		t.Fatalf("the build placed %d segments, want one a file: %d", len(placed), len(files))
	}
	// Merged as each is written, eight equal segments keep at most
	// 2·log2(8) = 6, and the build maps no more of them than that.
	for i, n := range placed {
		if n-before > 6 {
			t.Errorf("after its segment %d the build held %d mappings, past the 6 segments it keeps", i+1,
				n-before)
		}
	}
	if slices.Max(placed) == before {
		t.Error("the build mapped none of the segments it placed: it read them from somewhere else")
	}
	if after != before {
		t.Errorf("the node holds %d mappings after the build and held %d before it", after, before)
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
// repository's index is kept by the first gitlet.IndexHolders of its git order,
// and a holder that holds no copy fills one for it — deliberately, beside the
// read path's rule that only the home fills — while a node past the holders
// fills nothing, and a holder the order no longer names drops what it kept
// once the order has stood cache.IndexPlacementGrace.
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
	clk := newClock()
	placed := func(node string) func(*cache.Options) {
		return func(o *cache.Options) {
			clk.on(o)
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
	if !keeps(second, repo) {
		t.Error("a node the order moved past the holders dropped its index at once, within the grace")
	}
	clk.Add(cache.IndexPlacementGrace)
	cache.IndexPass(t.Context(), second)
	if keeps(second, repo) {
		t.Error("a node the order moved past the holders for the grace still keeps its index")
	}
	if got := cache.CodeIndexBytes(second); got != 0 {
		t.Errorf("the node counts %d bytes of code indexes after it dropped its one", got)
	}
}

// A HOLDER BACK WITHIN THE GRACE MOVES NO INDEX. The order is over the live
// Gitlet nodes, and a restarting node leaves it for the length of its restart,
// which made the next node of each of its repositories' orders a holder that
// filled a copy and built a whole index — dropped again the moment the node
// was back. A node that becomes a holder fills and builds nothing until its
// place has stood cache.IndexPlacementGrace, and a place that changes back
// meanwhile starts the grace again; one that stands builds.
//
// Mutation: act on a holder's place at once (codeindex.go's settled answering
// true for every holder) — the third node fills while the second restarts; or
// keep the grace's start when the place changes back (settled keeping since)
// — the second absence builds at once.
func TestAHolderBackWithinTheGraceMovesNoIndex(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	head, _ := w.push(t, repo, "refs/heads/main", map[string]string{"a.go": "package a\n"})
	var mu sync.Mutex
	order := []string{"git-1", "git-2", "git-3"}
	clk := newClock()
	third := w.open(t, clk.on, func(o *cache.Options) {
		o.Placement = &cache.Placement{Node: "git-3", Order: func(string) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			return slices.Clone(order), nil
		}}
	})
	look := func(live ...string) {
		t.Helper()
		mu.Lock()
		order = live
		mu.Unlock()
		cache.IndexPass(t.Context(), third)
	}
	look("git-1", "git-2", "git-3")
	look("git-1", "git-3") // git-2 restarting
	clk.Add(cache.IndexPlacementGrace / 2)
	look("git-1", "git-3")
	if third.Holds(repo) || keeps(third, repo) {
		t.Fatalf("the third node fills a copy (%t) or keeps an index (%t) while the second restarts within the "+
			"grace", third.Holds(repo), keeps(third, repo))
	}
	look("git-1", "git-2", "git-3") // back
	look("git-1", "git-3")          // and gone again
	clk.Add(cache.IndexPlacementGrace / 2)
	look("git-1", "git-3")
	if third.Holds(repo) || keeps(third, repo) {
		t.Fatalf("the third node fills a copy (%t) or keeps an index (%t) a half grace into the second's second "+
			"absence", third.Holds(repo), keeps(third, repo))
	}
	clk.Add(cache.IndexPlacementGrace / 2)
	look("git-1", "git-3")
	if tip, _, ok := cache.CodeIndexFiles(third, repo); !ok || tip != head {
		t.Errorf("the third node keeps the index at %s (%t) once the second has been gone the grace, want it "+
			"built at %s", tip, ok, head)
	}
}

// THE CODE INDEXES ARE TRIMMED BEFORE A REPOSITORY. What the node's indexes
// hold past their share of the budget (cache.CodeIndexShare) is theirs to
// give back, whole and searched least recently first
// (TestTheTrimKeepsWhatIsSearchedAndTakesNothingStraightBack): one dropped is
// built again from one tip's blobs, where a repository evicted is a cold sync
// of every pack.
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
// next node (gitlet.UnreadyIndexBuilding) only by the FIRST of the
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

// fourSegmentFiles is four files of disjoint text: at a segment a file
// (cache.IndexSegmentBytesWith(1)) a build writes four segments and merges
// the first three as it goes (TestAnIndexBuildMergesAsItWritesNotOnceAtTheEnd).
func fourSegmentFiles() map[string]string {
	files := map[string]string{}
	for i, word := range []string{"alpha", "bravo", "charlie", "delta"} {
		files[word+".txt"] = strings.Repeat(word+" "+strings.Repeat(string(rune('k'+i)), 3)+"\n", 8)
	}
	return files
}

// AN INDEX BUILD SYNCS ITS DIRECTORY ONCE. Nothing names a file a build
// places until its commit stores the record, and every staged file is synced
// as it is written, so the index's directory is synced once, before the
// commit — never after each placement, which cost a first build of many
// segments a second fsync a file. A build of four segments, merged twice,
// and a tip map places seven files and syncs the directory once.
//
// Mutation: sync the directory after each placement (codeindex.go's place,
// b.c.durableDir of the index's directory after the rename) — seven syncs.
func TestAnIndexBuildSyncsItsDirectoryOnce(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", fourSegmentFiles())
	var (
		mu     sync.Mutex
		synced []string
	)
	c := w.open(t, cache.IndexSegmentBytesWith(1), cache.SyncingWith(func(dir string) {
		if filepath.Base(dir) == "codeindex" {
			mu.Lock()
			defer mu.Unlock()
			synced = append(synced, dir)
		}
	}))
	indexOf(t, c, w.sync(t, c, repo), cache.IndexBuilt)
	_, paths, ok := cache.CodeIndexFiles(c, repo)
	if !ok {
		t.Fatal("the build stored no index")
	}
	mu.Lock()
	defer mu.Unlock()
	if want := []string{filepath.Dir(paths[0])}; !slices.Equal(synced, want) {
		t.Errorf("the build synced the index's directory %d times (%v), want once (%v)", len(synced), synced,
			want)
	}
}

// holding is a Placement that makes the cache git-1 and the first holder of
// every repository: what a case whose indexer it drives needs.
func holding(o *cache.Options) {
	o.Placement = &cache.Placement{Node: "git-1", Order: func(string) ([]string, error) {
		return []string{"git-1", "git-2"}, nil
	}}
}

// indexHashes counts the code index files a cache held to their trailers, by path.
type indexHashes struct {
	mu   sync.Mutex
	seen map[string]int
	// held, when set, is closed by the next hash, which then waits for
	// release.
	held, release chan struct{}
}

// on is an Options mutation that counts every code index file a check of
// the cache hashes.
func (h *indexHashes) on(o *cache.Options) {
	cache.WhileHashing(o, func(path string) {
		if filepath.Base(filepath.Dir(path)) != "codeindex" {
			return
		}
		h.mu.Lock()
		if h.seen == nil {
			h.seen = map[string]int{}
		}
		h.seen[path]++
		held, release := h.held, h.release
		h.held = nil
		h.mu.Unlock()
		if held != nil {
			close(held)
			<-release
		}
	})
}

func (h *indexHashes) counts() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return maps.Clone(h.seen)
}

// A SEARCH NEVER HASHES A CODE INDEX. After a restart nothing in the process
// has held an index's files to their trailers, and holding them is a cold
// read of every byte of the index: a search that finds it so reads the tip
// with git grep — however many search at once — and the indexer's next look
// verifies it, each file once, off every request's path, while a search that
// asks meanwhile greps rather than wait for it; the searches after it read
// the index and hash nothing.
//
// Mutation: let a search verify (codeindex.go's CodeIndex asks codeIndex with
// verify) — the searches hash the files; verify nothing in the indexer
// (Index's codeIndexVouched check skipped) — the index stays unopened; or let
// a search wait for the indexer's verifying opening (codeIndex's verifying
// answer skipped) — it waits out its context.
func TestASearchNeverHashesACodeIndex(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", map[string]string{"a.go": "package a\n", "b.go": "package b\n"})
	first := w.open(t, holding)
	cache.IndexPass(t.Context(), first)
	_, paths, ok := cache.CodeIndexFiles(first, repo)
	if !ok {
		t.Fatal("the indexer built no index")
	}
	var h indexHashes
	c := w.reopen(t, first, holding, h.on)
	s := w.sync(t, c, repo)
	if _, after, ok := cache.CodeIndexFiles(c, repo); !ok || !slices.Equal(after, paths) {
		t.Fatalf("the restarted copy keeps the index %v (%t), want the one built, %v", after, ok, paths)
	}
	searches := func() []bool {
		var wg sync.WaitGroup
		got := make([]bool, 4)
		for i := range got {
			wg.Go(func() { _, got[i] = c.CodeIndex(t.Context(), s) })
		}
		wg.Wait()
		return got
	}
	if got := searches(); slices.Contains(got, true) {
		t.Errorf("searches of an index nothing has verified read it (%v), want each to grep", got)
	}
	if got := h.counts(); len(got) != 0 {
		t.Errorf("the searches hashed the index's files %v, want none", got)
	}
	// The indexer's look, held in its first hash while a search asks: the
	// search greps at once rather than wait for the verification.
	held, release := make(chan struct{}), make(chan struct{})
	h.mu.Lock()
	h.held, h.release = held, release
	h.mu.Unlock()
	looked := make(chan struct{})
	go func() {
		defer close(looked)
		cache.IndexPass(t.Context(), c)
	}()
	select {
	case <-held:
	case <-looked:
		t.Fatalf("the indexer's look held no file of the index to its trailer (the searches hashed %v): "+
			"nothing verified it there", h.counts())
	}
	waiting, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	_, read := c.CodeIndex(waiting, s)
	waited := waiting.Err() != nil
	cancel()
	close(release)
	<-looked
	if read || waited {
		t.Errorf("a search during the indexer's verification read the index (%t) or waited for it (%t), want "+
			"it to grep at once", read, waited)
	}
	want := map[string]int{}
	for _, p := range paths {
		want[p] = 1
	}
	if got := h.counts(); !maps.Equal(got, want) {
		t.Errorf("the indexer's look hashed %v, want each of the index's files once: %v", got, want)
	}
	if got := searches(); slices.Contains(got, false) {
		t.Errorf("searches after the indexer verified the index answer %v, want each to read it", got)
	}
	if got := h.counts(); !maps.Equal(got, want) {
		t.Errorf("the searches after the verification hashed %v, want nothing more than %v", got, want)
	}
}

// A LOOK AT UNMOVED REFS READS NO TIP. The indexer looks at every repository
// it keeps an index of every minute, and reading a tip is a git process; only
// a round that writes the copy's refs can move the tip, so a look that finds
// the refs the index's tip was read under reads nothing — and one after any
// ref moved, the default branch's or another, reads the tip again.
//
// Mutation: read the tip whatever the refs (Index's unmoved answer skipped)
// — the looks at unmoved refs read it.
func TestALookAtUnmovedRefsReadsNoTip(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", map[string]string{"a.go": "package a\n"})
	var (
		mu    sync.Mutex
		reads int
	)
	c := w.open(t, cache.IndexTipReadingWith(func(string) {
		mu.Lock()
		defer mu.Unlock()
		reads++
	}))
	read := func(want int, after string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if reads != want {
			t.Errorf("the indexer read the tip %d times after %s, want %d", reads, after, want)
		}
	}
	indexOf(t, c, w.sync(t, c, repo), cache.IndexBuilt)
	read(1, "its first build")
	indexOf(t, c, w.sync(t, c, repo), cache.IndexCurrent)
	indexOf(t, c, w.sync(t, c, repo), cache.IndexCurrent)
	read(1, "two looks at refs nothing moved")
	w.push(t, repo, "refs/heads/topic", map[string]string{"b.go": "package b\n"})
	indexOf(t, c, w.sync(t, c, repo), cache.IndexCurrent)
	indexOf(t, c, w.sync(t, c, repo), cache.IndexCurrent)
	read(2, "a push to another branch and a look after it")
	w.push(t, repo, "refs/heads/main", map[string]string{"c.go": "package c\n"})
	indexOf(t, c, w.sync(t, c, repo), cache.IndexBuilt)
	read(3, "a push to the default branch")
}

// keeps reports whether repo's copy in c keeps a code index.
func keeps(c *cache.Cache, repo string) bool {
	_, _, ok := cache.CodeIndexFiles(c, repo)
	return ok
}

// THE TRIM KEEPS WHAT IS SEARCHED, AND TAKES NOTHING STRAIGHT BACK. Past their
// share, the node's code indexes lose the index of the repository SEARCHED
// least recently here — whatever its copy's readers did since — and the
// indexer builds a trimmed index again only into room for what it held, or
// for a repository searched more recently than an index it would displace:
// rebuilt at the next look, an index the trim took put the share over again
// and the trim took the next, every minute, for ever.
//
// Mutation: order the trim by the copy's last use alone (codeindex.go's
// trimCodeIndexes) — the searched index, read longer ago, goes; build a
// trimmed index again whatever the share (indexHeld's mayBuildIndex check
// skipped) — the trimmed one is rebuilt at once; or never let a search
// displace an index (mayBuildIndex answering false with no room) — the
// searched repository's index is not built back.
func TestTheTrimKeepsWhatIsSearchedAndTakesNothingStraightBack(t *testing.T) {
	t.Parallel()
	files := map[string]string{"a.go": "package a\n", "b.go": "package b\n"}
	probe := newSyncWorld(t, onMemory)
	one := probe.repo(t, 1)
	probe.push(t, one, "refs/heads/main", files)
	sized := probe.open(t)
	indexOf(t, sized, probe.sync(t, sized, one), cache.IndexBuilt)
	size := cache.CodeIndexBytes(sized)

	// A share that holds one index and a half: two are past it.
	w := newSyncWorld(t, onMemory)
	searchedRepo, quiet := w.repo(t, 1), w.repo(t, 2)
	w.push(t, searchedRepo, "refs/heads/main", files)
	w.push(t, quiet, "refs/heads/main", files)
	clk := newClock()
	c := w.open(t, clk.on, holding, func(o *cache.Options) { o.MaxBytes = cache.CodeIndexShare * (size + size/2) })
	s := w.sync(t, c, searchedRepo)
	indexOf(t, c, s, cache.IndexBuilt)
	c.IndexTip(s) // a search of it, here
	clk.Add(time.Minute)
	indexOf(t, c, w.sync(t, c, quiet), cache.IndexBuilt) // read after the search: its copy is the newer
	if got := cache.CodeIndexBytes(c); got != 2*size {
		t.Fatalf("the two indexes hold %d bytes, want %d each: the case needs two of the probe's size", got, size)
	}
	if !cache.TrimCodeIndexes(t.Context(), c) {
		t.Fatal("the trim dropped nothing of two indexes past their share")
	}
	if !keeps(c, searchedRepo) || keeps(c, quiet) {
		t.Fatalf("after the trim the searched repository keeps its index %t and the quiet one %t, want true and "+
			"false", keeps(c, searchedRepo), keeps(c, quiet))
	}

	clk.Add(time.Minute)
	cache.IndexPass(t.Context(), c)
	if keeps(c, quiet) {
		t.Error("the indexer built the trimmed index straight back, with no room for it in the share")
	}
	clk.Add(time.Minute)
	c.IndexTip(w.sync(t, c, quiet)) // a search of the trimmed one, newer than the other's
	cache.IndexPass(t.Context(), c)
	if !keeps(c, quiet) {
		t.Fatal("the indexer did not build back the index of a repository searched more recently than the " +
			"one it would displace")
	}
	cache.TrimCodeIndexes(t.Context(), c)
	if !keeps(c, quiet) || keeps(c, searchedRepo) {
		t.Errorf("after the second trim the newly searched repository keeps its index %t and the other %t, "+
			"want true and false", keeps(c, quiet), keeps(c, searchedRepo))
	}
}

// AN INDEXER'S LOOK IS NO USE OF THE COPY. The indexer looks at every copy it
// keeps an index of every minute; counted as a reader's use, that made each
// such copy look used within the minute to the budget's eviction, which then
// evicted the repositories people read first. A look leaves the copy's last
// use where its readers left it.
//
// Mutation: pin the indexer's sync as a use (sync.go's indexSync, use true)
// — the look moves the last use.
func TestAnIndexersLookIsNoUseOfTheCopy(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", map[string]string{"a.go": "package a\n"})
	clk := newClock()
	c := w.open(t, clk.on, holding)
	cache.IndexPass(t.Context(), c)
	if !keeps(c, repo) {
		t.Fatal("the indexer built no index")
	}
	before := cache.LastUsed(c, repo)
	clk.Add(time.Hour)
	cache.IndexPass(t.Context(), c)
	if got := cache.LastUsed(c, repo); !got.Equal(before) {
		t.Errorf("the indexer's look moved the copy's last use from %s to %s, want it where it was", before, got)
	}
}

// AN EVICTED COPY IS FILLED BACK ONLY INTO ROOM. A copy the indexer keeps an
// index in that the budget evicted is filled again only once the budget has
// room for what it held: filled at the next look, it evicted the next least
// used copy, a GET a pack every minute while the held copies outgrew the
// budget.
//
// Mutation: fill whatever the budget's room (indexHeld's roomToFill check
// skipped) — the evicted copy is filled at the next look.
func TestAnEvictedCopyIsFilledBackOnlyIntoRoom(t *testing.T) {
	t.Parallel()
	files := map[string]string{"a.go": "package a\n"}
	probe := newSyncWorld(t, onMemory)
	probe.push(t, probe.repo(t, 1), "refs/heads/main", files)
	sized := probe.open(t, holding)
	cache.IndexPass(t.Context(), sized)
	index, copyBytes := cache.CodeIndexBytes(sized), cache.Total(sized)
	// A budget whose share has room for the index, and which the copy alone
	// is past.
	budget := cache.CodeIndexShare * (index + 1)
	if index == 0 || copyBytes <= budget {
		t.Fatalf("the probe's copy holds %d bytes and its index %d: the case needs a copy past %d", copyBytes,
			index, budget)
	}
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", files)
	c := w.open(t, holding, func(o *cache.Options) { o.MaxBytes = budget })
	cache.IndexPass(t.Context(), c)
	if !keeps(c, repo) {
		t.Fatal("the indexer built no index")
	}
	if cache.EvictToBudget(c) != 1 || c.Holds(repo) {
		t.Fatal("the budget did not evict the copy: the case needs one past it")
	}
	cache.IndexPass(t.Context(), c)
	if c.Holds(repo) {
		t.Error("the indexer filled the evicted copy again with no room in the budget for it")
	}
}
