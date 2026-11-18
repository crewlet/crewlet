package cache_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE SWAP (swap.go): a round that takes a pack out of a copy a reader may
// hold answers from the copy's NEXT incarnation, hard-linked beside it from
// what the live rows still name, while the old one stays whole for its
// readers; a retirement keeps the commit-graph, a pack no retirement accounts
// for drops it.

// compactable is the cases' setup: a repository of three pushes, synced by a
// cache over the world and HELD by a reader; a case compacts the first two
// packs, never the third.
type compactable struct {
	w    *syncWorld
	clk  *clock
	c    *cache.Cache
	repo string
	rows []gitlet.PackRow
	held cache.Synced
}

// newCompactable is a compactable over a cache opened with each of opts;
// maintained, the maintainer has written its multi-pack-index with its bitmap
// and the commit-graph over the three packs.
func newCompactable(t *testing.T, maintained bool, opts ...func(*cache.Options)) *compactable {
	t.Helper()
	w := newSyncWorld(t, onMemory)
	k := &compactable{w: w, clk: newClock(), repo: w.repo(t, 1)}
	for range 3 {
		_, row := w.push(t, k.repo, "refs/heads/main", nil)
		k.rows = append(k.rows, row)
	}
	k.c = w.open(t, append([]func(*cache.Options){k.clk.on}, opts...)...)
	k.held = w.sync(t, k.c, k.repo)
	if maintained {
		k.clk.Add(cache.MidxDebounce)
		if ran := cache.MaintainerPass(k.c); !slices.Equal(ran, []string{k.repo}) {
			t.Fatalf("the maintainer ran %v, want %s", ran, k.repo)
		}
		if packs, bitmap, graph := cache.Indexed(k.c, k.repo); packs != 3 || !bitmap || !graph {
			t.Fatalf("the maintainer wrote an index over %d packs (bitmap %t) and a graph %t: the case needs "+
				"all three over the three packs", packs, bitmap, graph)
		}
	}
	return k
}

// compact writes a compaction of the first two packs — their merge stored,
// and one record retiring them and adding it — and answers its output's row.
func (k *compactable) compact(t *testing.T) gitlet.PackRow {
	t.Helper()
	return k.w.compacted(t, k.repo, k.rows[0], k.rows[1])
}

// bundleNames is the names of the files of the packs rows name, sorted.
func bundleNames(rows ...gitlet.PackRow) []string {
	var out []string
	for _, r := range rows {
		for _, ext := range []string{".idx", ".pack", ".rev"} {
			out = append(out, "pack-"+r.GitChecksum+ext)
		}
	}
	slices.Sort(out)
	return out
}

// sameInode reports whether a and b are one file — two names of one inode —
// failing t where either is not there.
func sameInode(t *testing.T, a, b string) bool {
	t.Helper()
	ai, err := os.Lstat(a)
	if err != nil {
		t.Fatal(err)
	}
	bi, err := os.Lstat(b)
	if err != nil {
		t.Fatal(err)
	}
	return os.SameFile(ai, bi)
}

// graphRel and midxRel are the commit-graph and the multi-pack-index in an
// incarnation.
var (
	graphRel = filepath.Join("objects", "info", "commit-graph")
	midxRel  = filepath.Join("objects", "pack", "multi-pack-index")
)

// bitmaps is the bitmaps in the incarnation at dir.
func bitmaps(t *testing.T, dir string) []string {
	t.Helper()
	found, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "*.bitmap"))
	if err != nil {
		t.Fatal(err)
	}
	return found
}

// swapLine is the one code_cache_swapped line w logged, failing t on none or
// more.
func swapLine(t *testing.T, w *syncWorld) map[string]any {
	t.Helper()
	lines := w.logs.lines(t, "code_cache_swapped")
	if len(lines) != 1 {
		t.Fatalf("code_cache_swapped = %v, want one line", lines)
	}
	return lines[0]
}

// A SWAP KEEPS EVERY PACK A READER HOLDS. When a delta's retired phase names
// two of a copy's three packs, the round answers from the NEXT incarnation,
// which holds exactly the compaction's output and the unnamed pack, each
// linked (the very inode), never copied: the image a cache built from nothing
// holds there. The old one, still held, keeps every file plus the installed
// output, so every pack the reader's refs may lead git to stays whole; its
// last release moves it into trash/. Logged and counted once as a swap with
// no graph to keep (none, never dropped: the copy held none).
//
// Mutations: link the leaving packs into the next incarnation too; take them
// out of the old one in place; count a graphless copy as graph dropped
// (swapped).
func TestASwapKeepsEveryPackAReaderHolds(t *testing.T) {
	t.Parallel()
	k := newCompactable(t, false)
	w, c, repo := k.w, k.c, k.repo
	out := k.compact(t)
	s := w.sync(t, c, repo)

	from, to := cache.Incarnation(k.held), cache.Incarnation(s)
	if to != from+1 || s.Dir == k.held.Dir {
		t.Fatalf("the round that retired two packs answered incarnation %d at %s, want the next past %d", to,
			s.Dir, from)
	}
	kept := bundleNames(out, k.rows[2])
	if got := packFiles(t, s.Dir); !slices.Equal(got, kept) {
		t.Errorf("the next incarnation holds %v, want the output and the pack no compaction named: %v", got, kept)
	}
	for _, name := range kept {
		if !sameInode(t, filepath.Join(k.held.Dir, "objects", "pack", name), filepath.Join(s.Dir, "objects",
			"pack", name)) {
			t.Errorf("%s in the next incarnation is not the old one's inode: a swap links, never copies", name)
		}
	}
	if !sameInode(t, filepath.Join(k.held.Dir, "packed-refs"), filepath.Join(s.Dir, "packed-refs")) {
		t.Error("the next incarnation's packed-refs is not the round's own, linked")
	}
	all := bundleNames(append(slices.Clone(k.rows), out)...)
	if got := packFiles(t, k.held.Dir); !slices.Equal(got, all) {
		t.Errorf("the incarnation a reader holds keeps %v, want every pack it held and the output: %v", got, all)
	}
	for _, r := range k.rows[:2] {
		idx := filepath.Join("objects", "pack", "pack-"+r.GitChecksum+".idx")
		if _, err := runGit(t, w.pool, k.held.Dir, "", "verify-pack", idx); err != nil {
			t.Errorf("the retired pack %s is not whole under the reader that holds it: %v", r.GitChecksum, err)
		}
	}
	if _, err := runGit(t, w.pool, k.held.Dir, "", "rev-list", "--objects", "--all"); err != nil {
		t.Errorf("a walk of the whole history over the old incarnation after the swap: %v", err)
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the next incarnation is\n%s\nwant what a cache built from nothing holds\n%s", got, want)
	}

	line := swapLine(t, w)
	if line["from"] != float64(from) || line["to"] != float64(to) || line["packs_kept"] != float64(2) ||
		line["packs_dropped"] != float64(2) || line["graph"] != "none" {
		t.Errorf("code_cache_swapped = %v, want from %d to %d, two packs kept and two dropped, no graph", line,
			from, to)
	}
	if got := counter(w.rec, metrics.CodeCacheSwaps, "graph", "none"); got != 1 {
		t.Errorf("%s{graph=none} = %d, want the one swap of a copy that held no graph", metrics.CodeCacheSwaps, got)
	}
	if got := counterOf(w.rec, metrics.CodeCacheSwaps, nil); got != 1 {
		t.Errorf("%s = %d, want the one swap", metrics.CodeCacheSwaps, got)
	}
	if got := cache.Leaving(c, repo); len(got) != 0 {
		t.Errorf("after the swap the copy has %v leaving, want nothing", got)
	}

	k.held.Release()
	if statOK(t, k.held.Dir) {
		t.Errorf("%s is still in repos/ after its last reader let it go", k.held.Dir)
	}
}

