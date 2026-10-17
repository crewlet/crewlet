package cache_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A DAMAGED MULTI-PACK-INDEX IS REPAIRED IN PLACE, WITH NO DOWNLOAD. The index
// (its reverse index, its bitmap) is a DERIVED file the maintainer writes from
// the packs the incarnation already holds, so [cache.RebuildMidx] drops it and
// reads the incarnation whole in place — the SAME incarnation, not a byte
// fetched from the object store — where [cache.RebuildCorrupt] reads the whole
// repository into a new directory, downloading every pack. The detection (git
// 2.47's "invalid MIDX object position", which a fetch's side band carries) is
// held in internal/code/host; this holds the REPAIR, so it needs no git that
// checks the index and runs on any.
//
// The commit-graph beside the index names no position in it, so the repair
// keeps it: dropped too, every walk of the repository would go by commit date
// until the maintainer had written it again.
//
// Mutation: handle RebuildMidx as a rebuild into a new incarnation (drop its
// inPlace branch), and the incarnation moves and the GET count rises; drop the
// dropMultiPackIndex step (apply), and the damaged index survives the repair;
// drop the commit-graph with it (dropIndexes), and the graph is gone.
func TestADamagedMidxIsRepairedInPlace(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	defer s.Release()

	// The maintainer writes the multi-pack-index with its bitmap, and the
	// commit-graph.
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want the repository", ran)
	}
	packDir := filepath.Join(s.Dir, "objects", "pack")
	bitmapOf(t, packDir)
	graph := filepath.Join(s.Dir, "objects", "info", "commit-graph")
	if _, err := os.Stat(graph); err != nil {
		t.Fatalf("the maintainer wrote no commit-graph: %v", err)
	}

	// Overwrite the reverse-index chunk past the index's objects — the damage
	// git reads through the bitmap as "invalid MIDX object position".
	midx := filepath.Join(packDir, "multi-pack-index")
	start, end := gittest.MidxChunk(t, midx, "RIDX")
	overwriteBytes(t, midx, start, bytes.Repeat([]byte{0xff}, int(end-start)))

	before := w.objects.total()
	repaired, err := c.Rebuild(t.Context(), s, cache.RebuildMidx)
	if err != nil {
		t.Fatalf("Rebuild(midx): %v", err)
	}
	defer repaired.Release()

	if cache.Incarnation(repaired) != cache.Incarnation(s) || repaired.Dir != s.Dir {
		t.Errorf("the midx repair answered incarnation %d at %s, want %d in place", cache.Incarnation(repaired),
			repaired.Dir, cache.Incarnation(s))
	}
	if got := w.objects.total() - before; got != 0 {
		t.Errorf("the midx repair downloaded %d objects, want none — the packs were already held", got)
	}
	// The damaged index is gone, and the repository reads: the round left the
	// multi-pack-index for the maintainer (one pack is below MidxRoundPacks),
	// and git reads the packs directly meanwhile.
	if _, err := runGit(t, w.pool, repaired.Dir, "", "rev-list", "--objects", "--all"); err != nil {
		t.Errorf("rev-list over the repaired incarnation: %v", err)
	}
	if _, err := os.Stat(graph); err != nil {
		t.Errorf("the midx repair dropped the commit-graph beside the index: %v", err)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "midx"); got != 1 {
		t.Errorf("%s{reason=midx} = %d, want one in-place repair", metrics.CodeCacheRebuilds, got)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "corrupt"); got != 0 {
		t.Errorf("%s{reason=corrupt} = %d: a damaged index is no whole-repository rebuild",
			metrics.CodeCacheRebuilds, got)
	}
	if lines := w.logs.lines(t, "code_cache_midx_repaired"); len(lines) != 1 || lines[0]["repo"] != repo {
		t.Errorf("code_cache_midx_repaired = %v, want one line naming %s", lines, repo)
	}

	// The maintainer rewrites a sound multi-pack-index with its bitmap, over
	// the same packs — it is due because the repair left none.
	clk.Add(cache.MidxMaxWait)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v after the repair, want the repository", ran)
	}
	bitmap := bitmapOf(t, packDir)
	if out, err := runGit(t, w.pool, repaired.Dir, "", "rev-list", "--objects", "--use-bitmap-index",
		"--count", "--all"); err != nil {
		t.Errorf("a bitmapped walk over the rewritten index: %v", err)
	} else if out == "" {
		t.Errorf("the bitmapped walk over %s counted nothing", bitmap)
	}
}

