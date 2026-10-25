//go:build linux

package cache_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// dropCached asks the kernel to drop every page it caches of the files under
// dir (posix_fadvise DONTNEED, each made durable first, since a dirty page is
// not dropped), so the next read of them is the volume's — and answers true:
// a benchmark's cold arm reads at the disk's own throughput.
func dropCached(b *testing.B, dir string) bool {
	b.Helper()
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := f.Sync(); err != nil {
			return err
		}
		return unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
	})
	if err != nil {
		b.Fatalf("dropping %s from the page cache: %v", dir, err)
	}
	return true
}
