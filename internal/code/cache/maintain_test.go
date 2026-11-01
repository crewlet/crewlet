package cache_test

import (
	"context"
	"encoding/binary"
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

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// caseRoundPacks is the round's index threshold a case lowers
// MidxRoundPacks to (cache.RoundPacksAt), so a handful of pushes exercises
// the round's own index rather than hundreds.
const caseRoundPacks = 8

// A FULL ROUND INDEXES MANY PACKS BEFORE IT ANSWERS: one push is one pack
// until the repository's home merges them, and a repository its compactions
// fall behind holds up to code.MaxPacksPerRepo, so the request a cold cache
// was synced for would otherwise be served by git searching every pack's
// index for every object.
// The round's index has no bitmap — a merge of the packs' own indexes — and
// a busy node answers without it rather than wait: slower, never wrong. The
// bitmap is the maintainer's, due as soon as the packs have stood still.
// Under MidxRoundPacks the round writes none: there the index costs the cold
// request more than it saves it (BenchmarkPackLookup), and the maintainer's
// own follows once the packs have stood still and the runs queued before it
// have ended.
func TestAFullRoundIndexesManyPacksBeforeItAnswers(t *testing.T) {
	t.Parallel()
	t.Run("at the threshold", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		clk := newClock()
		repo := w.repo(t, 1)
		for range caseRoundPacks {
			w.push(t, repo, "refs/heads/main", nil)
		}
		c := w.open(t, clk.on, cache.RoundPacksAt(caseRoundPacks))
		s := w.sync(t, c, repo)
		packDir := filepath.Join(s.Dir, "objects", "pack")
		if !statOK(t, filepath.Join(packDir, "multi-pack-index")) {
			t.Fatalf("a full round of %d packs, its threshold, answered with no multi-pack-index", caseRoundPacks)
		}
		for _, name := range names(t, packDir) {
			if filepath.Ext(name) == ".bitmap" {
				t.Errorf("the round's index has a bitmap (%s): that is the maintainer's walk", name)
			}
		}
		doc := readStateDoc(t, s.Dir)
		if midx := doc.midx(); midx.Covers != caseRoundPacks || midx.Step != string(cache.StepIndex) ||
			!doc.Maintained.LastRun.IsZero() || midx.Bytes == 0 {
			t.Errorf("the state records %+v and %+v, want an index over %d packs without a bitmap, its bytes, and "+
				"no maintenance run", doc.Derived, doc.Maintained, caseRoundPacks)
		}
		if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
			t.Errorf("fsck over the round's index: %v", err)
		}
		dst := filepath.Join(t.TempDir(), "clone.git")
		w.client.Run(t, "", "-c", "protocol.version=2", "clone", "--bare", "--no-local", s.Dir, dst)

		clk.Add(cache.MidxDebounce)
		if ran := cache.MaintainerPass(c); len(ran) != 1 {
			t.Fatalf("the maintainer ran %v over a round's index with no bitmap, want the repository", ran)
		}
		if midx := readStateDoc(t, s.Dir).midx(); midx.Covers != caseRoundPacks || midx.Step != string(cache.StepBitmap) {
			t.Errorf("after the maintainer the state's index is %+v, want %d packs with the bitmap", midx, caseRoundPacks)
		}
		bitmapOf(t, filepath.Join(s.Dir, "objects", "pack"))
	})
	t.Run("before the peel", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		for range caseRoundPacks {
			w.push(t, repo, "refs/heads/main", nil)
		}
		w.tag(t, repo, "v1", true)
		var indexed, peeled bool
		c := w.open(t, cache.RoundPacksAt(caseRoundPacks), func(o *cache.Options) {
			cache.BeforePeel(o, func(dir string) {
				peeled = true
				indexed = exists(filepath.Join(dir, "objects", "pack", "multi-pack-index"))
			})
		})
		w.sync(t, c, repo)
		if !peeled || !indexed {
			t.Errorf("the peel ran (%t) over an incarnation with an index (%t): the round's index comes first, "+
				"so the peel's every lookup does not search each pack in turn", peeled, indexed)
		}
	})
	t.Run("one pack fewer", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		for range caseRoundPacks - 1 {
			w.push(t, repo, "refs/heads/main", nil)
		}
		c := w.open(t, cache.RoundPacksAt(caseRoundPacks))
		s := w.sync(t, c, repo)
		if statOK(t, filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")) {
			t.Errorf("a full round of %d packs, one under its threshold, wrote an index", caseRoundPacks-1)
		}
	})
	t.Run("under MidxRoundPacks", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		packs := 2 * caseRoundPacks
		if packs >= cache.MidxRoundPacks {
			t.Fatalf("MidxRoundPacks %d is no more than %d packs: the case needs a count under it",
				cache.MidxRoundPacks, packs)
		}
		for range packs {
			w.push(t, repo, "refs/heads/main", nil)
		}
		c := w.open(t)
		s := w.sync(t, c, repo)
		if statOK(t, filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")) {
			t.Errorf("a full round of %d packs, under MidxRoundPacks (%d), wrote an index", packs, cache.MidxRoundPacks)
		}
	})
	t.Run("every read slot held", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		for range caseRoundPacks {
			w.push(t, repo, "refs/heads/main", nil)
		}
		c := w.open(t, cache.RoundPacksAt(caseRoundPacks))
		for range w.pool.Slots(gitproc.ClassRead) {
			holdSlot(t, w.pool, gitproc.ClassRead)
		}
		s := w.sync(t, c, repo)
		if statOK(t, filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")) {
			t.Error("a round with no read slot free wrote an index")
		}
		if len(w.logs.lines(t, "code_cache_round_index_skipped")) != 1 {
			t.Error("no code_cache_round_index_skipped line")
		}
	})
}

// graphFirst runs the graph-first runs that every copy a round has just read
// whole with no commit-graph is due for at once (GRAPH FIRST, maintain.go) —
// ahead of every index, each in a slot of its own — and waits for them,
// failing t unless they ran on exactly repos, before a case goes on to the
// ORDINARY runs it is about. The cache's clock does not move.
func graphFirst(t *testing.T, c *cache.Cache, repos ...string) {
	t.Helper()
	var ran []string
	for range len(repos) + 1 {
		got := cache.MaintainerPass(c)
		if len(got) == 0 {
			break
		}
		ran = append(ran, got...)
	}
	slices.Sort(ran)
	if want := slices.Sorted(slices.Values(repos)); !slices.Equal(ran, want) {
		t.Fatalf("the graph-first passes ran %v, want %v", ran, want)
	}
}

// holdSlot holds one slot of class in pool until the case ends: a git process
// reading a stdin that gives nothing until then.
func holdSlot(t *testing.T, pool *gitproc.Pool, class gitproc.Class) {
	t.Helper()
	holdSlotUntil(t, pool, class)
}

// blocking is a stdin that says it was read, then gives nothing until
// released, then ends.
type blocking struct {
	started, release chan struct{}
	once             bool
}

func (b *blocking) Read([]byte) (int, error) {
	if !b.once {
		b.once = true
		close(b.started)
	}
	<-b.release
	return 0, os.ErrClosed
}

// A ROUND NEVER REPLACES AN INDEX THAT EXISTS: git deletes a bitmap that does
// not match the index it writes, and the index on disk may be the
// maintainer's — so a full round asks the FILE, not the state, which reads
// none beside an index on disk when it was unreadable. A pack the round will
// sweep takes the index with it, and the round writes one of its own over the
// packs it keeps — as many as its threshold, which the case lowers — in the
// copy it answers, the next incarnation where a reader holds the old one
// (swap.go), to which the maintainer adds the bitmap.
func TestARoundNeverReplacesAnIndexThatExists(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	for range caseRoundPacks {
		w.push(t, repo, "refs/heads/main", nil)
	}
	before := w.log.state()
	_, last := w.push(t, repo, "refs/heads/main", nil)
	round := cache.RoundPacksAt(caseRoundPacks)
	c := w.open(t, clk.on, round)
	s := w.sync(t, c, repo)
	packDir := filepath.Join(s.Dir, "objects", "pack")
	midx := filepath.Join(packDir, "multi-pack-index")
	// The maintainer's index, with its bitmap — written here by the client's
	// git, as the maintainer's run writes it. git keeps an identical index
	// it is asked to write again, and deletes the bitmap beside it.
	w.client.Run(t, s.Dir, "-c", "core.multiPackIndex=true", "multi-pack-index", "write", "--bitmap", "--no-progress")
	bitmap := bitmapOf(t, packDir)
	first := inode(t, midx)

	c = w.reopen(t, c, clk.on, round)
	w.sync(t, c, repo)
	if inode(t, midx) != first || !statOK(t, bitmap) {
		t.Error("a full round after a restart replaced the index on disk, or its bitmap")
	}

	write(t, filepath.Join(s.Dir, "crewlet-state.json"), `{"format":`)
	c = w.reopen(t, c, clk.on, round)
	w.sync(t, c, repo)
	if inode(t, midx) != first || !statOK(t, bitmap) {
		t.Error("a full round with an unreadable state replaced the index on disk, or its bitmap")
	}

	// The reader the last sync handed out holds the copy, so the pack the
	// recreated log does not name leaves it by the swap: the round's own
	// index is written into the next incarnation, which it answers.
	w.log.recreate(before)
	s = w.sync(t, c, repo)
	if statOK(t, filepath.Join(s.Dir, "objects", "pack", "pack-"+last.GitChecksum+".idx")) {
		t.Fatal("the pack the recreated log does not name is still installed")
	}
	midx = filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")
	if !statOK(t, midx) || inode(t, midx) == first {
		t.Error("a full round that swept a pack under the index did not write an index of its own")
	}
	if got := bitmaps(t, s.Dir); len(got) != 0 {
		t.Errorf("the bitmap of an index whose pack was swept is still there: %v", got)
	}
	if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
		t.Errorf("fsck after a pack was swept from under the index: %v", err)
	}

	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v after the round's own index, want the repository", ran)
	}
	bitmapOf(t, filepath.Join(s.Dir, "objects", "pack"))
	if midx := readStateDoc(t, s.Dir).midx(); midx.Covers != caseRoundPacks || midx.Step != string(cache.StepBitmap) {
		t.Errorf("after the maintainer the state's index is %+v, want %d packs with the bitmap", midx, caseRoundPacks)
	}
}