// A MULTI-PACK-INDEX NAMED DAMAGED IS DROPPED WHATEVER ELSE ITS COPY IS BEING
// READ WHOLE FOR. Dropping it is a mark of its own on the incarnation, never
// the reason its whole read is counted under: a copy already marked to be read
// whole — for its shape, by a request beside this one, or for a code log
// recreated under the node — when a fetch's side band names its
// multi-pack-index still has the index dropped by the round
// Rebuild(RebuildMidx) joins, in place and with no download, and the
// maintainer rewrites it. Keyed on the read's reason instead, that round kept
// the damaged index, the fetch's retry met the same damage, and the request
// was answered as a copy still damaged after its repair — until the next
// request, once the mark had cleared.
//
// Mutation: drop the index only when the whole read's reason is midx
// (inc.reread), and both arms fail: the damaged index survives the round and
// the maintainer is not due.
func TestADamagedMidxIsDroppedWhateverElseItsCopyIsReadWholeFor(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mark   func(c *cache.Cache, repo string)
		reason cache.RebuildReason // what the whole read is counted under
	}{
		"its shape": {func(c *cache.Cache, repo string) { cache.MarkWhole(c, repo, cache.RebuildShape) },
			cache.RebuildShape},
		"a log recreated under it": {func(c *cache.Cache, _ string) { cache.Reread(c, cache.RebuildStream) },
			cache.RebuildStream},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t, clk.on)
			s := w.sync(t, c, repo)
			defer s.Release()
			clk.Add(cache.MidxDebounce)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				t.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			packDir := filepath.Join(s.Dir, "objects", "pack")
			bitmapOf(t, packDir)
			midx := filepath.Join(packDir, "multi-pack-index")
			start, end := gittest.MidxChunk(t, midx, "RIDX")
			overwriteBytes(t, midx, start, bytes.Repeat([]byte{0xff}, int(end-start)))

			tc.mark(c, repo)
			before := w.objects.total()
			repaired, err := c.Rebuild(t.Context(), s, cache.RebuildMidx)
			if err != nil {
				t.Fatalf("Rebuild(midx): %v", err)
			}
			defer repaired.Release()

			if cache.Incarnation(repaired) != cache.Incarnation(s) || repaired.Dir != s.Dir {
				t.Errorf("the midx repair answered incarnation %d at %s, want %d in place",
					cache.Incarnation(repaired), repaired.Dir, cache.Incarnation(s))
			}
			if got := w.objects.total() - before; got != 0 {
				t.Errorf("the midx repair downloaded %d objects, want none", got)
			}
			// One pack is below MidxRoundPacks, so the round writes no index
			// of its own: the damaged one is gone, not rewritten.
			if _, err := os.Stat(midx); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the damaged multi-pack-index survived the round its repair joined: %v", err)
			}
			if lines := w.logs.lines(t, "code_cache_midx_repaired"); len(lines) != 1 || lines[0]["repo"] != repo {
				t.Errorf("code_cache_midx_repaired = %v, want one line naming %s", lines, repo)
			}
			// ONE whole read, counted under the reason it was first marked
			// for; none of it a rebuild into a new incarnation.
			if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(tc.reason)); got != 1 {
				t.Errorf("%s{reason=%s} = %d, want the one whole read", metrics.CodeCacheRebuilds, tc.reason, got)
			}
			if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "corrupt"); got != 0 {
				t.Errorf("%s{reason=corrupt} = %d: a damaged index is no whole-repository rebuild",
					metrics.CodeCacheRebuilds, got)
			}

			clk.Add(cache.MidxMaxWait)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				t.Fatalf("the maintainer ran %v after the repair, want the repository", ran)
			}
			bitmapOf(t, packDir)
			if _, err := runGit(t, w.pool, repaired.Dir, "", "rev-list", "--objects", "--use-bitmap-index",
				"--count", "--all"); err != nil {
				t.Errorf("a bitmapped walk over the rewritten index: %v", err)
			}
		})
	}
}

// overwriteBytes writes b over path at offset at, making the file writable
// first: git writes its files read-only.
func overwriteBytes(t *testing.T, path string, at int64, b []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteAt(b, at); err != nil {
		t.Fatal(err)
	}
}
