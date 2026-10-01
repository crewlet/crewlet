package memory

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- seat pauses -------------------------------------------------------- //

// SeatPause reads one seat's pause.
func (f *Fleet) SeatPause(_ context.Context, handle string) (coord.SeatPause, bool, error) {
	if handle == "" {
		return coord.SeatPause{}, false, errors.New("coord/memory: a seat pause needs a handle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pauses[handle]
	return p, ok, nil
}

// ListSeatPauses returns every pause, ordered by handle.
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
	if _, exists := f.pauses[p.Handle]; exists {
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
	current, ok := f.pauses[p.Handle]
	if !ok || p.Version == 0 || current.Version != p.Version {
		return coord.SeatPause{}, false, nil
	}
	return f.storePauseLocked(p), true, nil
}

// DeleteSeatPause lifts a pause at a version.
func (f *Fleet) DeleteSeatPause(_ context.Context, handle string, version uint64) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/memory: a seat pause needs a handle")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	current, ok := f.pauses[handle]
	if !ok || version == 0 || current.Version != version {
		return false, nil
	}
	delete(f.pauses, handle)
	f.notifyPauseLocked(coord.SeatPauseUpdate{Handle: handle})
	return true, nil
}

// WatchSeatPauses streams every pause, the marker, then every change.
//
// The snapshot and the registration happen under ONE lock, which is the whole
// ordering guarantee: a write lands either before the snapshot (and is in it)
// or after the registration (and is delivered as a change). The KV watcher
// gives the same guarantee by being one ordered consumer over the bucket.
func (f *Fleet) WatchSeatPauses(ctx context.Context) (<-chan coord.SeatPauseUpdate, error) {
	w := &pauseWatcher{wake: make(chan struct{}, 1)}
	f.mu.Lock()
	for _, p := range f.sortedPausesLocked() {
		w.pending = append(w.pending, coord.SeatPauseUpdate{Handle: p.Handle, Pause: &p})
	}
	w.pending = append(w.pending, coord.SeatPauseUpdate{Current: true})
	if f.pauseWatchers == nil {
		f.pauseWatchers = map[*pauseWatcher]struct{}{}
	}
	f.pauseWatchers[w] = struct{}{}
	f.mu.Unlock()
	w.signal()

	out := make(chan coord.SeatPauseUpdate)
	go func() {
		defer close(out)
		defer func() {
			f.mu.Lock()
			delete(f.pauseWatchers, w)
			f.mu.Unlock()
		}()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.wake:
			}
			for _, u := range w.drain() {
				select {
				case out <- u:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// pauseWatcher is one open watch: an unbounded queue a writer appends to under
// the fleet's lock and never waits on, and a goroutine that hands it on.
//
// UNBOUNDED, because the alternative is a writer that blocks on a slow reader
// while holding the lock every other call in the twin takes. The set is the
// changes to a handful of seat pauses, which a reader consumes promptly.
type pauseWatcher struct {
	mu      sync.Mutex
	pending []coord.SeatPauseUpdate
	wake    chan struct{}
}

func (w *pauseWatcher) push(u coord.SeatPauseUpdate) {
	w.mu.Lock()
	w.pending = append(w.pending, u)
	w.mu.Unlock()
	w.signal()
}

func (w *pauseWatcher) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *pauseWatcher) drain() []coord.SeatPauseUpdate {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.pending
	w.pending = nil
	return out
}

// storePauseLocked writes a pause with the next store-wide version and tells
// every watcher.
func (f *Fleet) storePauseLocked(p coord.SeatPause) coord.SeatPause {
	f.version++
	p.Version = f.version
	p.At = p.At.UTC()
	if f.pauses == nil {
		f.pauses = map[string]coord.SeatPause{}
	}
	f.pauses[p.Handle] = p
	stored := p
	f.notifyPauseLocked(coord.SeatPauseUpdate{Handle: p.Handle, Pause: &stored})
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
	slices.SortFunc(out, func(a, b coord.SeatPause) int { return cmp.Compare(a.Handle, b.Handle) })
	return out
}
