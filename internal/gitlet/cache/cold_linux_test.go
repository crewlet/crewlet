//go:build linux

package cache_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// dropCached drops the kernel's cached pages of every file under dir
// (posix_fadvise DONTNEED, after a sync, since a dirty page is not dropped)
// and answers true, so a benchmark's cold arm reads at the disk's throughput.
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
