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
// AND EACH OF THEM IS OPENED AS A DIRECTORY, the last one too, so anything
// else in its place — a named pipe where `.crewlet` was, a device, a file — is
// refused at once rather than opened: the root opens the directories above the
// last as directories, and the last as it is asked to, which a plain open of a
// pipe there turned back into the wait above. Refused, it is named for what it
// is ([notDirOnWay]).
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
		// SOME KINDS ARE REFUSED BEFORE THERE IS A DESCRIPTOR TO ASK: a link
		// at the last element (ELOOP under O_NOFOLLOW) and a socket (ENXIO,
		// which a read of one used to report as a box that could not be
		// read, so a collection retried it until the run was lost). So the
		// name itself is asked what it is, in the same directory. Not for an
		// absent name, which is the answer "not written yet".
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

// replaceIn puts content at name beneath root WHOLE, BY REPLACING THE FILE
// rather than rewriting it: the one way the engine writes a local box's file,
// in its home and in its records alike ([writeHomeFile], [writeRecord]).
//
// A READER NEVER FINDS A WRITE HALF DONE. The engine writes into a box a run is
// using — the completion poll rewrites a seat's GitHub App token file under
// the running job, and the job's git credential helper reads it at every call
// — and a write that truncated the file and then wrote it in place left it
// empty, then part written, in between: a push that read it then
// authenticated with nothing. A write the engine's process stopped part way
// through left it so for good, and a record a later engine reads back is
// judged by what it holds: an empty witness ([jobEnd]) is not one this engine
// wrote, so a job its engine's restart killed was collected as one that ended
// rather than carried on. So the content is written to a fresh file beside
// name, synced, and renamed over name in one step ([hostbox.ReplaceFileIn]):
// a reader opens the old file or the new one, and a crash leaves one of the
// two.
//
// AND IT WRITES THROUGH NOTHING AT NAME. A rename replaces the directory entry:
// it follows no link there and rewrites no other name of the same file (a hard
// link the box made to a file of its own). The file staged beside it is
// created exclusively, which follows no link either, and every directory on
// the way is walked through the root, as a read's is.
//
// ONLY A REGULAR FILE IS REPLACED, or nothing at all. Anything else at name —
// a link, a directory, a named pipe, a socket, a device — is the box's own
// doing, so it is refused as what it is ([NotRegularFileError], Write) and
// left as the box made it, and so is anything but a directory on the way
// ([notDirOnWay]). It is looked at before the write, and again after one
// that failed, so a directory the box put there in between is refused as one
// too.
//
// NO STAGED FILE OUTLIVES A WRITE THAT FAILED: [hostbox.ReplaceFileIn] removes
// it. The new file has [hostbox.FileMode] whatever the one it replaces had, so
// new content is never handed a mode the box widened. name's directory must
// exist.
func replaceIn(root *os.Root, name string, content []byte) error {
	return replaceWith(root, name, content, hostbox.ReplaceFileIn)
}

// boxReplace is how [replaceWith] puts a whole file beneath a root:
// [hostbox.ReplaceFileIn], in every box.
//
// A PARAMETER rather than a call written in, for the one refusal nothing a
// test can leave in the tree beforehand reaches: something the box puts at
// the name AFTER the look that would have refused it, which the replacement
// itself then meets. So a test hands in a replacement that puts the obstacle
// down between the two.
type boxReplace func(root *os.Root, name string, content io.Reader) error

// replaceWith is [replaceIn] with its replacement handed in ([boxReplace]).
func replaceWith(root *os.Root, name string, content []byte, replace boxReplace) error {
	if refused := notRegularAt(root, name); refused != nil {
		return refused
	}
	if err := replace(root, name, bytes.NewReader(content)); err != nil {
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
// whose write then says why itself.
//
// A LOOK, NOT AN OPEN: the root walks the directories on the way as
// directories and looks at the last element without following it, so a named
// pipe anywhere on the path is named rather than waited on.
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
// looked at — the open's own error then says why.
//
// Each component is looked at through the root, which follows a link only to
// somewhere beneath itself, as the open did; a look opens nothing, so a pipe
// there is not waited on.
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