// A MERGE COMPACTION KEEPS THE COMMIT-GRAPH (X04): it drops no commit, so the
// graph names every commit the next incarnation holds, whichever round learns
// of it (a delta's retired phase, or a full answer's retirements above the
// copy's position). The next incarnation holds and records the old graph as
// the same inode, which git verifies; the multi-pack-index and its bitmap
// name the packs that left, so it holds neither until the maintainer writes
// them again. Counted and logged as a swap that kept the graph.
//
// Mutations: declare the graph dropped on a retirement (OnPackRetired); read
// a full answer's retired strays as unaccounted (packStrays): the full arm
// drops it.
func TestAMergeCompactionKeepsTheCommitGraph(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		learn func(k *compactable)
	}{
		{"a delta's retirements", func(*compactable) {}},
		{"a full answer's retirements", func(k *compactable) {
			cache.MarkWhole(k.c, k.repo, cache.RebuildShape, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := newCompactable(t, true)
			w, c, repo := k.w, k.c, k.repo
			k.compact(t)
			tc.learn(k)
			s := w.sync(t, c, repo)
			if s.Dir == k.held.Dir {
				t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
			}
			graph := filepath.Join(s.Dir, graphRel)
			if !statOK(t, graph) {
				t.Fatal("the next incarnation holds no commit-graph: a merge drops no commit")
			}
			if !sameInode(t, filepath.Join(k.held.Dir, graphRel), graph) {
				t.Error("the next incarnation's commit-graph is not the old one's, linked")
			}
			if _, err := runGit(t, w.pool, s.Dir, "", "commit-graph", "verify"); err != nil {
				t.Errorf("git verifies the carried commit-graph against the next incarnation's packs: %v", err)
			}
			if statOK(t, filepath.Join(s.Dir, midxRel)) || len(bitmaps(t, s.Dir)) != 0 {
				t.Error("the next incarnation holds a multi-pack-index or a bitmap over the packs that left")
			}
			if packs, bitmap, graph := cache.Indexed(c, repo); packs != 0 || bitmap || !graph {
				t.Errorf("the next incarnation records an index over %d packs (bitmap %t) and a graph %t, want "+
					"the graph alone", packs, bitmap, graph)
			}
			doc := readStateDoc(t, s.Dir)
			if _, midx := doc.Derived[string(cache.DerivedMidx)]; doc.graph().Bytes == 0 || midx {
				t.Errorf("the next incarnation's state records the graph %+v and the index %+v, want the graph "+
					"alone", doc.graph(), doc.midx())
			}
			if line := swapLine(t, w); line["graph"] != "kept" {
				t.Errorf("code_cache_swapped = %v, want the graph kept", line)
			}
			if got := counter(w.rec, metrics.CodeCacheSwaps, "graph", "kept"); got != 1 {
				t.Errorf("%s{graph=kept} = %d, want 1", metrics.CodeCacheSwaps, got)
			}
			if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
				t.Errorf("the next incarnation is\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// A STRAY NO RETIREMENT ACCOUNTS FOR DROPS THE GRAPH. A pack whose row left
// the log uncompacted (a branch deleted, its pack with it) may have taken
// commits, so a graph over it may name commits the copy no longer holds: the
// full round that finds it swaps, the next incarnation holding neither graph
// nor multi-pack-index while the old one keeps both and the stray for its
// reader. Logged and counted as a graph dropped.
//
// Mutations: read every stray as a retirement (packStrays): the graph names a
// gone commit; count a swap linking no graph as of a copy with none
// (swapped).
func TestAStrayNoRetirementAccountsForDropsTheGraph(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	for range 3 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	_, side := w.push(t, repo, "refs/heads/side", nil)
	c := w.open(t, clk.on)
	held := w.sync(t, c, repo)
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Fatalf("the maintainer ran %v, want %s", ran, repo)
	}
	if !statOK(t, filepath.Join(held.Dir, graphRel)) {
		t.Fatal("the maintainer wrote no commit-graph: the case needs one")
	}
	w.log.write(repo, nil, map[string]string{"refs/heads/side": ""})
	w.log.dropPack(repo, side.PackID)
	cache.MarkWhole(c, repo, cache.RebuildShape, 0)
	s := w.sync(t, c, repo)

	if s.Dir == held.Dir {
		t.Fatalf("the round that found a stray answered the incarnation a reader holds, %s", s.Dir)
	}
	if statOK(t, filepath.Join(s.Dir, graphRel)) || statOK(t, filepath.Join(s.Dir, midxRel)) {
		t.Error("the next incarnation holds a commit-graph or a multi-pack-index over a pack no row accounts for")
	}
	if packs, bitmap, graph := cache.Indexed(c, repo); packs != 0 || bitmap || graph {
		t.Errorf("the next incarnation records an index over %d packs (bitmap %t) and a graph %t, want none",
			packs, bitmap, graph)
	}
	if !statOK(t, filepath.Join(held.Dir, graphRel)) ||
		!statOK(t, filepath.Join(held.Dir, "objects", "pack", "pack-"+side.GitChecksum+".pack")) {
		t.Error("the incarnation a reader holds lost its graph or the stray")
	}
	if line := swapLine(t, w); line["graph"] != "dropped" || line["packs_dropped"] != float64(1) {
		t.Errorf("code_cache_swapped = %v, want the stray dropped and the graph with it", line)
	}
	if got := counter(w.rec, metrics.CodeCacheSwaps, "graph", "dropped"); got != 1 {
		t.Errorf("%s{graph=dropped} = %d, want 1", metrics.CodeCacheSwaps, got)
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the next incarnation is\n%s\nwant\n%s", got, want)
	}
}

// A COMPACTION A COPY MISSES UNTIL ITS RETIREMENTS ARE SWEPT DROPS THE GRAPH:
// an idle, maintained copy does not sync across a compaction before the
// code-retention duty sweeps its retirements, so its next round, below the
// sweep, reads it whole and serves none of them: the packs the compaction
// took out are ones no retirement accounts for, and leave by the swap with
// the graph and the multi-pack-index, the old copy keeping everything for its
// reader. Logged and counted as a graph dropped (the copy held one), never as
// none to keep.
//
// Mutations: read a below-sweep full answer's strays as retirements
// (packStrays): the graph is kept; count a swap linking no graph as of a copy
// with none (swapped).
func TestACompactionACopyMissesUntilItsRetirementsAreSweptDropsTheGraph(t *testing.T) {
	t.Parallel()
	k := newCompactable(t, true)
	w, c, repo := k.w, k.c, k.repo
	out := k.compact(t)
	w.log.sweepRetired(repo, w.log.head())
	s := w.sync(t, c, repo)
	if s.Dir == k.held.Dir {
		t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
	}
	if got, want := packFiles(t, s.Dir), bundleNames(out, k.rows[2]); !slices.Equal(got, want) {
		t.Errorf("the next incarnation holds %v, want the output and the pack no compaction named: %v", got, want)
	}
	if statOK(t, filepath.Join(s.Dir, graphRel)) || statOK(t, filepath.Join(s.Dir, midxRel)) {
		t.Error("the next incarnation holds a commit-graph or a multi-pack-index over packs no retirement " +
			"accounts for")
	}
	if !statOK(t, filepath.Join(k.held.Dir, graphRel)) {
		t.Error("the incarnation a reader holds lost its commit-graph")
	}
	if line := swapLine(t, w); line["graph"] != "dropped" || line["packs_dropped"] != float64(2) {
		t.Errorf("code_cache_swapped = %v, want the two packs dropped and the graph with them", line)
	}
	if got := counter(w.rec, metrics.CodeCacheSwaps, "graph", "dropped"); got != 1 {
		t.Errorf("%s{graph=dropped} = %d, want 1", metrics.CodeCacheSwaps, got)
	}
	if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the next incarnation is\n%s\nwant\n%s", got, want)
	}
}

// A LOG GONE BACK DROPS THE GRAPH, whatever a retired phase says. A log
// recreated or gone back names other records at its positions, so a full
// answer's retirements above the copy's position say nothing of its files:
// every stray leaves as unaccounted, and the graph with it, even over a
// compaction's retirements the full answer serves.
//
// Mutation: read a gone-back log's strays by the retired phase (apply's
// history false): the graph is kept.
func TestALogGoneBackDropsTheGraph(t *testing.T) {
	t.Parallel()
	k := newCompactable(t, true)
	w, c, repo := k.w, k.c, k.repo
	k.compact(t)
	cache.Reread(c, cache.RebuildStream)
	s := w.sync(t, c, repo)
	if s.Dir == k.held.Dir {
		t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
	}
	pages := w.log.firstPages(repo)
	if last := pages[len(pages)-1]; last.RetiredSince != k.held.Position {
		t.Fatalf("the full round asked for the retirements above %v, want above the copy's %v: the case needs "+
			"them served", last.RetiredSince, k.held.Position)
	}
	if statOK(t, filepath.Join(s.Dir, graphRel)) {
		t.Error("the next incarnation kept a commit-graph over a log gone back")
	}
	if line := swapLine(t, w); line["graph"] != "dropped" {
		t.Errorf("code_cache_swapped = %v, want the graph dropped", line)
	}
}

// A ROUND THAT ADDS ONLY STAYS IN PLACE: a delta adding a pack, one moving a
// ref and a full round finding no stray each answer the reader's
// incarnation, multi-pack-index and graph kept, with no swap logged or
// counted.
//
// Mutation: swap at every round's end (swapLeaving without its leaving
// guard): the first delta answers another incarnation.
func TestARoundThatAddsOnlyStaysInPlace(t *testing.T) {
	t.Parallel()
	k := newCompactable(t, true)
	w, c, repo := k.w, k.c, k.repo
	n := cache.Incarnation(k.held)
	for _, step := range []struct {
		name string
		do   func()
	}{
		{"a delta that adds a pack", func() { w.push(t, repo, "refs/heads/main", nil) }},
		{"a delta that moves a ref", func() { w.tag(t, repo, "v1", false) }},
		{"a full round with no stray", func() { cache.MarkWhole(c, repo, cache.RebuildShape, 0) }},
	} {
		step.do()
		s := w.sync(t, c, repo)
		if got := cache.Incarnation(s); got != n || s.Dir != k.held.Dir {
			t.Errorf("%s answered incarnation %d at %s, want %d in place", step.name, got, s.Dir, n)
		}
		if !statOK(t, filepath.Join(s.Dir, graphRel)) || !statOK(t, filepath.Join(s.Dir, midxRel)) {
			t.Errorf("after %s the copy lost its multi-pack-index or its graph", step.name)
		}
	}
	if got := counterOf(w.rec, metrics.CodeCacheSwaps, nil); got != 0 {
		t.Errorf("%s = %d, want no swap", metrics.CodeCacheSwaps, got)
	}
	if got := w.logs.lines(t, "code_cache_swapped"); len(got) != 0 {
		t.Errorf("code_cache_swapped = %v, want none", got)
	}
}

// A DELTA RETIRING A PACK IT NEVER HELD CONVERGES IN PLACE. A push another
// node received and compacted before this node's round is in the delta's
// retired phase though its packs phase never served it (its row left
// code_packs with the compaction), and no live row of the copy names it: the
// round skips that retirement. It converges from that ONE delta (no full
// read, no second read at the log's head), downloads no retired pack and the
// output once, records the snapshot's position and answers in place; a
// retired pack the copy does hold leaves by the swap, and only that one.
//
// Mutations: take every retired row out as leaving (apply): the first arm
// swaps; answer a retired row outside the live set ErrBadSnapshot: both arms
// read the repository whole.
func TestADeltaRetiringAPackItNeverHeldConvergesInPlace(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// held: the compaction merges one of the copy's own packs too.
		held bool
	}{
		{"only packs it never held", false},
		{"one it never held beside one it holds", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := newCompactable(t, false)
			w, c, repo := k.w, k.c, k.repo
			_, fourth := w.push(t, repo, "refs/heads/main", nil)
			_, fifth := w.push(t, repo, "refs/heads/main", nil)
			merged, kept := []gitlet.PackRow{fourth, fifth}, slices.Clone(k.rows)
			if tc.held {
				merged, kept = []gitlet.PackRow{k.rows[0], fourth}, []gitlet.PackRow{k.rows[1], k.rows[2], fifth}
			}
			out := w.compacted(t, repo, merged...)
			asked := len(w.log.firstPages(repo))
			s := w.sync(t, c, repo)

			pages := w.log.firstPages(repo)[asked:]
			if len(pages) != 1 || pages[0].Since != k.held.Position {
				t.Errorf("the round read %+v, want one delta from the copy's %v", pages, k.held.Position)
			}
			if s.Position != w.log.head() {
				t.Errorf("the round recorded %v, want the snapshot's %v", s.Position, w.log.head())
			}
			if moved := s.Dir != k.held.Dir; moved != tc.held {
				t.Fatalf("the round answered %s, the copy the reader holds being %s: swapped %t, want %t", s.Dir,
					k.held.Dir, moved, tc.held)
			}
			if got, want := packFiles(t, s.Dir), bundleNames(append(kept, out)...); !slices.Equal(got, want) {
				t.Errorf("the copy the round answered holds %v, want %v", got, want)
			}
			opens, _, _ := w.objects.counts()
			if opens[fourth.Object] != 0 {
				t.Errorf("the delta downloaded the retired pack %s it never held", fourth.GitChecksum)
			}
			if opens[out.Object] != 1 {
				t.Errorf("the delta downloaded the output %d times, want once", opens[out.Object])
			}
			if got := cache.Leaving(c, repo); len(got) != 0 {
				t.Errorf("the copy has %v leaving, want nothing", got)
			}
			swaps := map[bool]uint64{false: 0, true: 1}[tc.held]
			if got := counterOf(w.rec, metrics.CodeCacheSwaps, nil); got != swaps {
				t.Errorf("%s = %d, want %d", metrics.CodeCacheSwaps, got, swaps)
			}
			if got, want := w.image(t, c, repo), w.rebuilt(t, repo); got != want {
				t.Errorf("the copy is\n%s\nwant\n%s", got, want)
			}
		})
	}
}

// A SWAP COSTS LINKS, NOT DOWNLOADS: over fifteen packs whose index of pushes
// holds runs, the round learning of a compaction of two downloads the output
// alone, runs no git, and makes exactly three links per live pack, one each
// for HEAD, config and packed-refs and one per index run (the pack count,
// never the bytes), writing no file but its state. It syncs each directory
// it wrote ONCE: objects/pack, crewlet/ and the staged root (by the state's
// write) before the rename, repos/ after; never scratch/, never step by step.
//
// Mutations: link the leaving packs too; write the round's own
// multi-pack-index on a delta's swap (a git process); write HEAD and config
// afresh; sync the staged root as the derived files land as well.
func TestASwapCostsLinksNotDownloads(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	var calls func() []gittest.Call
	w.gitPath, calls = gittest.Recording(t)
	repo := w.repo(t, 1)
	var rows []gitlet.PackRow
	for range 13 {
		_, row := w.push(t, repo, "refs/heads/main", nil)
		rows = append(rows, row)
	}
	var links atomic.Int64
	var syncs syncLog
	c := w.open(t, cache.LinkWith(func(oldname, newname string) error {
		links.Add(1)
		return os.Link(oldname, newname)
	}), cache.SyncingWith(syncs.add))
	held := w.sync(t, c, repo)
	for range 2 {
		w.push(t, repo, "refs/heads/main", nil)
		w.sync(t, c, repo)
	}
	idx, ok := cache.Index(c, repo)
	if !ok || len(idx.Runs) == 0 {
		t.Fatalf("the index of pushes is %+v (kept %t): the case needs runs to carry", idx, ok)
	}
	out := w.compacted(t, repo, rows[0], rows[1])
	ran, opened := len(calls()), w.objects.total()
	syncs.reset()

	s := w.sync(t, c, repo)
	if s.Dir == held.Dir {
		t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
	}
	if got := len(calls()) - ran; got != 0 {
		t.Errorf("the round that swapped ran %d git processes, want none", got)
	}
	if got := w.objects.total() - opened; got != 1 {
		t.Errorf("the round that swapped opened %d objects, want the output's alone", got)
	}
	if opens, _, _ := w.objects.counts(); opens[out.Object] != 1 {
		t.Errorf("the output was opened %d times, want once", opens[out.Object])
	}
	const live = 15 - 2 + 1
	if got, want := links.Load(), int64(3*live+3+len(idx.Runs)+1); got != want {
		t.Errorf("the swap made %d links, want %d: three for each of %d live packs, HEAD, config, the "+
			"packed-refs, %d runs and the tag index", got, want, live, len(idx.Runs))
	}
	for _, name := range []string{"HEAD", "config", "packed-refs"} {
		if !sameInode(t, filepath.Join(held.Dir, name), filepath.Join(s.Dir, name)) {
			t.Errorf("the next incarnation's %s is not the old one's, linked", name)
		}
	}
	got := syncs.swap(w)
	want := map[string]int{".": 1, "objects/pack": 1, "crewlet": 1, "repos/": 1}
	if !maps.Equal(got, want) {
		t.Errorf("the swap synced %v, want each directory it wrote once — the staged root by its state's write, "+
			"objects/pack and crewlet/ — and repos/ after the rename: %v", got, want)
	}
	if line := swapLine(t, w); line["packs_kept"] != float64(live) {
		t.Errorf("code_cache_swapped = %v, want %d packs kept", line, live)
	}
	if got, ok := cache.Index(c, repo); !ok || len(got.Runs) != len(idx.Runs) {
		t.Errorf("the next incarnation's index of pushes is %+v (kept %t), want the %d runs carried", got, ok,
			len(idx.Runs))
	}
}

// A SWAP MOVES THE MEMO WHOLE, as a rebuild does (memo.go's carryMemo): a
// compaction keeps every object, so the old copy's answers hold in the next
// incarnation, and the promote renames the memo tree into it once, never a
// link per answer (a stat and a link per answer on every Gitlet node holding
// the repository, at every compaction). Every answer is served there as it
// was and counted once, the node's memo bytes unchanged; the left copy,
// still held, answers none; the move is logged as a rebuild's.
//
// Mutation: carry no memo at the promote (swap.go's carryMemo call): the
// next incarnation answers none.
func TestASwapMovesTheMemoWhole(t *testing.T) {
	t.Parallel()
	k := newCompactable(t, false)
	w, c, repo := k.w, k.c, k.repo
	keys := memoKeys("swap", 64)
	memoize(t, c, k.held, keys)
	before, _ := cache.MemoOf(c, repo)
	held := cache.MemoBytes(c)
	oldTree, err := os.Stat(cache.MemoTree(k.held.Dir))
	if err != nil {
		t.Fatal(err)
	}
	k.compact(t)
	s := w.sync(t, c, repo)
	if s.Dir == k.held.Dir {
		t.Fatalf("the compaction answered the copy a reader holds, %s: no swap", s.Dir)
	}

	newTree, err := os.Stat(cache.MemoTree(s.Dir))
	switch {
	case err != nil:
		t.Fatalf("the next incarnation holds no memo of the copy the swap left: %v", err)
	case !os.SameFile(oldTree, newTree):
		t.Error("the next incarnation's memo is another directory than the old copy's: a swap moves it whole")
	}
	if statOK(t, cache.MemoTree(k.held.Dir)) {
		t.Error("the copy the swap left still holds a memo tree beside the one the swap carried")
	}
	for i, key := range keys {
		if got, ok := c.MemoGet(s, key); !ok || string(got) != string(memoBody(i))+memoTail {
			t.Errorf("the next incarnation answers entry %d %q (%t), want the old copy's", i, got, ok)
		}
	}
	if _, ok := c.MemoGet(k.held, keys[0]); ok {
		t.Error("the copy the swap left answered from a memo the swap carried away")
	}
	if after, _ := cache.MemoOf(c, repo); after.Bytes != before.Bytes {
		t.Errorf("the next incarnation's memo counts %d bytes, want the old copy's %d", after.Bytes, before.Bytes)
	}
	if got := cache.MemoBytes(c); got != held {
		t.Errorf("the node counts its memo at %d bytes after the swap, want the %d it held once", got, held)
	}
	if lines := w.logs.lines(t, "code_cache_memo_carried"); len(lines) != 1 {
		t.Errorf("code_cache_memo_carried = %v, want the swap's one line", lines)
	}
}

// A READ OF WHAT A SWAP MOVED REFUSES THE COPY IT REPLACED. The swap leaves
// the replaced copy's live set empty, so a plan read from it would see no
// pack: a compaction planned over nothing, a weekly expunge judging clean a
// repository it never read. Packs and DroppingPacks of a Synced whose copy a
// swap replaced answer cache.ErrSuperseded; the copy the next sync pins reads
// what the swap carried.
//
// Mutation: read the replaced copy's live set (Packs not asking for its
// successor): it answers none and no error.
func TestAReadOfWhatASwapMovedRefusesTheCopyItReplaced(t *testing.T) {
	t.Parallel()
	k := newCompactable(t, false)
	w, c, repo := k.w, k.c, k.repo
	if before, err := c.Packs(k.held); err != nil || len(before) != len(k.rows) {
		t.Fatalf("the held copy read %d live packs before the swap (%v), want its %d", len(before), err,
			len(k.rows))
	}
	k.compact(t)
	s := w.sync(t, c, repo)
	defer s.Release()
	if s.Dir == k.held.Dir {
		t.Fatalf("the compaction answered the copy a reader holds, %s: no swap", s.Dir)
	}
	if got, err := c.Packs(k.held); got != nil || !errors.Is(err, cache.ErrSuperseded) {
		t.Errorf("the replaced copy's live packs = %+v, %v; want none and ErrSuperseded", got, err)
	}
	if got, err := c.DroppingPacks(k.held); got != nil || !errors.Is(err, cache.ErrSuperseded) {
		t.Errorf("the replaced copy's dropping packs = %+v, %v; want none and ErrSuperseded", got, err)
	}
	if got, err := c.Packs(s); err != nil || len(got) != len(k.rows)-1 {
		t.Errorf("the copy serving read %d live packs (%v), want the merge and the pack it left, %d", len(got),
			err, len(k.rows)-1)
	}
}

// A SWAPPED COPY'S STATE KEEPS ITS LAST USE. A boot reads last use from the
// state's modification time, and the swap's write of the next incarnation's
// state is no use of the copy, so it carries the last use as the swap took
// it (writeState's stampUse), never the write's moment: a node restarting
// right after a compaction finds the copy used when it was.
//
// Mutation: leave the next incarnation's last use unset in swap.go's
// writeState: the restart reads back the write's moment.
func TestASwappedCopysStateKeepsItsLastUse(t *testing.T) {
	t.Parallel()
	k := newCompactable(t, false)
	w, c, repo := k.w, k.c, k.repo
	used := cache.LastUsed(c, repo)
	if used.IsZero() {
		t.Fatal("the copy a reader holds records no use: the case needs one")
	}
	k.clk.Add(time.Hour)
	k.compact(t)
	s := w.sync(t, c, repo)
	if s.Dir == k.held.Dir {
		t.Fatalf("the compaction answered the copy a reader holds, %s: no swap", s.Dir)
	}
	info, err := os.Stat(filepath.Join(s.Dir, "crewlet-state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(used) {
		t.Errorf("the state the swap wrote is stamped %v, want the copy's last use %v", info.ModTime(), used)
	}
	again := w.reopen(t, c, k.clk.on)
	if got := cache.LastUsed(again, repo); !got.Equal(used) {
		t.Errorf("after a restart the copy was last used %v, want %v: a swap is no use of it", got, used)
	}
}

// A SWAP TELLS THE BACKFILL. The schedule places a copy by a look at its
// serving incarnation (schedule.go), and the next incarnation's index of
// pushes can differ (a run gone from the disk leaves it none: carryIndex).
// So the promote MARKS the copy, as a rebuild's does, and the next pick looks
// again: a placement from the old index, here by a pick while the swap was
// staged, never stands beside a look that says otherwise.
//
// Mutation: no mark at the promote (swap.go's backfillDirty): the schedule
// keeps the copy due for a history the new look no longer says.
func TestASwapTellsTheBackfill(t *testing.T) {
	t.Parallel()
	faults := &gitletfault.Script{}
	k := newCompactable(t, false, func(o *cache.Options) { o.Faults = faults })
	w, c, repo := k.w, k.c, k.repo
	w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo)
	idx, ok := cache.Index(c, repo)
	if !ok || len(idx.Runs) != 1 {
		t.Fatalf("the index of pushes after a delta is %+v (kept %t), want the delta's run", idx, ok)
	}
	// The run leaves the disk under the index that names it, so the swap
	// carries no index into the next incarnation.
	if err := os.Remove(filepath.Join(k.held.Dir, cache.RunRel(idx.Runs[0]))); err != nil {
		t.Fatal(err)
	}
	c.WantHistory(repo)

	release, reached := make(chan struct{}), make(chan struct{})
	var released sync.Once
	let := func() { released.Do(func() { close(release) }) }
	t.Cleanup(let) // before the cache stops, should the case end first
	faults.Arm(gitletfault.SyncSwapBeforeRename, gitletfault.Action{Once: true, Block: release, Reached: reached})
	k.compact(t)
	answered := make(chan cache.Synced, 1)
	go func() {
		s, err := c.Sync(context.Background(), repo, linearizable)
		if err != nil {
			t.Errorf("the round that learns of the compaction: %v", err)
		}
		answered <- s
	}()
	select {
	case <-reached:
	case <-time.After(hangDetector): //fixedwait:liveness
		t.Fatal("the round never reached the swap's rename")
	}
	// A pick while the swap is staged places the copy by the old
	// incarnation's index: its history is due.
	if got, err := cache.BackfillPick(c); err != nil || got != repo {
		t.Fatalf("the backfill's pick = %q, %v; want %s, whose history is wanted", got, err, repo)
	}
	let()
	s := <-answered
	if s.Dir == "" {
		t.FailNow()
	}
	t.Cleanup(s.Release)
	if s.Dir == k.held.Dir {
		t.Fatalf("the compaction answered the copy a reader holds, %s: no swap", s.Dir)
	}
	if got, kept := cache.Index(c, repo); kept {
		t.Fatalf("the next incarnation keeps the index %+v, whose run left the disk: the case needs none", got)
	}
	if problems := cache.BackfillAudit(c); len(problems) > 0 {
		t.Errorf("after the swap the backfill's schedule holds what no look at the copy says:\n\t%s",
			strings.Join(problems, "\n\t"))
	}
}

// copyTree copies the directory src to dst as a crash leaves it on the disk:
// directories, regular files with bytes and mode, symbolic links as such, and
// HARD LINKS as links (two names of one inode stay so), so what a boot counts
// by sole names is what it would count at src.
func copyTree(t *testing.T, src, dst string) {
	t.Helper()
	copied := map[[2]uint64]string{}
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		to := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			return os.MkdirAll(to, info.Mode().Perm()|0o700)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(target, to)
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s is neither a file, a link nor a directory", path)
		}
		st := info.Sys().(*syscall.Stat_t)
		key := [2]uint64{uint64(st.Dev), st.Ino} //nolint:unconvert // Dev is an int32 on darwin
		if first, ok := copied[key]; ok {
			return os.Link(first, to)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.WriteFile(to, body, info.Mode().Perm()); err != nil {
			return err
		}
		copied[key] = to
		return nil
	})
	if err != nil {
		t.Fatalf("copying %s to %s: %v", src, dst, err)
	}
}

