package pages

import (
	"github.com/crewlet/crewlet/internal/coord"
)

// The key classes in the pages family.
//
// ONE-LETTER, for the reason [work]'s are: a key is a subject token path and
// every byte is a byte in the broker's per-subject index on every member, for
// the life of the deployment.
const (
	// ClassContainer holds a container: s.<KEY>.
	ClassContainer = "s"

	// ClassPage holds a page head: p.<uuid>.
	ClassPage = "p"

	// ClassRevision holds one immutable past body: r.<page>.<n>.
	//
	// UNDER THE PAGE and numbered by VERSION rather than by a random id,
	// which is what makes "create the revision for version 7" a
	// first-writer-wins race with exactly one winner: two nodes saving at
	// once both try r.<page>.7, one loses, and the loser knows to re-read
	// rather than having written a second revision 7 nobody can order.
	ClassRevision = "r"

	// ClassComment holds one comment: m.<page>.<comment>.
	ClassComment = "m"

	// ClassChange holds one change: c.<page>.<uuid7>. Create-only.
	ClassChange = "c"

	// ClassTitle holds a title claim: t.<CONTAINER>.<normalised title>.
	//
	// THE TITLE IS IN THE KEY, which is why the key grammar escapes every
	// segment: a title contains spaces, dots and non-ASCII letters, and all
	// three are either refused by the store or silently re-tokenised into a
	// key of the wrong shape.
	ClassTitle = "t"
)

// ContainerKey is the coordination key for a container.
func ContainerKey(key string) string { return coord.DocumentKey(ClassContainer, key) }

// PageKey is the coordination key for a page head.
func PageKey(id string) string { return coord.DocumentKey(ClassPage, id) }

// RevisionKey is the coordination key for one revision.
func RevisionKey(pageID string, version int) string {
	return coord.DocumentKey(ClassRevision, pageID, itoa(version))
}

// RevisionPrefix covers one page's whole history.
func RevisionPrefix(pageID string) string {
	return coord.DocumentKey(ClassRevision, pageID)
}

// CommentKey is the coordination key for one comment.
func CommentKey(pageID, commentID string) string {
	return coord.DocumentKey(ClassComment, pageID, commentID)
}

// CommentPrefix covers one page's whole thread.
func CommentPrefix(pageID string) string {
	return coord.DocumentKey(ClassComment, pageID)
}

// ChangeKey is the coordination key for one change.
func ChangeKey(pageID, changeID string) string {
	return coord.DocumentKey(ClassChange, pageID, changeID)
}

// ChangePrefix covers one page's whole record.
func ChangePrefix(pageID string) string {
	return coord.DocumentKey(ClassChange, pageID)
}

// TitleKey is the coordination key for a container's hold on a title.
//
// The title is NORMALISED into the key, so "Deploy Runbook" and
// "deploy  runbook" claim the same one — which is what makes a title an
// address rather than a coin flip.
func TitleKey(container, title string) string {
	return coord.DocumentKey(ClassTitle, container, NormalizeTitle(title))
}

// ClassOf is which class a key belongs to, and false for a key this grammar
// did not write.
func ClassOf(key string) (string, bool) { return coord.KeyClass(key) }

// SegmentsOf recovers a key's parts.
func SegmentsOf(key string) ([]string, bool) { return coord.DocumentSegments(key) }

// PageIDOf recovers the page a key belongs to, for the classes that name one.
//
// The container and title classes name a CONTAINER rather than a page and
// answer false, so a caller cannot treat a claim key as a page id — which
// would file a claim's rows into a page's history.
func PageIDOf(key string) (string, bool) {
	segs, ok := SegmentsOf(key)
	if !ok || len(segs) < 2 {
		return "", false
	}
	switch segs[0] {
	case ClassPage, ClassRevision, ClassComment, ClassChange:
		return segs[1], true
	}
	return "", false
}

// itoa renders a version for a key without importing strconv into every
// caller.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		digits[i] = '-'
	}
	return string(digits[i:])
}