// bitmapOf is the multi-pack-index bitmap in packDir; it fails t when there
// is not exactly one.
func bitmapOf(t *testing.T, packDir string) string {
	t.Helper()
	var found []string
	for _, name := range names(t, packDir) {
		if filepath.Ext(name) == ".bitmap" {
			found = append(found, filepath.Join(packDir, name))
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s holds bitmaps %v, want one", packDir, found)
	}
	return found[0]
}

// THE MAINTAINER'S BITMAP CARRIES ITS LOOKUP TABLE: a table of the commits it
// holds a bitmap for, so a reader loads the bitmap of a commit only when it is
// asked for one. Without it every upload-pack that opens the bitmap reads
// every commit's entry before it answers — a blobless clone's lazy fetch of
// one blob included. Read from the file git wrote: the header's option word,
// bytes 6 and 7 big-endian, carries BITMAP_OPT_LOOKUP_TABLE (0x10). And the
// arm BenchmarkMidx prices it against is a bitmap WITHOUT the table, so the
// switch it flips is held here to writing one.
func TestTheMaintainersBitmapCarriesItsLookupTable(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	for range 3 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	defer s.Release()
	packDir := filepath.Join(s.Dir, "objects", "pack")
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want the repository", ran)
	}
	if opts := bitmapOptions(t, bitmapOf(t, packDir)); opts&bitmapLookupTable == 0 {
		t.Errorf("the maintainer's bitmap's options are %#04x: no lookup table (%#04x)", opts, bitmapLookupTable)
	}

	cache.SetBitmapLookupTable(c, false)
	if err := cache.ForgetMaintenance(c, repo); err != nil {
		t.Fatal(err)
	}
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want the repository", ran)
	}
	if opts := bitmapOptions(t, bitmapOf(t, packDir)); opts&bitmapLookupTable != 0 {
		t.Errorf("with the table switched off the bitmap's options are %#04x: the benchmark's arm without "+
			"the table would price the table", opts)
	}
}

// bitmapLookupTable is git's BITMAP_OPT_LOOKUP_TABLE, a bit of a bitmap
// header's option word.
const bitmapLookupTable = 0x10

// bitmapOptions is the option word of the bitmap at path: after the "BITM"
// signature and the version, bytes 6 and 7, big-endian.
func bitmapOptions(t *testing.T, path string) uint16 {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 8 || string(raw[:4]) != "BITM" {
		t.Fatalf("%s is no bitmap: it begins %q", path, raw[:min(len(raw), 8)])
	}
	return binary.BigEndian.Uint16(raw[6:8])
}

// inode is path's inode number: whether a file is the same file, not merely
// the same bytes.
func inode(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Sys().(*syscall.Stat_t).Ino
}

// THE MAINTAINER INDEXES AFTER A BURST: once a repository's packs have stood
// still for MidxDebounce it writes a multi-pack-index with its bitmap and a
// commit-graph, which git serves; then nothing for MidxMinInterval, and
// nothing while a burst is still landing — the debounce, not the count, is
// what holds a due repository back. Its commit-graph is written before all
// of that, at once (graph first), with no index.
func TestTheMaintainerIndexesAfterABurst(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	for range 9 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	graphFirst(t, c, repo)
	if statOK(t, filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")) {
		t.Fatal("the maintainer indexed packs a round just added before they had stood still")
	}
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Fatalf("the maintainer ran %v while the packs a round just added had not stood still", ran)
	}
	ranAt := clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != repo {
		t.Fatalf("the maintainer ran %v, want %s", ran, repo)
	}
	packDir := filepath.Join(s.Dir, "objects", "pack")
	bitmapOf(t, packDir)
	if !statOK(t, filepath.Join(packDir, "multi-pack-index")) || !statOK(t, filepath.Join(s.Dir, "objects", "info", "commit-graph")) {
		t.Fatal("the maintainer left no multi-pack-index or no commit-graph")
	}
	if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
		t.Errorf("fsck over the maintainer's files: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "clone.git")
	w.client.Run(t, "", "-c", "protocol.version=2", "clone", "--bare", "--no-local", s.Dir, dst)
	doc := readStateDoc(t, s.Dir)
	midx, graph := doc.midx(), doc.graph()
	if midx.Covers != 9 || midx.Step != string(cache.StepBitmap) || graph.Step != string(cache.StepGraph) ||
		midx.Bytes == 0 || graph.Bytes == 0 || !doc.Maintained.LastRun.Equal(ranAt) ||
		doc.derivedBytes() != heldBy(t, s.Dir)-doc.DiskBytes {
		t.Errorf("the persisted derived files are %+v and the run %+v, want an index over 9 packs with the bitmap "+
			"and the graph, each with its bytes and together all the maintainer's, and the last run at %v",
			doc.Derived, doc.Maintained, ranAt)
	}
	if got := w.logs.lines(t, "code_cache_maintained"); len(got) != 2 || got[0]["first"] != true ||
		got[1]["first"] != false || got[1]["packs"] != float64(9) || got[1]["index"] != "bitmap" {
		t.Errorf("code_cache_maintained = %v, want the graph-first run's line and the bitmap's over 9 packs", got)
	}

	clk.Add(time.Minute)
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("the maintainer ran %v inside MidxMinInterval", ran)
	}

	// MidxPacks more packs make it due by count; landed just before the
	// interval is up, they hold it back until they have stood still.
	clk.Set(ranAt.Add(cache.MidxMinInterval - cache.MidxDebounce/2))
	for range cache.MidxPacks {
		w.push(t, repo, "refs/heads/main", nil)
	}
	w.sync(t, c, repo)
	clk.Set(ranAt.Add(cache.MidxMinInterval))
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("the maintainer ran %v while a burst was still landing", ran)
	}
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Errorf("the maintainer ran %v once the burst stood still, want the repository", ran)
	}
}

// A REPOSITORY THAT NEVER STANDS STILL IS STILL MAINTAINED: pushed to more
// often than every MidxDebounce, its packs never stand still, and a debounce
// with no ceiling would leave every one of them outside any index for as long
// as the pushes kept coming — every lookup a search of each pack's own .idx.
// So the oldest pack no index covers waits MidxMaxWait at most, however
// recent the newest is: the one run the interval allows, at exactly that
// bound and not a pass sooner.
func TestARepositoryThatNeverStandsStillIsStillMaintained(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, repo).Release()
	graphFirst(t, c, repo)
	first := clk.Now() // the round added the packs no index covers
	var ranAt []time.Time
	for clk.Now().Before(first.Add(cache.MidxMaxWait + cache.MidxDebounce)) {
		clk.Add(cache.MidxDebounce - time.Second)
		w.push(t, repo, "refs/heads/main", nil)
		w.sync(t, c, repo).Release()
		if ran := cache.MaintainerPass(c); len(ran) == 1 {
			ranAt = append(ranAt, clk.Now())
		}
	}
	if len(ranAt) != 1 {
		t.Fatalf("pushed every %v for %v, the maintainer ran at %v; want once, the first pass at or past "+
			"MidxMaxWait (%v) after the first pack it did not cover", cache.MidxDebounce-time.Second,
			cache.MidxMaxWait+cache.MidxDebounce, ranAt, cache.MidxMaxWait)
	}
	if waited := ranAt[0].Sub(first); waited < cache.MidxMaxWait || waited >= cache.MidxMaxWait+cache.MidxDebounce {
		t.Errorf("the run came %v after the first pack it covers, want within a push of MidxMaxWait (%v)",
			waited, cache.MidxMaxWait)
	}
}

// ONE PACK IS INDEXED ONCE, AND MIDXPACKS MORE AGAIN: the bitmap and the
// commit-graph are what a clone counts objects from and what a walk reads
// commits from, and neither needs a second pack to be worth having — a
// history imported in one push is one pack, and without them every clone of
// it walks the whole history. So a repository with no index is due at its
// first pack, and one with the maintainer's is due again only once MidxPacks
// packs have gathered beside it — fewer cost a request only the walk of their
// own commits and trees, a twentieth of a millisecond a pack, against a run
// that rewrites the whole index. An empty repository has nothing to index.
func TestOnePackIsIndexedOnceAndMidxPacksMoreAgain(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	empty, repo := w.repo(t, 1), w.repo(t, 2)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, empty).Release()
	s := w.sync(t, c, repo)
	defer s.Release()
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != repo {
		t.Fatalf("the maintainer ran %v, want the repository of one pack and not the empty one", ran)
	}
	bitmapOf(t, filepath.Join(s.Dir, "objects", "pack"))
	if !statOK(t, filepath.Join(s.Dir, "objects", "info", "commit-graph")) {
		t.Error("the run over one pack left no commit-graph")
	}
	if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
		t.Errorf("fsck over an index of one pack: %v", err)
	}

	for range cache.MidxPacks - 1 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	w.sync(t, c, repo).Release()
	clk.Add(cache.MidxMinInterval)
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("the maintainer ran %v with %d packs beside its index", ran, cache.MidxPacks-1)
	}
	w.push(t, repo, "refs/heads/main", nil)
	w.sync(t, c, repo).Release()
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Errorf("the maintainer ran %v with %d packs beside its index, want the repository", ran, cache.MidxPacks)
	}
}

