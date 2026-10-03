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
// Every write goes through the register's [leaderBucket], so each one is
// SETTLED there; every read asks the stream leader.

// SeatPause reads one seat's pause, from the stream leader.
func (f *FleetStore) SeatPause(ctx context.Context, seat uuid.UUID) (coord.SeatPause, bool, error) {
	if seat == uuid.Nil {
		return coord.SeatPause{}, false, errors.New("coord/kv: a seat pause needs a seat id")
	}
	entry, err := f.positions.Get(ctx, coord.SeatPauseKey(seat))
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
	slices.SortFunc(out, func(a, b coord.SeatPause) int { return cmp.Compare(a.Seat.String(), b.Seat.String()) })
	return out, nil
}

// CreateSeatPause writes a pause for a seat that has none.
//
// Create rather than Put, so the first writer wins and every other is told the
// seat was already paused. A key a resume purged is created again, which
// [leaderBucket.Create] does by conditioning on the purge marker's own
// revision, read from the leader — and a racing creator that lost over that
// marker is reported the same way, by [lostCreateRace].
func (f *FleetStore) CreateSeatPause(ctx context.Context, p coord.SeatPause) (coord.SeatPause, bool, error) {
	if err := p.Validate(); err != nil {
		return coord.SeatPause{}, false, err
	}
	raw, err := encodeSeatPause(p)
	if err != nil {
		return coord.SeatPause{}, false, err
	}
	revision, err := f.positions.Create(ctx, coord.SeatPauseKey(p.Seat), raw)
	switch {
	case err != nil && lostCreateRace(err):
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
		// NO VERSION IS A LOST RACE, never an unconditional write: an
		// expected revision of 0 means "the key must not exist yet", which
		// would CREATE a pause for a caller that never read one.
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
// # The leader answers every update
//
// The obvious shape — the client's KV watcher over the class, delivering its
// initial values, its marker and its changes — is a REPLICA's read twice over
// (see bucket.go): the ordered consumer behind it is placed on any member, so
// its starting picture can miss a pause the quorum already holds, and a change
// it delivers late can be a pause that has since been lifted. Handed to the
// engine, the first is a paused seat it starts working and the second is a
// resumed seat it holds again.
//
// So this is built from the two reads that are the leader's. The change HINT
// ([leaderBucket.Watch]) is started first and the leader-closed listing
// ([FleetStore.ListSeatPauses]) taken second, so a write landing between the
// two is in the listing and hinted rather than in neither; the listing is
// delivered, then the marker; and every hint after it is CONFIRMED by reading
// that seat's pause from the leader, whose answer — a pause, or none — is the
// update delivered. A hint for a write already replaced, or delivered twice,
// costs one more leader read and changes nothing the consumer holds.
//
// A read that fails ENDS THE WATCH rather than being skipped: skipped, the
// change it was about would be one every node forgot. A closed channel is the
// contract's word for "stopped hearing", which the consumer answers by
// watching again — and the listing that starts the next watch delivers what
// this one missed.
func (f *FleetStore) WatchSeatPauses(ctx context.Context) (<-chan coord.SeatPauseUpdate, error) {
	ctx, cancel := context.WithCancel(ctx)
	hints, err := f.positions.Watch(ctx, coord.DocumentFilter(coord.SeatPauseClass))
	if err != nil {
		cancel()
		return nil, unavailable("watch the seat pauses", err)
	}
	current, err := f.ListSeatPauses(ctx)
	if err != nil {
		cancel()
		return nil, err
	}
	out := make(chan coord.SeatPauseUpdate)
	go func() {
		// cancel stops the hint's own goroutine, whichever way this one
		// ends.
		defer cancel()
		defer close(out)
		send := func(u coord.SeatPauseUpdate) bool {
			select {
			case out <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, p := range current {
			if !send(coord.SeatPauseUpdate{Seat: p.Seat, Pause: &p}) {
				return
			}
		}
		if !send(coord.SeatPauseUpdate{Current: true}) {
			return
		}
		for {
			select {
			case <-ctx.Done():
				return
			case key, ok := <-hints:
				if !ok {
					// The client ended the watch (a closed connection, a
					// deleted consumer): stopped hearing, never a resume.
					return
				}
				seat, ok := coord.SeatPauseSeat(key)
				if !ok {
					continue
				}
				p, paused, err := f.SeatPause(ctx, seat)
				if err != nil {
					log.WarnContext(ctx, "seat_pause_unconfirmed", "seat", seat.String(),
						"error", err.Error(),
						"detail", "a change to this seat's pause could not be read from "+
							"the leader; the watch ends and is opened again, and the "+
							"listing that opens it delivers the change")
					return
				}
				u := coord.SeatPauseUpdate{Seat: seat}
				if paused {
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
