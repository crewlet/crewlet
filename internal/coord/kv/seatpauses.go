package kv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- seat pauses -------------------------------------------------------- //

// A pause is filed in the positions register under its own class — see
// [coord.SeatPauseKey] for why that bucket, which is the one with no age.

// SeatPause reads one seat's pause.
func (f *FleetStore) SeatPause(ctx context.Context, handle string) (coord.SeatPause, bool, error) {
	if handle == "" {
		return coord.SeatPause{}, false, errors.New("coord/kv: a seat pause needs a handle")
	}
	entry, err := f.positions.Get(ctx, coord.SeatPauseKey(handle))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.SeatPause{}, false, nil
	case err != nil:
		return coord.SeatPause{}, false, unavailable("read the pause of seat "+handle, err)
	}
	p, err := decodeSeatPause(handle, entry.Value(), entry.Revision())
	if err != nil {
		return coord.SeatPause{}, false, err
	}
	return p, true, nil
}

// ListSeatPauses returns every pause, ordered by handle.
//
// A DECODE FAILURE IS RAISED, not skipped: a pause dropped from this listing is
// a paused seat every node reads as free to work.
func (f *FleetStore) ListSeatPauses(ctx context.Context) ([]coord.SeatPause, error) {
	var out []coord.SeatPause
	err := f.eachUnder(ctx, f.positions, coord.DocumentFilter(coord.SeatPauseClass),
		"the seat pauses", func(kve jetstream.KeyValueEntry) error {
			handle, ok := seatPauseHandle(kve.Key())
			if !ok {
				return nil
			}
			p, err := decodeSeatPause(handle, kve.Value(), kve.Revision())
			if err != nil {
				return err
			}
			out = append(out, p)
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.SeatPause) int { return cmp.Compare(a.Handle, b.Handle) })
	return out, nil
}

// CreateSeatPause writes a pause for a seat that has none.
//
// Create rather than Put, so the first writer wins and every other is told the
// seat was already paused. A key a resume purged is created again, which the
// client does by conditioning on the purge marker's own revision — and a
// racing creator that lost over that marker is reported the same way, by
// [lostCreateRace].
func (f *FleetStore) CreateSeatPause(ctx context.Context, p coord.SeatPause) (coord.SeatPause, bool, error) {
	if err := p.Validate(); err != nil {
		return coord.SeatPause{}, false, err
	}
	raw, err := encodeSeatPause(p)
	if err != nil {
		return coord.SeatPause{}, false, err
	}
	revision, err := f.positions.Create(ctx, coord.SeatPauseKey(p.Handle), raw)
	switch {
	case err != nil && lostCreateRace(err):
		return coord.SeatPause{}, false, nil
	case err != nil:
		return coord.SeatPause{}, false, unavailable("pause seat "+p.Handle, err)
	}
	return storedSeatPause(p, revision), true, nil
}

// UpdateSeatPause writes a pause at the version it was read at.
func (f *FleetStore) UpdateSeatPause(ctx context.Context, p coord.SeatPause) (coord.SeatPause, bool, error) {
	if err := p.Validate(); err != nil {
		return coord.SeatPause{}, false, err
	}
	if p.Version == 0 {
		// NO VERSION IS A LOST RACE, never an unconditional write: the
		// client reads an expected revision of 0 as "the key must not exist
		// yet", which would CREATE a pause for a caller that never read one.
		return coord.SeatPause{}, false, nil
	}
	raw, err := encodeSeatPause(p)
	if err != nil {
		return coord.SeatPause{}, false, err
	}
	revision, err := f.positions.Update(ctx, coord.SeatPauseKey(p.Handle), raw, p.Version)
	switch {
	case err != nil && lostUpdateRace(err):
		return coord.SeatPause{}, false, nil
	case err != nil:
		return coord.SeatPause{}, false, unavailable("amend the pause of seat "+p.Handle, err)
	}
	return storedSeatPause(p, revision), true, nil
}

// DeleteSeatPause lifts a pause at a version.
//
// PURGE rather than delete, on this estate's standing rule: a delete leaves a
// tombstone revision in a bucket with no age, which outlives the deployment.
func (f *FleetStore) DeleteSeatPause(ctx context.Context, handle string, version uint64) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: a seat pause needs a handle")
	}
	if version == 0 {
		// The client drops a LastRevision of 0 and purges unconditionally,
		// so a caller that never read a version would lift whatever pause
		// is there — somebody else's included.
		return false, nil
	}
	err := f.positions.Purge(ctx, coord.SeatPauseKey(handle), jetstream.LastRevision(version))
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	default:
		return false, unavailable("resume seat "+handle, err)
	}
}

// WatchSeatPauses streams every pause, the marker, then every change.
//
// A KV watcher over the class filter, so the broker sends this class and no
// other of the eight sharing the register. Deletes are NOT ignored — a purge
// is exactly what a resume looks like on the wire.
func (f *FleetStore) WatchSeatPauses(ctx context.Context) (<-chan coord.SeatPauseUpdate, error) {
	w, err := f.positions.Watch(ctx, coord.DocumentFilter(coord.SeatPauseClass))
	if err != nil {
		return nil, unavailable("watch the seat pauses", err)
	}
	out := make(chan coord.SeatPauseUpdate)
	go func() {
		defer close(out)
		defer func() { _ = w.Stop() }()
		send := func(u coord.SeatPauseUpdate) bool {
			select {
			case out <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case kve, ok := <-w.Updates():
				if !ok {
					// The client ended the watch (a closed connection, a
					// deleted consumer). Closing is the contract's word for
					// "stopped hearing", which the consumer answers by
					// watching again — never by reading it as a resume.
					return
				}
				if kve == nil {
					if !send(coord.SeatPauseUpdate{Current: true}) {
						return
					}
					continue
				}
				handle, ok := seatPauseHandle(kve.Key())
				if !ok {
					continue
				}
				u := coord.SeatPauseUpdate{Handle: handle}
				if op := kve.Operation(); op != jetstream.KeyValueDelete && op != jetstream.KeyValuePurge {
					p, err := decodeSeatPause(handle, kve.Value(), kve.Revision())
					if err != nil {
						// A record this build cannot read ends the watch
						// rather than being skipped: skipped, it would
						// be a pause every node forgot. The consumer
						// watches again and logs the same failure.
						log.WarnContext(ctx, "seat_pause_undecodable", "handle", handle,
							"error", err.Error())
						return
					}
					u.Pause = &p
				}
				if !send(u) {
					return
				}
			}
		}
	}()
	return out, nil
}

// seatPauseHandle recovers the handle from a key of the pause class.
func seatPauseHandle(key string) (string, bool) {
	segments, ok := coord.DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != coord.SeatPauseClass {
		return "", false
	}
	return segments[1], true
}

func encodeSeatPause(p coord.SeatPause) ([]byte, error) {
	p.At = p.At.UTC()
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode the pause of seat %s: %w", p.Handle, err)
	}
	return raw, nil
}

// decodeSeatPause reads a record back, with the handle taken from the KEY: a
// record cannot describe a seat other than the one it is filed under.
func decodeSeatPause(handle string, raw []byte, version uint64) (coord.SeatPause, error) {
	var p coord.SeatPause
	if err := json.Unmarshal(raw, &p); err != nil {
		return coord.SeatPause{}, unavailable("decode the pause of seat "+handle, err)
	}
	p.Handle = handle
	p.At = p.At.UTC()
	p.Version = version
	return p, nil
}

func storedSeatPause(p coord.SeatPause, revision uint64) coord.SeatPause {
	p.At = p.At.UTC()
	p.Version = revision
	return p
}
