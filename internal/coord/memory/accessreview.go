package memory

import (
	"bytes"
	"context"
	"errors"
)

// ---- the seats' pending access reviews --------------------------------- //

// AccessReview is seat handle's pending review record, false where it has none.
func (f *Fleet) AccessReview(_ context.Context, handle string) ([]byte, bool, error) {
	if handle == "" {
		return nil, false, errors.New("coord/memory: an access review is read per seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	value, ok := f.accessReviews[handle]
	return bytes.Clone(value), ok, nil
}

// PutAccessReview records seat handle's pending review.
func (f *Fleet) PutAccessReview(_ context.Context, handle string, value []byte) error {
	if handle == "" {
		return errors.New("coord/memory: an access review needs its seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accessReviews[handle] = bytes.Clone(value)
	return nil
}

// DeleteAccessReview drops seat handle's pending review.
func (f *Fleet) DeleteAccessReview(_ context.Context, handle string) error {
	if handle == "" {
		return errors.New("coord/memory: an access review needs its seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.accessReviews, handle)
	return nil
}
