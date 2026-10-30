package kv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- seat pauses -------------------------------------------------------- //

// A pause is filed in the positions register under its own class, by the
// seat's id — see [coord.SeatPauseKey] for why that bucket, which is the one
// with no age, and the file doc of internal/coord/seatpause.go for why the id.

// SeatPause reads one seat's pause, by the seat's id.
func (f *FleetStore) SeatPause(ctx context.Context, seat uuid.UUID) (coord.SeatPause, bool, error) {
	if seat == uuid.Nil {
		return coord.SeatPause{}, false, errors.New("coord/kv: a seat pause needs a seat id")
	}
	entry, err := f.get(ctx, f.positions, coord.SeatPauseKey(seat))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.SeatPause{}, false, nil
	case err != nil:
		return coord.SeatPause{}, false, unavailable("read the pause of seat "+seat.String(), err)
	}
	p, err := decodeSeatPause(seat, entry.Value(), entry.Revision())
	if err != nil {
		return coord.SeatPause{}, false, err
	}
	return p, true, nil
}

// ListSeatPauses returns every pause, ordered by seat id.
//
// A DECODE FAILURE IS RAISED, not skipped: a pause dropped from this listing is
// a paused seat every node reads as free to work. A KEY that names no seat id
// is skipped instead — this build files no pause under one, so no seat it
// runs is paused by it, and raising would let one stray key unpause nothing
// while stopping every node from reading the pauses that do exist.
func (f *FleetStore) ListSeatPauses(ctx context.Context) ([]coord.SeatPause, error) {
	var out []coord.SeatPause
	err := f.eachUnder(ctx, f.positions, coord.DocumentFilter(coord.SeatPauseClass),
		"the seat pauses", func(kve jetstream.KeyValueEntry) error {
			seat, ok := coord.SeatPauseSeat(kve.Key())
			if !ok {
				return nil
			}
			p, err := decodeSeatPause(seat, kve.Value(), kve.Revision())
			if err != nil {
				return err
			}
			out = append(out, p)
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b coord.SeatPause) int {
		return cmp.Compare(a.Seat.String(), b.Seat.String())
	})
	return out, nil
}

// CreateSeatPause writes a pause for a seat that has none.
//
// Create rather than Put, so the first writer wins and every other is told the
// seat was already paused. A key a resume purged is created again through
// [createKey], which asks the stream leader what the key holds rather than
// conditioning on a marker a sweep may already have removed, and reports a
// racing creator that won as the one shape, [jetstream.ErrKeyExists].
func (f *FleetStore) CreateSeatPause(ctx context.Context, p coord.SeatPause) (coord.SeatPause, bool, error) {
	if err := p.Validate(); err != nil {
		return coord.SeatPause{}, false, err
	}
	raw, err := encodeSeatPause(p)
	if err != nil {
		return coord.SeatPause{}, false, err
	}
	revision, err := f.create(ctx, f.positions, coord.SeatPauseKey(p.Seat), raw)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		return coord.SeatPause{}, false, nil
	case err != nil:
		return coord.SeatPause{}, false, unavailable("pause seat "+p.Seat.String(), err)
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
	revision, err := f.positions.Update(ctx, coord.SeatPauseKey(p.Seat), raw, p.Version)
	switch {
	case err != nil && lostUpdateRace(err):
		return coord.SeatPause{}, false, nil
	case err != nil:
		return coord.SeatPause{}, false, unavailable("amend the pause of seat "+p.Seat.String(), err)
	}
	return storedSeatPause(p, revision), true, nil
}

// DeleteSeatPause lifts a pause at a version.
//
// PURGE rather than delete, on this estate's standing rule: a delete leaves a
// tombstone revision in a bucket with no age, which outlives the deployment.
func (f *FleetStore) DeleteSeatPause(ctx context.Context, seat uuid.UUID, version uint64) (bool, error) {
	if seat == uuid.Nil {
		return false, errors.New("coord/kv: a seat pause needs a seat id")
	}
	if version == 0 {
		// The client drops a LastRevision of 0 and purges unconditionally,
		// so a caller that never read a version would lift whatever pause
		// is there — somebody else's included.
		return false, nil
	}
	err := f.positions.Purge(ctx, coord.SeatPauseKey(seat), jetstream.LastRevision(version))
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	default:
		return false, unavailable("resume seat "+seat.String(), err)
	}
}

// WatchSeatPauses streams every pause, the marker, then every change.
//
// A KV watcher over the class filter, so the broker sends this class and no
// other of the ten sharing the register. Deletes are NOT ignored — a purge
// is exactly what a resume looks like on the wire. The answer and the changes
// are [watchRecords]': the first answer certified, so a pause missing from it
// is never a hold a node lifts and a seat it lets take work, and a change the
// consumer delivers late filtered by revision.
//
// That lag is carried rather than closed. Confirming each change with a leader
// read would close it too, at a read per change on every node; it is not
// done, because a pause is a person's gesture and a moment's lag in honouring
// one moves no work that a fence does not already stop.
func (f *FleetStore) WatchSeatPauses(ctx context.Context) (<-chan coord.SeatPauseUpdate, error) {
	return watchRecords(ctx, f, f.positions, coord.DocumentFilter(coord.SeatPauseClass),
		recordWatch[coord.SeatPauseUpdate]{
			what: "the seat pauses",
			ours: func(key string) bool {
				_, ok := coord.SeatPauseSeat(key)
				return ok
			},
			update: func(kve jetstream.KeyValueEntry) (coord.SeatPauseUpdate, error) {
				seat, _ := coord.SeatPauseSeat(kve.Key())
				u := coord.SeatPauseUpdate{Seat: seat}
				if removed(kve) {
					return u, nil
				}
				p, err := decodeSeatPause(seat, kve.Value(), kve.Revision())
				if err != nil {
					return coord.SeatPauseUpdate{}, err
				}
				u.Pause = &p
				return u, nil
			},
			undecodable: func(ctx context.Context, kve jetstream.KeyValueEntry, err error) {
				seat, _ := coord.SeatPauseSeat(kve.Key())
				log.WarnContext(ctx, "seat_pause_undecodable", "seat", seat.String(),
					"error", err.Error())
			},
			current: coord.SeatPauseUpdate{Current: true},
		})
}

func encodeSeatPause(p coord.SeatPause) ([]byte, error) {
	p.At = p.At.UTC()
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode the pause of seat %s: %w", p.Seat, err)
	}
	return raw, nil
}

// decodeSeatPause reads a record back, with the seat taken from the KEY: a
// record cannot describe a seat other than the one it is filed under.
func decodeSeatPause(seat uuid.UUID, raw []byte, version uint64) (coord.SeatPause, error) {
	var p coord.SeatPause
	if err := json.Unmarshal(raw, &p); err != nil {
		return coord.SeatPause{}, unavailable("decode the pause of seat "+seat.String(), err)
	}
	p.Seat = seat
	p.At = p.At.UTC()
	p.Version = version
	return p, nil
}

func storedSeatPause(p coord.SeatPause, revision uint64) coord.SeatPause {
	p.At = p.At.UTC()
	p.Version = revision
	return p
}
