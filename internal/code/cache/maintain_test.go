package cache_test

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// caseRoundPacks is the round's index threshold a case lowers
// MidxRoundPacks to (cache.RoundPacksAt), so a handful of pushes exercises
// the round's own index rather than hundreds.
const caseRoundPacks = 8

// A FULL ROUND INDEXES MANY PACKS BEFORE IT ANSWERS: one push is one pack and
// nothing compacts them, so the request a cold cache was synced for would
// otherwise be served by git searching every pack's index for every object.
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
		if doc.Midx.Packs != caseRoundPacks || doc.Midx.Bitmap || !doc.Midx.LastRun.IsZero() || doc.Midx.Bytes == 0 {
			t.Errorf("the state's midx is %+v, want %d packs, no bitmap, no maintenance run, its bytes", doc.Midx,
				caseRoundPacks)
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
		if doc := readStateDoc(t, s.Dir); doc.Midx.Packs != caseRoundPacks || !doc.Midx.Bitmap {
			t.Errorf("after the maintainer the state's midx is %+v, want %d packs with the bitmap", doc.Midx, caseRoundPacks)
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
// packs it keeps — as many as its threshold, which the case lowers — to which
// the maintainer adds the bitmap.
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

	w.log.recreate(before)
	s = w.sync(t, c, repo)
	if statOK(t, filepath.Join(s.Dir, "objects", "pack", "pack-"+last.GitChecksum+".idx")) {
		t.Fatal("the pack the recreated log does not name is still installed")
	}
	if !statOK(t, midx) || inode(t, midx) == first {
		t.Error("a full round that swept a pack under the index did not write an index of its own")
	}
	if statOK(t, bitmap) {
		t.Error("the bitmap of an index whose pack was swept is still there")
	}
	if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
		t.Errorf("fsck after a pack was swept from under the index: %v", err)
	}

	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v after the round's own index, want the repository", ran)
	}
	bitmapOf(t, filepath.Join(s.Dir, "objects", "pack"))
	if doc := readStateDoc(t, s.Dir); doc.Midx.Packs != caseRoundPacks || !doc.Midx.Bitmap {
		t.Errorf("after the maintainer the state's midx is %+v, want %d packs with the bitmap", doc.Midx, caseRoundPacks)
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
	if doc.Midx.Packs != 9 || !doc.Midx.Bitmap || !doc.Midx.LastRun.Equal(ranAt) || doc.Midx.Bytes != heldBy(t, s.Dir)-doc.DiskBytes {
		t.Errorf("the persisted midx is %+v, want 9 packs, the bitmap, last run at %v and the maintainer's bytes", doc.Midx, ranAt)
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
	doc.Midx.Took = 2 * time.Minute
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

// A SWEEP DROPS THE MULTI-PACK-INDEX WITH A PACK: a full round that removes a
// pack the rows no longer name takes the index, its bitmap and the
// commit-graph with it, so no reader meets an index naming a pack that is
// gone — and the maintainer writes a new one once it is due again.
func TestASweepDropsTheMultiPackIndexWithAPack(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	clk := newClock()
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", nil)
	w.push(t, repo, "refs/heads/main", nil)
	before := w.log.state()
	_, dropped := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t, clk.on)
	s := w.sync(t, c, repo)
	clk.Add(cache.MidxDebounce)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v, want the repository", ran)
	}
	packDir := filepath.Join(s.Dir, "objects", "pack")
	bitmap := bitmapOf(t, packDir)
	graph := filepath.Join(s.Dir, "objects", "info", "commit-graph")

	w.log.recreate(before)
	s = w.sync(t, c, repo)
	for _, path := range []string{
		filepath.Join(packDir, "pack-"+dropped.GitChecksum+".pack"),
		filepath.Join(packDir, "multi-pack-index"), bitmap, graph,
	} {
		if statOK(t, path) {
			t.Errorf("%s is still there after the sweep removed a pack under it", path)
		}
	}
	if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
		t.Errorf("fsck after a pack was removed under the index: %v", err)
	}
	if doc := readStateDoc(t, s.Dir); doc.Midx.Packs != 0 || doc.Midx.Bitmap || doc.Midx.Bytes != 0 {
		t.Errorf("the state's midx is %+v after its files were dropped, want none", doc.Midx)
	}

	clk.Add(cache.MidxMinInterval)
	if ran := cache.MaintainerPass(c); len(ran) != 1 {
		t.Fatalf("the maintainer ran %v after the sweep, want the repository", ran)
	}
	bitmapOf(t, packDir)
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
	if doc := readStateDoc(t, s.Dir); !doc.Midx.LastRun.IsZero() || doc.Midx.Packs != 0 {
		t.Errorf("a run refused a slot recorded %+v", doc.Midx)
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
	c := w.open(t, clk.on, func(o *cache.Options) {
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
	if doc := readStateDoc(t, s.Dir); doc.Midx.Graph || !doc.Midx.LastRun.IsZero() {
		t.Errorf("a run refused its slot recorded %+v, want nothing", doc.Midx)
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
	if doc := readStateDoc(t, s.Dir); !doc.Midx.Graph || !doc.Midx.LastRun.IsZero() {
		t.Errorf("the state's midx is %+v after the graph-first run, want its graph and no run the duty "+
			"cycle counts", doc.Midx)
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
	if doc := readStateDoc(t, s.Dir); !doc.Midx.LastRun.IsZero() || doc.Midx.Took != 0 {
		t.Errorf("a run Stop ended persisted %+v, want no run", doc.Midx)
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
	if doc := readStateDoc(t, s.Dir); !doc.Midx.LastRun.Equal(ranAt) || doc.Midx.Packs != 0 {
		t.Errorf("the failed run recorded %+v, want its end and no index", doc.Midx)
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
