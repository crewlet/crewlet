package work

import (
	"strings"

	"github.com/crewlet/crewlet/internal/coord"
)

// The key classes in the work family.
//
// ONE-LETTER CLASSES, and the brevity is load-bearing rather than terse: a
// key is a subject token path under the bucket, and every byte of every key
// is a byte in the broker's per-subject index on every member, held for the
// life of the deployment. A million changes at four extra bytes each is four
// megabytes of index per member bought for readability nobody needs — the
// class is always read through [ClassOf], never by eye.
const (
	// ClassItem holds an item head: i.<uuid>.
	ClassItem = "i"

	// ClassComment holds one comment: m.<item>.<comment>. Under the item,
	// so a prefix listing gets a whole thread — which the boot reconcile
	// and the sweep both need and a flat class could not give.
	ClassComment = "m"

	// ClassChange holds one change: c.<item>.<uuid7>. CREATE-ONLY and
	// never rewritten, which is what makes the feed safe: a bucket with
	// history 1 terminates an un-acked message when its key is rewritten.
	ClassChange = "c"

	// ClassCounter holds a project's key sequence: n.<PROJECT>.
	//
	// ITS OWN CLASS, not a field on anything, because minting is a
	// compare-and-set on the smallest possible record — putting the
	// counter on a project document would make every mint contend with
	// every edit to that project's settings.
	ClassCounter = "n"
)

// ItemKey is the coordination key for an item head.
func ItemKey(id string) string { return coord.DocumentKey(ClassItem, id) }

// CommentKey is the coordination key for one comment.
func CommentKey(itemID, commentID string) string {
	return coord.DocumentKey(ClassComment, itemID, commentID)
}

// CommentPrefix is the key prefix covering one item's whole thread.
//
// NO TRAILING SEPARATOR: a listing matches on WHOLE SEGMENTS, so the prefix is
// the segments themselves and the boundary is the listing's business. Adding
// the separator here would make the prefix "m.<id>." and match nothing, which
// reads as an item with no comments rather than as a malformed query.
func CommentPrefix(itemID string) string {
	return coord.DocumentKey(ClassComment, itemID)
}

// ChangeKey is the coordination key for one change.
func ChangeKey(itemID, changeID string) string {
	return coord.DocumentKey(ClassChange, itemID, changeID)
}

// ChangePrefix is the key prefix covering one item's whole history, on the
// same terms as [CommentPrefix].
func ChangePrefix(itemID string) string {
	return coord.DocumentKey(ClassChange, itemID)
}

// CounterKey is the coordination key for a project's sequence.
func CounterKey(project string) string { return coord.DocumentKey(ClassCounter, project) }

// ClassOf is which class a key belongs to, and false for a key this grammar
// did not write.
//
// A key it did not write is SKIPPED by every reader rather than guessed at:
// a newer build writes classes this one has no rule for, and a rolling
// upgrade must not wedge the older half's projector.
func ClassOf(key string) (string, bool) { return coord.KeyClass(key) }

// SegmentsOf recovers a key's parts.
func SegmentsOf(key string) ([]string, bool) { return coord.DocumentSegments(key) }

// ItemIDOf recovers the item a key belongs to, for every class that names one.
//
// The counter class names a PROJECT rather than an item and answers false, so
// a caller cannot accidentally treat a project key as an item id — which
// would put the counter's rows into an item's thread.
func ItemIDOf(key string) (string, bool) {
	segs, ok := SegmentsOf(key)
	if !ok || len(segs) < 2 {
		return "", false
	}
	switch segs[0] {
	case ClassItem, ClassComment, ClassChange:
		return segs[1], true
	}
	return "", false
}

// ChangeSubjectFilter is the consumer filter selecting every change key in a
// bucket, and nothing else.
//
// THE WHOLE REASON THE CLASS IS THE FIRST TOKEN. A consumer subscribed to
// this gets every change and no heads, no comments and no counters — so the
// feed never has to decode a record to discover it should ignore it, and a
// head rewrite (which compaction may terminate mid-delivery) is never on the
// feed at all.
func ChangeSubjectFilter(bucketSubjectPrefix string) string {
	return strings.TrimSuffix(bucketSubjectPrefix, ".") + "." + ClassChange + ".>"
}
