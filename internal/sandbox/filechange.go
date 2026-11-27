package sandbox

import (
	"bytes"
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
// modification time was put back.
func ChangeOf(body io.Reader) (FileChange, error) {
	if t, ok := body.(changeTeller); ok {
		return t.Change()
	}
	return FileChanged, nil
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
