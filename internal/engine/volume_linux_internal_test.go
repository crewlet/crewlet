package engine

import (
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/config"
)

// ANOTHER FILESYSTEM IS ANOTHER VOLUME, read off the machine rather than off a
// fake: procfs, which every Linux node mounts at /proc, is a filesystem of its
// own with no space at all, so it shares nothing with the temporary directory
// a case writes to — and a cache whose store lives there has a volume of its
// own, which takes seven eighths of its room.
//
// Linux alone, because /proc is the one directory known to be another
// filesystem on every Linux machine; the arithmetic it reaches is
// platform-free and held on every platform by the cases beside it.
//
// Mutation: make sameVolume answer true whatever it reads, and both halves go
// red.
func TestAnotherFilesystemIsAnotherVolume(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if sameVolume(dir, "/proc") {
		t.Fatalf("%s and /proc read as one volume", dir)
	}
	if !sameVolume(dir, filepath.Join(dir, "beside")) {
		t.Fatalf("two directories of %s read as two volumes", dir)
	}

	const gib = int64(1) << 30
	b := config.DefaultBootstrap()
	b.Store.Path = "/proc/crewlet.db"
	layout := prepareCache(t, filepath.Join(dir, "git"))
	defer layout.Release()
	size, err := codeCacheBudget(&b, layout, 16, cacheVolume{
		free:   func(string) (int64, error) { return 100 * gib, nil },
		same:   sameVolume,
		broker: noBroker,
	})
	if err != nil {
		t.Fatalf("codeCacheBudget: %v", err)
	}
	// THE ROOM: the free space less the request spools' gibibyte and the
	// revision facts' patch spools, plus what the cache holds.
	room := 99*gib - query.PatchSpoolAllowance + layout.Held
	if !size.dedicated || size.budget != config.DerivedCacheMaxBytes(size.room, true) ||
		size.budget != room/8*7+room%8*7/8 {
		t.Fatalf("a cache whose store, snapshots and estate are on another filesystem sized %+v, "+
			"want a volume of its own and seven eighths of its room", size)
	}
}
