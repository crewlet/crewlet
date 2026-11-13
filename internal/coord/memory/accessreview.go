package memory

import (
	"bytes"
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the seats' pending access reviews --------------------------------- //

// accessReview is one seat's record and its revision.
type accessReview struct {
	value    []byte
	revision uint64
}

// AccessReview is seat handle's record and its revision, false where none.
func (f *Fleet) AccessReview(_ context.Context, handle string) ([]byte, uint64, bool, error) {
	if handle == "" {
		return nil, 0, false, errors.New("coord/memory: an access review is read per seat")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.accessReviews[handle]
	return bytes.Clone(r.value), r.revision, ok, nil
}

// CreateAccessReview writes seat handle's first record, false where one exists.
func (f *Fleet) CreateAccessReview(_ context.Context, handle string, value []byte) (bool, error) {
	if handle == "" {
		return false, errors.New("coord/memory: an access review needs its seat")
	}
	if err := coord.HoldValue("the access review of "+handle, len(value)); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.accessReviews[handle]; ok {
		return false, nil
	}
	f.accessReviewRevision++
	f.accessReviews[handle] = accessReview{value: bytes.Clone(value), revision: f.accessReviewRevision}
	return true, nil
}

// UpdateAccessReview replaces the record read at revision, false where it moved.
func (f *Fleet) UpdateAccessReview(_ context.Context, handle string, value []byte, revision uint64) (bool, error) {
	if handle == "" || revision == 0 {
		return false, errors.New("coord/memory: an access review is updated at the revision it was read at")
	}
	if err := coord.HoldValue("the access review of "+handle, len(value)); err != nil {
		return false, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.accessReviews[handle]; !ok || r.revision != revision {
		return false, nil
	}
	f.accessReviewRevision++
	f.accessReviews[handle] = accessReview{value: bytes.Clone(value), revision: f.accessReviewRevision}
	return true, nil
}

// DeleteAccessReview drops the record read at revision, false where it moved.
func (f *Fleet) DeleteAccessReview(_ context.Context, handle string, revision uint64) (bool, error) {
	if handle == "" || revision == 0 {
		return false, errors.New("coord/memory: an access review is deleted at the revision it was read at")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.accessReviews[handle]; !ok || r.revision != revision {
		return false, nil
	}
	delete(f.accessReviews, handle)
	return true, nil
}

// AccessReviewRevisions is every seat's record revision, by handle.
func (f *Fleet) AccessReviewRevisions(context.Context) (map[string]uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]uint64{}
	for handle, r := range f.accessReviews {
		out[handle] = r.revision
	}
	return out, nil
}
