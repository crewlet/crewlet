package kv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- model retries and device sign-ins ---------------------------------- //

// Both are one record per model entry, filed in the positions register under
// a class of their own and keyed by the entry's key: see internal/coord's
// modelretry.go and devicesignin.go for why that bucket, which has no age.
// They share every verb but their decoding, so the verbs are written once
// here, over [entryClass], rather than twice beside the seat pauses' own.

// entryClass is one record class keyed by a model entry: how its keys are
// spelled and read back, and how its records are encoded.
type entryClass[T any] struct {
	// name is the class, what a failure names ("model retries").
	name string
	// filter selects the class's keys.
	filter string
	// key spells an entry's key, and entry reads one back (false for a key
	// this class did not write).
	key   func(entry string) string
	entry func(key string) (string, bool)
	// stored stamps a record with the entry its key names and the version
	// it was read or written at, and normalizes its instants: a record
	// cannot describe an entry other than the one it is filed under.
	stored func(v T, entry string, version uint64) T
}

var (
	retryClass = entryClass[coord.ModelRetry]{
		name:   "the model retries",
		filter: coord.DocumentFilter(coord.ModelRetryClass),
		key:    coord.ModelRetryKey,
		entry:  coord.ModelRetryEntry,
		stored: func(r coord.ModelRetry, entry string, version uint64) coord.ModelRetry {
			r.Key, r.At, r.Version = entry, r.At.UTC(), version
			return r
		},
	}
	signInClass = entryClass[coord.DeviceSignIn]{
		name:   "the device sign-ins",
		filter: coord.DocumentFilter(coord.DeviceSignInClass),
		key:    coord.DeviceSignInKey,
		entry:  coord.DeviceSignInEntry,
		stored: func(s coord.DeviceSignIn, entry string, version uint64) coord.DeviceSignIn {
			s.Entry, s.Version = entry, version
			s.StartedAt, s.ExpiresAt = s.StartedAt.UTC(), s.ExpiresAt.UTC()
			return s
		},
	}
)

func (c entryClass[T]) encode(entry string, v T) ([]byte, error) {
	raw, err := json.Marshal(c.stored(v, entry, 0))
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode %s record of %s: %w", c.name, entry, err)
	}
	return raw, nil
}

func (c entryClass[T]) decode(entry string, raw []byte, version uint64) (T, error) {
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return v, unavailable(fmt.Sprintf("decode %s record of %s", c.name, entry), err)
	}
	return c.stored(v, entry, version), nil
}

// read reads one entry's record from the stream leader.
func (c entryClass[T]) read(ctx context.Context, f *FleetStore, entry string) (T, bool, error) {
	var zero T
	if entry == "" {
		return zero, false, fmt.Errorf("coord/kv: %s need an entry key", c.name)
	}
	kve, err := f.get(ctx, f.positions, c.key(entry))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return zero, false, nil
	case err != nil:
		return zero, false, unavailable("read "+c.name+" of "+entry, err)
	}
	v, err := c.decode(entry, kve.Value(), kve.Revision())
	if err != nil {
		return zero, false, err
	}
	return v, true, nil
}

