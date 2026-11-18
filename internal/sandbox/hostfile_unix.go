//go:build unix

package sandbox

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/crewlet/crewlet/internal/hostbox"
)

// openRegularIn opens name beneath root (a local box's directory opened as an
// os.Root: [boxLayout.openHome], [boxLayout.openRecords]) for reading, and only
// a REGULAR FILE, as it lies; an absent path answers [fs.ErrNotExist], which
// callers read as "not written yet". The box's side of the mount writes every
// name on the path, so: the open is NON-BLOCKING (a named pipe would block it
// uncancellably, wedging the completion poll) and the descriptor is checked
// before a byte is read (a device may never end), anything else being a
// [NotRegularFileError], never an empty file; every directory on the way is
// walked through the root, which follows a link only beneath itself, and opened
// as a directory ([notDirOnWay]); and the last element is opened O_NOFOLLOW, so
// a link there, even one inside the box, is refused as a link rather than
// followed or, dangling, read as not written yet.
func openRegularIn(root *os.Root, name string) (*os.File, error) {
	host := filepath.Join(root.Name(), name)
	dir, err := root.OpenFile(filepath.Dir(name), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, unix.ENOTDIR) {
			if refused := notDirOnWay(root, filepath.Dir(name), false); refused != nil {
				return nil, refused
			}
		}
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
				unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
			if !errors.Is(openErr, unix.EINTR) {
				break
			}
		}
		// SOME KINDS ARE REFUSED BEFORE THERE IS A DESCRIPTOR TO ASK: a link at
		// the last element (ELOOP under O_NOFOLLOW) and a socket (ENXIO, which
		// must not read as a box that could not be read, retried until the run
		// is lost). So the name itself is asked, in the same directory; not an
		// absent name, which is "not written yet".
		if openErr != nil && !errors.Is(openErr, unix.ENOENT) {
			kind = kindAt(int(dirfd), base)
		}
	}); err != nil {
		return nil, &fs.PathError{Op: "open", Path: host, Err: err}
	}
	if openErr != nil {
		if kind != "" {
			return nil, &NotRegularFileError{Path: host, Kind: kind}
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
		return nil, &NotRegularFileError{Path: host, Kind: what}
	}
	if err := unix.SetNonblock(fd, false); err != nil {
		_ = unix.Close(fd)
		return nil, &fs.PathError{Op: "open", Path: host, Err: err}
	}
	return os.NewFile(uintptr(fd), host), nil
}

// replaceIn puts content at name beneath root WHOLE, BY REPLACING THE FILE: the
// one way the engine writes a local box's file ([writeHomeFile], [writeRecord]).
// A run reads files the engine writes while it works (its git credential helper
// reads the seat's token file at every call), and a later engine judges a
// record by what it holds (an empty [jobEnd] witness is not its own), so no
// reader may find a write half done: the content is staged beside name, synced
// and renamed over it ([hostbox.ReplaceFileIn], which removes the staged file
// on failure). The rename follows no link at name and rewrites no other name
// of the file; the new file has [hostbox.FileMode], never a mode the box
// widened. Anything but a regular file at name, or a directory on the way, is
// the box's doing and refused as what it is ([notRegularAt]), looked at before
// the write and again after one that failed. name's directory must exist.
func replaceIn(root *os.Root, name string, content []byte) error {
	return replaceFrom(root, name, bytes.NewReader(content))
}

// replaceFrom is [replaceIn] for content read as it is written: a streamed
// write ([Sandbox.WriteStream]), copied into the staged file a piece at a time.
// A source that fails part way fails the write before anything is renamed, so
// the file at name is the one that was there.
func replaceFrom(root *os.Root, name string, content io.Reader) error {
	return replaceWith(root, name, content, hostbox.ReplaceFileIn)
}

// boxReplace is how [replaceWith] puts a whole file beneath a root:
// [hostbox.ReplaceFileIn], in every box. A parameter so a test can put an
// obstacle at the name between the look and the replacement, the one refusal
// nothing left in the tree beforehand reaches.
type boxReplace func(root *os.Root, name string, content io.Reader) error

// replaceWith is [replaceFrom] with its replacement handed in ([boxReplace]).
func replaceWith(root *os.Root, name string, content io.Reader, replace boxReplace) error {
	if refused := notRegularAt(root, name); refused != nil {
		return refused
	}
	if err := replace(root, name, content); err != nil {
		if refused := notRegularAt(root, name); refused != nil {
			return refused
		}
		return err
	}
	return nil
}

// notRegularAt is the refusal of a write at name beneath root
// ([NotRegularFileError], Write): what is at name where that is not a regular
// file, or what is on the way to it where that is not a directory. nil for a
// regular file, for nothing at all, and for a name that cannot be looked at,
// whose write then says why itself. A look, not an open, so a named pipe
// anywhere on the path is named rather than waited on.
func notRegularAt(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	switch {
	case err == nil:
		if info.Mode().Type() == 0 {
			return nil
		}
		return &NotRegularFileError{Path: filepath.Join(root.Name(), name), Kind: modeKind(info.Mode()), Write: true}
	case errors.Is(err, unix.ENOTDIR):
		if refused := notDirOnWay(root, filepath.Dir(name), true); refused != nil {
			return refused
		}
	}
	return nil
}

// notDirOnWay is the refusal of a path whose way to its file holds something
// other than a directory: the first such component of dir, named for what it
// is, or nil where every component is a directory by now, or one cannot be
// looked at (the open's own error then says why). Each is looked at through
// the root, as the open walked it; a look opens nothing, so a pipe is not
// waited on.
func notDirOnWay(root *os.Root, dir string, write bool) *NotRegularFileError {
	way := ""
	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		if part == "" || part == "." {
			continue
		}
		way = filepath.Join(way, part)
		switch kind := kindIn(root, way); kind {
		case "":
			return nil
		case "a directory":
		default:
			return &NotRegularFileError{Path: filepath.Join(root.Name(), way), Kind: kind,
				Write: write, OnTheWay: true}
		}
	}
	return nil
}

// kindIn names what name is beneath root ([modeKind]), or "" for a name that
// cannot be looked at there.
func kindIn(root *os.Root, name string) string {
	info, err := root.Stat(name)
	if err != nil {
		return ""
	}
	return modeKind(info.Mode())
}

// modeKind names what a file mode describes, as a reader says it — a
// directory included, which [fileKind] reads as "a directory" too.
func modeKind(mode fs.FileMode) string {
	switch mode.Type() {
	case 0:
		return "a regular file"
	case fs.ModeDir:
		return "a directory"
	case fs.ModeNamedPipe:
		return "a named pipe"
	case fs.ModeDevice | fs.ModeCharDevice:
		return "a character device"
	case fs.ModeDevice:
		return "a block device"
	case fs.ModeSocket:
		return "a socket"
	case fs.ModeSymlink:
		return kindSymlink
	}
	return "something other than a file"
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
// file. The mode's width differs between platforms, so it is switched on where
// it lies rather than converted.
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
