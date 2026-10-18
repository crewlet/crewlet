package cache_test

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A DAMAGED MULTI-PACK-INDEX IS REPAIRED IN PLACE, WITH NO DOWNLOAD. The index
// (its reverse index, its bitmap) is a DERIVED file the maintainer writes from
// the packs the incarnation already holds, so [cache.RebuildDerived] drops it and
// reads the incarnation whole in place — the SAME incarnation, not a byte
// fetched from the object store — where [cache.RebuildCorrupt] reads the whole
// repository into a new directory, downloading every pack. The detection (git
// 2.47's "invalid MIDX object position", which pack-objects reports to the
// trace) is held in internal/code/host; this holds the REPAIR, so it needs no
// git that checks the index and runs on any.
//
// The commit-graph beside the index names no position in it, so the repair
// keeps it: dropped too, every walk of the repository would go by commit date
// until the maintainer had written it again.
//
// Mutation: handle RebuildDerived as a rebuild into a new incarnation (drop its
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
	repaired, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildDerived, Drops: gitproc.PartMultiPackIndex})
	if err != nil {
		t.Fatalf("Rebuild(derived): %v", err)
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
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "derived"); got != 1 {
		t.Errorf("%s{reason=derived} = %d, want one in-place repair", metrics.CodeCacheRebuilds, got)
	}
	if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "corrupt"); got != 0 {
		t.Errorf("%s{reason=corrupt} = %d: a damaged index is no whole-repository rebuild",
			metrics.CodeCacheRebuilds, got)
	}
	if lines := w.logs.lines(t, "code_cache_derived_repaired"); len(lines) != 1 || lines[0]["repo"] != repo {
		t.Errorf("code_cache_derived_repaired = %v, want one line naming %s", lines, repo)
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
// recreated under the node — when a git process names its
// multi-pack-index damaged still has the index dropped by the round
// Rebuild(RebuildDerived) joins, in place and with no download, and the
// maintainer rewrites it. Keyed on the read's reason instead, that round kept
// the damaged index, the fetch's retry met the same damage, and the request
// was answered as a copy still damaged after its repair — until the next
// request, once the mark had cleared.
//
// Mutation: drop the index only when the whole read's reason is derived
// (inc.reread), and both arms fail: the damaged index survives the round and
// the maintainer is not due.
func TestADamagedMidxIsDroppedWhateverElseItsCopyIsReadWholeFor(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		mark   func(c *cache.Cache, repo string)
		reason cache.RebuildReason // what the whole read is counted under
	}{
		"its shape": {func(c *cache.Cache, repo string) { cache.MarkWhole(c, repo, cache.RebuildShape, 0) },
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
			repaired, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildDerived, Drops: gitproc.PartMultiPackIndex})
			if err != nil {
				t.Fatalf("Rebuild(derived): %v", err)
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
			if lines := w.logs.lines(t, "code_cache_derived_repaired"); len(lines) != 1 || lines[0]["repo"] != repo {
				t.Errorf("code_cache_derived_repaired = %v, want one line naming %s", lines, repo)
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

// A ROUND'S OWN GIT MEETING A DAMAGED INDEX DROPS IT IN PLACE. A delta round
// peels the tags it sets with cat-file, which loads the multi-pack-index, and
// an index git cannot read at all — its signature overwritten (F), or a chunk
// it requires missing (F7) — fails that peel with a report of the index's
// format: a DERIVED file. The round marks the incarnation to be read whole
// with the index dropped, and the caller is answered by the next, full round
// in the SAME incarnation — the index gone, no pack fetched again beyond the
// tag's own — counted `derived`. Sent to a rebuild instead, a derived file
// cost a new incarnation and a download of every pack.
//
// And damage that survives the drop: a round that dropped the very files its
// git then names (here a knob writes the damaged index back before the full
// round's peel) asks a whole rebuild, `corrupt`, which replaces the
// incarnation once — rather than marking the same drop again for ever.
//
// Mutations: send RebuildDerived to askRebuild from damagedRound (a new
// incarnation and every pack fetched again); drop the dropped guard (the
// second arm marks the drop again and again, never answering).
func TestARoundsOwnGitMeetingADamagedIndexDropsItInPlace(t *testing.T) {
	t.Parallel()
	damages := map[string]func(t *testing.T, midx string){
		"F its signature overwritten": func(t *testing.T, midx string) {
			overwriteBytes(t, midx, 0, []byte{0xde, 0xad, 0xbe, 0xef})
		},
		"F7 its pack names missing": func(t *testing.T, midx string) {
			gittest.RenameChunk(t, midx, gittest.MidxHeader, "PNAM", "XNAM")
		},
	}
	for name, damage := range damages {
		for _, again := range []bool{false, true} {
			arm := name
			if again {
				arm += "/damaged again after the drop"
			}
			t.Run(arm, func(t *testing.T) {
				t.Parallel()
				w := newSyncWorld(t, onMemory)
				clk := newClock()
				repo := w.repo(t, 1)
				w.push(t, repo, "refs/heads/main", nil)
				var damaged []byte
				var fired atomic.Bool
				var first string
				c := w.open(t, clk.on, func(o *cache.Options) {
					cache.BeforePeel(o, func(dir string) {
						midx := filepath.Join(dir, "objects", "pack", "multi-pack-index")
						if _, err := os.Stat(midx); !again || dir != first || err == nil || damaged == nil ||
							!fired.CompareAndSwap(false, true) {
							return
						}
						if err := os.WriteFile(midx, damaged, 0o444); err != nil {
							t.Error(err)
						}
					})
				})
				s := w.sync(t, c, repo)
				first = s.Dir
				clk.Add(cache.MidxDebounce)
				if ran := cache.MaintainerPass(c); len(ran) != 1 {
					t.Fatalf("the maintainer ran %v, want the repository", ran)
				}
				midx := filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")
				damage(t, midx)
				var err error
				if damaged, err = os.ReadFile(midx); err != nil {
					t.Fatal(err)
				}

				w.tag(t, repo, "v1", true) // a tag object in a pack of its own: the next round peels it
				before := w.objects.total()
				after, err := c.Sync(t.Context(), repo, statelog.Freshness{Level: statelog.ReadLinearizable})
				if err != nil {
					t.Fatalf("the sync whose round met the damaged index: %v", err)
				}
				defer after.Release()

				if !again {
					if cache.Incarnation(after) != cache.Incarnation(s) || after.Dir != s.Dir {
						t.Errorf("answered incarnation %d at %s, want %d in place", cache.Incarnation(after), after.Dir,
							cache.Incarnation(s))
					}
					if got := w.objects.total() - before; got != 1 {
						t.Errorf("the sync fetched %d objects, want the tag's pack alone", got)
					}
					if _, err := os.Stat(midx); !errors.Is(err, fs.ErrNotExist) {
						t.Errorf("the damaged index survived the round its own git met it in: %v", err)
					}
					if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "derived"); got != 1 {
						t.Errorf("%s{reason=derived} = %d, want the one drop in place", metrics.CodeCacheRebuilds, got)
					}
					if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "corrupt"); got != 0 {
						t.Errorf("%s{reason=corrupt} = %d: a derived file is no whole-repository rebuild",
							metrics.CodeCacheRebuilds, got)
					}
					return
				}
				if !fired.Load() {
					t.Fatal("the knob never wrote the damaged index back: the case tested nothing")
				}
				if got := cache.Incarnation(after); got != cache.Incarnation(s)+1 {
					t.Errorf("answered incarnation %d, want %d — one rebuild", got, cache.Incarnation(s)+1)
				}
				if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "corrupt"); got != 1 {
					t.Errorf("%s{reason=corrupt} = %d, want the one rebuild damage surviving its drop asks",
						metrics.CodeCacheRebuilds, got)
				}
				if _, err := runGit(t, w.pool, after.Dir, "", "rev-list", "--objects", "--all"); err != nil {
					t.Errorf("rev-list over the rebuilt incarnation: %v", err)
				}
			})
		}
	}
}

