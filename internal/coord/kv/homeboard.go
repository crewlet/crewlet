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

// ---- the people's Home boards ------------------------------------------ //

// One record per seat in the positions register (internal/coord's
// homeboard.go). Reads are the leader's ([FleetStore.get]) and a first write
// is [createKey], to write over a forgotten board's purge marker, as chat read
// state's are (chatread.go); the stamp's writes retry as its do.

// decodeHomeBoard reads a stored board back.
func decodeHomeBoard(handle string, raw []byte) (coord.HomeBoard, error) {
	var b coord.HomeBoard
	if err := json.Unmarshal(raw, &b); err != nil {
		return coord.HomeBoard{}, fmt.Errorf("coord/kv: decode the Home board of %s: %w", handle, err)
	}
	b.Board = bytes.Clone(b.Board)
	return b, nil
}

// encodeHomeBoard is a board as stored: without HTML escaping, so the board
// reads back in the bytes it was saved in.
func encodeHomeBoard(handle string, b coord.HomeBoard) ([]byte, error) {
	raw, err := plainjson.Marshal(b, "")
	if err != nil {
		return nil, fmt.Errorf("coord/kv: encode the Home board of %s: %w", handle, err)
	}
	return raw, nil
}

// homeBoardAt reads a seat's board and its revision, zero where none is stored.
func (f *FleetStore) homeBoardAt(ctx context.Context, handle string) (coord.HomeBoard, uint64, error) {
	entry, err := f.get(ctx, f.positions, coord.HomeBoardKey(handle))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.HomeBoard{}, 0, nil
	case err != nil:
		return coord.HomeBoard{}, 0, unavailable("read the Home board of "+handle, err)
	}
	b, err := decodeHomeBoard(handle, entry.Value())
	if err != nil {
		return coord.HomeBoard{}, 0, err
	}
	return b, entry.Revision(), nil
}

// HomeBoard is a seat's board and its revision, false where none is stored.
func (f *FleetStore) HomeBoard(ctx context.Context, handle string) (coord.HomeBoard, uint64, bool, error) {
	if handle == "" {
		return coord.HomeBoard{}, 0, false, errors.New("coord/kv: a Home board is read per seat")
	}
	b, revision, err := f.homeBoardAt(ctx, handle)
	return b, revision, revision != 0 && err == nil, err
}

// SaveHomeBoard replaces a seat's board if it is still at revision, keeping
// its absence stamp. One round, no retry: a revision that moved is the
// caller's to read again, never this store's to write over.
func (f *FleetStore) SaveHomeBoard(ctx context.Context, handle string, board json.RawMessage,
	at time.Time, revision uint64) (uint64, bool, error) {

	if handle == "" {
		return 0, false, errors.New("coord/kv: a Home board is saved per seat")
	}
	if err := coord.CheckHomeBoard(board); err != nil {
		return 0, false, err
	}
	stored, held, err := f.homeBoardAt(ctx, handle)
	switch {
	case err != nil:
		return 0, false, err
	case held != revision:
		return 0, false, nil
	}
	next := coord.HomeBoard{Board: board, UpdatedAt: at.UTC(), AbsentSince: stored.AbsentSince}
	raw, err := encodeHomeBoard(handle, next)
	if err != nil {
		return 0, false, err
	}
	return f.putSeatRecordAt(ctx, "save the Home board of "+handle, coord.HomeBoardKey(handle), raw, revision)
}

