package sandbox

import (
	"bytes"
	"errors"
	"io"
	"os"
)

// FileChange is what became of a box file between the open a read was made
// from and now ([ChangeOf]): a read that is to stand for the file, or for its
// start, asks it once it has read what it wanted.
type FileChange string

const (
	// FileUnchanged is a file as the open found it: what was read of it is
	// the file as it stood.
	FileUnchanged FileChange = "unchanged"
	// FileAppended is a file grown past the size the open found: what was
	// read, to that size, is the start of a file still being written.
	FileAppended FileChange = "appended"
	// FileChanged is a file shrunk below the size the open found, or written
	// over within it, or a body that cannot tell: what was read may join two
	// versions of the file, so it stands for neither.
	FileChanged FileChange = "changed"
)

// Valid reports whether c is one of the changes a body tells.
func (c FileChange) Valid() bool {
	switch c {
	case FileUnchanged, FileAppended, FileChanged:
		return true
	}
	return false
}

// changeTeller is a body that tells what became of its file since its open.
type changeTeller interface {
	Change() (FileChange, error)
}

// ChangeOf is what became of the file body was opened on ([Sandbox.OpenWhole],
// [Sandbox.OpenRange], [Sandbox.OpenTail], [Sandbox.OpenFile]) since the open.
// A body that cannot tell is [FileChanged]: a read that cannot show it stood
// for one version of a file shows nothing of it. A local box asks the
// descriptor it opened ([hostFile]) for its device, inode, size, modification
// time and change time; the change time, which every write and every reset of
// the modification time sets, also catches a same-size rewrite whose
// modification time was put back. What it cannot tell: a process inside the
// box that deliberately truncates and regrows the harness's own output past
// the open's size reads as an append; nothing else cuts a job's streams, since
// the executor's clear before each job start leaves them new files.
func ChangeOf(body io.Reader) (FileChange, error) {
	if t, ok := body.(changeTeller); ok {
		return t.Change()
	}
	return FileChanged, nil
}

// ErrCannotReadAgain is [ReadAgain] of a body that cannot read its range
// again: a reader that needs those bytes again fails closed.
var ErrCannotReadAgain = errors.New("sandbox: this read cannot read its file again")

// againReader is a body that reads its range again.
type againReader interface {
	ReadAgain(p []byte, off int64) (int, error)
}

// ReadAgain reads len(p) bytes of the range body was opened on
// ([Sandbox.OpenRange], [Sandbox.OpenTail], [Sandbox.OpenWhole],
// [Sandbox.OpenFile]) again, from off within it, as [io.ReaderAt] does: from
// the file the open opened, as it stands now, never past the range's end at
// the open, and never by its path, which may name another file since. A body
// that cannot is [ErrCannotReadAgain]. What it reads is the file now: whether
// it is what an earlier read read is the caller's to check, with [ChangeOf].
func ReadAgain(body io.Reader, p []byte, off int64) (int, error) {
	if r, ok := body.(againReader); ok {
		return r.ReadAgain(p, off)
	}
	return 0, ErrCannotReadAgain
}

// hostSection is a section of a file on the engine host, closed with it,
// telling what became of the file since its open and reading it again.
type hostSection struct {
	io.Reader
	io.Closer
	section *io.SectionReader
	host    hostFile
}

// hostRange is the n bytes of f from off, as its open found it.
func hostRange(f *os.File, off, n int64) hostSection {
	s := io.NewSectionReader(f, off, n)
	return hostSection{Reader: s, Closer: f, section: s, host: openedFile(f)}
}

// Change implements [ChangeOf].
func (h hostSection) Change() (FileChange, error) { return h.host.change() }

// ReadAgain implements [ReadAgain].
func (h hostSection) ReadAgain(p []byte, off int64) (int, error) { return h.section.ReadAt(p, off) }

// hostWhole is a file on the engine host read to its end ([Sandbox.OpenFile]),
// telling what became of it since its open.
type hostWhole struct {
	*os.File
	host hostFile
}

// Change implements [ChangeOf].
func (h hostWhole) Change() (FileChange, error) { return h.host.change() }

// ReadAgain implements [ReadAgain], to the size the open found.
func (h hostWhole) ReadAgain(p []byte, off int64) (int, error) {
	return io.NewSectionReader(h.File, 0, h.host.opened.Size).ReadAt(p, off)
}

// absentBody is a file that was not there at its open: nothing was read and
// nothing promised, so whatever is there now is no change to what was read.
type absentBody struct{ io.Reader }

func (absentBody) Close() error { return nil }

// Change implements [ChangeOf].
func (absentBody) Change() (FileChange, error) { return FileUnchanged, nil }

// ReadAgain implements [ReadAgain]: nothing, as the open read.
func (absentBody) ReadAgain(p []byte, _ int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return 0, io.EOF
}

// absentFile is the body of a file not there at its open.
func absentFile() io.ReadCloser { return absentBody{bytes.NewReader(nil)} }

// fakeBody is a fake box's file as its open found it ([FakeSandbox]), told as
// a host file is ([hostFile]): its inode is asked again, so a file written
// over the path since is one the reader holds open, unchanged; an edit in
// place since is a change by the size it leaves, grown past the open's an
// append whatever it wrote, as a host's size is all a host can tell.
type fakeBody struct {
	io.Reader
	box    *FakeSandbox
	path   string
	inode  uint64
	edits  uint64
	opened []byte
	// from and n are the range the open read of opened.
	from, n int64
}

func (fakeBody) Close() error { return nil }

// Change implements [ChangeOf].
func (f fakeBody) Change() (FileChange, error) {
	f.box.mu.Lock()
	now, there := f.box.files[f.path]
	same := there && f.box.inodes[f.path] == f.inode
	edits := f.box.edits[f.path]
	f.box.mu.Unlock()
	switch {
	case !same || edits == f.edits:
		return FileUnchanged, nil
	case len(now) > len(f.opened):
		return FileAppended, nil
	}
	return FileChanged, nil
}

// ReadAgain implements [ReadAgain]: the file as it stands now while the open's
// inode is still at the path, else as the open found it, as a host's inode
// written over at its path is left as it was.
func (f fakeBody) ReadAgain(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("sandbox: a negative offset")
	}
	f.box.mu.Lock()
	file := f.opened
	if now, there := f.box.files[f.path]; there && f.box.inodes[f.path] == f.inode {
		file = now
	}
	end := min(f.from+f.n, int64(len(file)))
	start := min(f.from+off, end)
	n := copy(p, file[start:end])
	f.box.delivered += int64(n)
	f.box.mu.Unlock()
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