// THE DUTY CYCLE BOUNDS A HEAVY REPOSITORY: a run that took two minutes is
// not followed by another for twenty — MidxDutyCycle times as long — however
// soon MidxMinInterval allows one, so no repository takes more than a tenth of
// a core's time. What the last run took survives a restart in the state.
func TestTheDutyCycleBoundsAHeavyRepository(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	ranAt := clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want the repository", ran)
	}
	c.Stop(context.Background())
	doc := readStateDoc(t, s.Dir)
	doc.Maintained.Took = 2 * time.Minute
	writeState(t, s.Dir, doc)

	c = w.open(t, clk.on)
	for range cache.MidxPacks {
		w.push(t, repo, "refs/heads/main", nil)
	}
	w.sync(t, c, repo)
	clk.Set(ranAt.Add(cache.MidxMinInterval))
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("the maintainer ran %v %v after a run that took two minutes", ran, cache.MidxMinInterval)
	}
	clk.Set(ranAt.Add(cache.MidxDutyCycle * 2 * time.Minute))
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Errorf("the maintainer ran %v twenty minutes after a two-minute run, want the repository", ran)
	}
}

// MAINTENANCE NEVER DELAYS A SYNC: the maintainer holds a pin, never the
// repository's round, so a sync that removes nothing completes while a run is
// in flight and leaves it running; and a sync that must remove a pack STOPS
// the run first — an index naming a missing pack fails every read of the
// repository — rather than wait for it to finish.
func TestMaintenanceNeverDelaysASync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	for range 3 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	before := w.log.state()
	_, dropped := w.push(t, repo, "refs/heads/main", nil)
	reached := make(chan struct{})
	var once sync.Once
	ended := make(chan error, 1)
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			if step != "multi-pack-index" {
				return
			}
			once.Do(func() { close(reached) })
			<-ctx.Done()
			ended <- ctx.Err()
		})
	})
	w.sync(t, c, repo)
	clk.Add(cache.MidxDebounce)
	passed := make(chan []string, 1)
	go func() { passed <- cache.MaintainerPass(c) }()
	select {
	case <-reached:
	case <-time.After(hangDetector):
		t.Fatalf("the maintainer started no run in %v", hangDetector)
	}

	w.push(t, repo, "refs/heads/main", nil)
	ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
	defer cancel()
	s, err := c.Sync(ctx, repo, linearizable)
	if err != nil {
		t.Fatalf("a sync while a maintenance run is held: %v", err)
	}
	s.Release()
	if !cache.Maintaining(c, repo) {
		t.Fatal("a sync that removes nothing stopped the maintenance run")
	}

	w.log.recreate(before)
	s, err = c.Sync(ctx, repo, linearizable)
	if err != nil {
		t.Fatalf("a sync that removes a pack while a maintenance run is held: %v", err)
	}
	defer s.Release()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the held run's context ended with %v, want a cancellation", err)
		}
	case <-time.After(hangDetector):
		t.Fatal("a sync that removed a pack left the maintenance run on its incarnation running")
	}
	select {
	case ran := <-passed:
		if len(ran) != 0 {
			t.Errorf("a cancelled run was reported as maintained: %v", ran)
		}
	case <-time.After(hangDetector):
		t.Fatalf("the maintainer's pass has not ended %v after its run was cancelled", hangDetector)
	}
	if got := w.logs.lines(t, "code_cache_maintenance_cancelled"); len(got) != 1 || got[0]["step"] != "multi-pack-index" {
		t.Errorf("code_cache_maintenance_cancelled = %v, want one line at multi-pack-index", got)
	}
	packDir := filepath.Join(s.Dir, "objects", "pack")
	if statOK(t, filepath.Join(packDir, "pack-"+dropped.GitChecksum+".idx")) {
		t.Error("the pack the recreated log does not name is still installed")
	}
	if statOK(t, filepath.Join(packDir, "multi-pack-index.lock")) {
		t.Error("the cancelled run left its lock")
	}
	if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
		t.Errorf("fsck after a sync stopped a maintenance run: %v", err)
	}
}

// A DROP STOPS A RUN WRITING WHAT IT DROPS, at whatever change makes it — a
// delta's pack added included, which never stops a run otherwise: git reads a
// write's inputs as it starts, so a run in flight across a change its
// declaration drops on would rename in a file built over the inputs before
// it, and record it current. Over declarations in which the multi-pack-index
// is dropped when a pack is added, a delta adding a pack while the
// maintainer's index step is held stops the run before the pack is in: the
// held step sees its context cancelled, the delta is answered, and the
// incarnation records no index and holds none. The graph the run wrote first
// is kept, its declaration keeping it when a pack is added.
//
// Mutation: drop dropFor's stopWriting, and the run is left running across
// the delta.
func TestADropStopsARunWritingWhatItDrops(t *testing.T) {
	t.Parallel()
	dropOnAdd := cache.DerivedFiles()
	for i := range dropOnAdd {
		if dropOnAdd[i].Name == cache.DerivedMidx {
			dropOnAdd[i].OnPackAdded = cache.Drop
		}
	}
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	for range 3 {
		w.push(t, repo, "refs/heads/main", nil)
	}
	reached := make(chan struct{})
	var once sync.Once
	ended := make(chan error, 1)
	c := w.open(t, clk.on, cache.WithDerived(dropOnAdd), func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			if step != "multi-pack-index" {
				return
			}
			once.Do(func() { close(reached) })
			<-ctx.Done()
			ended <- ctx.Err()
		})
	})
	s := w.sync(t, c, repo)
	clk.Add(cache.MidxDebounce)
	_, wait := cache.StartMaintainerPass(c)
	select {
	case <-reached:
	case <-time.After(hangDetector):
		t.Fatalf("the maintainer started no run in %v", hangDetector)
	}

	w.push(t, repo, "refs/heads/main", nil)
	ctx, cancel := context.WithTimeout(t.Context(), hangDetector)
	defer cancel()
	after, err := c.Sync(ctx, repo, linearizable)
	if err != nil {
		t.Fatalf("a delta adding a pack while the run is held: %v", err)
	}
	after.Release()
	select {
	case err := <-ended:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the held run's context ended with %v, want a cancellation", err)
		}
	case <-time.After(hangDetector):
		t.Fatal("a delta whose pack drops the index left the run writing the index running")
	}
	if ran := wait(); len(ran) != 0 {
		t.Errorf("a stopped run was reported as maintained: %v", ran)
	}
	if statOK(t, filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")) {
		t.Error("a multi-pack-index is on disk after the delta its declaration drops it on")
	}
	covered, _, graphed := cache.Indexed(c, repo)
	if covered != 0 {
		t.Errorf("the incarnation records an index over %d packs after the drop", covered)
	}
	if !graphed || !statOK(t, filepath.Join(s.Dir, "objects", "info", "commit-graph")) {
		t.Errorf("the graph the run wrote first: recorded %t, want it kept — its declaration keeps it when a "+
			"pack is added", graphed)
	}
}

// NO RUN STARTS WHILE AN ADOPTION HOLDS THE REPOSITORY, and the last to let go
// wakes the maintainer. An adoption adds a pack, and a declaration may drop
// what a run writes when one is added — a drop that stops a run in flight, so
// a run started between it and the pack's links would write over the packs
// before the change and record the file current after it. A pass while an
// adoption holds the repository's write lock starts nothing on it, however
// due — a copy with no graph is due at once — and owes the maintainer a wake,
// which the adoption sends as it lets go, rather than leave the graph to the
// next tick.
//
// Mutations: drop runnable's adoption term (the pass starts the graph-first
// run under the adoption); drop releaseWrite's owed wake (no kick follows the
// adoption).
func TestNoRunStartsWhileAnAdoptionHoldsTheRepository(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	var c *cache.Cache
	var started []string
	var held bool
	c = w.open(t, func(o *cache.Options) {
		cache.WhileAdopting(o, func(string) {
			held = true
			// The cache is never started: a kick stays where it was sent.
			cache.MaintainerKicked(c)
			var wait func() []string
			started, wait = cache.StartMaintainerPass(c)
			wait()
		})
	})
	a := w.receive(t, c, repo)
	if _, _, graphed := cache.Indexed(c, repo); graphed {
		t.Fatal("the copy holds a commit-graph: the case needs one due at once")
	}
	if err := c.Adopt(t.Context(), repo, a.sc, a.row.Ref()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if !held {
		t.Fatal("no adoption reached the knob: the case held nothing")
	}
	if len(started) != 0 {
		t.Errorf("a pass while an adoption held the repository started runs on %v, want none", started)
	}
	if !cache.MaintainerKicked(c) {
		t.Error("the adoption let the repository go without waking the maintainer its pass was owed")
	}
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Errorf("the pass after the adoption ran %v, want the repository's graph", ran)
	}
}

