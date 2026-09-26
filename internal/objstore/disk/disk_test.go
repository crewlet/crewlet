package disk

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// A SECOND OPENER OF ONE DIRECTORY IS REFUSED BY NAME, and the directory is
// free again once the first closes it: two engines sharing a chunk directory
// would each collect the other's chunks as garbage it does not recognise.
func TestADirectoryHasOneOwner(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	first, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Open(root); !errors.Is(err, ErrLocked) {
		t.Fatalf("a second Open = %v, want ErrLocked", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(root)
	if err != nil {
		t.Fatalf("Open after the owner closed = %v", err)
	}
	_ = again.Close()
}

// A CHUNK READS BACK AS IT WAS WRITTEN, under the name its bytes hash to.
func TestAChunkReadsBackUnderItsHash(t *testing.T) {
	t.Parallel()
	s := open(t)
	data := []byte("the rollback runbook")
	h := objstore.HashOf(data)
	if err := s.Put(h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := s.Get(h)
	if err != nil || string(got) != string(data) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if !s.Has(h) {
		t.Fatal("Has is false for a stored chunk")
	}
	// AND AGAIN IS NOTHING: the same name is the same bytes.
	if err := s.Put(h, data); err != nil {
		t.Fatalf("a second Put of the same chunk: %v", err)
	}
}

// BYTES THAT DO NOT MATCH THEIR NAME ARE REFUSED, so a peer's corrupted copy
// never becomes this node's.
func TestBytesUnderTheWrongNameAreRefused(t *testing.T) {
	t.Parallel()
	s := open(t)
	err := s.Put(objstore.HashOf([]byte("one thing")), []byte("another"))
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("Put = %v, want ErrMismatch", err)
	}
}

// A CHUNK THAT ROTTED ON DISK IS NOT SERVED: it is removed and reported not
// held, which is what sends repair to fetch a good copy from a peer.
func TestARottenChunkIsRemovedRatherThanServed(t *testing.T) {
	t.Parallel()
	s := open(t)
	data := []byte("bytes the disk will flip")
	h := objstore.HashOf(data)
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(h), []byte("flipped"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(h); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get of a rotten chunk = %v, want ErrNotFound", err)
	}
	if s.Has(h) {
		t.Fatal("the rotten chunk is still there")
	}
}

// A WALK SEES EVERY CHUNK, ONCE, AND STEPS OVER A WRITE IN PROGRESS — which
// is somebody's Put, so removing it would fail that Put at its rename.
func TestAWalkSeesEveryChunkAndLeavesAStagedWriteAlone(t *testing.T) {
	t.Parallel()
	s := open(t)
	var want []objstore.Hash
	for _, body := range []string{"a", "b", "c"} {
		h := objstore.HashOf([]byte(body))
		if err := s.Put(h, []byte(body)); err != nil {
			t.Fatal(err)
		}
		want = append(want, h)
	}
	inFlight := filepath.Join(filepath.Dir(s.path(want[0])), staged+"in-flight")
	if err := os.WriteFile(inFlight, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []objstore.Hash
	if err := s.Walk(func(h Held) error {
		got = append(got, h.Hash)
		if h.Written.IsZero() || h.Size != 1 {
			t.Errorf("chunk %s walked as %+v", h.Hash, h)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("walked %v, want %v", got, want)
	}
	if _, err := os.Stat(inFlight); err != nil {
		t.Fatalf("the walk removed a write in progress: %v", err)
	}
}

// WHAT A CRASH LEFT STAGED IS CLEARED WHEN THE DIRECTORY IS OPENED, which is
// the one moment nothing can be writing it.
func TestOpeningClearsWhatACrashLeftStaged(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	h := objstore.HashOf([]byte("kept"))
	if err := s.Put(h, []byte("kept")); err != nil {
		t.Fatal(err)
	}
	debris := filepath.Join(filepath.Dir(s.path(h)), staged+"crashed")
	if err := os.WriteFile(debris, []byte("half"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, err := os.Stat(debris); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a staged write left by a crash survived Open")
	}
	if !reopened.Has(h) {
		t.Fatal("Open removed a chunk")
	}
}

// A WALK OF ONE GROUP SEES THAT GROUP AND NOTHING ELSE.
func TestAGroupWalkSeesOnlyItsGroup(t *testing.T) {
	t.Parallel()
	s := open(t)
	byGroup := map[int][]objstore.Hash{}
	for i := range 64 {
		body := []byte{byte(i)}
		h := objstore.HashOf(body)
		if err := s.Put(h, body); err != nil {
			t.Fatal(err)
		}
		byGroup[h.PG()] = append(byGroup[h.PG()], h)
	}
	for pg, want := range byGroup {
		var got []objstore.Hash
		if err := s.WalkGroup(pg, func(h Held) error {
			got = append(got, h.Hash)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("group %d walked %v, want %v", pg, got, want)
		}
	}
	// A GROUP NOTHING WAS EVER WRITTEN TO is empty, not an error.
	for pg := range 256 {
		if _, used := byGroup[pg]; used {
			continue
		}
		if err := s.WalkGroup(pg, func(Held) error {
			t.Fatalf("group %d walked a chunk", pg)
			return nil
		}); err != nil {
			t.Fatalf("an empty group: %v", err)
		}
		break
	}
}

// A WRITE OF A CHUNK ALREADY HELD RESTARTS ITS GRACE, and the collector
// judges by that: bytes uploaded again for a record that has not landed must
// not be collected on the age of the copy a deleted file left behind.
func TestAWriteRestartsTheCollectorsGrace(t *testing.T) {
	t.Parallel()
	s := open(t)
	data := []byte("uploaded twice")
	h := objstore.HashOf(data)
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(s.path(h), old, old); err != nil {
		t.Fatal(err)
	}
	cutoff := time.Now().Add(-24 * time.Hour)

	// Written again: the chunk is young, so the collector leaves it.
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	removed, err := s.Collect(h, cutoff)
	if err != nil || removed {
		t.Fatalf("Collect after a fresh write = %v, %v; want kept", removed, err)
	}

	// Aged past the cutoff with no write since: collected.
	if err := os.Chtimes(s.path(h), old, old); err != nil {
		t.Fatal(err)
	}
	removed, err = s.Collect(h, cutoff)
	if err != nil || !removed {
		t.Fatalf("Collect of an aged chunk = %v, %v; want removed", removed, err)
	}
	if s.Has(h) {
		t.Fatal("a collected chunk is still held")
	}
	// And collecting what is gone is nothing.
	if removed, err := s.Collect(h, cutoff); err != nil || removed {
		t.Fatalf("Collect of a gone chunk = %v, %v", removed, err)
	}
}

// A ROTTEN COPY IS WRITTEN OVER, not counted: a Put that found bytes under
// the name that no longer match it replaces them with the caller's, which
// were checked.
func TestAPutReplacesARottenCopy(t *testing.T) {
	t.Parallel()
	s := open(t)
	data := []byte("the good bytes")
	h := objstore.HashOf(data)
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.path(h), []byte("rot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(h, data); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(h)
	if err != nil || string(got) != string(data) {
		t.Fatalf("Get after a repairing Put = %q, %v", got, err)
	}
}

// A DELETE OF WHAT IS ALREADY GONE IS NOT AN ERROR — two passes racing on one
// chunk agree about where it ended up.
func TestDeletingAGoneChunkIsNothing(t *testing.T) {
	t.Parallel()
	s := open(t)
	h := objstore.HashOf([]byte("x"))
	if err := s.Delete(h); err != nil {
		t.Fatalf("Delete of an absent chunk: %v", err)
	}
}
