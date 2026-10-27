package store

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// VolumeFree is what an unprivileged process may still write on the volume
// dir is on: statfs's available blocks, never its free ones, which count the
// reserve only root can reach — and this engine does not run as root.
//
// THE MEASURE a judgement of whether a copy has room reads — the donor
// snapshot's gate — of what the copy writes where it lands and of the log it
// holds back on the live file's own volume (copies.go).
//
// BOTH CONVERSIONS ARE LOAD-BEARING ACROSS THE RELEASE MATRIX, whatever a
// linter on linux says: Statfs_t.Bsize is int64 on linux and uint32 on darwin,
// and both are release targets.
func VolumeFree(dir string) (int64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("store: measure the free space on %s: %w", dir, err)
	}
	//nolint:unconvert // int64(st.Bsize) is a no-op on linux and required on darwin
	return int64(st.Bavail) * int64(st.Bsize), nil
}