// over is a world over the cache directory dir — a crash's copy of w's disk,
// booted as the next node would — reading w's log and store.
func (w *syncWorld) over(t *testing.T, dir string) *syncWorld {
	t.Helper()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	o := &syncWorld{world: &world{logs: &logs{}}, client: w.client, log: w.log, backend: w.backend, mem: w.mem,
		s3: w.s3, store: w.store, objects: w.objects, rec: rec, repos: w.repos,
		bundles: map[string]gittest.Bundled{}}
	o.prepare(t, dir)
	return o
}

// A CRASH BEFORE THE SWAP'S RENAME LEAVES THE OLD INCARNATION highest in
// repos/, its state naming the floor the leaving packs' retirements are
// above (LeavingSince), and the next one staged under scratch/, which the
// boot moves aside. The boot's first round, full, asks retirements from that
// floor, not the copy's position (none lies at or past it), reads the leaving
// packs as retirements, and keeps the graph IN PLACE, the very inode (nobody
// holds a copy before its first round), dropping the multi-pack-index and
// moving the retired packs aside: a fresh cache's image. The node that did
// not die answers from the old incarnation, its swap failing at the point.
//
// Mutation: write no LeavingSince (stateValue): the boot reads the retired
// packs as unaccounted and drops the graph.
func TestACrashBeforeTheSwapsRenameLeavesTheOldIncarnation(t *testing.T) {
	t.Parallel()
	faults := &gitletfault.Script{}
	k := newCompactable(t, true, func(o *cache.Options) { o.Faults = faults })
	w, c, repo := k.w, k.c, k.repo
	n := cache.Incarnation(k.held)
	release, reached := make(chan struct{}), make(chan struct{})
	faults.Arm(gitletfault.SyncSwapBeforeRename, gitletfault.Action{Once: true, Block: release, Err: errKilled,
		Reached: reached})
	out := k.compact(t)
	answered := make(chan int, 1)
	go func() {
		s, err := c.Sync(context.Background(), repo, linearizable)
		if err != nil {
			answered <- -1
			return
		}
		answered <- cache.Incarnation(s)
		s.Release()
	}()
	select {
	case <-reached:
	case <-time.After(hangDetector): //fixedwait:liveness
		t.Fatal("the round never reached the swap's rename")
	}
	crashed := filepath.Join(t.TempDir(), "crashed")
	copyTree(t, w.layout.Dir, crashed)
	close(release)
	if got := <-answered; got != n {
		t.Fatalf("the node whose swap failed at its point answered incarnation %d, want the old %d", got, n)
	}
	if lines := w.logs.lines(t, "code_cache_swap_failed"); len(lines) != 1 || lines[0]["step"] != "rename" {
		t.Errorf("code_cache_swap_failed = %v, want one line at the rename's step, the point's", lines)
	}

	// The disk the crash left.
	old := filepath.Join(crashed, "repos", repo+"."+strconv.Itoa(n)+".git")
	if got := names(t, filepath.Join(crashed, "repos")); !slices.Equal(got, []string{filepath.Base(old)}) {
		t.Fatalf("the crash left %v in repos/, want the old incarnation alone", got)
	}
	staged := names(t, filepath.Join(crashed, "scratch"))
	if len(staged) != 1 || !strings.HasPrefix(staged[0], "swap."+repo+".") {
		t.Fatalf("the crash left %v in scratch/, want the staged incarnation", staged)
	}
	if doc := readStateDoc(t, old); doc.LeavingSince != k.held.Position {
		t.Fatalf("the old incarnation's state says what leaves it is retired above %v, want above %v", doc.LeavingSince,
			k.held.Position)
	}
	graph := filepath.Join(old, graphRel)
	before := inode(t, graph)

	b := w.over(t, crashed)
	c2 := b.open(t, k.clk.on)
	s := b.sync(t, c2, repo)
	if got := cache.Incarnation(s); got != n || s.Dir != old {
		t.Fatalf("the boot's first round answered incarnation %d at %s, want %d in place", got, s.Dir, n)
	}
	pages := w.log.firstPages(repo)
	if last := pages[len(pages)-1]; !last.Since.IsZero() || last.RetiredSince != k.held.Position {
		t.Errorf("the boot's first round asked from %v for the retirements above %v, want a full answer's above "+
			"the state's floor %v", last.Since, last.RetiredSince, k.held.Position)
	}
	if !statOK(t, graph) || inode(t, graph) != before {
		t.Error("the boot's first round dropped the commit-graph over a merge: its packs were read as packs no row " +
			"accounts for")
	}
	if statOK(t, filepath.Join(old, midxRel)) {
		t.Error("the boot's first round kept the multi-pack-index over the retired packs")
	}
	if got, want := packFiles(t, old), bundleNames(out, k.rows[2]); !slices.Equal(got, want) {
		t.Errorf("after the boot's first round the copy holds %v, want %v", got, want)
	}
	if got := names(t, filepath.Join(crashed, "scratch")); len(got) != 0 {
		t.Errorf("scratch/ holds %v after the boot, want the staged incarnation moved aside", got)
	}
	if doc := readStateDoc(t, old); !doc.LeavingSince.IsZero() {
		t.Errorf("after the boot's first round the state says %v is leaving's floor, want none", doc.LeavingSince)
	}
	if got, want := b.image(t, c2, repo), w.rebuilt(t, repo); got != want {
		t.Errorf("the copy after the boot is\n%s\nwant\n%s", got, want)
	}
}

