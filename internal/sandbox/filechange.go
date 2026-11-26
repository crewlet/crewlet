package sandbox

import (
	"bytes"
	"io"
	"io/fs"
	"os"
)

// FileChange is what became of a box file between the open a read was made
// from and now ([ChangeOf]): a read that is to stand for the file, or for its
// start, asks it once it has read what it wanted.
type FileChange string

const (
	// FileUnchanged is a file of the size and modification time the open
	// found: what was read of it is the file as it stood.
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
// for one version of a file shows nothing of it.
func ChangeOf(body io.Reader) (FileChange, error) {
	if t, ok := body.(changeTeller); ok {
		return t.Change()
	}
	return FileChanged, nil
}

// hostFile is a file on the engine host opened for a read, and what its open
// found of it: the same descriptor is asked again, so a file renamed over the
// path changes nothing for the inode read, and only that inode written in
// place, shrunk or grown, is a change ([ChangeOf]).
type hostFile struct {
	file   *os.File
	opened fs.FileInfo
}

// change compares the file now with its open.
func (h hostFile) change() (FileChange, error) {
	now, err := h.file.Stat()
	if err != nil {
		return "", err
	}
	switch {
	case now.Size() < h.opened.Size():
		return FileChanged, nil
	case now.Size() > h.opened.Size():
		return FileAppended, nil
	case !now.ModTime().Equal(h.opened.ModTime()):
		return FileChanged, nil
	}
	return FileUnchanged, nil
}

// hostSection is a section of a file on the engine host, closed with it, and
// telling what became of the file since its open.
type hostSection struct {
	io.Reader
	io.Closer
	host hostFile
}

// Change implements [ChangeOf].
func (h hostSection) Change() (FileChange, error) { return h.host.change() }

// hostWhole is a file on the engine host read to its end ([Sandbox.OpenFile]),
// telling what became of it since its open.
type hostWhole struct {
	*os.File
	host hostFile
}

// Change implements [ChangeOf].
func (h hostWhole) Change() (FileChange, error) { return h.host.change() }

// absentBody is a file that was not there at its open: nothing was read and
// nothing promised, so whatever is there now is no change to what was read.
type absentBody struct{ io.Reader }

func (absentBody) Close() error { return nil }

// Change implements [ChangeOf].
func (absentBody) Change() (FileChange, error) { return FileUnchanged, nil }

// absentFile is the body of a file not there at its open.
func absentFile() io.ReadCloser { return absentBody{bytes.NewReader(nil)} }

// fakeBody is a fake box's file as its open found it ([FakeSandbox]): its
// inode is asked again, so a file written over the path since is one the
// reader holds open, unchanged, and only an edit in place is a change.
type fakeBody struct {
	io.Reader
	box    *FakeSandbox
	path   string
	inode  uint64
	opened []byte
}

func (fakeBody) Close() error { return nil }

// Change implements [ChangeOf].
func (f fakeBody) Change() (FileChange, error) {
	f.box.mu.Lock()
	now, there := f.box.files[f.path]
	same := there && f.box.inodes[f.path] == f.inode
	f.box.mu.Unlock()
	switch {
	case !same || bytes.Equal(now, f.opened):
		return FileUnchanged, nil
	case len(now) > len(f.opened) && bytes.HasPrefix(now, f.opened):
		return FileAppended, nil
	}
	return FileChanged, nil
}