// A SWEEP DROPS THE MULTI-PACK-INDEX WITH A PACK: a full round that finds a
// pack the rows no longer name — the log recreated holding fewer — takes the
// index, its bitmap and the commit-graph with it, so no reader meets an index
// naming a pack that is gone, nor a graph naming a commit that left with it:
// IN PLACE from a copy nobody can hold yet (a boot's first round), and BY THE
// SWAP from one a reader holds (swap.go), whose next incarnation never holds
// them while the reader's copy keeps every one. Either way the copy the round
// answers is whole to git and records no file over the packs, and the
// maintainer writes a new index once it is due again.
//
// Mutation: declare the graph kept on a pack removed (OnPackRemoved), and both
// arms keep it.
func TestASweepDropsTheMultiPackIndexWithAPack(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// boot: the node restarts after the log went back, so its first
		// round reads a copy nobody holds; otherwise a reader holds it.
		boot bool
	}{
		{"by the swap, from a copy a reader holds", false},
		{"in place, from a copy a boot's first round reads", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			clk := newClock()
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			w.push(t, repo, "refs/heads/main", nil)
			before := w.log.state()
			_, dropped := w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t, clk.on)
			held := w.sync(t, c, repo)
			clk.Add(cache.MidxDebounce)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				t.Fatalf("the maintainer ran %v, want the repository", ran)
			}
			heldBitmap := bitmapOf(t, filepath.Join(held.Dir, "objects", "pack"))

			w.log.recreate(before)
			if tc.boot {
				held.Release()
				c = w.reopen(t, c, clk.on)
			}
			s := w.sync(t, c, repo)
			if inPlace := s.Dir == held.Dir; inPlace != tc.boot {
				t.Fatalf("the round answered %s, the copy the reader held being %s: in place %t, want %t", s.Dir,
					held.Dir, inPlace, tc.boot)
			}
			packDir := filepath.Join(s.Dir, "objects", "pack")
			for _, path := range []string{
				filepath.Join(packDir, "pack-"+dropped.GitChecksum+".pack"),
				filepath.Join(packDir, "multi-pack-index"),
				filepath.Join(s.Dir, "objects", "info", "commit-graph"),
			} {
				if statOK(t, path) {
					t.Errorf("%s is still there after the sweep removed a pack under it", path)
				}
			}
			if got := bitmaps(t, s.Dir); len(got) != 0 {
				t.Errorf("%v are still there after the sweep removed a pack under them", got)
			}
			if !tc.boot && !statOK(t, heldBitmap) {
				t.Errorf("the copy the reader holds lost %s", heldBitmap)
			}
			if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
				t.Errorf("fsck after a pack was removed under the index: %v", err)
			}
			if doc := readStateDoc(t, s.Dir); len(doc.overPacks()) != 0 {
				t.Errorf("the state records %+v after the derived files were dropped, want none over the packs",
					doc.Derived)
			}

			clk.Add(cache.MidxMinInterval)
			if ran := cache.MaintainerPass(c); len(ran) != 1 {
				t.Fatalf("the maintainer ran %v after the sweep, want the repository", ran)
			}
			bitmapOf(t, packDir)
		})
	}
}

// MAINTENANCE RUNS ONLY IN A FREE MAINTENANCE SLOT: a pool of two process
// slots has one maintenance slot, and with it held the maintainer runs and
// records nothing — it never waits for one, and a run that never started is
// no run — and runs once it is free.
func TestMaintenanceRunsOnlyInAFreeMaintenanceSlot(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	clk.Add(cache.MidxDebounce)
	if got := w.pool.Slots(gitproc.ClassMaintenance); got != 1 {
		t.Fatalf("the pool has %d maintenance slots, want 1", got)
	}
	release := holdSlotUntil(t, w.pool, gitproc.ClassMaintenance)
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Fatalf("the maintainer ran %v with the maintenance slot held", ran)
	}
	if doc := readStateDoc(t, s.Dir); !doc.Maintained.LastRun.IsZero() || doc.midx().Covers != 0 {
		t.Errorf("a run refused a slot recorded %+v and %+v", doc.Derived, doc.Maintained)
	}
	if got := w.logs.lines(t, "code_cache_maintenance_failed"); len(got) != 0 {
		t.Errorf("a run refused a slot logged a failure: %v", got)
	}
	release()
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Errorf("the maintainer ran %v with the slot free, want the repository", ran)
	}
}

// ONE LONG RUN DOES NOT HOLD THE OTHERS BACK: a bitmap over the largest
// repository the defaults admit is hours, and the maintenance lane is half the
// node's cores — so with a run held in flight, another repository due in the
// same pass runs to its end in a slot of its own rather than waiting behind
// it. A lane of three slots: the maintainer keeps one free for a person.
func TestOneLongRunDoesNotHoldTheOthersBack(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	w.processes = 6
	clk := newClock()
	a, b := w.repo(t, 1), w.repo(t, 2)
	for _, repo := range []string{a, b} {
		w.push(t, repo, "refs/heads/main", nil)
		w.push(t, repo, "refs/heads/main", nil)
	}
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			first := false
			once.Do(func() { first = true })
			if first {
				close(held)
				select {
				case <-release:
				case <-ctx.Done():
				}
			}
		})
	})
	if got := w.pool.Slots(gitproc.ClassMaintenance); got != 3 {
		t.Fatalf("the pool has %d maintenance slots, want 3", got)
	}
	sa, sb := w.sync(t, c, a), w.sync(t, c, b)
	defer sa.Release()
	defer sb.Release()
	clk.Add(cache.MidxDebounce)
	passed := make(chan []string, 1)
	go func() { passed <- cache.MaintainerPass(c) }()
	select {
	case <-held:
	case <-time.After(hangDetector):
		t.Fatalf("the maintainer started no run in %v", hangDetector)
	}
	// A pass that waited for its first run would never reach this: the held
	// run ends only once the case lets it.
	eventually(t, "the other repository's run ends while the first is held", func() bool {
		return len(w.logs.lines(t, "code_cache_maintained")) == 1
	})
	ended := w.logs.lines(t, "code_cache_maintained")[0]["repo"]
	dirs := map[string]string{a: sa.Dir, b: sb.Dir}
	heldRepo := map[string]string{a: b, b: a}[ended.(string)]
	if !cache.Maintaining(c, heldRepo) {
		t.Errorf("the held run on %s is not in flight", heldRepo)
	}
	if !statOK(t, filepath.Join(dirs[ended.(string)], "objects", "info", "commit-graph")) {
		t.Errorf("the run that ended left %s with no commit-graph", ended)
	}
	close(release)
	select {
	case ran := <-passed:
		if len(ran) != 2 {
			t.Errorf("the pass answered %v, want both repositories once the held run was let go", ran)
		}
	case <-time.After(hangDetector):
		t.Fatalf("the pass has not ended %v after its held run was let go", hangDetector)
	}
}

// THE MAINTAINER LEAVES A MAINTENANCE SLOT FOR A PERSON: `crewlet code
// verify`, which every refused rebuild tells an operator to run, checks a
// repository in the maintenance lane and is refused, never queued, when no
// slot is free — so however long the maintainer's runs last, one slot of a
// lane of two is never theirs. Two repositories due, each run holding a slot
// for as long as the case lets it: one run starts, and a maintenance-class
// process started meanwhile is admitted. (Only a graph-first run, a walk of
// the commits of seconds, may take that slot: graphFirst below, and
// TestAGraphLessCopyIsGraphedFirst.)
func TestTheMaintainerLeavesAMaintenanceSlotForAPerson(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	w.processes = 4
	clk := newClock()
	a, b := w.repo(t, 1), w.repo(t, 2)
	for _, repo := range []string{a, b} {
		w.push(t, repo, "refs/heads/main", nil)
		w.push(t, repo, "refs/heads/main", nil)
	}
	// Each run holds a maintenance slot of its own until the case lets it
	// go, as the git of a long run does, and says so once it holds it.
	release, holding := make(chan struct{}), make(chan struct{}, 2)
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			if step != "multi-pack-index" {
				return
			}
			//nolint:contextcheck // the slot is the case's, held for the run
			let := holdSlotUntil(t, w.pool, gitproc.ClassMaintenance)
			holding <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			let()
		})
	})
	if got := w.pool.Slots(gitproc.ClassMaintenance); got != 2 {
		t.Fatalf("the pool has %d maintenance slots, want 2", got)
	}
	sa, sb := w.sync(t, c, a), w.sync(t, c, b)
	defer sa.Release()
	defer sb.Release()
	graphFirst(t, c, a, b)
	clk.Add(cache.MidxDebounce)

	started, wait := cache.StartMaintainerPass(c)
	for range started {
		select {
		case <-holding:
		case <-time.After(hangDetector):
			t.Fatalf("a run the pass started holds no slot after %v", hangDetector)
		}
	}
	if len(started) != 1 {
		t.Errorf("the pass started runs on %v in a lane of two slots, want one: the other slot is a person's", started)
	}
	_, err := w.pool.Run(t.Context(), gitproc.Cmd{Args: []string{"version"}, Class: gitproc.ClassMaintenance, Deadline: time.Minute})
	if err != nil {
		t.Errorf("a maintenance-class process started while the maintainer runs = %v, want it admitted", err)
	}
	close(release)
	if ran := wait(); len(ran) != len(started) {
		t.Errorf("the pass's runs ended as %v, want %v maintained", ran, started)
	}
}