// A FAILED SWAP SERVES THE OLD INCARNATION AND IS RETRIED. The failing round
// answers from the copy it applied to (every live pack and its refs; the
// staging moved aside, the leaving packs kept and their floor in the state),
// and the next round, here an empty delta, swaps. A swap failing because a
// file it links is gone from the copy (a live pack's, or HEAD, config,
// packed-refs) marks the copy to be read whole, since every swap would link
// it again: the next round is full, restores the file (a pack with one
// download, a root file anew) and swaps.
//
// Mutations: forget the leaving packs on failure; try no swap on a round
// applying nothing new; mark no full read for a gone pack or root file (the
// next delta fails again).
func TestAFailedSwapServesTheOldIncarnationAndIsRetried(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// opts makes the swap fail, and gone is the file of the copy, relative
		// to it, the case removes first: "" for none.
		opts func(*cache.Options)
		gone func(k *compactable) string
		// step is the step the failed swap names; refetch, that the next
		// round downloads the third pack again.
		step    string
		refetch bool
	}{
		{"a link refused", cache.LinkWith(refuseOnce(aPack)), nil, "packs", false},
		{"a live pack's file gone from the copy", nil, func(k *compactable) string {
			return filepath.Join("objects", "pack", "pack-"+k.rows[2].GitChecksum+".rev")
		}, "packs", true},
		{"a root file gone from the copy", nil, func(*compactable) string { return "config" }, "root", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var opts []func(*cache.Options)
			if tc.opts != nil {
				opts = append(opts, tc.opts)
			}
			k := newCompactable(t, false, opts...)
			w, c, repo := k.w, k.c, k.repo
			n := cache.Incarnation(k.held)
			var gone string
			if tc.gone != nil {
				gone = tc.gone(k)
				if err := os.Remove(filepath.Join(k.held.Dir, gone)); err != nil {
					t.Fatal(err)
				}
			}
			out := k.compact(t)
			s := w.sync(t, c, repo)
			if got := cache.Incarnation(s); got != n || s.Dir != k.held.Dir {
				t.Fatalf("the round whose swap failed answered incarnation %d at %s, want the old %d", got, s.Dir, n)
			}
			lines := w.logs.lines(t, "code_cache_swap_failed")
			if len(lines) != 1 || lines[0]["step"] != tc.step || lines[0]["incarnation"] != float64(n) {
				t.Errorf("code_cache_swap_failed = %v, want one line at the %s step of incarnation %d", lines, tc.step,
					n)
			}
			if got := counter(w.rec, metrics.CodeCacheSwaps, "graph", "failed"); got != 1 {
				t.Errorf("%s{graph=failed} = %d, want 1", metrics.CodeCacheSwaps, got)
			}
			leaving := cache.Leaving(c, repo)
			for _, r := range k.rows[:2] {
				if ev := leaving[filepath.Join("objects", "pack", r.GitChecksum)]; ev != cache.EventPackRetired {
					t.Errorf("after the failed swap pack %s is leaving objects/pack by %q, want a retirement",
						r.GitChecksum, ev)
				}
			}
			if got := names(t, w.path("scratch")); len(got) != 0 {
				t.Errorf("scratch/ holds %v after the failed swap, want the staging moved aside", got)
			}
			if doc := readStateDoc(t, s.Dir); doc.LeavingSince != k.held.Position {
				t.Errorf("the state of the copy says what leaves it is retired above %v, want above %v",
					doc.LeavingSince, k.held.Position)
			}
			want := bundleNames(append(slices.Clone(k.rows), out)...)
			want = slices.DeleteFunc(want, func(name string) bool {
				return gone != "" && filepath.Join("objects", "pack", name) == gone
			})
			if got := packFiles(t, s.Dir); !slices.Equal(got, want) {
				t.Errorf("the copy the failed swap left holds %v, want every pack it held: %v", got, want)
			}

			opened, _, _ := w.objects.counts()
			before := opened[k.rows[2].Object]
			s2 := w.sync(t, c, repo)
			if got := cache.Incarnation(s2); got != n+1 {
				t.Fatalf("the next round answered incarnation %d, want the swap into %d", got, n+1)
			}
			if got, want := packFiles(t, s2.Dir), bundleNames(out, k.rows[2]); !slices.Equal(got, want) {
				t.Errorf("the next incarnation holds %v, want %v", got, want)
			}
			if got := cache.Leaving(c, repo); len(got) != 0 {
				t.Errorf("after the swap %v is leaving, want nothing", got)
			}
			if doc := readStateDoc(t, s2.Dir); !doc.LeavingSince.IsZero() {
				t.Errorf("the next incarnation's state says %v is leaving's floor, want none", doc.LeavingSince)
			}
			if gone != "" && !statOK(t, filepath.Join(s2.Dir, gone)) {
				t.Errorf("the next incarnation holds no %s: the round that read the copy whole did not put it back",
					gone)
			}
			opened, _, _ = w.objects.counts()
			if got, want := opened[k.rows[2].Object]-before, map[bool]int{false: 0, true: 1}[tc.refetch]; got != want {
				t.Errorf("the round that swapped downloaded the third pack %d times, want %d", got, want)
			}
			if got := counter(w.rec, metrics.CodeCacheSwaps, "graph", "none"); got != 1 {
				t.Errorf("%s{graph=none} = %d, want the swap tried again, of a copy with no graph",
					metrics.CodeCacheSwaps, got)
			}
		})
	}
}

