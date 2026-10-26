//go:build unix

package sandbox

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// openRegularIn opens name beneath root — a directory of a local box, opened
// as an os.Root ([boxLayout.openHome], [boxLayout.openRecords]) — for reading,
// and only a REGULAR FILE, as it lies.
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
// EVERY DIRECTORY ON THE WAY IS WALKED THROUGH THE ROOT, one component at a
// time, so none of them — a link the box put in place of one, whenever it put
// it there — leads the open outside the box: the root follows a link only to
// somewhere beneath itself, and refuses the rest ([os.Root]). A path checked
// and then opened by name was two walks of whatever the names held at each
// moment, and the box's side writes the names.
//
// AND NOT THROUGH A LINK AT THE LAST ELEMENT, not even one inside the box:
// the file is opened in the directory the root reached with O_NOFOLLOW, so a
// link there is refused as what it is — a link — rather than followed, or read
// as a file not written yet, which a dangling link had been while the open
// followed it.
//
// A path that is not there answers [fs.ErrNotExist] through the error, which
// is what the callers read as "not written yet".
func openRegularIn(root *os.Root, name string) (*os.File, error) {
	return openInAs(root, name, unix.O_RDONLY, false)
}

// openWritableIn opens name beneath root for WRITING it whole — created where
// nothing is, emptied where a regular file is — under the same rule as
// [openRegularIn], for the same reason.
//
// The engine writes into a box after something has run in it: a shim and a
// coding CLI's configuration after the setup steps' commands, which run
// whatever a checkout's install scripts say, and a follow-up run's
// configuration into a box a coding agent has had to itself. A plain
// os.WriteFile there followed a link at the last element — one that leads
// nowhere, so the write CREATED a file wherever the link pointed on the engine
// host — and an open of a named pipe for writing waited for a reader for good,
// wedging the launch. So the open is non-blocking and follows no link, and
// anything but a regular file is refused before a byte is written.
func openWritableIn(root *os.Root, name string) (*os.File, error) {
	return openInAs(root, name, unix.O_WRONLY|unix.O_CREAT|unix.O_TRUNC, true)
}

// openInAs is the one open behind [openRegularIn] and [openWritableIn]: the
// directory reached through root, the last element opened in it by its
// descriptor.
func openInAs(root *os.Root, name string, mode int, write bool) (*os.File, error) {
	host := filepath.Join(root.Name(), name)
	dir, err := root.Open(filepath.Dir(name))
	if err != nil {
		return nil, err
	}
	defer func() { _ = dir.Close() }()
	raw, err := dir.SyscallConn()
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: host, Err: err}
	}
	base := filepath.Base(name)
	var (
		fd      int
		openErr error
		kind    string
	)
	if err := raw.Control(func(dirfd uintptr) {
		for {
			fd, openErr = unix.Openat(int(dirfd), base,
				mode|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, uint32(hostbox.FileMode))
			if !errors.Is(openErr, unix.EINTR) {
				break
			}
		}
		// SOME KINDS ARE REFUSED BEFORE THERE IS A DESCRIPTOR TO ASK: a link
		// at the last element (ELOOP under O_NOFOLLOW), a socket (ENXIO,
		// which a read of one used to report as a box that could not be
		// read, so a collection retried it until the run was lost), and for
		// a write a pipe nobody reads (ENXIO) and a directory (EISDIR). So
		// the name itself is asked what it is, in the same directory. Not
		// for an absent name, which is the answer "not written yet".
		if openErr != nil && !errors.Is(openErr, unix.ENOENT) {
			kind = kindAt(int(dirfd), base)
		}
	}); err != nil {
		return nil, &fs.PathError{Op: "open", Path: host, Err: err}
	}
	if openErr != nil {
		if kind != "" {
			return nil, &NotRegularFileError{Path: host, Kind: kind, Write: write}
		}
		return nil, &fs.PathError{Op: "open", Path: host, Err: openErr}
	}
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		_ = unix.Close(fd)
		return nil, &fs.PathError{Op: "stat", Path: host, Err: err}
	}
	if what := fileKind(st); what != "" {
		_ = unix.Close(fd)
		return nil, &NotRegularFileError{Path: host, Kind: what, Write: write}
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, &fs.PathError{Op: "open", Path: host, Err: err}
	}
	return os.NewFile(uintptr(fd), host), nil
}

// kindAt names what base is in the directory dirfd, without following a link
// at it, or "" for a regular file and for a name that cannot be looked up.
func kindAt(dirfd int, base string) string {
	var st unix.Stat_t
	if err := unix.Fstatat(dirfd, base, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return ""
	}
	return fileKind(st)
}

// fileKind names what st describes, as a reader says it, or "" for a regular
// file. (The mode's width differs between platforms, so it is switched on
// where it lies rather than converted.)
func fileKind(st unix.Stat_t) string {
	switch st.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		return ""
	case unix.S_IFIFO:
		return "a named pipe"
	case unix.S_IFCHR:
		return "a character device"
	case unix.S_IFBLK:
		return "a block device"
	case unix.S_IFSOCK:
		return "a socket"
	case unix.S_IFDIR:
		return "a directory"
	case unix.S_IFLNK:
		return kindSymlink
	}
	return "something other than a file"
}