// list reads every record of the class, ordered by entry key. A decode failure
// is RAISED, as the seat pauses' listing raises it; a key naming no entry is
// skipped, since this build files nothing under one.
func (c entryClass[T]) list(ctx context.Context, f *FleetStore) ([]T, error) {
	type row struct {
		entry string
		v     T
	}
	var rows []row
	err := f.eachUnder(ctx, f.positions, c.filter, c.name, func(kve jetstream.KeyValueEntry) error {
		entry, ok := c.entry(kve.Key())
		if !ok {
			return nil
		}
		v, err := c.decode(entry, kve.Value(), kve.Revision())
		if err != nil {
			return err
		}
		rows = append(rows, row{entry, v})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(rows, func(a, b row) int { return cmp.Compare(a.entry, b.entry) })
	out := make([]T, len(rows))
	for i, r := range rows {
		out[i] = r.v
	}
	return out, nil
}

// create writes an entry's record if it has none, through [createKey] for the
// reason [FleetStore.CreateSeatPause] gives.
func (c entryClass[T]) create(ctx context.Context, f *FleetStore, entry string, v T) (T, bool, error) {
	var zero T
	raw, err := c.encode(entry, v)
	if err != nil {
		return zero, false, err
	}
	revision, err := f.create(ctx, f.positions, c.key(entry), raw)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		return zero, false, nil
	case err != nil:
		return zero, false, unavailable("create "+c.name+" record of "+entry, err)
	}
	return c.stored(v, entry, revision), true, nil
}

// update writes an entry's record at the version it was read at. No version is
// a lost race, never an unconditional write, for
// [FleetStore.UpdateSeatPause]'s reason.
func (c entryClass[T]) update(ctx context.Context, f *FleetStore, entry string, v T, version uint64) (T, bool, error) {
	var zero T
	if version == 0 {
		return zero, false, nil
	}
	raw, err := c.encode(entry, v)
	if err != nil {
		return zero, false, err
	}
	revision, err := f.positions.Update(ctx, c.key(entry), raw, version)
	switch {
	case err != nil && lostUpdateRace(err):
		return zero, false, nil
	case err != nil:
		return zero, false, unavailable("update "+c.name+" record of "+entry, err)
	}
	return c.stored(v, entry, revision), true, nil
}

// remove purges an entry's record at a version: PURGE rather than delete, on
// the register's standing rule ([FleetStore.DeleteSeatPause]).
func (c entryClass[T]) remove(ctx context.Context, f *FleetStore, entry string, version uint64) (bool, error) {
	if entry == "" {
		return false, fmt.Errorf("coord/kv: %s need an entry key", c.name)
	}
	if version == 0 {
		return false, nil
	}
	err := f.positions.Purge(ctx, c.key(entry), jetstream.LastRevision(version))
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	default:
		return false, unavailable("remove "+c.name+" record of "+entry, err)
	}
}

