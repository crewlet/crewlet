package cache_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// A FULL ROUND INDEXES MANY PACKS BEFORE IT ANSWERS: one push is one pack and
// nothing compacts them, so the request a cold cache was synced for would
// otherwise be served by git searching every pack's index for every object.
// The round's index has no bitmap — a merge of the packs' own indexes — and
// a busy node answers without it rather than wait: slower, never wrong.
func TestAFullRoundIndexesManyPacksBeforeItAnswers(t *testing.T) {
	t.Parallel()
	t.Run("at MidxPacks", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		for range cache.MidxPacks {
			w.push(t, repo, "refs/heads/main", nil)
		}
		c := w.open(t)
		s := w.sync(t, c, repo)
		packDir := filepath.Join(s.Dir, "objects", "pack")
		if !statOK(t, filepath.Join(packDir, "multi-pack-index")) {
			t.Fatal("a full round of MidxPacks packs answered with no multi-pack-index")
		}
		for _, name := range names(t, packDir) {
			if filepath.Ext(name) == ".bitmap" {
				t.Errorf("the round's index has a bitmap (%s): that is the maintainer's walk", name)
			}
		}
		doc := readStateDoc(t, s.Dir)
		if doc.Midx.Packs != cache.MidxPacks || doc.Midx.Bitmap || !doc.Midx.LastRun.IsZero() || doc.Midx.Bytes == 0 {
			t.Errorf("the state's midx is %+v, want %d packs, no bitmap, no maintenance run, its bytes", doc.Midx,
				cache.MidxPacks)
		}
		if _, err := runGit(t, w.pool, s.Dir, "", "fsck", "--connectivity-only", "--no-progress"); err != nil {
			t.Errorf("fsck over the round's index: %v", err)
		}
		dst := filepath.Join(t.TempDir(), "clone.git")
		w.client.Run(t, "", "-c", "protocol.version=2", "clone", "--bare", "--no-local", s.Dir, dst)
	})
	t.Run("before the peel", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		for range cache.MidxPacks {
			w.push(t, repo, "refs/heads/main", nil)
		}
		w.tag(t, repo, "v1", true)
		var indexed, peeled bool
		c := w.open(t, func(o *cache.Options) {
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
		for range cache.MidxPacks - 1 {
			w.push(t, repo, "refs/heads/main", nil)
		}
		c := w.open(t)
		s := w.sync(t, c, repo)
		if statOK(t, filepath.Join(s.Dir, "objects", "pack", "multi-pack-index")) {
			t.Error("a full round under MidxPacks packs wrote an index")
		}
	})
	t.Run("every read slot held", func(t *testing.T) {
		t.Parallel()
		w := newSyncWorld(t, onMemory)
		repo := w.repo(t, 1)
		for range cache.MidxPacks {
			w.push(t, repo, "refs/heads/main", nil)
		}
		c := w.open(t)
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

// holdSlot holds one slot of class in pool until the case ends: a git process
// reading a stdin that gives nothing until then.
func holdSlot(t *testing.T, pool *gitproc.Pool, class gitproc.Class) {
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
	t.Cleanup(func() {
		close(release)
		<-done
	})
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
// packs it keeps.
func TestARoundNeverReplacesAnIndexThatExists(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	for range cache.MidxPacks {
		w.push(t, repo, "refs/heads/main", nil)
	}
	before := w.log.state()
	_, last := w.push(t, repo, "refs/heads/main", nil)
	c := w.open(t)
	s := w.sync(t, c, repo)
	packDir := filepath.Join(s.Dir, "objects", "pack")
	midx := filepath.Join(packDir, "multi-pack-index")
	// The maintainer's index, with its bitmap — written here by the client's
	// git, as the maintainer's run writes it. git keeps an identical index
	// it is asked to write again, and deletes the bitmap beside it.
	w.client.Run(t, s.Dir, "-c", "core.multiPackIndex=true", "multi-pack-index", "write", "--bitmap", "--no-progress")
	bitmap := bitmapOf(t, packDir)
	first := inode(t, midx)

	c = w.reopen(t, c)
	w.sync(t, c, repo)
	if inode(t, midx) != first || !statOK(t, bitmap) {
		t.Error("a full round after a restart replaced the index on disk, or its bitmap")
	}

	write(t, filepath.Join(s.Dir, "crewlet-state.json"), `{"format":`)
	c = w.reopen(t, c)
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