// refuseOnce is a link that refuses the first call whose new name refuse
// accepts, as a file system at its links' limit does, and makes every other.
func refuseOnce(refuse func(newname string) bool) func(oldname, newname string) error {
	var refused atomic.Bool
	return func(oldname, newname string) error {
		if refuse(newname) && refused.CompareAndSwap(false, true) {
			return &os.LinkError{Op: "link", Old: oldname, New: newname, Err: syscall.EMLINK}
		}
		return os.Link(oldname, newname)
	}
}

// aPack and thePackedRefs are what refuseOnce refuses: a link into a staged
// objects/pack, which the packs step makes, and the packed-refs', which the
// root step makes once every live pack is linked.
func aPack(newname string) bool {
	return filepath.Base(filepath.Dir(newname)) == "pack" && strings.HasPrefix(filepath.Base(newname), "pack-")
}

func thePackedRefs(newname string) bool { return filepath.Base(newname) == "packed-refs" }

// syncLog is every directory a cache told its syncing knob it was about to
// sync (cache.SyncingWith), in order.
type syncLog struct {
	mu   sync.Mutex
	dirs []string
}

func (l *syncLog) add(dir string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dirs = append(l.dirs, dir)
}

func (l *syncLog) reset() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.dirs = nil
}

// swap is what a swap synced, counted by directory: each under a staging in
// scratch/, by its path relative to the staged root ("." for the root), and
// scratch/ and repos/ themselves as "scratch/" and "repos/".
func (l *syncLog) swap(w *syncWorld) map[string]int {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := map[string]int{}
	for _, dir := range l.dirs {
		switch {
		case dir == w.path("repos"):
			out["repos/"]++
		case dir == w.path("scratch"):
			out["scratch/"]++
		default:
			rel, err := filepath.Rel(w.path("scratch"), dir)
			if err != nil {
				continue
			}
			first, rest, _ := strings.Cut(filepath.ToSlash(rel), "/")
			if !strings.HasPrefix(first, "swap.") {
				continue
			}
			if rest == "" {
				rest = "."
			}
			out[rest]++
		}
	}
	return out
}

