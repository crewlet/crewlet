package memory

import (
	"context"
	"errors"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- model retries and device sign-ins ---------------------------------- //

// entryRecords is one record class keyed by a model entry: the records and the
// open watches of it. Its methods run under the fleet's lock, which the
// callers below take, and draw versions from the fleet's one counter for the
// reason [Fleet.version] gives.
type entryRecords[T, U any] struct {
	records  map[string]T
	watchers map[*watcher[U]]struct{}

	// stamp sets a record's version and normalizes it; update builds the
	// update a watch delivers (a nil record is a removal, an empty entry
	// the marker).
	stamp  func(v T, version uint64) T
	update func(v *T, entry string, current bool) U
}

func (r *entryRecords[T, U]) storeLocked(f *Fleet, entry string, v T) T {
	f.version++
	v = r.stamp(v, f.version)
	if r.records == nil {
		r.records = map[string]T{}
	}
	r.records[entry] = v
	r.notifyLocked(&v, entry)
	return v
}

func (r *entryRecords[T, U]) removeLocked(entry string) {
	delete(r.records, entry)
	r.notifyLocked(nil, entry)
}

func (r *entryRecords[T, U]) notifyLocked(v *T, entry string) {
	for w := range r.watchers {
		var copied *T
		if v != nil {
			// Each watcher gets its own copy, as the pause watch's do.
			c := *v
			copied = &c
		}
		w.push(r.update(copied, entry, false))
	}
}

func (r *entryRecords[T, U]) sortedLocked() []T {
	out := make([]T, 0, len(r.records))
	for _, entry := range slices.Sorted(maps.Keys(r.records)) {
		out = append(out, r.records[entry])
	}
	return out
}

// watchLocked opens a watch whose first answer is every record and the marker,
// taken under the same lock as the registration, as [Fleet.WatchSeatPauses]
// does.
func (r *entryRecords[T, U]) watchLocked(ctx context.Context, f *Fleet) <-chan U {
	var current []U
	for _, entry := range slices.Sorted(maps.Keys(r.records)) {
		v := r.records[entry]
		current = append(current, r.update(&v, entry, false))
	}
	w := newWatcher(append(current, r.update(nil, "", true))...)
	if r.watchers == nil {
		r.watchers = map[*watcher[U]]struct{}{}
	}
	r.watchers[w] = struct{}{}
	return w.serve(ctx, func() {
		f.mu.Lock()
		delete(r.watchers, w)
		f.mu.Unlock()
	})
}

func newRetryRecords() entryRecords[coord.ModelRetry, coord.ModelRetryUpdate] {
	return entryRecords[coord.ModelRetry, coord.ModelRetryUpdate]{
		stamp: func(v coord.ModelRetry, version uint64) coord.ModelRetry {
			v.At, v.Version = v.At.UTC(), version
			return v
		},
		update: func(v *coord.ModelRetry, entry string, current bool) coord.ModelRetryUpdate {
			return coord.ModelRetryUpdate{Retry: v, Key: entry, Current: current}
		},
	}
}

func newSignInRecords() entryRecords[coord.DeviceSignIn, coord.DeviceSignInUpdate] {
	return entryRecords[coord.DeviceSignIn, coord.DeviceSignInUpdate]{
		stamp: func(v coord.DeviceSignIn, version uint64) coord.DeviceSignIn {
			v.StartedAt, v.ExpiresAt, v.Version = v.StartedAt.UTC(), v.ExpiresAt.UTC(), version
			return v
		},
		update: func(v *coord.DeviceSignIn, entry string, current bool) coord.DeviceSignInUpdate {
			return coord.DeviceSignInUpdate{SignIn: v, Entry: entry, Current: current}
		},
	}
}

var errNoEntry = errors.New("coord/memory: a model record needs an entry key")

// ModelRetry reads one entry's retry.
func (f *Fleet) ModelRetry(_ context.Context, entry string) (coord.ModelRetry, bool, error) {
	if entry == "" {
		return coord.ModelRetry{}, false, errNoEntry
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.retries.records[entry]
	return r, ok, nil
}

// ListModelRetries returns every retry, ordered by entry key.
func (f *Fleet) ListModelRetries(context.Context) ([]coord.ModelRetry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.retries.sortedLocked(), nil
}

// PutModelRetry records a retry, keeping the later stamp.
func (f *Fleet) PutModelRetry(_ context.Context, r coord.ModelRetry) (coord.ModelRetry, error) {
	if err := r.Validate(); err != nil {
		return coord.ModelRetry{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.retries.records[r.Key]; ok && !r.At.After(held.At) {
		return held, nil
	}
	return f.retries.storeLocked(f, r.Key, r), nil
}

// DeleteModelRetry removes an entry's retry at a version.
func (f *Fleet) DeleteModelRetry(_ context.Context, entry string, version uint64) (bool, error) {
	if entry == "" {
		return false, errNoEntry
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.retries.records[entry]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.retries.removeLocked(entry)
	return true, nil
}

// WatchModelRetries streams every retry, the marker, then every change.
func (f *Fleet) WatchModelRetries(ctx context.Context) (<-chan coord.ModelRetryUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.retries.watchLocked(ctx, f), nil
}

// DeviceSignIn reads one entry's session.
func (f *Fleet) DeviceSignIn(_ context.Context, entry string) (coord.DeviceSignIn, bool, error) {
	if entry == "" {
		return coord.DeviceSignIn{}, false, errNoEntry
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.signIns.records[entry]
	return s, ok, nil
}

// ListDeviceSignIns returns every session, ordered by entry key.
func (f *Fleet) ListDeviceSignIns(context.Context) ([]coord.DeviceSignIn, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signIns.sortedLocked(), nil
}

// CreateDeviceSignIn writes a session for an entry that has no record.
func (f *Fleet) CreateDeviceSignIn(_ context.Context, s coord.DeviceSignIn) (coord.DeviceSignIn, bool, error) {
	if err := s.Validate(); err != nil {
		return coord.DeviceSignIn{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.signIns.records[s.Entry]; exists {
		return coord.DeviceSignIn{}, false, nil
	}
	return f.signIns.storeLocked(f, s.Entry, s), true, nil
}

// UpdateDeviceSignIn writes a session at the version it was read at.
func (f *Fleet) UpdateDeviceSignIn(_ context.Context, s coord.DeviceSignIn) (coord.DeviceSignIn, bool, error) {
	if err := s.Validate(); err != nil {
		return coord.DeviceSignIn{}, false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.signIns.records[s.Entry]
	if !ok || s.Version == 0 || held.Version != s.Version {
		return coord.DeviceSignIn{}, false, nil
	}
	return f.signIns.storeLocked(f, s.Entry, s), true, nil
}

// DeleteDeviceSignIn removes an entry's session at a version.
func (f *Fleet) DeleteDeviceSignIn(_ context.Context, entry string, version uint64) (bool, error) {
	if entry == "" {
		return false, errNoEntry
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.signIns.records[entry]
	if !ok || version == 0 || held.Version != version {
		return false, nil
	}
	f.signIns.removeLocked(entry)
	return true, nil
}

// WatchDeviceSignIns streams every session, the marker, then every change.
func (f *Fleet) WatchDeviceSignIns(ctx context.Context) (<-chan coord.DeviceSignInUpdate, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.signIns.watchLocked(ctx, f), nil
}
