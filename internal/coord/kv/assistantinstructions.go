package kv

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/plainjson"
)

// A person's Ask Crewlet instructions, in the positions register under
// [coord.AssistantInstructionsKey]: the Home board's mechanism (homeboard.go)
// for a record of its own class.

func decodeSeatInstructions(handle string, raw []byte) (coord.SeatInstructions, error) {
	var s coord.SeatInstructions
	if err := json.Unmarshal(raw, &s); err != nil {
		return coord.SeatInstructions{}, fmt.Errorf("coord/kv: decode the instructions of %s: %w", handle, err)
	}
	s.Instructions = bytes.Clone(s.Instructions)
	return s, nil
}

func encodeSeatInstructions(handle string, s coord.SeatInstructions) ([]byte, error) {
	raw, err := plainjson.Marshal(s, "")
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode the instructions of %s: %w", handle, err)
	}
	return raw, nil
}

func (f *FleetStore) seatInstructionsAt(ctx context.Context, handle string) (coord.SeatInstructions, uint64, error) {
	entry, err := f.get(ctx, f.positions, coord.AssistantInstructionsKey(handle))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.SeatInstructions{}, 0, nil
	case err != nil:
		return coord.SeatInstructions{}, 0, unavailable("read the instructions of "+handle, err)
	}
	s, err := decodeSeatInstructions(handle, entry.Value())
	if err != nil {
		return coord.SeatInstructions{}, 0, err
	}
	return s, entry.Revision(), nil
}

// SeatInstructions implements [coord.AssistantInstructions].
func (f *FleetStore) SeatInstructions(ctx context.Context, handle string) (coord.SeatInstructions, uint64, bool, error) {
	if handle == "" {
		return coord.SeatInstructions{}, 0, false, errors.New("coord/kv: instructions are read per seat")
	}
	s, revision, err := f.seatInstructionsAt(ctx, handle)
	return s, revision, revision != 0 && err == nil, err
}

// SaveSeatInstructions implements [coord.AssistantInstructions].
func (f *FleetStore) SaveSeatInstructions(ctx context.Context, handle string, doc json.RawMessage,
	at time.Time, revision uint64) (uint64, bool, error) {

	if handle == "" {
		return 0, false, errors.New("coord/kv: instructions are saved per seat")
	}
	if err := coord.CheckAssistantInstructions(doc); err != nil {
		return 0, false, err
	}
	stored, held, err := f.seatInstructionsAt(ctx, handle)
	switch {
	case err != nil:
		return 0, false, err
	case held != revision:
		return 0, false, nil
	}
	next := coord.SeatInstructions{Instructions: doc, UpdatedAt: at.UTC(), AbsentSince: stored.AbsentSince}
	raw, err := encodeSeatInstructions(handle, next)
	if err != nil {
		return 0, false, err
	}
	return f.putSeatRecordAt(ctx, "save the instructions of "+handle,
		coord.AssistantInstructionsKey(handle), raw, revision)
}

// SetSeatInstructionsAbsence implements [coord.AssistantInstructions].
func (f *FleetStore) SetSeatInstructionsAbsence(ctx context.Context, handle string, since time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: an absence on instructions is stamped per seat")
	}
	what := "stamp the absence on the instructions of " + handle
	var exists bool
	settled, err := casRetry(ctx, &f.positionTrips, what, func() (bool, error) {
		stored, revision, err := f.seatInstructionsAt(ctx, handle)
		if err != nil {
			return false, err
		}
		exists = revision != 0
		if !exists {
			return true, nil
		}
		next, changed := stored.WithAbsence(since)
		if !changed {
			return true, nil
		}
		raw, err := encodeSeatInstructions(handle, next)
		if err != nil {
			return false, err
		}
		_, written, err := f.putSeatRecordAt(ctx, what, coord.AssistantInstructionsKey(handle), raw, revision)
		return written, err
	})
	switch {
	case err != nil:
		return false, err
	case !settled:
		return false, contended("SetSeatInstructionsAbsence", handle)
	}
	return exists, nil
}

// ForgetSeatInstructions implements [coord.AssistantInstructions].
func (f *FleetStore) ForgetSeatInstructions(ctx context.Context, handle string, absentBy time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: instructions are forgotten per seat")
	}
	what := "forget the instructions of " + handle
	var removed bool
	settled, err := casRetry(ctx, &f.positionTrips, what, func() (bool, error) {
		stored, revision, err := f.seatInstructionsAt(ctx, handle)
		if err != nil {
			return false, err
		}
		if revision == 0 || !stored.AbsentBy(absentBy) {
			removed = false
			return true, nil
		}
		err = f.positions.Purge(ctx, coord.AssistantInstructionsKey(handle), jetstream.LastRevision(revision))
		switch {
		case err == nil:
			removed = true
			return true, nil
		case lostUpdateRace(err):
			return false, nil
		}
		return false, writeUnknown(what, err)
	})
	switch {
	case err != nil:
		return false, err
	case !settled:
		return false, contended("ForgetSeatInstructions", handle)
	}
	return removed, nil
}

// SeatInstructionsOwners implements [coord.AssistantInstructions].
func (f *FleetStore) SeatInstructionsOwners(ctx context.Context) ([]coord.SeatInstructionsOwner, error) {
	var owners []coord.SeatInstructionsOwner
	err := f.eachUnder(ctx, f.positions, coord.AssistantInstructionsFilter(), "the Ask Crewlet instructions",
		func(entry jetstream.KeyValueEntry) error {
			handle, ok := coord.AssistantInstructionsHandle(entry.Key())
			if !ok {
				return nil
			}
			s, err := decodeSeatInstructions(handle, entry.Value())
			if err != nil {
				return err
			}
			owners = append(owners, coord.SeatInstructionsOwner{Handle: handle, AbsentSince: s.AbsentSince})
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(owners, func(a, b coord.SeatInstructionsOwner) int { return cmp.Compare(a.Handle, b.Handle) })
	return owners, nil
}
