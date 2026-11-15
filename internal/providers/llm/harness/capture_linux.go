package harness

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// memoryCaptures: linux has an anonymous file in memory (memfd_create), so a
// child handed or printing a credential captures into one.
const memoryCaptures = true

// reopenCapture opens the engine's read-write description of the file w
// names, through /proc/self/fd: a second description of the very inode w
// holds, whatever has since been renamed onto the path w was opened by.
func reopenCapture(w *os.File, _ string) (*os.File, error) {
	return os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", w.Fd()), os.O_RDWR, 0) //nolint:gosec // this process's own descriptor
}

// openMemoryCapture makes a capture in memory for stream: an anonymous
// regular file with no name in any directory and no block on any disk, the
// engine's description r read-write, and the child's w a second description
// of it, appending.
func openMemoryCapture(stream string) (r, w *os.File, err error) {
	fd, err := unix.MemfdCreate("crewlet-"+stream, unix.MFD_CLOEXEC)
	if err != nil {
		return nil, nil, err
	}
	r = os.NewFile(uintptr(fd), "memfd:crewlet-"+stream) //nolint:gosec // a descriptor the kernel just returned is not negative
	w, err = os.OpenFile(fmt.Sprintf("/proc/self/fd/%d", fd), os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		_ = r.Close()
		return nil, nil, err
	}
	return r, w, nil
}

// reserveRoom allocates n bytes of f's blocks from off, its size kept.
func reserveRoom(f *os.File, off, n int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_KEEP_SIZE, off, n) //nolint:gosec // a descriptor fits an int
}

// punchHole gives n bytes of f from off back to the volume, its size kept.
func punchHole(f *os.File, off, n int64) error {
	return unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, off, n) //nolint:gosec // a descriptor fits an int
}

// blockSize is f's filesystem's preferred block size, which a punch is
// aligned to; a page where the kernel says nothing.
func blockSize(f *os.File) int64 {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil || st.Blksize <= 0 { //nolint:gosec // a descriptor fits an int
		return 4096
	}
	return int64(st.Blksize) //nolint:unconvert // an int32 on linux/arm64
}

// allocated is how many bytes of the volume f's blocks take.
func allocated(f *os.File) int64 {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil { //nolint:gosec // a descriptor fits an int
		return 0
	}
	return st.Blocks * 512
}
