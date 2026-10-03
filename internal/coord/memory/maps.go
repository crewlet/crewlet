package memory

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the placement maps ------------------------------------------------ //
//
// The estate map: one record changed only by compare-and-set, and watched.

// versioned is the map's record, and nil until one is written.
type versioned struct {
	value   []byte
	version uint64
}

// readLocked answers a map's record, reporting false when none was written.
func (v *versioned) readLocked() ([]byte, uint64, bool) {
	if v.value == nil {
		return nil, 0, false
	}
	return slices.Clone(v.value), v.version, true
}

// EstateMap reads the estate map.
func (f *Fleet) EstateMap(context.Context) (coord.EstateMapRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	value, version, found := f.estateMap.readLocked()
	return coord.EstateMapRecord{Value: value, Version: version}, found, nil
}

// CreateEstateMap writes the first estate map, leaving an existing one alone.
func (f *Fleet) CreateEstateMap(_ context.Context, value []byte) (coord.EstateMapRecord, bool, error) {
	version, created, err := f.writeMap(&f.estateMap, value, 0, true, "an estate map")
	if !created || err != nil {
		return coord.EstateMapRecord{}, false, err
	}
	return coord.EstateMapRecord{Value: slices.Clone(value), Version: version}, true, nil
}

// UpdateEstateMap writes the estate map at the version it was read at.
func (f *Fleet) UpdateEstateMap(_ context.Context, value []byte, version uint64) (coord.EstateMapRecord, bool, error) {
	version, won, err := f.writeMap(&f.estateMap, value, version, false, "an estate map")
	if !won || err != nil {
		return coord.EstateMapRecord{}, false, err
	}
	return coord.EstateMapRecord{Value: slices.Clone(value), Version: version}, true, nil
}

// writeMap is a map's create (create true: only when none exists) or its
// update (only at the version held), under a fresh version drawn from the
// sequence every other record here takes its versions from. A write of the
// estate map is handed to every watch of it before the lock is let go, so no
// watch can see two writes out of order.
func (f *Fleet) writeMap(into *versioned, value []byte, version uint64, create bool, what string) (uint64, bool, error) {
	if len(value) == 0 {
		return 0, false, errors.New("coord/memory: " + what + " needs a value")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case create && into.value != nil:
		return 0, false, nil
	case !create && (into.value == nil || into.version != version):
		return 0, false, nil
	}
	f.version++
	*into = versioned{value: slices.Clone(value), version: f.version}
	if into == &f.estateMap {
		rec := coord.EstateMapRecord{Value: into.value, Version: into.version}
		for w := range f.estateWatches {
			w.push(rec)
		}
	}
	return f.version, true, nil
}

// WatchEstateMap delivers the estate map as it changes ([coord.EstateMaps]):
// the current version first, then later ones in order, the newest always.
//
// IT SKIPS AS THE BROKER DOES. A write lands in each watch's ONE slot under the
// fleet's lock, replacing a version the watch had not yet handed over, which
// is what the broker's one-version bucket does to a reader that fell behind.
// A twin that queued every version would certify a reader that acted on each
// step — one that is wrong on every restart, where only the current map is
// handed over — and it would pass here and fail in a fleet. A writer never
// waits on a reader either way. A watch holds a goroutine that hands the slot
// to its reader, and ends with ctx.
func (f *Fleet) WatchEstateMap(ctx context.Context) (<-chan coord.EstateMapRecord, error) {
	w := &mapWatch{wake: make(chan struct{}, 1)}
	f.mu.Lock()
	if f.estateWatches == nil {
		f.estateWatches = map[*mapWatch]struct{}{}
	}
	f.estateWatches[w] = struct{}{}
	if f.estateMap.value != nil {
		w.push(coord.EstateMapRecord{Value: f.estateMap.value, Version: f.estateMap.version})
	}
	f.mu.Unlock()

	out := make(chan coord.EstateMapRecord)
	go func() {
		defer close(out)
		defer func() {
			f.mu.Lock()
			delete(f.estateWatches, w)
			f.mu.Unlock()
		}()
		for {
			if rec, ok := w.take(); ok {
				select {
				case out <- coord.EstateMapRecord{Value: slices.Clone(rec.Value), Version: rec.Version}:
				case <-ctx.Done():
					return
				}
				continue
			}
			select {
			case <-w.wake:
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

// mapWatch is one watch's slot: the newest version not yet handed to its
// reader.
type mapWatch struct {
	mu      sync.Mutex
	pending *coord.EstateMapRecord
	wake    chan struct{}
}

// push puts a version in the slot, replacing one not yet handed over, and
// wakes the watch's goroutine. Writes arrive under the fleet's lock in version
// order, so the slot only ever moves forward.
func (w *mapWatch) push(rec coord.EstateMapRecord) {
	w.mu.Lock()
	w.pending = &rec
	w.mu.Unlock()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// take empties the slot.
func (w *mapWatch) take() (coord.EstateMapRecord, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending == nil {
		return coord.EstateMapRecord{}, false
	}
	rec := *w.pending
	w.pending = nil
	return rec, true
}
