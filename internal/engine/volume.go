package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Whether two directories draw on one pool of space decides the code cache's
// budget (config.DerivedCacheMaxBytes). They are ONE VOLUME when any reading
// says so: the same st_dev; the same nonzero f_fsid (overlay and bind mounts);
// or, on two filesystems with blocks, the same bytes available (f_bavail ×
// f_bsize) at one instant, within the coarser block size ([sameSpaceLeft]):
// the one reading a pool's btrfs subvolumes and quota-less ZFS datasets share,
// each having its own st_dev and f_fsid. The instant is read, not assumed: the
// cache's volume is read before and after the other, and the pair is apart
// only if it held still ([volumeReads]).
//
// UNCERTAIN IS SHARED: the wrong yes lets one tenant fill the disk, so a
// directory that cannot be read, or readings that never hold still, share.

// volumeReads is how many times a pair whose readings moved under them is read
// before it is judged shared: a volume that changes between every pair of
// statfs calls three times running has an identity that cannot be read.
const volumeReads = 3

// volumeStat is what decides whether two directories draw on one pool of
// space.
type volumeStat struct {
	// dev is the device id (stat's st_dev).
	dev uint64

	// fsid is the filesystem id (statfs's f_fsid), zero where the filesystem
	// gives none — which is no id at all, so two zeros are never one volume.
	fsid [2]int32

	// blocks is the filesystem's size in blocks (f_blocks); a filesystem of
	// none (proc, sysfs) holds no space to share.
	blocks uint64

	// avail is what an unprivileged process may still write (f_bavail ×
	// f_bsize), [volumeFree]'s figure.
	avail int64

	// bsize is the block avail is counted in (f_bsize): what it is rounded
	// down to, so two readings of one pool's space agree only to within it.
	bsize int64
}

// sameVolume reports whether two directories draw on one pool of space.
func sameVolume(a, b string) bool { return sameVolumeBy(statVolume, a, b) }

// sameVolumeBy is [sameVolume] over a reading of a directory's volume —
// [statVolume] in production, a fake in a test.
func sameVolumeBy(read func(dir string) (volumeStat, error), a, b string) bool {
	for range volumeReads {
		before, errA := read(a)
		other, errB := read(b)
		after, errC := read(a)
		if errA != nil || errB != nil || errC != nil {
			return true
		}
		switch {
		case before.dev == other.dev:
			return true
		case before.fsid != [2]int32{} && before.fsid == other.fsid:
			return true
		case before.blocks == 0 || other.blocks == 0:
			// A FILESYSTEM OF NO BLOCKS (proc, sysfs) holds no space, so the
			// space left cannot make the pair one: apart, asked before the
			// instant, or a busy cache volume would use up the reads and be
			// judged to share with procfs.
			return false
		case before.avail != after.avail:
			// THE CACHE'S VOLUME MOVED while the other was read: not one
			// instant, so read again. To the byte: one filesystem, one block.
			continue
		}
		return sameSpaceLeft(before, other)
	}
	return true
}

// sameSpaceLeft reports whether two readings of the space left are one pool's
// at one instant: within the COARSER of their two blocks, since each rounds the
// pool's space down to its own f_bsize (a ZFS dataset's recordsize, so datasets
// at 16 KiB and 128 KiB read one pool up to 128 KiB apart). Two filesystems
// that close are read as one, the safe direction. A zero block compares to the
// byte.
func sameSpaceLeft(a, b volumeStat) bool {
	diff := a.avail - b.avail
	if diff < 0 {
		diff = -diff
	}
	return diff < max(a.bsize, b.bsize, 1)
}

// statVolume reads the volume dir is on, or will be made on.
//
// A DIRECTORY NOT MADE YET is judged by its nearest existing ancestor, the
// filesystem it will be made on. Any other failure is an error, which
// [sameVolumeBy] reads as sharing.
func statVolume(dir string) (volumeStat, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return volumeStat{}, err
	}
	for {
		var st unix.Stat_t
		err := unix.Stat(dir, &st)
		switch {
		case err == nil:
			var fsst unix.Statfs_t
			if serr := unix.Statfs(dir, &fsst); serr != nil {
				return volumeStat{}, fmt.Errorf("engine: read the volume of %s: %w", dir, serr)
			}
			//nolint:unconvert // Stat_t.Dev is uint64 on linux and int32 on
			// darwin, and Statfs_t.Bsize int64 and uint32: both are release
			// targets, and the conversions are what make one expression
			// compile on the whole matrix.
			return volumeStat{dev: uint64(st.Dev), fsid: fsst.Fsid.Val, blocks: uint64(fsst.Blocks),
				avail: int64(fsst.Bavail) * int64(fsst.Bsize), bsize: int64(fsst.Bsize)}, nil
		case !errors.Is(err, fs.ErrNotExist):
			return volumeStat{}, fmt.Errorf("engine: read the volume of %s: %w", dir, err)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return volumeStat{}, fmt.Errorf("engine: no ancestor of %s exists", dir)
		}
		dir = parent
	}
}
