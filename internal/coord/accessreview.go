package coord

import "context"

// AccessReviews holds each agent seat's access change waiting on its review
// (ADR-0111): written when a change is first measured, deleted when a turn
// that was told it ends, or when the change is reverted before any turn was.
//
// The whole company, now: the node holding the seat writes it, and whichever
// node serves a person's Inbox reads it. The value is opaque, as
// [SeatAccess]'s is (internal/accessreview's vocabulary).
type AccessReviews interface {
	// AccessReview is seat handle's record, false where it has none.
	AccessReview(ctx context.Context, handle string) ([]byte, bool, error)
	// PutAccessReview records it, replacing any prior one.
	PutAccessReview(ctx context.Context, handle string, value []byte) error
	// DeleteAccessReview drops it; deleting one that is not there is fine.
	DeleteAccessReview(ctx context.Context, handle string) error
}

// AccessReviewClass is the key class a record is filed under in the
// positions register, which no clock ages.
const AccessReviewClass = "access_review"

// AccessReviewKey is one seat's key.
func AccessReviewKey(handle string) string { return DocumentKey(AccessReviewClass, handle) }
