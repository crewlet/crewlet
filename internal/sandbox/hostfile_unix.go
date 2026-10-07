//go:build unix

package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
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
	var (
		fd  int
		err error
	)
	for {
		fd, err = syscall.Open(target, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	switch {
	case errors.Is(err, syscall.ELOOP) && isLink(target):
		// ELOOP is also a loop among the links above the last element; only
		// a link AT it is the refusal O_NOFOLLOW makes, so that is asked.
		return nil, &NotRegularFileError{Path: target, Kind: kindSymlink}
	case err != nil:
		return nil, &fs.PathError{Op: "open", Path: target, Err: err}
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		_ = syscall.Close(fd)
		return nil, &fs.PathError{Op: "stat", Path: target, Err: err}
	}
	if what := fileKind(st); what != "" {
		_ = syscall.Close(fd)
		return nil, &NotRegularFileError{Path: target, Kind: what}
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		_ = syscall.Close(fd)
		return nil, &fs.PathError{Op: "open", Path: target, Err: err}
	}
	return os.NewFile(uintptr(fd), target), nil
}

// isLink reports whether target itself is a symbolic link.
func isLink(target string) bool {
	info, err := os.Lstat(target)
	return err == nil && info.Mode()&fs.ModeSymlink != 0
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