// putSeatRecordAt writes a seat's personal record (a board, its instructions)
// over the revision read (zero: none), answering where it landed, or false
// where another writer got there first; it classifies every other failure, as
// [FleetStore.putChatReadAt] does.
func (f *FleetStore) putSeatRecordAt(ctx context.Context, what, key string, raw []byte,
	revision uint64) (uint64, bool, error) {

	if revision == 0 {
		landed, err := f.create(ctx, f.positions, key, raw)
		switch {
		case err == nil:
			return landed, true, nil
		case errors.Is(err, jetstream.ErrKeyExists):
			return 0, false, nil
		case errors.Is(err, errCreateUnanswered):
			return 0, false, writeUnknown(what, err)
		default:
			return 0, false, unavailable(what, err)
		}
	}
	landed, err := f.positions.Update(ctx, key, raw, revision)
	switch {
	case err == nil:
		return landed, true, nil
	case lostUpdateRace(err):
		return 0, false, nil
	default:
		return 0, false, writeUnknown(what, err)
	}
}

// SetHomeBoardAbsence stamps or clears when a board's seat was first found
// missing from the company, reporting whether the seat has a board. Re-read
// and re-applied on a lost race ([casRetry]): a save beside it loses nothing.
func (f *FleetStore) SetHomeBoardAbsence(ctx context.Context, handle string, since time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: a Home board's absence is stamped per seat")
	}
	var exists bool
	settled, err := casRetry(ctx, &f.positionTrips, "stamp the absence on the Home board of "+handle,
		func() (bool, error) {
			stored, revision, err := f.homeBoardAt(ctx, handle)
			if err != nil {
				return false, err
			}
			if revision == 0 {
				exists = false
				return true, nil
			}
			exists = true
			next, changed := stored.WithAbsence(since)
			if !changed {
				return true, nil
			}
			raw, err := encodeHomeBoard(handle, next)
			if err != nil {
				return false, err
			}
			_, written, err := f.putSeatRecordAt(ctx, "stamp the absence on the Home board of "+handle,
				coord.HomeBoardKey(handle), raw, revision)
			return written, err
		})
	switch {
	case err != nil:
		return false, err
	case !settled:
		return false, contended("SetHomeBoardAbsence", handle)
	}
	return exists, nil
}

// ForgetHomeBoard purges a seat's board if it was stamped absent at or before
// absentBy, judged on the record the purge is conditioned on, as
// [FleetStore.ForgetChatRead] judges a read state.
func (f *FleetStore) ForgetHomeBoard(ctx context.Context, handle string, absentBy time.Time) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: a Home board is forgotten per seat")
	}
	var removed bool
	settled, err := casRetry(ctx, &f.positionTrips, "forget the Home board of "+handle, func() (bool, error) {
		stored, revision, err := f.homeBoardAt(ctx, handle)
		if err != nil {
			return false, err
		}
		if revision == 0 || !stored.AbsentBy(absentBy) {
			removed = false
			return true, nil
		}
		err = f.positions.Purge(ctx, coord.HomeBoardKey(handle), jetstream.LastRevision(revision))
		switch {
		case err == nil:
			removed = true
			return true, nil
		case lostUpdateRace(err):
			// Moved or gone under the read: judge it again.
			return false, nil
		}
		return false, writeUnknown("forget the Home board of "+handle, err)
	})
	switch {
	case err != nil:
		return false, err
	case !settled:
		return false, contended("ForgetHomeBoard", handle)
	}
	return removed, nil
}

// HomeBoardOwners is every seat holding a board, by handle, with its absence
// stamp. An undecodable board is raised: an owner left out is never forgotten.
func (f *FleetStore) HomeBoardOwners(ctx context.Context) ([]coord.HomeBoardOwner, error) {
	var owners []coord.HomeBoardOwner
	err := f.eachUnder(ctx, f.positions, coord.HomeBoardFilter(), "the Home boards",
		func(entry jetstream.KeyValueEntry) error {
			handle, ok := coord.HomeBoardHandle(entry.Key())
			if !ok {
				return nil
			}
			b, err := decodeHomeBoard(handle, entry.Value())
			if err != nil {
				return err
			}
			owners = append(owners, coord.HomeBoardOwner{Handle: handle, AbsentSince: b.AbsentSince})
			return nil
		})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(owners, func(a, b coord.HomeBoardOwner) int { return cmp.Compare(a.Handle, b.Handle) })
	return owners, nil
}