// A STOPPED RUN COUNTS FOR THE DUTY CYCLE (M3-O15). The swap stops a
// maintenance run on the copy it leaves before linking a derived file, and
// records the stop as the repository's last run (its end, its length so
// far), so the next run waits its interval from the stop: a repository
// compacted more often than its index run lasts is spaced by the duty cycle,
// not due again at once, holding a lane slot forever and writing nothing.
//
// Mutation: record nothing of the stop (stopRun): the next pass runs the
// repository again at once.
func TestAStoppedRunCountsForTheDutyCycle(t *testing.T) {
	t.Parallel()
	var hold atomic.Bool
	reached := make(chan string, 1)
	k := newCompactable(t, true, cache.MidxPacksAt(1), func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			if hold.Load() {
				select {
				case reached <- step:
				default:
				}
				<-ctx.Done()
			}
		})
	})
	w, c, repo := k.w, k.c, k.repo
	w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo)
	k.clk.Add(cache.MidxMinInterval)
	hold.Store(true)
	started, wait := cache.StartMaintainerPass(c)
	if !slices.Equal(started, []string{repo}) {
		t.Fatalf("the maintainer started runs on %v, want %s", started, repo)
	}
	select {
	case <-reached:
	case <-time.After(hangDetector): //fixedwait:liveness
		t.Fatal("the maintenance run never reached its step")
	}

	k.compact(t)
	stopAt := k.clk.Now()
	s := w.sync(t, c, repo)
	if s.Dir == k.held.Dir {
		t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
	}
	if ran := wait(); len(ran) != 0 {
		t.Errorf("the run the swap stopped ran every step: %v", ran)
	}
	if lines := w.logs.lines(t, "code_cache_maintenance_cancelled"); len(lines) != 1 {
		t.Errorf("code_cache_maintenance_cancelled = %v, want the run the swap stopped", lines)
	}
	last, took := cache.Maintained(c, repo)
	if !last.Equal(stopAt) || took <= 0 {
		t.Errorf("the repository's last run ended at %v after %v, want the stop at %v after what the run had spent",
			last, took, stopAt)
	}
	hold.Store(false)
	k.clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("the maintainer ran %v %v after the run the swap stopped, inside its interval", ran,
			cache.MidxDebounce)
	}
	k.clk.Add(cache.MidxMinInterval)
	if ran := cache.MaintainerPass(c); !slices.Equal(ran, []string{repo}) {
		t.Errorf("the maintainer ran %v once the interval from the stop passed, want %s", ran, repo)
	}
	if packs, bitmap, graph := cache.Indexed(c, repo); packs != 3 || !bitmap || !graph {
		t.Errorf("after the next run the copy records an index over %d packs (bitmap %t) and a graph %t, want "+
			"all three packs", packs, bitmap, graph)
	}
}