// watchEntries streams every record, the marker, then every change: the certified
// first answer and the revision filter of [FleetStore.WatchSeatPauses], for
// the reasons it gives. deliver builds one update: the record (nil on a
// removal) and its entry, or the marker.
func watchEntries[T, U any](ctx context.Context, f *FleetStore, c entryClass[T],
	deliver func(v *T, entry string, current bool) U) (<-chan U, error) {

	current, w, err := watchUnder(ctx, f.js, f.positions, c.filter, c.name)
	if err != nil {
		return nil, err
	}
	held := make(map[string]uint64, len(current))
	answer := make([]U, 0, len(current)+1)
	for _, key := range slices.Sorted(maps.Keys(current)) {
		kve := current[key]
		held[key] = kve.Revision()
		entry, ok := c.entry(key)
		if !ok || kve.Operation() != jetstream.KeyValuePut {
			continue
		}
		v, err := c.decode(entry, kve.Value(), kve.Revision())
		if err != nil {
			_ = w.Stop()
			return nil, err
		}
		answer = append(answer, deliver(&v, entry, false))
	}
	answer = append(answer, deliver(nil, "", true))

	out := make(chan U)
	go func() {
		defer close(out)
		defer func() { _ = w.Stop() }()
		send := func(u U) bool {
			select {
			case out <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, u := range answer {
			if !send(u) {
				return
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case kve, ok := <-w.Updates():
				if !ok {
					return
				}
				if kve == nil {
					continue
				}
				entry, ok := c.entry(kve.Key())
				if !ok {
					continue
				}
				if r, seen := held[kve.Key()]; seen && kve.Revision() <= r {
					continue
				}
				held[kve.Key()] = kve.Revision()
				var record *T
				if op := kve.Operation(); op != jetstream.KeyValueDelete && op != jetstream.KeyValuePurge {
					v, err := c.decode(entry, kve.Value(), kve.Revision())
					if err != nil {
						log.WarnContext(ctx, "model_record_undecodable", "class", c.name,
							"entry", entry, "error", err.Error())
						return
					}
					record = &v
				}
				if !send(deliver(record, entry, false)) {
					return
				}
			}
		}
	}()
	return out, nil
}

// ModelRetry reads one entry's retry.
func (f *FleetStore) ModelRetry(ctx context.Context, entry string) (coord.ModelRetry, bool, error) {
	return retryClass.read(ctx, f, entry)
}

// ListModelRetries returns every retry, ordered by entry key.
func (f *FleetStore) ListModelRetries(ctx context.Context) ([]coord.ModelRetry, error) {
	return retryClass.list(ctx, f)
}

// PutModelRetry records a retry, keeping the later stamp.
//
// A READ, THEN A CREATE OR AN UPDATE AT THE VERSION READ, retried on a lost
// race: two requests landing together each either win or find a stamp at
// least as late as their own, so no request that was later is overwritten.
func (f *FleetStore) PutModelRetry(ctx context.Context, r coord.ModelRetry) (coord.ModelRetry, error) {
	if err := r.Validate(); err != nil {
		return coord.ModelRetry{}, err
	}
	for range maxRetryRounds {
		held, found, err := retryClass.read(ctx, f, r.Key)
		if err != nil {
			return coord.ModelRetry{}, err
		}
		if found && !r.At.After(held.At) {
			return held, nil
		}
		var wrote bool
		var stored coord.ModelRetry
		if found {
			stored, wrote, err = retryClass.update(ctx, f, r.Key, r, held.Version)
		} else {
			stored, wrote, err = retryClass.create(ctx, f, r.Key, r)
		}
		if err != nil {
			return coord.ModelRetry{}, err
		}
		if wrote {
			return stored, nil
		}
	}
	return coord.ModelRetry{}, contended("PutModelRetry", r.Key)
}

// maxRetryRounds bounds [FleetStore.PutModelRetry]'s compare-and-set: a person's
// gesture contends with at most the handful of other people retrying the same
// entry at the same moment, so eight lost rounds is a store that is not
// settling rather than a busy key.
const maxRetryRounds = 8

// DeleteModelRetry removes an entry's retry at a version.
func (f *FleetStore) DeleteModelRetry(ctx context.Context, entry string, version uint64) (bool, error) {
	return retryClass.remove(ctx, f, entry, version)
}

// WatchModelRetries streams every retry, the marker, then every change.
func (f *FleetStore) WatchModelRetries(ctx context.Context) (<-chan coord.ModelRetryUpdate, error) {
	return watchEntries(ctx, f, retryClass,
		func(v *coord.ModelRetry, entry string, current bool) coord.ModelRetryUpdate {
			return coord.ModelRetryUpdate{Retry: v, Key: entry, Current: current}
		})
}

// DeviceSignIn reads one entry's session.
func (f *FleetStore) DeviceSignIn(ctx context.Context, entry string) (coord.DeviceSignIn, bool, error) {
	return signInClass.read(ctx, f, entry)
}

// ListDeviceSignIns returns every session, ordered by entry key.
func (f *FleetStore) ListDeviceSignIns(ctx context.Context) ([]coord.DeviceSignIn, error) {
	return signInClass.list(ctx, f)
}

// CreateDeviceSignIn writes a session for an entry that has no record.
func (f *FleetStore) CreateDeviceSignIn(ctx context.Context, s coord.DeviceSignIn) (coord.DeviceSignIn, bool, error) {
	if err := s.Validate(); err != nil {
		return coord.DeviceSignIn{}, false, err
	}
	return signInClass.create(ctx, f, s.Entry, s)
}

// UpdateDeviceSignIn writes a session at the version it was read at.
func (f *FleetStore) UpdateDeviceSignIn(ctx context.Context, s coord.DeviceSignIn) (coord.DeviceSignIn, bool, error) {
	if err := s.Validate(); err != nil {
		return coord.DeviceSignIn{}, false, err
	}
	return signInClass.update(ctx, f, s.Entry, s, s.Version)
}

// DeleteDeviceSignIn removes an entry's session at a version.
func (f *FleetStore) DeleteDeviceSignIn(ctx context.Context, entry string, version uint64) (bool, error) {
	return signInClass.remove(ctx, f, entry, version)
}

// WatchDeviceSignIns streams every session, the marker, then every change.
func (f *FleetStore) WatchDeviceSignIns(ctx context.Context) (<-chan coord.DeviceSignInUpdate, error) {
	return watchEntries(ctx, f, signInClass,
		func(v *coord.DeviceSignIn, entry string, current bool) coord.DeviceSignInUpdate {
			return coord.DeviceSignInUpdate{SignIn: v, Entry: entry, Current: current}
		})
}
