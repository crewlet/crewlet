//go:build unix

package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"syscall"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// openHostRegular opens a file a local box holds, for reading — and only a
// REGULAR FILE, as it lies.
//
// THE OTHER SIDE OF THE MOUNT IS NOT THE ENGINE'S. A container box's home is
// written by the agent inside it, which can run any command, and the engine
// reads that directory from the host. A plain open(2) of a named pipe blocks
// until a writer appears and cannot be cancelled, so `mkfifo ~/.crewlet/done`
// wedged the completion poll — which polled one box after another, so no run
// on the node was polled or kept alive again — and a character device that
// never ends (`/dev/zero`'s numbers, made with the mknod a default container
// keeps) streamed for as long as the reader would read. So the open is made
// NON-BLOCKING, which returns at once whatever the path is, and the
// descriptor is checked before a byte is read: anything but a regular file is
// a [NotRegularFileError] naming the path and what it is, which a poll
// reports and a collection describes as that piece's refusal — never an empty
// file, which is the reading of a run that has not written it yet. The flag
// is cleared again for the read, so a regular file reads as it always did.
//
// NOT THROUGH A LINK, EITHER. The path arrives resolved by the escape check,
// which follows every link in it that leads somewhere, so a link still at its
// last element when it is opened is one of two things: a link that leads
// nowhere (the check resolves a missing last element to itself), or one put
// there since by the side of the mount that does not have to stay inside it.
// O_NOFOLLOW refuses both rather than following either out of the box, and
// both are refused as what they are — a link — rather than read as a file
// not written yet, which a dangling link had been while the open followed it.
//
// A path that is not there answers [fs.ErrNotExist] through the error, which
// is what the callers read as "not written yet".
func openHostRegular(target string) (*os.File, error) {
	return openHostFileAs(target, syscall.O_RDONLY, false)
}

// openHostWritable opens a file a local box holds for WRITING it whole —
// created where nothing is, emptied where a regular file is — under the same
// rule as [openHostRegular], for the same reason.
//
// The engine writes into a box after something has run in it: a shim and a
// coding CLI's configuration after the setup steps' commands, which run
// whatever a checkout's install scripts say, and a follow-up run's
// configuration into a box a coding agent has had to itself. A plain
// os.WriteFile there followed a link at the last element — one that leads
// nowhere passes the escape check as itself, so the write CREATED a file
// wherever the link pointed on the engine host — and an open of a named pipe
// for writing waited for a reader for good, wedging the launch. So the open
// is non-blocking and follows no link, and anything but a regular file is
// refused before a byte is written.
func openHostWritable(target string) (*os.File, error) {
	return openHostFileAs(target, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_TRUNC, true)
}

// openHostFileAs is the one open behind [openHostRegular] and
// [openHostWritable].
func openHostFileAs(target string, mode int, write bool) (*os.File, error) {
	var (
		fd  int
		err error
	)
	for {
		fd, err = syscall.Open(target, mode|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW,
			uint32(hostbox.FileMode))
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		// SOME KINDS ARE REFUSED BEFORE THERE IS A DESCRIPTOR TO ASK: a
		// link at the last element (ELOOP under O_NOFOLLOW), a socket
		// (ENXIO, which a read of one used to report as a box that could
		// not be read, so a collection retried it until the run was lost),
		// and for a write a pipe nobody reads (ENXIO) and a directory
		// (EISDIR). So the path itself is asked what it is. Not for an
		// absent path, which is the answer "not written yet"; and a path
		// whose own lookup fails — a loop among the links ABOVE the last
		// element is ELOOP too — names no kind and stays the error it was.
		if !errors.Is(err, syscall.ENOENT) {
			if what := kindAt(target); what != "" {
				return nil, &NotRegularFileError{Path: target, Kind: what, Write: write}
			}
		}
		return nil, &fs.PathError{Op: "open", Path: target, Err: err}
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		_ = syscall.Close(fd)
		return nil, &fs.PathError{Op: "stat", Path: target, Err: err}
	}
	if what := fileKind(st); what != "" {
		_ = syscall.Close(fd)
		return nil, &NotRegularFileError{Path: target, Kind: what, Write: write}
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		_ = syscall.Close(fd)
		return nil, &fs.PathError{Op: "open", Path: target, Err: err}
	}
	return os.NewFile(uintptr(fd), target), nil
}

// kindAt names what target itself is, without following a link at it, or ""
// for a regular file and for a path that cannot be looked up.
func kindAt(target string) string {
	var st syscall.Stat_t
	if err := syscall.Lstat(target, &st); err != nil {
		return ""
	}
	return fileKind(st)
}

// fileKind names what st describes, as a reader says it, or "" for a regular
// file. (The mode's width differs between platforms, so it is switched on
// where it lies rather than converted.)
func fileKind(st syscall.Stat_t) string {
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFREG:
		return ""
	case syscall.S_IFIFO:
		return "a named pipe"
	case syscall.S_IFCHR:
		return "a character device"
	case syscall.S_IFBLK:
		return "a block device"
	case syscall.S_IFSOCK:
		return "a socket"
	case syscall.S_IFDIR:
		return "a directory"
	case syscall.S_IFLNK:
		return kindSymlink
	}
	return "something other than a file"
}
