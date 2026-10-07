// Package memobj is the object store's in-memory twin: a [objstore.Backend]
// over a map, for tests.
//
// It is certified by the same suite as the real backends (objstoretest), so a
// test that passes against it is a test of the contract rather than of a
// stand-in that agrees only with itself — which is why it keeps the rules a
// stand-in would skip: a put that fails or is cancelled stores nothing, and a
// read with no deadline is refused.
package memobj

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// Backend is the twin.
type Backend struct {
	mu      sync.Mutex
	objects map[string]held
	now     func() time.Time
}

type held struct {
	data    []byte
	written time.Time
	// digest is the SHA-256 of the bytes as they were PUT — kept apart
	// from data, as a real backend keeps its digest apart from its bytes,
	// so [Backend.Corrupt] changes one and not the other.
	digest objstore.Hash
}

// New is an empty twin on the wall clock.
func New() *Backend { return NewAt(time.Now) }

// NewAt is an empty twin whose written instants come from now, so a test can
// age an object past the collector's grace without waiting a day.
func NewAt(now func() time.Time) *Backend {
	return &Backend{objects: map[string]held{}, now: now}
}

// Put implements [objstore.Backend]. It reads r whole before it stores
// anything, so a read that fails — the caller's context ending among them —
// leaves nothing under the name. The twin keeps no metadata: nothing reads it
// back but an operator's own tools, and the twin has none.
func (b *Backend) Put(ctx context.Context, name string, r io.Reader, _ objstore.PutMeta) error {
	data, err := io.ReadAll(objstore.ContextReader(ctx, r))
	if err != nil {
		return fmt.Errorf("memobj: put %s: %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("memobj: put %s: %w", name, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[name] = held{data: data, written: b.now().UTC(), digest: objstore.HashOf(data)}
	return nil
}

// Get implements [objstore.Backend], over a copy of the range taken when it
// is called, read under ctx as every backend's stream is.
func (b *Backend) Get(ctx context.Context, name string, off, n int64) (io.ReadCloser, error) {
	if err := objstore.CheckRead(ctx, off, n); err != nil {
		return nil, fmt.Errorf("memobj: get %s: %w", name, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objects[name]
	if !ok {
		return nil, objstore.ErrNotFound
	}
	size := int64(len(o.data))
	start := min(off, size)
	end := size
	if n >= 0 {
		end = min(start+n, size)
	}
	return io.NopCloser(objstore.ContextReader(ctx, bytes.NewReader(slices.Clone(o.data[start:end])))), nil
}

// Stat implements [objstore.Backend].
func (b *Backend) Stat(ctx context.Context, name string) (objstore.Info, error) {
	if err := ctx.Err(); err != nil {
		return objstore.Info{}, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	o, ok := b.objects[name]
	if !ok {
		return objstore.Info{}, objstore.ErrNotFound
	}
	return o.info(name), nil
}

// Delete implements [objstore.Backend].
func (b *Backend) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, name)
	return nil
}

// List implements [objstore.Backend]. It visits a snapshot, so a visitor may
// put and delete as it goes.
func (b *Backend) List(ctx context.Context, visit func(objstore.Info) error) error {
	b.mu.Lock()
	snapshot := make([]objstore.Info, 0, len(b.objects))
	for _, name := range slices.Sorted(maps.Keys(b.objects)) {
		snapshot = append(snapshot, b.objects[name].info(name))
	}
	b.mu.Unlock()
	for _, info := range snapshot {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := visit(info); err != nil {
			return err
		}
	}
	return nil
}

func (o held) info(name string) objstore.Info {
	return objstore.Info{Name: name, Size: int64(len(o.data)), Written: o.written, Digest: o.digest}
}

// Corrupt replaces name's bytes without touching its name or its digest —
// what a failing disk does — for the tests that a corrupt object is never
// served.
func (b *Backend) Corrupt(name string, data []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	o := b.objects[name]
	o.data = slices.Clone(data)
	b.objects[name] = o
}