// MAINTENANCE RUNS BESIDE THE LARGEST AS MEMORY ALLOWS: a bitmap holds about
// BitmapBytesPerObject an object of its repository, and a node that has just joined
// finds its largest repositories due together — so the runs beside the
// largest in flight are held to an allowance, while the largest itself, the
// price of serving its repository at all, starts whenever a slot is free.
// Three repositories due in a lane with room for three runs, and an allowance
// that holds one of the two smaller beside the larger: two run, and the third
// waits for the next pass.
func TestMaintenanceRunsBesideTheLargestAsMemoryAllows(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	w.processes = 8
	clk := newClock()
	large, small1, small2 := w.repo(t, 1), w.repo(t, 2), w.repo(t, 3)
	for range 4 {
		w.push(t, large, "refs/heads/main", nil)
	}
	for _, repo := range []string{small1, small2} {
		w.push(t, repo, "refs/heads/main", nil)
		w.push(t, repo, "refs/heads/main", nil)
	}
	release := make(chan struct{})
	c := w.open(t, clk.on, cache.NoFixedParts(), func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			if step != "multi-pack-index" {
				return
			}
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	})
	if got := cache.MaintenanceMemory(c); got != int64(w.pool.Slots(gitproc.ClassRead))*cache.MaintenanceMemoryPerCore {
		t.Errorf("the maintenance allowance is %d, want MaintenanceMemoryPerCore for each of the pool's %d cores",
			got, w.pool.Slots(gitproc.ClassRead))
	}
	for _, repo := range []string{large, small1, small2} {
		defer w.sync(t, c, repo).Release()
	}
	el, e1, e2 := cache.BitmapCharge(c, large), cache.BitmapCharge(c, small1), cache.BitmapCharge(c, small2)
	allowance := max(e1, e2)
	if el <= allowance || e1+e2 <= allowance || e1 == 0 {
		t.Fatalf("estimates %d (large), %d and %d against an allowance of %d: the case needs the large run past "+
			"the allowance and one small run, not two, within it", el, e1, e2, allowance)
	}
	cache.SetMaintenanceMemory(c, allowance)
	clk.Add(cache.MidxDebounce)

	started, wait := cache.StartMaintainerPass(c)
	// Most packs left uncovered first, then by id: the large repository,
	// then the first of the two small ones.
	want := []string{large, min(small1, small2)}
	slices.Sort(want)
	if !slices.Equal(started, want) {
		t.Errorf("the pass started runs on %v, want the large repository and one small one beside it, %v", started, want)
	}
	close(release)
	wait()
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Errorf("the next pass ran %v, want the repository the allowance left waiting", ran)
	}
}

// A MAINTENANCE RUN IS CHARGED BY ITS REPOSITORY'S OBJECTS, as the packs'
// rows count them — a multiplication, never a read of the disk: its own
// figure an object, and room for a transfer of the repository beside it — and the
// count is exact on every incarnation the maintainer may run on: set whole by
// a full round (a restart's first, which installs nothing it already holds),
// and raised by every pack a delta installs.
func TestAMaintenanceRunIsChargedByItsRepositorysObjects(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	var objects int64
	for range 3 {
		_, row := w.push(t, repo, "refs/heads/main", nil)
		objects += row.Objects
	}
	perObject := int64(cache.BitmapBytesPerObject + gitproc.TransferBytesPerObject)
	c := w.open(t)
	w.sync(t, c, repo).Release()
	if got, want := cache.BitmapCharge(c, repo), objects*perObject; got != want {
		t.Errorf("a cold sync's charge is %d, want %d objects at %d bytes", got, objects, perObject)
	}
	c = w.reopen(t, c)
	w.sync(t, c, repo).Release()
	if got, want := cache.BitmapCharge(c, repo), objects*perObject; got != want {
		t.Errorf("after a restart's full round, which installs nothing, the charge is %d, want %d", got, want)
	}
	_, row := w.push(t, repo, "refs/heads/main", nil)
	objects += row.Objects
	w.sync(t, c, repo).Release()
	if got, want := cache.BitmapCharge(c, repo), objects*perObject; got != want {
		t.Errorf("after a delta installed a pack, the charge is %d, want %d", got, want)
	}
}

// A COMMIT-GRAPH REFUSED A SLOT IS ASKED FOR AGAIN AT ONCE: a graph-first
// run whose step finds no slot of the lane free — another process took them
// between the pass and the run — never started, so it records nothing and
// pushes nothing back, and the next pass asks again with no interval to wait
// out. Once it runs, the graph is written alone: no index, and no run the
// duty cycle counts, so the bitmap after it is not held back by it.
func TestACommitGraphRefusedASlotIsAskedForAgainAtOnce(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	var take sync.Once
	var releases []func()
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(_ context.Context, step string) {
			if step == "commit-graph" {
				// Every slot of the lane, held past the run's own context,
				// which ends with the run.
				//nolint:contextcheck // the slots are the case's, not the run's
				take.Do(func() {
					for range w.pool.Slots(gitproc.ClassMaintenance) {
						releases = append(releases, holdSlotUntil(t, w.pool, gitproc.ClassMaintenance))
					}
				})
			}
		})
	})
	s := w.sync(t, c, repo)
	defer s.Release()
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Fatalf("a graph-first run refused its slot was reported as maintained: %v", ran)
	}
	for _, release := range releases {
		release()
	}
	midx := filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")
	graph := filepath.Join(s.Dir, "objects", "info", "commit-graph")
	if statOK(t, graph) || statOK(t, midx) {
		t.Fatal("a run refused its slot wrote a file")
	}
	if doc := readStateDoc(t, s.Dir); doc.graph().Step != "" || !doc.Maintained.LastRun.IsZero() {
		t.Errorf("a run refused its slot recorded %+v and %+v, want nothing", doc.Derived, doc.Maintained)
	}
	if got := w.logs.lines(t, "code_cache_maintenance_failed"); len(got) != 0 {
		t.Errorf("a step refused a slot was logged as a failure: %v", got)
	}

	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the next pass ran %v, want the repository's graph at once", ran)
	}
	if !statOK(t, graph) || statOK(t, midx) {
		t.Error("the graph-first run did not write the graph alone")
	}
	if doc := readStateDoc(t, s.Dir); doc.graph().Step != string(cache.StepGraph) || !doc.Maintained.LastRun.IsZero() {
		t.Errorf("the state records %+v and %+v after the graph-first run, want its graph and no run the duty "+
			"cycle counts", doc.Derived, doc.Maintained)
	}
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 || !statOK(t, midx) {
		t.Errorf("the pass once the packs stood still ran %v, want the repository's index, unheld by the "+
			"graph-first run before it", ran)
	}
}

// A GRAPH-LESS COPY IS GRAPHED FIRST. Without a commit-graph every walk git
// makes over a repository goes by date, and a push of a commit dated before
// its parent walks every commit in each of them; on a node that just joined,
// or a copy rebuilt, the maintainer's queue is every other repository's
// bitmap — hours. So a copy a round has read whole with no graph is due for
// it at once, with no debounce, and in a slot of its own beside a run that
// holds the maintainer's: the one kept for a person's verify, lent for the
// seconds a walk of the commits takes and never for the index and bitmap the
// copy may be due for as well, which are hours on a large repository. Both
// ways the copy can reach the pass: before its packs have stood still, and
// with its ordinary run due too — every graph-less copy after a restart,
// whose first full round adds no packs. Either way the run writes the graph
// alone, and a maintenance-class process started once it is written is
// admitted while the maintainer's own slot is still held.
//
// Mutation: drop the graph-first plan (runPlan.first) from plan, or hold it
// to the maintainer's own slots in startMaintenance, and the copy waits for
// the long run; admit a graph-first plan carrying an index at the lane's
// limit, and the due copy's run goes on to its index in the kept slot.
func TestAGraphLessCopyIsGraphedFirst(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		due  bool
	}{
		{name: "before its packs have stood still"},
		{name: "due for its index as well", due: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			w.processes = 4
			clk := newClock()
			long, fresh := w.repo(t, 1), w.repo(t, 2)
			w.push(t, long, "refs/heads/main", nil)
			w.push(t, fresh, "refs/heads/main", nil)
			c, indexing, release := holdEveryIndex(t, w, clk)
			sl := w.sync(t, c, long)
			defer sl.Release()
			graphFirst(t, c, long)
			clk.Add(cache.MidxDebounce)
			// THE LONG RUN: long's bitmap, holding the maintainer's one slot
			// of the two for as long as the case lets it.
			letGo := startHeld(t, c, indexing, release, long)
			defer letGo()

			// THE FRESH COPY, read whole with no graph: graphed at once,
			// beside the long run.
			sf := w.sync(t, c, fresh)
			defer sf.Release()
			if tc.due {
				clk.Add(cache.MidxDebounce)
			}
			graphedAlone(t, w, c, indexing, fresh, sf.Dir)
		})
	}
}

