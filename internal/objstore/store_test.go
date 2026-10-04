package objstore_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
)

func newStore(t *testing.T, backend objstore.Backend) (*objstore.Store, *coordmem.Fleet) {
	t.Helper()
	locks := coordmem.NewFleet()
	s, err := objstore.NewStore(backend, locks, "node-a")
	if err != nil {
		t.Fatal(err)
	}
	return s, locks
}

// split uploads body through s and answers its manifest.
func split(t *testing.T, s *objstore.Store, body []byte) objstore.Manifest {
	t.Helper()
	m, err := objstore.Split(t.Context(), bytes.NewReader(body), int64(len(body)),
		func(ctx context.Context, c objstore.Chunk, data []byte) error {
			return s.Put(ctx, c.Hash, data)
		})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func patterned(n int, mul int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * mul)
	}
	return b
}

// AN OBJECT STREAMS BACK WHOLE, and a manifest naming another object fails at
// the end rather than reading to a clean finish.
func TestAnObjectStreamsBackWholeAndAWrongManifestFails(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t, memobj.New())
	body := patterned(3*objstore.ChunkSize+99, 7)
	m := split(t, s, body)
	r, err := s.Open(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("read back %d bytes, %v; want the %d written", len(got), err, len(body))
	}
	lying := m
	lying.Hash = objstore.HashOf([]byte("some other object"))
	r, err = s.Open(t.Context(), lying)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("a manifest naming another object read to a clean end")
	}
}

// A CHUNK THE STORE LOST IS AN ERROR AT THE READ THAT NEEDED IT, and closing a
// reader half way stops it.
func TestAReaderStopsWhenClosedAndFailsOnAMissingChunk(t *testing.T) {
	t.Parallel()
	backend := memobj.New()
	s, _ := newStore(t, backend)
	m := split(t, s, patterned(2*objstore.ChunkSize, 3))
	r, err := s.Open(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Read(make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := backend.Delete(t.Context(), m.Chunks[1].Hash); err != nil {
		t.Fatal(err)
	}
	r, err = s.Open(t.Context(), m)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := io.ReadAll(r); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("reading past a missing chunk = %v, want ErrNotFound", err)
	}
}

// A RANGED READ ANSWERS EXACTLY THE BYTES ASKED FOR, across chunk boundaries,
// short at the end of the object and empty past it.
func TestARangedReadAnswersItsRange(t *testing.T) {
	t.Parallel()
	s, _ := newStore(t, memobj.New())
	body := patterned(2*objstore.ChunkSize+500, 13)
	m := split(t, s, body)
	for _, c := range []struct{ off, n int64 }{
		{0, 10}, {objstore.ChunkSize - 5, 10}, {objstore.ChunkSize, objstore.ChunkSize},
		{2*objstore.ChunkSize + 490, 100}, {int64(len(body)), 5}, {0, int64(len(body))},
	} {
		got, err := s.ReadAt(t.Context(), m, c.off, c.n)
		if err != nil {
			t.Fatalf("ReadAt(%d, %d): %v", c.off, c.n, err)
		}
		end := min(c.off+c.n, int64(len(body)))
		if want := body[min(c.off, end):end]; !bytes.Equal(got, want) {
			t.Fatalf("ReadAt(%d, %d) answered %d bytes, want %d", c.off, c.n, len(got), len(want))
		}
	}
}

// A CHUNK WHOSE BYTES NO LONGER MATCH ITS NAME IS NEVER HANDED OUT, and bytes
// that are not what a put names are never stored.
func TestCorruptBytesAreRefusedBothWays(t *testing.T) {
	t.Parallel()
	backend := memobj.New()
	s, _ := newStore(t, backend)
	data := []byte("the real bytes")
	h := objstore.HashOf(data)
	if err := s.Put(t.Context(), h, []byte("other bytes")); err == nil {
		t.Fatal("a put of bytes that are not what the name says was stored")
	}
	if err := s.Put(t.Context(), h, data); err != nil {
		t.Fatal(err)
	}
	backend.Corrupt(h, []byte("rotted"))
	if _, err := s.Get(t.Context(), h); !errors.Is(err, objstore.ErrCorrupt) {
		t.Fatalf("Get of a rotted chunk = %v, want ErrCorrupt", err)
	}
}

// countingBackend counts the puts that reached the backend.
type countingBackend struct {
	objstore.Backend
	puts atomic.Int32
}

func (b *countingBackend) Put(ctx context.Context, h objstore.Hash, data []byte) error {
	b.puts.Add(1)
	return b.Backend.Put(ctx, h, data)
}

// A RE-PUT OF A CHUNK THE STORE HOLDS WAITS FOR ITS LOCK — the one the
// collector deletes under — and still lands, so the chunk is young again
// whichever of the two went first.
func TestARePutWaitsForTheChunksLock(t *testing.T) {
	t.Parallel()
	backend := &countingBackend{Backend: memobj.New()}
	s, locks := newStore(t, backend)
	data := []byte("re-used")
	h := objstore.HashOf(data)
	if err := s.Put(t.Context(), h, data); err != nil {
		t.Fatal(err)
	}
	if took, err := locks.LockChunk(t.Context(), string(h), "collector"); err != nil || !took {
		t.Fatalf("the collector's lock: %v, %v", took, err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Put(t.Context(), h, data) }()
	select {
	case err := <-done:
		t.Fatalf("a re-put landed while the collector held the chunk: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := locks.UnlockChunk(t.Context(), string(h), "collector"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the re-put after the lock was let go: %v", err)
	}
	if got := backend.puts.Load(); got != 2 {
		t.Fatalf("%d puts reached the backend, want 2", got)
	}
	// AND LET GO AFTER: the collector can take it again.
	if took, err := locks.LockChunk(t.Context(), string(h), "collector"); err != nil || !took {
		t.Fatalf("the re-put kept the lock: %v, %v", took, err)
	}
}

// A FIRST PUT TAKES NO LOCK: nothing can be deleting a chunk the store does
// not hold, and an upload of new bytes must not wait on the collector.
func TestAFirstPutTakesNoLock(t *testing.T) {
	t.Parallel()
	s, locks := newStore(t, memobj.New())
	data := []byte("new")
	h := objstore.HashOf(data)
	if took, err := locks.LockChunk(t.Context(), string(h), "collector"); err != nil || !took {
		t.Fatal(took, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if err := s.Put(ctx, h, data); err != nil {
		t.Fatalf("a first put waited on a lock: %v", err)
	}
}
