//go:build unix

package sandbox

import (
	"os"

	"golang.org/x/sys/unix"
)

// hostFile is a file on the engine host opened for a read, and what its open
// found of it ([ChangeOf]): the same descriptor is asked again, so a file
// renamed over the path changes nothing for the inode read, and only that
// inode written, cut or grown in place is a change. The descriptor is its own
// identity, the inode it opened for as long as it is open, so no device or
// inode is compared: they could never differ.
type hostFile struct {
	file   *os.File
	opened unix.Stat_t
	err    error
}

// openedFile is f as its open found it.
func openedFile(f *os.File) hostFile {
	st, err := fstat(f)
	return hostFile{file: f, opened: st, err: err}
}

// fstat is f's own record in the kernel.
func fstat(f *os.File) (unix.Stat_t, error) {
	var st unix.Stat_t
	rc, err := f.SyscallConn()
	if err != nil {
		return st, err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = unix.Fstat(int(fd), &st) }); err != nil {
		return st, err
	}
	return st, serr
}

// change compares the file now with its open.
func (h hostFile) change() (FileChange, error) {
	if h.err != nil {
		return "", h.err
	}
	now, err := fstat(h.file)
	if err != nil {
		return "", err
	}
	switch {
	case now.Size < h.opened.Size:
		return FileChanged, nil
	case now.Size > h.opened.Size:
		return FileAppended, nil
	case now.Mtim != h.opened.Mtim:
		return FileChanged, nil
	case now.Ctim != h.opened.Ctim && now.Nlink == h.opened.Nlink:
		// The change time, which no user sets, tells a write whose
		// modification time was put back; but an unlink moves it too, with
		// the link count, and the clear before each job unlinks the last
		// job's streams under a read of them, so a moved count leaves it
		// unread.
		return FileChanged, nil
	}
	return FileUnchanged, nil
}