// ANOTHER GRAPH-LESS COPY IS GRAPHED WHILE THE FIRST ONE'S BITMAP RUNS. A
// graph-first run that finds one of the maintainer's own slots free takes it
// for its whole plan — the graph, then the index and bitmap it is due for —
// and the next graph-less copy, finding the maintainer's slots taken, is lent
// the person's for its graph alone rather than waiting out that bitmap; the
// slot is the person's again as soon as the graph is written.
//
// Mutation: admit a graph-first plan carrying an index at the lane's limit in
// startMaintenance, and the second copy's run goes on to its index in the
// kept slot; drop the graph-only fallback on noSlot, and the second copy is
// not graphed until the first one's bitmap ends.
func TestAnotherGraphLessCopyIsGraphedWhileTheFirstOnesBitmapRuns(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	w.processes = 4
	clk := newClock()
	first, second := w.repo(t, 1), w.repo(t, 2)
	w.push(t, first, "refs/heads/main", nil)
	w.push(t, second, "refs/heads/main", nil)
	c, indexing, release := holdEveryIndex(t, w, clk)
	s1 := w.sync(t, c, first)
	defer s1.Release()
	clk.Add(cache.MidxDebounce)
	// THE FIRST COPY: graph-less and due, in the maintainer's own slot — its
	// graph, then its bitmap, held for as long as the case lets it.
	letGo := startHeld(t, c, indexing, release, first)
	defer letGo()
	if !statOK(t, filepath.Join(s1.Dir, "objects", "info", "commit-graph")) {
		t.Errorf("%s's run reached its index before writing its graph", first)
	}

	s2 := w.sync(t, c, second)
	defer s2.Release()
	clk.Add(cache.MidxDebounce)
	graphedAlone(t, w, c, indexing, second, s2.Dir)
}

// holdEveryIndex opens w's cache on clk with a lane of two maintenance slots,
// every multi-pack-index step of a run holding a maintenance slot — as the
// git of a long bitmap does — until release is closed, and saying so on
// indexing first.
func holdEveryIndex(t *testing.T, w *syncWorld, clk *clock) (c *cache.Cache, indexing chan struct{},
	release chan struct{}) {

	t.Helper()
	indexing, release = make(chan struct{}, 2), make(chan struct{})
	c = w.open(t, clk.on, func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			if step != "multi-pack-index" {
				return
			}
			//nolint:contextcheck // the slot is the case's, held for the run
			let := holdSlotUntil(t, w.pool, gitproc.ClassMaintenance)
			indexing <- struct{}{}
			select {
			case <-release:
			case <-ctx.Done():
			}
			let()
		})
	})
	if got := w.pool.Slots(gitproc.ClassMaintenance); got != 2 {
		t.Fatalf("the pool has %d maintenance slots, want 2", got)
	}
	return c, indexing, release
}

// startHeld starts a pass that must start repo's run alone, and waits for
// that run to hold the maintainer's slot at its index step (holdEveryIndex);
// it answers what lets every held run go and waits for the pass's, for the
// case to defer.
func startHeld(t *testing.T, c *cache.Cache, indexing <-chan struct{}, release chan struct{},
	repo string) (letGo func()) {

	t.Helper()
	started, wait := cache.StartMaintainerPass(c)
	letGo = sync.OnceFunc(func() {
		close(release)
		wait()
	})
	if len(started) != 1 || started[0] != repo {
		letGo()
		t.Fatalf("the pass started %v, want %s's run alone", started, repo)
	}
	select {
	case <-indexing:
	case <-time.After(hangDetector):
		letGo()
		t.Fatalf("%s's run holds no slot at its index after %v", repo, hangDetector)
	}
	return letGo
}

// graphedAlone runs a pass while the maintainer's one slot of two is held at
// an index step, and fails t unless it ran repo's graph-first run alone — the
// graph written, no index, the run ended without reaching its index — and a
// maintenance-class process started after it is admitted: the kept slot is
// the person's again.
func graphedAlone(t *testing.T, w *syncWorld, c *cache.Cache, indexing <-chan struct{}, repo, dir string) {
	t.Helper()
	ran := make(chan []string, 1)
	go func() { ran <- cache.MaintainerPass(c) }()
	select {
	case got := <-ran:
		if len(got) != 1 || got[0] != repo {
			t.Fatalf("with the maintainer's slot held, the pass ran %v, want %s's graph", got, repo)
		}
	case <-indexing:
		t.Fatalf("%s's graph-first run went on to its index in the slot kept for a person", repo)
	case <-time.After(hangDetector):
		t.Fatalf("the pass over %s has not ended after %v", repo, hangDetector)
	}
	if !statOK(t, filepath.Join(dir, "objects", "info", "commit-graph")) ||
		statOK(t, filepath.Join(dir, "objects", "pack", "multi-pack-index")) {
		t.Errorf("%s's graph-first run did not write the graph alone", repo)
	}
	_, err := w.pool.Run(t.Context(), gitproc.Cmd{Args: []string{"version"}, Class: gitproc.ClassMaintenance,
		Deadline: time.Minute})
	if err != nil {
		t.Errorf("a maintenance-class process started once %s's graph was written = %v, want it admitted",
			repo, err)
	}
}

// A GRAPH-FIRST RUN COMES BEFORE EVERY BITMAP IN A PASS: two repositories due
// — one for its bitmap, with more packs uncovered, one read whole with no
// graph — in a lane whose maintainer runs one at a time: the graph-less copy
// is started first, and the bitmap at the next pass.
//
// Mutation: sort graph-first candidates by packs uncovered alone in
// maintainerPass, and the bitmap is started first.
func TestAGraphFirstRunIsStartedAheadOfABitmap(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	w.processes = 2
	clk := newClock()
	heavy, fresh := w.repo(t, 1), w.repo(t, 2)
	for range 4 {
		w.push(t, heavy, "refs/heads/main", nil)
	}
	w.push(t, fresh, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	if got := w.pool.Slots(gitproc.ClassMaintenance); got != 1 {
		t.Fatalf("the pool has %d maintenance slots, want 1", got)
	}
	sh := w.sync(t, c, heavy)
	defer sh.Release()
	graphFirst(t, c, heavy)
	clk.Add(cache.MidxDebounce)
	sf := w.sync(t, c, fresh)
	defer sf.Release()
	started, wait := cache.StartMaintainerPass(c)
	wait()
	if len(started) != 1 || started[0] != fresh {
		t.Fatalf("the pass started %v, want %s's graph ahead of %s's bitmap", started, fresh, heavy)
	}
	if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != heavy {
		t.Errorf("the next pass ran %v, want %s's bitmap", ran, heavy)
	}
}

// THE END OF A RUN STATS ONLY WHAT THE DECLARATIONS OWN. A maintenance run
// counts what it leaves against the budget as it ends, and a full round that
// stops the run — and so every request that round answers — waits for that
// end. objects/pack holds three files a pack beside the index's, with no cap
// on the packs, so the count lists each directory a declaration owns a file
// in once and stats only the declared files there: none of the packs' own
// files, however many the copy holds, while every file the runs wrote — the
// graph-first run's graph, the bitmap run's index and bitmap — is counted.
//
// Mutation: derivedSizes sizes every entry it lists (a nil keep), and the
// end of each run stats every pack's .pack, .idx and .rev.
func TestTheEndOfARunStatsOnlyTheDerivedFiles(t *testing.T) {
	t.Parallel()
	const pushes = 12
	var counting atomic.Bool
	var mu sync.Mutex
	statted := map[string]int{}
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	for range pushes {
		w.push(t, repo, "refs/heads/main", nil)
	}
	c := w.open(t, clk.on, cache.StatingWith(func(rel string) {
		if counting.Load() {
			mu.Lock()
			statted[rel]++
			mu.Unlock()
		}
	}))
	s := w.sync(t, c, repo)
	defer s.Release()

	// Only the maintainer's runs are counted: the full round's own measure
	// counts the packs' bytes, and stats every entry to do it.
	counting.Store(true)
	graphFirst(t, c, repo)
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != repo {
		t.Fatalf("the bitmap pass ran %v, want %s", ran, repo)
	}
	counting.Store(false)
	if packs, bitmap, graph := cache.Indexed(c, repo); packs != pushes || !bitmap || !graph {
		t.Fatalf("the runs left an index over %d packs (bitmap %t) and graph %t; the case needs every pack "+
			"indexed with a bitmap, and the graph", packs, bitmap, graph)
	}

	mu.Lock()
	defer mu.Unlock()
	var bitmaps int
	for rel, n := range statted {
		if len(cache.OwnersOf(rel)) == 0 {
			t.Errorf("the end of a run statted %s %d times, which no declaration owns", rel, n)
		}
		if matched, _ := filepath.Match("objects/pack/multi-pack-index-*.bitmap", rel); matched {
			bitmaps++
		}
	}
	for _, rel := range []string{"objects/pack/multi-pack-index", "objects/info/commit-graph"} {
		if statted[rel] == 0 {
			t.Errorf("the end of the runs never statted %s: what a run wrote went uncounted", rel)
		}
	}
	if bitmaps == 0 {
		t.Error("the end of the bitmap run never statted its bitmap: what it wrote went uncounted")
	}
}

// A RUN STOPPED FROM OUTSIDE IS NO RUN: a rolling deploy's Stop, a full
// round's sweep or a retire ends a run its repository did not fail, and
// recorded as one it would push the next run back by the interval — or by
// MidxDutyCycle times however long it had run, hours for a bitmap a deploy
// cut short near its end — with nothing written. So nothing is recorded or
// persisted, and after a restart the repository is due once its packs have
// stood still.
func TestARunStoppedFromOutsideIsNoRun(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	reached := make(chan struct{})
	var once sync.Once
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			once.Do(func() { close(reached) })
			<-ctx.Done()
		})
	})
	s := w.sync(t, c, repo)
	s.Release()
	clk.Add(cache.MidxDebounce)
	passed := make(chan []string, 1)
	go func() { passed <- cache.MaintainerPass(c) }()
	select {
	case <-reached:
	case <-time.After(hangDetector):
		t.Fatalf("the maintainer started no run in %v", hangDetector)
	}
	c.Stop(context.Background())
	select {
	case ran := <-passed:
		if len(ran) != 0 {
			t.Errorf("a run Stop ended was reported as maintained: %v", ran)
		}
	case <-time.After(hangDetector):
		t.Fatalf("the pass has not ended %v after Stop", hangDetector)
	}
	if doc := readStateDoc(t, s.Dir); !doc.Maintained.LastRun.IsZero() || doc.Maintained.Took != 0 {
		t.Errorf("a run Stop ended persisted %+v, want no run", doc.Maintained)
	}

	c = w.open(t, clk.on)
	w.sync(t, c, repo).Release()
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Errorf("after the restart the maintainer ran %v once the packs stood still, want the repository", ran)
	}
}