// A SWAP CARRIES EVERY VERDICT OF THE FILES IT LINKS: a linked file is the
// same inode, its bytes those this process placed, checked or scrubbed. The
// next incarnation keeps the vouches for files it holds (live packs'
// indexes, the output's, the commit-graph) and none other, the last scrub's
// schedule, and a hint's hash of a live pack: a full round hashes nothing, a
// hint about the scrubbed pack hashes nothing again, and the scrub waits its
// interval. The clock moves a second between scrub and swap, so a schedule
// stamped afresh reads the swap's instant, not the scrub's.
//
// Mutations: carry no vouch; carry no packChecks; carry no scrub schedule;
// stamp the last scrub's end, or its interval's base, at the swap.
func TestASwapCarriesEveryVerdictOfTheFilesItLinks(t *testing.T) {
	t.Parallel()
	var (
		mu     sync.Mutex
		hashed []string
		record atomic.Bool
	)
	k := newCompactable(t, true, func(o *cache.Options) {
		cache.WhileHashing(o, func(path string) {
			if record.Load() {
				mu.Lock()
				hashed = append(hashed, path)
				mu.Unlock()
			}
		})
	})
	w, c, repo := k.w, k.c, k.repo
	k.clk.Add(time.Second)
	w.sync(t, c, repo)
	k.clk.Add(cache.ScrubInterval)
	if done := cache.ScrubPass(c); !slices.Equal(done, []string{repo}) {
		t.Fatalf("the scrub pass scrubbed %v, want %s", done, repo)
	}
	scrubbedAt, found, base := cache.Scrubbed(c, repo)
	if scrubbedAt.IsZero() || found {
		t.Fatalf("the scrub ended at %v having found anything %t: the case needs a sound scrub", scrubbedAt, found)
	}
	var log hashLog
	cache.SetHashHook(c, log.hook)
	hint := gitproc.Hint{Object: strings.Repeat("ab", 20), Pack: k.rows[2].GitChecksum}
	if f, ok := c.ConfirmHint(t.Context(), k.held, hint); ok {
		t.Fatalf("a hint about a sound pack: ConfirmHint = %+v, want no repair", f)
	}
	vouched := cache.Vouched(c, repo)

	k.clk.Add(time.Second) // the swap's instant is not the scrub's
	out := k.compact(t)
	s := w.sync(t, c, repo)
	if s.Dir == k.held.Dir {
		t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
	}
	var want []string
	for _, rel := range vouched {
		if statOK(t, filepath.Join(s.Dir, rel)) && sameInode(t, filepath.Join(k.held.Dir, rel),
			filepath.Join(s.Dir, rel)) {
			want = append(want, rel)
		}
	}
	for _, ext := range []string{".idx", ".rev"} {
		want = append(want, filepath.Join("objects", "pack", "pack-"+out.GitChecksum+ext))
	}
	slices.Sort(want)
	for _, rel := range []string{graphRel, filepath.Join("objects", "pack", "pack-"+k.rows[2].GitChecksum+".idx")} {
		if !slices.Contains(want, rel) {
			t.Fatalf("the old incarnation vouched for no %s: the case carries nothing", rel)
		}
	}
	if got := cache.Vouched(c, repo); !slices.Equal(got, want) {
		t.Errorf("the next incarnation vouches for\n%v\nwant every vouch of a file it holds, the same inode:\n%v",
			got, want)
	}
	if at, f, b := cache.Scrubbed(c, repo); !at.Equal(scrubbedAt) || f != found || !b.Equal(base) {
		t.Errorf("the next incarnation was scrubbed at %v (found %t, base %v), want the old one's %v (%t, %v)", at,
			f, b, scrubbedAt, found, base)
	}
	if done := cache.ScrubPass(c); len(done) != 0 {
		t.Errorf("the next incarnation was scrubbed again at once: %v", done)
	}
	if f, ok := c.ConfirmHint(t.Context(), s, hint); ok {
		t.Errorf("a hint about the sound pack over the next incarnation: ConfirmHint = %+v, want no repair", f)
	}
	if got := log.hashed(); !slices.Equal(got, []string{k.rows[2].GitChecksum}) {
		t.Errorf("hint hashes %v, want the one before the swap: its verdict is of the same files", got)
	}

	record.Store(true)
	cache.MarkWhole(c, repo, cache.RebuildShape, 0)
	w.sync(t, c, repo)
	mu.Lock()
	defer mu.Unlock()
	if len(hashed) != 0 {
		t.Errorf("a full round over the next incarnation hashed %v, want nothing: every file it holds is vouched",
			hashed)
	}
}

// A SCRUB OF A COPY THE SWAP LEAVES LANDS ON THE NEXT INCARNATION: the files
// it reads are the next one's inodes, so its verdict (when it ended, nothing
// found) is the next incarnation's, not due again before its interval.
// Dropped with the old one, a scrub of a repository compacted more often
// than a scrub lasts would never land, and be due again at once forever. The
// clock moves a second after the scrub begins and again after the swap, so
// scrub start, swap and scrub end are three instants.
//
// Mutations: record the verdict on the incarnation the scrub read
// (runScrub's serving); stamp the verdict, or its interval's base, at the
// scrub's start.
func TestAScrubOfACopyTheSwapLeavesLandsOnTheNextIncarnation(t *testing.T) {
	t.Parallel()
	hold := newHeldHash()
	k := newCompactable(t, false, func(o *cache.Options) { cache.WhileHashing(o, hold.at) })
	w, c, repo := k.w, k.c, k.repo
	k.clk.Add(time.Second)
	w.sync(t, c, repo)
	k.clk.Add(cache.ScrubInterval)
	hold.armed.Store(filepath.Base(k.held.Dir))
	done := make(chan []string, 1)
	go func() { done <- cache.ScrubPass(c) }()
	select {
	case <-hold.reached:
	case <-time.After(hangDetector): //fixedwait:liveness
		t.Fatal("the scrub never hashed the copy")
	}
	k.clk.Add(time.Second) // the swap's instant is not the scrub's start
	k.compact(t)
	s := w.sync(t, c, repo)
	if s.Dir == k.held.Dir {
		t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
	}
	if at, _, _ := cache.Scrubbed(c, repo); !at.IsZero() {
		t.Fatalf("the next incarnation was scrubbed at %v before the scrub ended", at)
	}
	end := k.clk.Add(time.Second) // nor the scrub's end the swap's
	close(hold.release)
	select {
	case got := <-done:
		if !slices.Equal(got, []string{repo}) {
			t.Errorf("the scrub pass scrubbed %v, want %s", got, repo)
		}
	case <-time.After(hangDetector): //fixedwait:liveness
		t.Fatal("the scrub pass never ended")
	}
	at, found, base := cache.Scrubbed(c, repo)
	if !at.Equal(end) || found || !base.Equal(end) {
		t.Errorf("the next incarnation was scrubbed at %v (found %t, base %v), want the scrub's end %v, nothing "+
			"found, its interval from there", at, found, base, end)
	}
	if why, running := cache.ScrubDue(c, repo); why != "" || running {
		t.Errorf("after the scrub landed the next incarnation's scrub is due for %q (running %t), want only its "+
			"interval", why, running)
	}
}

