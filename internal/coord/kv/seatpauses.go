package kv

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
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
// is exactly what a resume looks like on the wire.
//
// # The first answer is a certified listing, and the call waits for it
//
// Everything before [coord.SeatPauseUpdate.Current] is the answer a node
// REPLACES its copy with, so a pause missing from it is a hold the node lifts
// and a seat it lets take work. The client's own end of the initial values
// cannot be that answer: it ends them on a count guess, while a pause live
// throughout can still be ahead of the cursor, and it never ends them at all
// for a consumer whose pending messages were removed before it reached them —
// a resume's purge marker swept from the register, say — so that answer never
// came, and every node that met it deferred its deliveries until somebody
// paused or resumed a seat. So the first answer is [watchUnder]'s, certified
// against the register's key index exactly as [FleetStore.ListSeatPauses] is,
// and the client's marker is ignored whenever it arrives.
//
// The call returns once that answer is in hand, and a failure to certify it
// is the call's error rather than a closed channel: there is no answer for a
// consumer to have stopped hearing. A record in it this build cannot read
// fails the call the same way, naming the seat.
//
// # A change after the answer is the consumer's, and filtered by revision
//
// Every update after the answer comes off the same consumer, which is a
// replica's copy (walk.go): a change it delivers can be LATE, never wrong in
// order, because the consumer delivers a key's revisions in the order the
// stream holds them. The revision filter below is what keeps a late one from
// undoing a newer one the answer already holds, so the worst a lagging replica
// costs a node is the lag itself — a pause or a resume taken up a moment
// later, never one taken back. Confirming each change with a leader read would
// close that lag too, at a read per change on every node; it is not done,
// because a pause is a person's gesture and a moment's lag in honouring one
// moves no work that a fence does not already stop.
func (f *FleetStore) WatchSeatPauses(ctx context.Context) (<-chan coord.SeatPauseUpdate, error) {
	current, w, err := watchUnder(ctx, f.js, f.positions,
		coord.DocumentFilter(coord.SeatPauseClass), "the seat pauses")
	if err != nil {
		return nil, err
	}
	// held is the revision of each key the answer holds — a marker's too,
	// since a resume the answer saw is a revision of its key like any other.
	held := make(map[string]uint64, len(current))
	answer := make([]coord.SeatPauseUpdate, 0, len(current)+1)
	for _, key := range slices.Sorted(maps.Keys(current)) {
		kve := current[key]
		held[key] = kve.Revision()
		seat, ok := coord.SeatPauseSeat(key)
		if !ok || kve.Operation() != jetstream.KeyValuePut {
			continue
		}
		p, err := decodeSeatPause(seat, kve.Value(), kve.Revision())
		if err != nil {
			// RAISED rather than skipped, as the listing raises it: a
			// pause dropped from the answer is a seat every node reads
			// as free to work. The consumer watches again and says why.
			_ = w.Stop()
			return nil, err
		}
		answer = append(answer, coord.SeatPauseUpdate{Seat: seat, Pause: &p})
	}
	answer = append(answer, coord.SeatPauseUpdate{Current: true})

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
					// The client ended the watch (a closed connection, a
					// deleted consumer). Closing is the contract's word for
					// "stopped hearing", which the consumer answers by
					// watching again — never by reading it as a resume.
					return
				}
				if kve == nil {
					// The client's end of the initial values, late or
					// never: the certified answer has already ended them.
					continue
				}
				seat, ok := coord.SeatPauseSeat(kve.Key())
				if !ok {
					continue
				}
				if r, seen := held[kve.Key()]; seen && kve.Revision() <= r {
					// NOT A CHANGE: the revision the answer holds for this
					// key, or an older one — the consumer reaching a key its
					// pass was ended before, or one the leader's read was
					// ahead of it on. Sent, an older revision would undo the
					// newer one on every node until the consumer caught up.
					continue
				}
				held[kve.Key()] = kve.Revision()
				u := coord.SeatPauseUpdate{Seat: seat}
				if op := kve.Operation(); op != jetstream.KeyValueDelete && op != jetstream.KeyValuePurge {
					p, err := decodeSeatPause(seat, kve.Value(), kve.Revision())
					if err != nil {
						// A record this build cannot read ends the watch
						// rather than being skipped: skipped, it would
						// be a pause every node forgot. The consumer
						// watches again and logs the same failure.
						log.WarnContext(ctx, "seat_pause_undecodable", "seat", seat.String(),
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