// holdSlotUntil holds one slot of class in pool until the release it answers
// is called (or the case ends).
func holdSlotUntil(t *testing.T, pool *gitproc.Pool, class gitproc.Class) func() {
	t.Helper()
	started, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = pool.Run(context.Background(), gitproc.Cmd{
			Args: []string{"hash-object", "--stdin"}, Class: class, Deadline: 5 * time.Minute,
			Stdin: &blocking{started: started, release: release},
		})
	}()
	<-started
	stop := sync.OnceFunc(func() {
		close(release)
		<-done
	})
	t.Cleanup(stop)
	return stop
}

// A FAILED MAINTENANCE LEAVES NOTHING BEHIND: the run is the only writer of
// the index and the commit-graph in its incarnation, so a lock still there
// when it fails — a killed writer's, which git cannot remove — is its own to
// remove, as is the temporary file git writes a bitmap into, which git does
// not clean up when it is killed. A failed run is still a run: it waits out
// MidxMinInterval, and the one after it succeeds — a graph-first run too,
// whose graph is then due only by the ordinary rules rather than asked again
// every pass.
func TestAFailedMaintenanceLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	debris := []string{
		filepath.Join(s.Dir, "objects", "pack", "multi-pack-index.lock"),
		filepath.Join(s.Dir, "objects", "pack", "tmp_bitmap_Ab12Cd"),
		filepath.Join(s.Dir, "objects", "info", "commit-graph.lock"),
	}
	for _, path := range debris {
		write(t, path, "a killed writer's\n")
	}
	ranAt := clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Fatalf("a run git refused was reported as maintained: %v", ran)
	}
	for _, path := range debris {
		if statOK(t, path) {
			t.Errorf("the failed run left %s", path)
		}
	}
	if got := w.logs.lines(t, "code_cache_maintenance_failed"); len(got) != 1 || got[0]["step"] != "commit-graph" {
		t.Errorf("code_cache_maintenance_failed = %v, want one line at commit-graph, the graph-first run's first step", got)
	}
	if doc := readStateDoc(t, s.Dir); !doc.Maintained.LastRun.Equal(ranAt) || doc.midx().Covers != 0 {
		t.Errorf("the failed run recorded %+v and %+v, want its end and no index", doc.Derived, doc.Maintained)
	}
	clk.Add(time.Minute)
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Errorf("the maintainer ran %v inside MidxMinInterval of a failed run", ran)
	}
	clk.Set(ranAt.Add(cache.MidxMinInterval))
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Errorf("the maintainer ran %v after a failed run's interval, want the repository", ran)
	}
}

// THE MAINTAINER WORKS ONLY ON WHAT THIS PROCESS HAS READ WHOLE: a directory
// a previous process left may hold strays its first round is about to sweep,
// and an index written over them would be dropped with them. Once a round has
// converged it, it is due.
func TestTheMaintainerWaitsForAWholeRead(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	w.sync(t, c, repo)
	c = w.reopen(t, c, clk.on)
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 0 {
		t.Fatalf("the maintainer ran %v over a directory this process has not read whole", ran)
	}
	w.sync(t, c, repo)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Errorf("the maintainer ran %v once a round converged it, want the repository", ran)
	}
}

// A FULL ROUND WAKES THE MAINTAINER ONCE IT HAS LET THE REPOSITORY GO. A pass
// starts no run on a repository a round holds (runnable), so the wake is
// sent once the round no longer runs: a copy's first round leaves it with no
// commit-graph, due at once, and the graph is written with no tick at all.
// Woken from inside the round, as it once was, the pass met the round still
// running and left the repository to the next tick, MidxDebounce later — the
// graph-first run, an index repair and a copy's first index each waited for
// it.
//
// Mutation: kick the maintainer from converge (inside the round), and the
// kick is pending while the round still holds the repository.
func TestAFullRoundWakesTheMaintainerOnceItLetsTheRepositoryGo(t *testing.T) {
	t.Parallel()
	t.Run("not while the round runs", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		var c *cache.Cache
		var held atomic.Bool
		c = w.open(t, func(o *cache.Options) {
			cache.BeforeRoundEnds(o, func(string) {
				// The cache is never started: a kick stays where it was sent.
				held.Store(true)
				if cache.KickPending(c) {
					t.Error("the round woke the maintainer while it still held the repository")
				}
			})
		})
		w.sync(t, c, repo)
		if !held.Load() {
			t.Fatal("no round reached the knob: the case held nothing")
		}
		eventually(t, "the wake once the round let go", func() bool { return cache.KickPending(c) })
	})
	t.Run("a started cache", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		w.push(t, repo, "refs/heads/main", nil)
		c := w.open(t, func(o *cache.Options) { cache.MaintainerTicksFrom(o, make(chan time.Time)) })
		c.Start(t.Context())
		s := w.sync(t, c, repo)
		graph := filepath.Join(s.Dir, "objects", "info", "commit-graph")
		eventually(t, "the graph-first run the round woke", func() bool { return statOK(t, graph) })
	})
}

// A STARTED CACHE MAINTAINS AT ITS TICKS, AND STOP ENDS A RUN: Start runs
// the maintainer, which looks at every tick and runs what is due then — not
// before — and Stop cancels a run in flight and waits for it, so the
// directory is free — the claim back — when Stop returns. Each tick is a
// send the maintainer must take, so the one after a pass is taken only once
// that pass has ended.
func TestStopEndsAMaintenanceRun(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	ticks := make(chan time.Time)
	reached := make(chan struct{})
	var once sync.Once
	c := w.open(t, clk.on, func(o *cache.Options) {
		cache.MaintainerTicksFrom(o, ticks)
		cache.BeforeMaintaining(o, func(ctx context.Context, step string) {
			// The graph-first run, due at once, runs as it would; the
			// index's run is the one held.
			if step != "multi-pack-index" {
				return
			}
			once.Do(func() { close(reached) })
			<-ctx.Done()
		})
	})
	// The round's kick is pending when the cache starts: the first tick's
	// pass answers it, and the second is taken once that pass has ended.
	w.sync(t, c, repo)
	c.Start(t.Context())
	tick := func() {
		t.Helper()
		select {
		case ticks <- time.Now():
		case <-time.After(hangDetector):
			t.Fatalf("the maintainer did not take a tick in %v", hangDetector)
		}
	}
	tick()
	tick()
	select {
	case <-reached:
		t.Fatal("the maintainer ran a repository whose packs had not stood still")
	default:
	}
	// The second tick's pass may still be running as the clock moves, and
	// may run the repository itself; else the next tick does.
	clk.Add(cache.MidxDebounce)
	select {
	case ticks <- time.Now():
	case <-reached:
	case <-time.After(hangDetector):
		t.Fatalf("the maintainer did not take a tick in %v", hangDetector)
	}
	select {
	case <-reached:
	case <-time.After(hangDetector):
		t.Fatalf("the maintainer's tick started no run in %v", hangDetector)
	}

	stopped := make(chan struct{})
	go func() {
		c.Stop(context.Background())
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(hangDetector):
		t.Fatalf("Stop has not returned %v after it cancelled a maintenance run", hangDetector)
	}
	if got := w.logs.lines(t, "code_cache_maintenance_cancelled"); len(got) != 1 {
		t.Errorf("code_cache_maintenance_cancelled logged %d times, want once", len(got))
	}
	layout, err := cache.Prepare(t.Context(), w.layout.Dir)
	if err != nil {
		t.Fatalf("Prepare after Stop returned = %v, want the directory free", err)
	}
	layout.Release()
}

// EVERY GRAPH WRITE COMPUTES EVERY FILTER, AND IS CHARGED ITS TREES: every
// commit-graph write the cache declares — own mode's whole graph and the
// node's tip — computes the changed-path filter of every commit it covers
// that lacks one (--changed-paths, and no --max-new-filters: a bound left a
// large repository's reads diffing most of its history for thousands of
// writes and bounded nothing a write holds), and is charged, whatever it
// covers, the trees its filters' diffs keep in git's delta base cache up to
// that cache's cap (Write.BaseBytes, GraphBaseBytes) — a figure an object
// alone admits a short tail's tip or a small repository's graph at a fraction
// of what it holds.
//
// Mutations: bound either write's filters (--max-new-filters=100 in its
// argument list); drop --changed-paths from one; drop the fixed part from
// either declaration — each is red.
func TestEveryGraphWriteComputesEveryFilterAndIsChargedItsTrees(t *testing.T) {
	t.Parallel()
	graphs := 0
	for _, d := range cache.DerivedFiles() {
		for _, wr := range d.Writes {
			if wr.Writer.Subcommand != "commit-graph" {
				continue
			}
			graphs++
			for _, a := range wr.Args {
				if strings.HasPrefix(a, "--max-new-filters") {
					t.Errorf("%s's %s write spells %q: it computes every filter its commits lack", d.Name,
						wr.Step, a)
				}
			}
			if !slices.Contains(wr.Args, "--changed-paths") {
				t.Errorf("%s's %s write %q writes no changed-path filters", d.Name, wr.Step, wr.Args)
			}
			if wr.BaseBytes != cache.GraphBaseBytes {
				t.Errorf("%s's %s write is charged %d whatever it covers, want GraphBaseBytes, %d, for the trees "+
					"its filters' diffs hold", d.Name, wr.Step, wr.BaseBytes, cache.GraphBaseBytes)
			}
		}
	}
	if graphs != 2 {
		t.Errorf("the cache declares %d commit-graph writes, want own mode's graph and the node's tip", graphs)
	}
}

