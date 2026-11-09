package kv

import (
	"bytes"
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seats' pending access reviews --------------------------------- //

// AccessReview is seat handle's record and its revision, false where none.
func (f *FleetStore) AccessReview(ctx context.Context, handle string) ([]byte, uint64, bool, error) {
	if handle == "" {
		return nil, 0, false, errors.New("coord/kv: an access review is read per seat")
	}
	entry, err := f.get(ctx, f.positions, coord.AccessReviewKey(handle))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, 0, false, nil
	case err != nil:
		return nil, 0, false, unavailable("read "+handle+"'s access review", err)
	}
	return bytes.Clone(entry.Value()), entry.Revision(), true, nil
}

// CreateAccessReview writes seat handle's first record, false where one exists.
func (f *FleetStore) CreateAccessReview(ctx context.Context, handle string, value []byte) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/kv: an access review needs its seat")
	}
	_, err := f.create(ctx, f.positions, coord.AccessReviewKey(handle), value)
	switch {
	case errors.Is(err, jetstream.ErrKeyExists):
		return false, nil
	case err != nil:
		return false, unavailable("create "+handle+"'s access review", err)
	}
	return true, nil
}

// UpdateAccessReview replaces the record read at revision, false where it moved.
func (f *FleetStore) UpdateAccessReview(ctx context.Context, handle string, value []byte,
	revision uint64) (bool, error) {

	if handle == "" || revision == 0 {
		return false, errors.New("coord/kv: an access review is updated at the revision it was read at")
	}
	_, err := f.positions.Update(ctx, coord.AccessReviewKey(handle), value, revision)
	switch {
	case err != nil && lostUpdateRace(err):
		return false, nil
	case err != nil:
		return false, unavailable("update "+handle+"'s access review", err)
	}
	return true, nil
}

// DeleteAccessReview purges the record read at revision, false where it moved.
func (f *FleetStore) DeleteAccessReview(ctx context.Context, handle string, revision uint64) (bool, error) {
	if handle == "" || revision == 0 {
		return false, errors.New("coord/kv: an access review is deleted at the revision it was read at")
	}
	err := f.positions.Purge(ctx, coord.AccessReviewKey(handle), jetstream.LastRevision(revision))
	switch {
	case err == nil:
		return true, nil
	case lostUpdateRace(err):
		return false, nil
	}
	return false, unavailable("delete "+handle+"'s access review", err)
}

// AccessReviewRevisions is every seat's record revision, by handle.
func (f *FleetStore) AccessReviewRevisions(ctx context.Context) (map[string]uint64, error) {
	out := map[string]uint64{}
	err := f.eachUnder(ctx, f.positions, coord.AccessReviewFilter(), "the access reviews",
		func(kve jetstream.KeyValueEntry) error {
			if handle, ok := coord.AccessReviewHandle(kve.Key()); ok {
				out[handle] = kve.Revision()
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}
