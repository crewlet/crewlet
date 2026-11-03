package engine

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// WHETHER TWO DIRECTORIES DRAW ON ONE POOL OF SPACE — the question the code
// cache's budget turns on, since a volume of its own takes seven eighths of
// what it has room for and a volume it shares takes half of what its bounded
// tenants leave (config.DerivedCacheMaxBytes).
//
// # Why the device id is not the whole answer
//
// A POOLED filesystem gives each subvolume or dataset a device id of its own
// while every one of them writes into the same space: btrfs subvolumes (the
// default layout of Fedora and openSUSE, where /home and /var are two of them)
// and ZFS datasets without a quota. A cache on one and the store on another
// compared by st_dev alone were judged apart, and the cache took seven eighths
// of the pool's free space while the broker beside the store still claimed
// three quarters of the same pool. So a pair is ONE VOLUME when any of three
// readings says so:
//
//   - the same device id (stat's st_dev) — one filesystem;
//   - the same filesystem id (statfs's f_fsid), where the filesystem gives one
//     — overlay and bind mounts of one filesystem;
//   - the same bytes available (f_bavail × f_bsize) read at ONE INSTANT, on two
//     filesystems that have any blocks at all — what a pool reports for every
//     subvolume and dataset that draws on it. btrfs masks each subvolume's id
//     into its f_fsid, and a ZFS dataset's f_blocks counts what it refers to,
//     so the space left is the one reading both share. "The same" is within
//     the coarser of the two block sizes, never to the byte: a ZFS dataset
//     counts in its own recordsize and rounds the pool's space down to it, so
//     two datasets of one pool tuned differently (a database's 16 KiB beside
//     the default 128 KiB) report it up to a block apart ([sameSpaceLeft]).
//
// "One instant" is read, not assumed: the cache's volume is read before and
// after the other, and a pair the space left could make one is judged APART
// only when the cache's reading did not move across the other's — a pool that
// changed and changed back in that window is the one shape that escapes. A
// volume that keeps moving is read again, a few times, and then read as
// shared. A pair one of whose filesystems has no blocks at all needs no
// instant: no reading of space can make it one, so it is apart at once.
//
// # Uncertain is shared
//
// The question decides whether one tenant may take a volume to itself, and
// the wrong yes is the one that fills the disk. So a directory that cannot be
// read reads as sharing, and so does a pair whose readings never hold still.
// Two distinct filesystems that happen to report the same space to within a
// block are read as one — the cheap direction to be wrong in.

// volumeReads is how many times a pair whose readings moved under them is read
// again before it is judged shared: three — a volume busy enough to change
// between every pair of statfs calls three times running is one whose
// identity cannot be read, and shared is the safe answer for it.
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
			// A FILESYSTEM OF NO BLOCKS (proc, sysfs) holds no space, so
			// the space left — the one reading the instant is for — cannot
			// make the pair one: apart, whether or not the cache's volume
			// moved. Asked after the instant, a cache volume written to
			// between every pair of reads (any busy disk: a suite's other
			// cases writing beside it) was read again until the reads ran
			// out, and then judged to share with procfs.
			return false
		case before.avail != after.avail:
			// THE CACHE'S VOLUME MOVED while the other was read, so the
			// space left on each is not one instant's: read again. To the
			// byte, since both readings are of one filesystem in one block.
			continue
		}
		return sameSpaceLeft(before, other)
	}
	return true
}

// sameSpaceLeft reports whether two readings of the space left are one pool's
// at one instant: within the COARSER of their two blocks, since each counts
// the pool's space in its own block and rounds down to it — a ZFS dataset's
// f_bsize is its recordsize, so a store's dataset at 16 KiB and a cache's at
// 128 KiB read the same pool up to 128 KiB apart, and compared to the byte
// they agreed only when the pool's free bytes fell within 16 KiB of a 128 KiB
// boundary. Two readings of one quantity rounded down to blocks of b1 and b2
// lie within max(b1, b2) of it and of each other; two filesystems that happen
// to be that close are read as one, the safe direction. A block that reads as
// none compares to the byte.
func sameSpaceLeft(a, b volumeStat) bool {
	diff := a.avail - b.avail
	if diff < 0 {
		diff = -diff
	}
	return diff < max(a.bsize, b.bsize, 1)
}

// statVolume reads the volume dir is on, or will be made on.
//
// A DIRECTORY NOT MADE YET is judged by its nearest ancestor that exists, which
// is the filesystem it will be made on — the snapshots' directory, say, before
// the first snapshot. Any other failure is an error, which [sameVolumeBy] reads
// as sharing.
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