// A REPORT ABOUT A COPY THE SWAP LEFT REACHES THE COPY SERVING ITS FILES. A
// clone over the old incarnation outlives the swap, and every kept file is
// the same inode, so what its git reports (a pack's damage, a crash, a
// derived file's) is evidence for the next one's SCRUB, due at once, never a
// drop there (a reported multi-pack-index never crosses a swap, so a drop
// would read the copy whole for nothing); a report held for the clone's own
// repair is handed on there at its release. Dropped, a damaged pack both
// copies share would wait for the next day's scrub.
//
// Mutations: route by the run's incarnation (observe's at): all arms drop;
// record a swapped-out derived part as a drop: a repair round is asked; hand
// a held report on only from the current copy (handOn): the held arm is lost.
func TestAReportAboutACopyTheSwapLeftReachesTheCopyServing(t *testing.T) {
	t.Parallel()
	packDamage := func(began time.Time) gitproc.Result {
		return gitproc.Result{Began: began, Exit: 128,
			Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: gitproc.PartPack, Began: began}}
	}
	for _, tc := range []struct {
		name string
		res  func(began time.Time) gitproc.Result
		// held: the run is the reader's own, named on its Cmd.
		held bool
	}{
		{"a pack's damage", packDamage, false},
		{"a crash", func(began time.Time) gitproc.Result {
			return gitproc.Result{Began: began, Exit: -1, Crashed: true}
		}, false},
		{"a multi-pack-index's damage", func(began time.Time) gitproc.Result {
			return derivedReport("", gitproc.PartMultiPackIndex, began).Result
		}, false},
		{"a pack's damage held for the reader", packDamage, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			k := newCompactable(t, true)
			w, c, repo := k.w, k.c, k.repo
			k.compact(t)
			if s := w.sync(t, c, repo); s.Dir == k.held.Dir {
				t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
			}
			if why, running := cache.ScrubDue(c, repo); why != "" || running {
				t.Fatalf("before any report the next incarnation's scrub is due for %q (running %t)", why, running)
			}
			ran := gitproc.Ran{Dir: k.held.Dir, Result: tc.res(time.Now())}
			if tc.held {
				ran.Repairer = k.held.Repairer
			}
			cache.Report(c, ran)
			if tc.held {
				if why, _ := cache.ScrubDue(c, repo); why != "" {
					t.Errorf("a report held for its reader made the scrub due for %q before the reader decided", why)
				}
				k.held.Release()
			}
			if why, _ := cache.ScrubDue(c, repo); why != "report" {
				t.Errorf("after a report about the copy the swap left, the next incarnation's scrub is due for %q, "+
					"want report", why)
			}
			if cache.RepairPending(c, repo) {
				t.Error("a report about the copy the swap left asked a repair round of the copy serving")
			}
		})
	}
}

// A REPORT FROM THE STAGED NEXT INCARNATION IS THE COPY'S. A full round's own
// index is written in the swap's staging (step 5) over the copy's very pack
// inodes, so its git's reports are the copy's scrub evidence: due on the copy
// the swap leaves while staged, and carried over by the promote. A report
// from a staging no swap of the repository is making goes nowhere.
//
// Mutation: route nothing from a staging (observe without swapAt): the
// report is dropped and no scrub is due.
func TestAReportFromTheStagedNextIncarnationIsTheCopys(t *testing.T) {
	t.Parallel()
	faults := &gitletfault.Script{}
	k := newCompactable(t, false, func(o *cache.Options) { o.Faults = faults })
	w, c, repo := k.w, k.c, k.repo
	k.compact(t)
	release, reached := make(chan struct{}), make(chan struct{})
	faults.Arm(gitletfault.SyncSwapBeforeRename, gitletfault.Action{Once: true, Block: release, Reached: reached})
	answered := make(chan cache.Synced, 1)
	go func() {
		s, err := c.Sync(context.Background(), repo, linearizable)
		if err != nil {
			t.Errorf("the round that swaps: %v", err)
		}
		answered <- s
	}()
	select {
	case <-reached:
	case <-time.After(hangDetector): //fixedwait:liveness
		t.Fatal("the round never reached the swap's rename")
	}
	staged := names(t, w.path("scratch"))
	if len(staged) != 1 || !strings.HasPrefix(staged[0], "swap."+repo+".") {
		close(release)
		t.Fatalf("scratch/ holds %v while the swap is staged, want the staged incarnation, named by its repository",
			staged)
	}
	began := time.Now()
	cache.Report(c, gitproc.Ran{Dir: w.path("scratch", "swap."+repo+".0.git"),
		Result: gitproc.Result{Began: began, Exit: -1, Crashed: true}})
	if why, _ := cache.ScrubDue(c, repo); why != "" {
		t.Errorf("a report from a staging no swap is making made the scrub due for %q", why)
	}
	cache.Report(c, gitproc.Ran{Dir: w.path("scratch", staged[0]),
		Result: gitproc.Result{Began: began, Exit: -1, Crashed: true}})
	if why, _ := cache.ScrubDue(c, repo); why != "report" {
		t.Errorf("a crash of a git in the staged next incarnation made the copy's scrub due for %q, want report", why)
	}
	close(release)
	s := <-answered
	defer s.Release()
	if s.Dir == k.held.Dir {
		t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
	}
	if why, _ := cache.ScrubDue(c, repo); why != "report" {
		t.Errorf("after the promote the next incarnation's scrub is due for %q, want the report's, carried", why)
	}
}

// A REQUEST WHOSE COPY THE SWAP REPLACED RUNS ONCE MORE, ITS REPAIR UNSPENT
// (swappedAnswer). A clone pinned the old incarnation, a compaction swapped
// it, and its git then failed there: the swap wrote into the copy after the
// clone began, and the next incarnation holds only the files it kept. So the
// repair is answered from the serving copy AS IT STANDS (Kind SyncNone, read
// by the host as no repair, counted reports{part=pack,outcome=stale}),
// nothing repaired or downloaded; damage the clone's next run meets there, in
// a shared pack, is that copy's to repair in place, with one download.
//
// Mutation: answer a swapped-out copy by a sync (repairRequest's rebuilt or
// purged branch): the host spends its repair on it.
func TestARequestWhoseCopyTheSwapReplacedRunsOnceMoreItsRepairUnspent(t *testing.T) {
	t.Parallel()
	k := newCompactable(t, false)
	w, c, repo := k.w, k.c, k.repo
	k.compact(t)
	s := w.sync(t, c, repo)
	if s.Dir == k.held.Dir {
		t.Fatalf("the round answered the incarnation a reader holds, %s: no swap", s.Dir)
	}
	shared := filepath.Join(s.Dir, "objects", "pack", "pack-"+k.rows[2].GitChecksum+".idx")
	if !sameInode(t, filepath.Join(k.held.Dir, "objects", "pack", "pack-"+k.rows[2].GitChecksum+".idx"), shared) {
		t.Fatal("the next incarnation holds the third pack's index as another file: the case needs it shared")
	}
	flipInPlace(t, shared)
	gets := w.objects.total()

	r, err := c.Rebuild(t.Context(), k.held, cache.Failure{Reason: cache.RebuildCorrupt, Began: time.Now()})
	if err != nil {
		t.Fatalf("Rebuild of the copy the swap replaced: %v", err)
	}
	if r.Kind != cache.SyncNone || r.Dir != s.Dir {
		t.Errorf("the request over the copy the swap replaced was answered %s at %s, want %s as it stands at %s",
			r.Kind, r.Dir, cache.SyncNone, s.Dir)
	}
	if got := reports(w, "pack", "stale"); got != 1 {
		t.Errorf("reports{part=pack,outcome=stale} = %d, want the request's one", got)
	}
	if got := w.objects.total() - gets; got != 0 {
		t.Errorf("answering the request downloaded %d bundles, want none", got)
	}

	again, err := c.Rebuild(t.Context(), r, cache.Failure{Reason: cache.RebuildCorrupt, Began: time.Now()})
	if err != nil {
		t.Fatalf("Rebuild of the copy serving: %v", err)
	}
	defer again.Release()
	r.Release()
	if again.Kind == cache.SyncNone || cache.Incarnation(again) != cache.Incarnation(s) {
		t.Errorf("the request's next failure was answered %s on incarnation %d, want a repair in place on %d",
			again.Kind, cache.Incarnation(again), cache.Incarnation(s))
	}
	if got := w.objects.total() - gets; got != 1 {
		t.Errorf("the repair in place downloaded %d bundles, want the one pack whose index failed", got)
	}
	if bad, err := cache.Unsound(again.Dir); err != nil || len(bad) != 0 {
		t.Errorf("after the repair the copy serving holds %v failing their checksums (%v)", bad, err)
	}
}
