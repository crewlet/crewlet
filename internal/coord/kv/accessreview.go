package kv

import (
	"bytes"
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seats' pending access reviews --------------------------------- //

// AccessReview is seat handle's pending review record, false where it has none.
func (f *FleetStore) AccessReview(ctx context.Context, handle string) ([]byte, bool, error) {
	if handle == "" {
		return nil, false, errors.New("coord/kv: an access review is read per seat")
	}
	entry, err := f.get(ctx, f.positions, coord.AccessReviewKey(handle))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil, false, nil
	case err != nil:
		return nil, false, unavailable("read "+handle+"'s access review", err)
	}
	return bytes.Clone(entry.Value()), true, nil
}

// PutAccessReview records seat handle's pending review.
func (f *FleetStore) PutAccessReview(ctx context.Context, handle string, value []byte) error {
	if handle == "" {
		return errors.New("coord/kv: an access review needs its seat")
	}
	if _, err := f.positions.Put(ctx, coord.AccessReviewKey(handle), value); err != nil {
		return unavailable("record "+handle+"'s access review", err)
	}
	return nil
}

// DeleteAccessReview drops seat handle's pending review, purged as every key
// of a bucket no clock ages is (markers.go).
func (f *FleetStore) DeleteAccessReview(ctx context.Context, handle string) error {
	if handle == "" {
		return errors.New("coord/kv: an access review needs its seat")
	}
	err := f.positions.Purge(ctx, coord.AccessReviewKey(handle))
	if err != nil && !errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("delete "+handle+"'s access review", err)
	}
	return nil
}
