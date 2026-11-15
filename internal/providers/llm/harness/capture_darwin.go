package harness

import (
	"errors"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// memoryCaptures: darwin has no anonymous file in memory, so a child handed
// or printing a credential takes a file capture like every other, and its
// bytes reach the volume's blocks (the design's risk 7).
const memoryCaptures = false

// reopenCapture opens the engine's read-write description of the file w was
// opened as, BY PATH: on darwin /dev/fd/<n> is a dup of the same description
// (fd(4)), and the pin needs a second one. A path replaced between the two
// opens is refused as that ([sameFile]), never read as a filesystem without
// per-description locks.
func reopenCapture(w *os.File, path string) (*os.File, error) {
	r, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // a random name in the engine's own directory
	if err != nil {
		return nil, err
	}
	if err := sameFile(w, r); err != nil {
		_ = r.Close()
		return nil, err
	}
	return r, nil
}

// openMemoryCapture is never reached on darwin ([memoryCaptures]).
func openMemoryCapture(string) (*os.File, *os.File, error) {
	return nil, nil, errors.ErrUnsupported
}

// reserveRoom allocates n more bytes of f's blocks past its physical end of
// file, its size kept: F_PREALLOCATE counts from the physical end, and the
// caller hands it what the room it keeps lacks.
func reserveRoom(f *os.File, _, n int64) error {
	store := unix.Fstore_t{Flags: unix.F_ALLOCATEALL, Posmode: unix.F_PEOFPOSMODE, Length: n}
	return unix.FcntlFstore(f.Fd(), unix.F_PREALLOCATE, &store)
}

// fpunchhole is F_PUNCHHOLE's argument (<sys/fcntl.h>), which x/sys declares
// no type or wrapper for.
type fpunchhole struct {
	flags    uint32
	reserved uint32
	offset   int64
	length   int64
}

// punchHole gives n bytes of f from off back to the volume, its size kept.
//
// THE ARGUMENT IS PINNED, which also puts it on the heap. fcntl takes it as an
// integer, and x/sys has no wrapper for F_PUNCHHOLE that carries it as a
// pointer, as its FcntlFstore and FcntlFlock do. Left on the goroutine's
// stack, the struct can move while the call into x/sys grows or shrinks that
// stack, and an integer is no pointer the copy adjusts: the kernel would read
// a range to punch out of freed memory. runtime.Pinner keeps the struct where
// it was allocated until it is unpinned, and Pin takes it as an interface,
// so it escapes to the heap.
func punchHole(f *os.File, off, n int64) error {
	arg := &fpunchhole{offset: off, length: n}
	var pin runtime.Pinner
	pin.Pin(arg)
	defer pin.Unpin()
	_, err := unix.FcntlInt(f.Fd(), unix.F_PUNCHHOLE, int(uintptr(unsafe.Pointer(arg)))) //nolint:gosec // arg is pinned until fcntl returns (above)
	return err
}

// blockSize is f's filesystem's preferred block size, which a punch is
// aligned to (F_PUNCHHOLE refuses a range that is not); a page where the
// kernel says nothing.
func blockSize(f *os.File) int64 {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil || st.Blksize <= 0 { //nolint:gosec // a descriptor fits an int
		return 4096
	}
	return int64(st.Blksize)
}

// allocated is how many bytes of the volume f's blocks take.
func allocated(f *os.File) int64 {
	var st unix.Stat_t
	if err := unix.Fstat(int(f.Fd()), &st); err != nil { //nolint:gosec // a descriptor fits an int
		return 0
	}
	return st.Blocks * 512
}
