// Package objstoretest is the object store's conformance suite: ONE set of
// cases every [objstore.Backend] passes — the JetStream object store, an
// S3-compatible bucket and the in-memory twin alike.
//
// A twin that agreed only with itself would prove nothing, and the collector
// rests on two properties no single backend's own tests would think to state:
// that a put of a chunk already held moves its written instant, and that a
// listing visits every chunk the backend holds.
package objstoretest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// Factory builds an empty backend for one case.
type Factory func(t *testing.T) objstore.Backend

// Options tunes the suite to what a backend can promise.
type Options struct {
	// Granularity is the coarsest step the backend's written instants
	// move in — a second for S3's LastModified. The re-put case waits
	// past it before asking whether the instant moved.
	Granularity time.Duration
}

// Run runs every case against backends built by newBackend.
func Run(t *testing.T, newBackend Factory, opts Options) {
	t.Helper()
	cases := []struct {
		name string
		run  func(t *testing.T, b objstore.Backend, opts Options)
	}{
		{"a_put_chunk_reads_back_whole", aPutChunkReadsBackWhole},
		{"a_chunk_never_put_is_not_found", aChunkNeverPutIsNotFound},
		{"a_deleted_chunk_is_gone_and_a_second_delete_is_fine", aDeletedChunkIsGone},
		{"a_re_put_moves_the_written_instant", aRePutMovesTheWrittenInstant},
		{"a_listing_visits_every_chunk_once", aListingVisitsEveryChunkOnce},
		{"a_listing_stops_at_the_visitors_error", aListingStopsAtTheVisitorsError},
		{"a_full_sized_chunk_round_trips", aFullSizedChunkRoundTrips},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.run(t, newBackend(t), opts)
		})
	}
}

func chunk(seed string) ([]byte, objstore.Hash) {
	data := []byte("chunk " + seed)
	return data, objstore.HashOf(data)
}

func aPutChunkReadsBackWhole(t *testing.T, b objstore.Backend, _ Options) {
	ctx := t.Context()
	data, h := chunk("whole")
	before := time.Now().Add(-time.Minute)
	if err := b.Put(ctx, h, data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := b.Get(ctx, h)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("Get = %q, %v; want %q", got, err, data)
	}
	written, err := b.Stat(ctx, h)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if written.Before(before) || written.After(time.Now().Add(time.Minute)) {
		t.Fatalf("Stat = %v, want about now", written)
	}
	if written.Location() != time.UTC {
		t.Fatalf("Stat answered %v in %v; every instant here is UTC", written, written.Location())
	}
}

func aChunkNeverPutIsNotFound(t *testing.T, b objstore.Backend, _ Options) {
	ctx := t.Context()
	_, h := chunk("absent")
	if _, err := b.Get(ctx, h); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Get of a chunk never put = %v, want ErrNotFound", err)
	}
	if _, err := b.Stat(ctx, h); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Stat of a chunk never put = %v, want ErrNotFound", err)
	}
}

func aDeletedChunkIsGone(t *testing.T, b objstore.Backend, _ Options) {
	ctx := t.Context()
	data, h := chunk("deleted")
	if err := b.Put(ctx, h, data); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, h); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := b.Get(ctx, h); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want ErrNotFound", err)
	}
	if _, err := b.Stat(ctx, h); !errors.Is(err, objstore.ErrNotFound) {
		t.Fatalf("Stat after Delete = %v, want ErrNotFound", err)
	}
	if err := b.Delete(ctx, h); err != nil {
		t.Fatalf("a second Delete = %v; deleting what is not there is not an error", err)
	}
	if err := b.Put(ctx, h, data); err != nil {
		t.Fatalf("a Put after a Delete: %v", err)
	}
	if got, err := b.Get(ctx, h); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("a chunk put again after a delete reads %q, %v", got, err)
	}
}

// THE COLLECTOR'S GRACE RESTS ON THIS: a file re-using a chunk re-puts it,
// and the re-put is what makes it young again.
func aRePutMovesTheWrittenInstant(t *testing.T, b objstore.Backend, opts Options) {
	ctx := t.Context()
	data, h := chunk("re-put")
	if err := b.Put(ctx, h, data); err != nil {
		t.Fatal(err)
	}
	first, err := b.Stat(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(max(opts.Granularity, 10*time.Millisecond) + 100*time.Millisecond)
	if err = b.Put(ctx, h, data); err != nil {
		t.Fatal(err)
	}
	second, err := b.Stat(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	if !second.After(first) {
		t.Fatalf("a re-put left the written instant at %v (first put %v); the collector "+
			"would delete a chunk a new file had just re-used", second, first)
	}
	listed := map[objstore.Hash]time.Time{}
	if err := b.List(ctx, func(held objstore.Held) error {
		listed[held.Hash] = held.Written
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !listed[h].After(first) {
		t.Fatalf("the listing says %v for a chunk re-put after %v", listed[h], first)
	}
}

func aListingVisitsEveryChunkOnce(t *testing.T, b objstore.Backend, _ Options) {
	ctx := t.Context()
	want := map[objstore.Hash]bool{}
	for i := range 25 {
		data, h := chunk(fmt.Sprintf("listed-%d", i))
		if err := b.Put(ctx, h, data); err != nil {
			t.Fatal(err)
		}
		want[h] = true
	}
	_, gone := chunk("listed-gone")
	if err := b.Put(ctx, gone, []byte("chunk listed-gone")); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete(ctx, gone); err != nil {
		t.Fatal(err)
	}
	seen := map[objstore.Hash]int{}
	if err := b.List(ctx, func(held objstore.Held) error {
		seen[held.Hash]++
		if held.Written.IsZero() {
			t.Errorf("%s was listed with no written instant", held.Hash)
		}
		return nil
	}); err != nil {
		t.Fatalf("List: %v", err)
	}
	for h := range want {
		if seen[h] != 1 {
			t.Errorf("%s was listed %d times, want once", h, seen[h])
		}
	}
	if seen[gone] != 0 {
		t.Errorf("a deleted chunk was listed")
	}
	if len(seen) != len(want) {
		t.Errorf("listed %d chunks, want %d", len(seen), len(want))
	}
}

func aListingStopsAtTheVisitorsError(t *testing.T, b objstore.Backend, _ Options) {
	ctx := t.Context()
	for i := range 3 {
		data, h := chunk(fmt.Sprintf("stop-%d", i))
		if err := b.Put(ctx, h, data); err != nil {
			t.Fatal(err)
		}
	}
	stop := errors.New("enough")
	visits := 0
	err := b.List(ctx, func(objstore.Held) error {
		visits++
		return stop
	})
	if !errors.Is(err, stop) || visits != 1 {
		t.Fatalf("List = %v after %d visits; want the visitor's error after one", err, visits)
	}
}

func aFullSizedChunkRoundTrips(t *testing.T, b objstore.Backend, _ Options) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	data := make([]byte, objstore.ChunkSize)
	for i := range data {
		data[i] = byte(i*31 + i/251)
	}
	h := objstore.HashOf(data)
	if err := b.Put(ctx, h, data); err != nil {
		t.Fatalf("Put of a full chunk: %v", err)
	}
	got, err := b.Get(ctx, h)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("a full chunk read back %d bytes, %v", len(got), err)
	}
}
