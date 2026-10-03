// Package memobj is the object store's in-memory twin: a [objstore.Backend]
// over a map, for tests.
//
// It is certified by the same suite as the real backends (objstoretest), so a
// test that passes against it is a test of the contract rather than of a
// stand-in that agrees only with itself.
package memobj

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// Backend is the twin.
type Backend struct {
	mu     sync.Mutex
	chunks map[objstore.Hash]held
	now    func() time.Time
}

type held struct {
	data    []byte
	written time.Time
}

// New is an empty twin on the wall clock.
func New() *Backend { return NewAt(time.Now) }

// NewAt is an empty twin whose written instants come from now, so a test can
// age a chunk past the collector's grace without waiting a day.
func NewAt(now func() time.Time) *Backend {
	return &Backend{chunks: map[objstore.Hash]held{}, now: now}
}

// Put implements [objstore.Backend].
func (b *Backend) Put(ctx context.Context, h objstore.Hash, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.chunks[h] = held{data: slices.Clone(data), written: b.now().UTC()}
	return nil
}

// Get implements [objstore.Backend].
func (b *Backend) Get(ctx context.Context, h objstore.Hash) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.chunks[h]
	if !ok {
		return nil, objstore.ErrNotFound
	}
	return slices.Clone(c.data), nil
}

// Stat implements [objstore.Backend].
func (b *Backend) Stat(ctx context.Context, h objstore.Hash) (time.Time, error) {
	if err := ctx.Err(); err != nil {
		return time.Time{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.chunks[h]
	if !ok {
		return time.Time{}, objstore.ErrNotFound
	}
	return c.written, nil
}

// Delete implements [objstore.Backend].
func (b *Backend) Delete(ctx context.Context, h objstore.Hash) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.chunks, h)
	return nil
}

// List implements [objstore.Backend]. It visits a snapshot, so a visitor may
// put and delete as it goes.
func (b *Backend) List(ctx context.Context, visit func(objstore.Held) error) error {
	b.mu.Lock()
	snapshot := make([]objstore.Held, 0, len(b.chunks))
	for _, h := range slices.Sorted(maps.Keys(b.chunks)) {
		snapshot = append(snapshot, objstore.Held{Hash: h, Written: b.chunks[h].written})
	}
	b.mu.Unlock()
	for _, held := range snapshot {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(held); err != nil {
			return err
		}
	}
	return nil
}

// Corrupt replaces h's bytes without moving its name — what a failing disk
// does — for the test that a corrupt chunk is never served.
func (b *Backend) Corrupt(h objstore.Hash, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.chunks[h]
	c.data = slices.Clone(data)
	b.chunks[h] = c
}