// A DAMAGED GRAPH IS DROPPED IN PLACE AND ITS INDEX KEPT. A commit-graph whose
// commits name a parent past the graph (I8: every first-parent field
// 0x00ffffff) is fatal to any walk (`invalid parent position %u`, a
// commit-graph format): the repair drops the GRAPH alone, the
// multi-pack-index and its bitmap left on disk, no pack fetched, counted
// `derived`, and the maintainer's next pass writes the graph again (a copy
// read whole with none gets its graph first). A chunk-format message names
// neither file — a graph whose table of contents has its first id zeroed
// (I5′: `terminating chunk id appears earlier than expected`) — so its
// repair drops BOTH.
//
// Each repair is asked with what a real git run over the damaged copy was
// classified as (cache.DamageReason: its Reason and Drops), so the case holds
// the classification and the drop together.
//
// Mutations: make dropCommitGraph a no-op (the I8 arm keeps its damaged
// graph); have PartChunk drop the index alone (the I5′ arm keeps its graph).
func TestADamagedGraphIsDroppedInPlaceAndItsIndexKept(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		damage    func(t *testing.T, graph string)
		indexKept bool
	}{
		{"I8 every parent past the graph", func(t *testing.T, graph string) {
			start, end := gittest.GraphChunk(t, graph, "CDAT")
			const entry = 20 + 4 + 4 + 8 // tree, first parent, second parent, generation and date
			for at := start; at+entry <= end; at += entry {
				overwriteBytes(t, graph, at+20, []byte{0x00, 0xff, 0xff, 0xff})
			}
		}, true},
		{"I5′ its first chunk id zeroed", func(t *testing.T, graph string) {
			overwriteBytes(t, graph, gittest.ChunkRow(gittest.GraphHeader, 0), []byte{0, 0, 0, 0})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t, clk.on)
			s := w.sync(t, c, repo)
			clk.Add(cache.MidxDebounce)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				t.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			packDir := filepath.Join(s.Dir, "objects", "pack")
			bitmap := bitmapOf(t, packDir)
			midx := filepath.Join(packDir, "multi-pack-index")
			graph := filepath.Join(s.Dir, "objects", "info", "commit-graph")
			tc.damage(t, graph)

			res, err := w.pool.Run(t.Context(), gitproc.Cmd{Dir: s.Dir, Args: []string{"rev-list", "--all"},
				Class: gitproc.ClassRead, Deadline: time.Minute})
			if err == nil {
				err = res.Damage
			}
			f, ok := cache.DamageReason(err)
			if !ok || f.Reason != cache.RebuildDerived {
				t.Fatalf("rev-list over the damaged graph read as %q, %v (%v), want derived", f.Reason, ok, err)
			}
			before := w.objects.total()
			repaired, err := c.Rebuild(t.Context(), s, f)
			if err != nil {
				t.Fatalf("Rebuild(derived): %v", err)
			}
			defer repaired.Release()

			if cache.Incarnation(repaired) != cache.Incarnation(s) || repaired.Dir != s.Dir {
				t.Errorf("the repair answered incarnation %d, want %d in place", cache.Incarnation(repaired),
					cache.Incarnation(s))
			}
			if got := w.objects.total() - before; got != 0 {
				t.Errorf("the repair fetched %d objects, want none", got)
			}
			if _, err := os.Stat(graph); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the damaged graph survived its repair: %v", err)
			}
			for _, path := range []string{midx, bitmap} {
				if _, err := os.Stat(path); (err == nil) != tc.indexKept {
					t.Errorf("%s after the repair: %v, want kept %v", filepath.Base(path), err, tc.indexKept)
				}
			}
			if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "derived"); got != 1 {
				t.Errorf("%s{reason=derived} = %d, want the one drop", metrics.CodeCacheRebuilds, got)
			}
			if lines := w.logs.lines(t, "code_cache_derived_repaired"); len(lines) != 1 {
				t.Errorf("code_cache_derived_repaired = %v, want one line", lines)
			}

			// Graph-first: the maintainer's next pass writes it again.
			cache.MaintainerPass(c)
			if _, err := os.Stat(graph); err != nil {
				t.Errorf("the maintainer's pass after the repair wrote no graph: %v", err)
			}
			if _, err := runGit(t, w.pool, repaired.Dir, "", "rev-list", "--all"); err != nil {
				t.Errorf("rev-list over the repaired copy: %v", err)
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
