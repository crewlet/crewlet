package cache

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A PACK IS KNOWN BY ITS INODE AND ITS SIZE (fileID): git freshens a .pack's
// time, not its bytes, whenever a process writes an object it holds, so a
// freshened pack keeps every verdict about it (a scrub's hash, the repair
// mark, a confirmed hint). Its .idx and .rev, which git never freshens, keep
// their time in their identity, as every other file does; a pack renamed in
// over the name, or cut, is another identity.
//
// Mutation: read a pack's modification time into its identity (red).
func TestAPackIsKnownByItsInodeAndSize(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cs := strings.Repeat("5e", hashTrailer)
	write := func(rel, body string) string {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return path
	}
	pack := write(packFileRel(placeLive, cs, ".pack"), "PACK the bytes of a pack")
	idx := write(packFileRel(placeLive, cs, ".idx"), "an index")
	before, idxBefore := lstatID(pack), lstatID(idx)

	// Git's freshening: the time moved, an hour on, and not a byte.
	later := time.Now().Add(time.Hour)
	for _, path := range []string{pack, idx} {
		if err := os.Chtimes(path, later, later); err != nil {
			t.Fatal(err)
		}
	}
	if after := lstatID(pack); after != before {
		t.Errorf("a freshened pack is identity %+v, was %+v: git's utime spoiled every verdict about it", after,
			before)
	}
	if after := lstatID(idx); after == idxBefore {
		t.Errorf("an index whose time moved kept its identity %+v: only a pack's time is no part of it", after)
	}
	if info, err := os.Lstat(pack); err != nil || idOf(info) != before {
		t.Errorf("the pack's identity from its Lstat = %+v (%v), want the path's own %+v", idOf(info), err, before)
	}

	// Another pack under the name is another identity, whatever its time:
	// written beside it and renamed in, as every writer of a copy places
	// one, so the two are never one inode.
	staged := write(packFileRel(placeLive, cs, ".pack.new"), "PACK the bytes of a pack")
	if err := os.Rename(staged, pack); err != nil {
		t.Fatal(err)
	}
	if again := lstatID(pack); again == before {
		t.Errorf("a pack renamed in over the name kept the old identity %+v", again)
	}
	if err := os.Truncate(pack, 4); err != nil {
		t.Fatal(err)
	}
	if cut := lstatID(pack); cut.size != 4 || cut.ino == 0 {
		t.Errorf("a cut pack is identity %+v, want its size in it", cut)
	}
}
