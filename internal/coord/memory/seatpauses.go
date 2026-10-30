package memory

import (
	"cmp"
	"context"
	"errors"
	"slices"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- seat pauses -------------------------------------------------------- //

// SeatPause reads one seat's pause, by the seat's id.
func (f *Fleet) SeatPause(_ context.Context, seat uuid.UUID) (coord.SeatPause, bool, error) {
	if seat == uuid.Nil {
		return coord.SeatPause{}, false, errors.New("coord/memory: a seat pause needs a seat id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pauses[seat]
	return p, ok, nil
}

// ListSeatPauses returns every pause, ordered by seat id.
func (f *Fleet) ListSeatPauses(context.Context) ([]coord.SeatPause, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sortedPausesLocked(), nil
}

// CreateSeatPause writes a pause for a seat that has none.
func (f *Fleet) CreateSeatPause(_ context.Context, p coord.SeatPause) (coord.SeatPause, bool, error) {
	if err := p.Validate(); err != nil {
		return coord.SeatPause{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.pauses[p.Seat]; exists {
		return coord.SeatPause{}, false, nil
	}
	return f.storePauseLocked(p), true, nil
}

// UpdateSeatPause writes a pause at the version it was read at.
func (f *Fleet) UpdateSeatPause(_ context.Context, p coord.SeatPause) (coord.SeatPause, bool, error) {
	if err := p.Validate(); err != nil {
		return coord.SeatPause{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.pauses[p.Seat]
	if !ok || p.Version == 0 || current.Version != p.Version {
		return coord.SeatPause{}, false, nil
	}
	return f.storePauseLocked(p), true, nil
}

// DeleteSeatPause lifts a pause at a version.
func (f *Fleet) DeleteSeatPause(_ context.Context, seat uuid.UUID, version uint64) (bool, error) {
	if seat == uuid.Nil {
		return false, errors.New("coord/memory: a seat pause needs a seat id")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.pauses[seat]
	if !ok || version == 0 || current.Version != version {
		return false, nil
	}
	delete(f.pauses, seat)
	f.notifyPauseLocked(coord.SeatPauseUpdate{Seat: seat})
	return true, nil
}

// WatchSeatPauses streams every pause, the marker, then every change.
//
// The snapshot and the registration happen under ONE lock, which is the whole
// ordering guarantee (watch.go). The KV backend gives the same guarantee by
// certifying its first answer against the bucket's key index and reading every
// change after it off the same consumer — see its WatchSeatPauses. This twin
// has one copy, so every update it delivers is already the current state.
func (f *Fleet) WatchSeatPauses(ctx context.Context) (<-chan coord.SeatPauseUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var current []coord.SeatPauseUpdate
	for _, p := range f.sortedPausesLocked() {
		current = append(current, coord.SeatPauseUpdate{Seat: p.Seat, Pause: &p})
	}
	w := newWatcher(append(current, coord.SeatPauseUpdate{Current: true})...)
	if f.pauseWatchers == nil {
		f.pauseWatchers = map[*watcher[coord.SeatPauseUpdate]]struct{}{}
	}
	f.pauseWatchers[w] = struct{}{}
	return w.serve(ctx, func() {
		f.mu.Lock()
		delete(f.pauseWatchers, w)
		f.mu.Unlock()
	}), nil
}

// storePauseLocked writes a pause with the next store-wide version and tells
// every watcher.
func (f *Fleet) storePauseLocked(p coord.SeatPause) coord.SeatPause {
	f.version++
	p.Version = f.version
	p.At = p.At.UTC()
	if f.pauses == nil {
		f.pauses = map[uuid.UUID]coord.SeatPause{}
	}
	f.pauses[p.Seat] = p
	stored := p
	f.notifyPauseLocked(coord.SeatPauseUpdate{Seat: p.Seat, Pause: &stored})
	return p
}

func (f *Fleet) notifyPauseLocked(u coord.SeatPauseUpdate) {
	for w := range f.pauseWatchers {
		if u.Pause != nil {
			// Each watcher gets its own copy, so one consumer that keeps
			// the pointer cannot see another's edits.
			copied := *u.Pause
			u.Pause = &copied
		}
		w.push(u)
	}
}

func (f *Fleet) sortedPausesLocked() []coord.SeatPause {
	out := make([]coord.SeatPause, 0, len(f.pauses))
	for _, p := range f.pauses {
		out = append(out, p)
	}
	slices.SortFunc(out, func(a, b coord.SeatPause) int { return cmp.Compare(a.Seat.String(), b.Seat.String()) })
	return out
}
