package coord

import "context"

// AccessReviews holds each agent seat's access change waiting on its review
// (ADR-0111). Any node may measure a seat, so every write is conditional on
// the revision it read; the value is internal/accessreview's.
type AccessReviews interface {
	// AccessReview is seat handle's record and its revision, false where none.
	AccessReview(ctx context.Context, handle string) ([]byte, uint64, bool, error)
	// CreateAccessReview writes a first record, false where one exists.
	CreateAccessReview(ctx context.Context, handle string, value []byte) (bool, error)
	// UpdateAccessReview replaces the record read at revision, false where it moved.
	UpdateAccessReview(ctx context.Context, handle string, value []byte, revision uint64) (bool, error)
	// DeleteAccessReview drops the record read at revision, false where it moved.
	DeleteAccessReview(ctx context.Context, handle string, revision uint64) (bool, error)
	// AccessReviewRevisions is every seat's record revision, by handle.
	AccessReviewRevisions(ctx context.Context) (map[string]uint64, error)
}

// AccessReviewClass is the key class a record is filed under in the
// positions register, which no clock ages.
const AccessReviewClass = "access_review"

// AccessReviewKey is one seat's key.
func AccessReviewKey(handle string) string { return DocumentKey(AccessReviewClass, handle) }

// AccessReviewFilter selects every seat's record.
func AccessReviewFilter() string { return DocumentFilter(AccessReviewClass) }

// AccessReviewHandle recovers the seat's handle from a key, false for another class.
func AccessReviewHandle(key string) (string, bool) {
	segments, ok := DocumentSegments(key)
	if !ok || len(segments) != 2 || segments[0] != AccessReviewClass || segments[1] == "" {
		return "", false
	}
	return segments[1], true
}