// THE MAINTAINER'S STEPS ARE THE DECLARED WRITES (ADR-0079). What a pass and
// a round run, and what a refusal is said and counted as, are read from the
// declarations (cache.DerivedFiles): every multi-pack-index and commit-graph
// process the cache starts runs exactly one declared write's argument list —
// the round's index the round write's, the maintainer's its bitmap, index and
// graph writes', and the node's tip over an installed chain the tip's — and
// every declared write runs, since an argument list that no process ran is one
// somebody changed beside its declaration. And when the node's memory refuses
// everything — of a copy in own mode and of one in chain mode — the refusals
// name exactly the declared maintainer steps, each line's estimate the write's
// own charge.
//
// Mutation: hard-code the graph's argument list in runMaintenance with
// --split, and the recorded process is no declared write's; give the round's
// index its own list beside its declaration, and the round's process is.
func TestTheMaintainersStepsAreTheDeclaredWrites(t *testing.T) {
	t.Parallel()
	// Git's writes: an in-process one (the index of pushes') runs no git
	// process, and the writers gate ties it to its file instead.
	var declared []cache.Write
	for _, d := range cache.DerivedFiles() {
		for _, wr := range d.Writes {
			if wr.Writer.Subcommand != "" {
				declared = append(declared, wr)
			}
		}
	}
	maintainer := func(yield func(cache.Write) bool) {
		for _, wr := range declared {
			if wr.Trigger == cache.TriggerMaintainer && !yield(wr) {
				return
			}
		}
	}

	t.Run("the processes", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		var calls func() []gittest.Call
		w.gitPath, calls = gittest.Recording(t)
		clk := newClock()
		indexed, plain := w.repo(t, 1), w.repo(t, 2)
		for range caseRoundPacks {
			w.push(t, indexed, "refs/heads/main", nil)
		}
		// plain holds some two thousand objects: past them its index without a
		// bitmap — IndexBaseBytes beside an amount an object — fits a budget
		// a byte short of its bitmap's, where a smaller repository's bitmap
		// is the cheaper of the two (840 bytes an object against 320 and a
		// mebibyte: past about 2 020 objects).
		const plainFiles = 2200
		files := make(map[string]string, plainFiles)
		for i := range plainFiles {
			files["f"+strconv.Itoa(i)] = strconv.Itoa(i) + "\n"
		}
		w.push(t, plain, "refs/heads/main", files)
		w.push(t, plain, "refs/heads/main", nil)
		c := w.open(t, clk.on, cache.RoundPacksAt(caseRoundPacks))

		// The round's own index, then the maintainer's bitmap and graph.
		w.sync(t, c, indexed).Release()
		clk.Add(cache.MidxDebounce)
		if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != indexed {
			t.Fatalf("the first pass ran %v, want %s", ran, indexed)
		}
		// An index without a bitmap: the bitmap a byte past what the node
		// can give, and the graph, whose fixed part (GraphBaseBytes) is far
		// past it, refused beside it — own mode's graph ran for indexed.
		w.sync(t, c, plain).Release()
		budget := cache.BitmapCharge(c, plain) - 1
		if cache.IndexCharge(c, plain) > budget {
			t.Fatalf("the premise: %s's index without a bitmap is charged %d, past the budget %d a byte short of "+
				"its bitmap's: push it more objects", plain, cache.IndexCharge(c, plain), budget)
		}
		cache.SetMaintenanceBudget(c, budget)
		clk.Add(cache.MidxDebounce)
		if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != plain {
			t.Fatalf("the second pass ran %v, want %s", ran, plain)
		}
		if packs, bitmap, _ := cache.Indexed(c, plain); packs != 2 || bitmap {
			t.Fatalf("%s holds an index over %d packs (bitmap %t); the case needs both packs indexed without a "+
				"bitmap", plain, packs, bitmap)
		}
		// And the node's tip over an installed chain, on a budget of its own
		// charge: plain, indexed a moment ago, is not due again whatever the
		// budget, and its graph's charge — its fixed part beside its two
		// thousand objects — is past the tip's, its fixed part beside a tail
		// of a few.
		chained := w.repo(t, 3)
		p := w.pushN(t, chained, 3)
		w.log.publish(chained, w.buildChain(t, p[:2]), nil)
		w.sync(t, c, chained).Release()
		cache.SetMaintenanceBudget(c, cache.TipCharge(c, chained))
		if ran := cache.MaintainerPass(c); len(ran) != 1 || ran[0] != chained {
			t.Fatalf("the third pass ran %v, want %s's tip", ran, chained)
		}

		ran := map[int]int{}
		for _, call := range calls() {
			if len(call.Args) == 0 || (call.Args[0] != "multi-pack-index" && call.Args[0] != "commit-graph") {
				continue
			}
			i := slices.IndexFunc(declared, func(wr cache.Write) bool { return slices.Equal(wr.Args, call.Args) })
			if i < 0 {
				t.Errorf("the cache ran git %q, which is no declared write's argument list", call.Args)
				continue
			}
			ran[i]++
		}
		for i, wr := range declared {
			if ran[i] == 0 {
				t.Errorf("the %s write %s (%q) never ran: the cache runs something else in its place", wr.Trigger,
					wr.Step, wr.Args)
			}
		}
	})

	t.Run("refused for memory", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		clk := newClock()
		// A copy in own mode, refused its index, its bitmap and its graph,
		// and one in chain mode, refused its tip.
		repo, chained := w.repo(t, 1), w.repo(t, 2)
		w.push(t, repo, "refs/heads/main", nil)
		w.push(t, repo, "refs/heads/main", nil)
		p := w.pushN(t, chained, 3)
		w.log.publish(chained, w.buildChain(t, p[:2]), nil)
		c := w.open(t, clk.on)
		w.sync(t, c, repo).Release()
		w.sync(t, c, chained).Release()
		cache.SetMaintenanceBudget(c, 1)
		clk.Add(cache.MidxDebounce)
		if ran := cache.MaintainerPass(c); len(ran) != 0 {
			t.Fatalf("on a budget of one byte the maintainer ran %v", ran)
		}

		want := map[string]int64{}
		for wr := range maintainer {
			over := repo
			if wr.Step == cache.StepTipGraph {
				over = chained
			}
			want[string(wr.Step)] = cache.WriteCharge(c, over, wr)
		}
		counted := map[string]uint64{}
		gauged := map[string]float64{}
		for _, s := range w.rec.Read() {
			switch s.Name {
			case metrics.CodeCacheMaintenanceRefusals:
				counted[s.Attrs["step"]] += s.Total
			case metrics.CodeCacheMaintenanceRefused:
				gauged[s.Attrs["step"]] = s.Value
			}
		}
		for step := range want {
			if counted[step] != 1 {
				t.Errorf("%s{step=%s} = %d, want the declared step counted once", metrics.CodeCacheMaintenanceRefusals,
					step, counted[step])
			}
		}
		for step := range counted {
			if _, ok := want[step]; !ok {
				t.Errorf("%s counts step %q, which no declared maintainer write is", metrics.CodeCacheMaintenanceRefusals,
					step)
			}
		}
		for step, v := range gauged {
			// A PUBLICATION'S STEP is the maintainer's background work beside
			// its declared writes, gauged at every pass, and nothing was
			// published here, so nothing of it is refused.
			if step == cache.PublishStep {
				if v != 0 {
					t.Errorf("%s{step=%s} = %v, want 0 with no publication refused",
						metrics.CodeCacheMaintenanceRefused, step, v)
				}
				continue
			}
			if _, ok := want[step]; !ok || v != 1 {
				t.Errorf("%s{step=%s} = %v, want 1 for a declared step", metrics.CodeCacheMaintenanceRefused, step, v)
			}
		}
		if len(gauged) == 0 {
			t.Errorf("%s gauges no step while everything is refused", metrics.CodeCacheMaintenanceRefused)
		}
		lines := w.logs.lines(t, "code_cache_maintenance_refused")
		if len(lines) != len(want) {
			t.Errorf("the refusals were logged %d times, want once a declared step, %d", len(lines), len(want))
		}
		for _, l := range lines {
			step, _ := l["step"].(string)
			charge, ok := want[step]
			if !ok {
				t.Errorf("a refusal names step %q, which no declared maintainer write is", step)
				continue
			}
			if l["estimate"] != float64(charge) {
				t.Errorf("the %s refusal's estimate is %v, want its declared write's charge %d", step, l["estimate"],
					charge)
			}
		}
	})
}
