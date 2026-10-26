package inbox

import (
	"slices"
	"time"
)

// Classifier is a person's marks as the lookups a page of their notices is
// classified against — built once per page, from the record read in the same
// snapshot as the page, so every notice on it is judged against one instant
// of the marks.
//
// IT ALSO HOLDS THE SETS A DOMAIN'S SCAN FILTERS BY ([Classifier.Seen],
// [Classifier.ReadMarks], [Classifier.UnreadMarks], [Classifier.Asleep]). An
// inbox read that filtered the page AFTER reading it short-paged it — a
// person with sixty snoozed notices among their newest fifty opened an inbox
// of nothing with a cursor behind it — so a domain states its filters as
// predicates on its own scan, and they are only right while they agree with
// the marks the page then carries. Both read the same lookups here.
type Classifier struct {
	seen    Position
	primary map[string]bool

	// read is the entries ABOVE the seen-through position that this person
	// has nonetheless marked read, unread the entries AT OR BELOW it they
	// marked unread again, and snoozed the sleeping ones. All three are
	// keyed on the record id an [Entry] names, which is the notice row's
	// own record id.
	read    map[string]bool
	unread  map[string]bool
	snoozed map[string]*time.Time
}

// Classifier is m as the lookups a page is classified against. primary is the
// split that APPLIES — the person's declaration, or the domain's shipped
// default where they declared none — which the domain resolves, because the
// default is that domain's.
func (m Marks[R]) Classifier(primary []R) Classifier {
	out := Classifier{
		seen:    m.SeenThrough,
		primary: make(map[string]bool, len(primary)),
		read:    make(map[string]bool, len(m.Read)),
		unread:  make(map[string]bool, len(m.Unread)),
		snoozed: make(map[string]*time.Time, len(m.Snoozed)),
	}
	for _, reason := range primary {
		out.primary[string(reason)] = true
	}
	for _, entry := range m.Read {
		out.read[entry.RecordID] = true
	}
	for _, entry := range m.Unread {
		out.unread[entry.RecordID] = true
	}
	for _, entry := range m.Snoozed {
		out.snoozed[entry.RecordID] = entry.Until
	}
	return out
}

// Ref is what one notice's mark is decided from: which notice, where it sits
// on its domain's log, and the reason it reached this person under.
type Ref struct {
	RecordID string
	At       Position
	Reason   string
}

// Mark is one notice's state for the person whose inbox it is in.
type Mark struct {
	// Primary says which half of the split the notice fell in.
	Primary bool

	// Read, Snoozed and SnoozedUntil are the person's own mark over it. A
	// notice they have never touched is neither.
	Read         bool
	Snoozed      bool
	SnoozedUntil *time.Time
}

// Classify is the mark one notice carries for this person at now.
//
// READ IS THE SEEN-THROUGH POSITION FIRST and the entry list second, which is
// the whole reason the position exists: the read list is PRUNED at every
// write to what sits above it ([Apply]), so a notice below the position
// carries no read entry at all. Reading only the lists would report every
// pruned notice unread — which is every notice older than the person's last
// visit, the exact set an inbox must not resurface. Reading only the position
// would miss what they marked read out of order, which is what a person
// working their queue does.
//
// AND AN UNREAD MARK OUTRANKS THE POSITION. It is the one way to bring back a
// notice the position already covers — after a "mark all read", that is every
// notice — and a reader that consulted only the position and the read list
// made that gesture a mark nothing could ever see.
//
// A SNOOZE WHOSE TIME HAS COME IS NOT SNOOZED: the entry is reported due by
// the domain's own read rather than silently promoted, because putting one
// back is a write and a read that performed one would change the fleet's
// state from a path with no operation id and no record.
func (c Classifier) Classify(n Ref, now time.Time) Mark {
	mark := Mark{
		Primary: c.primary[n.Reason],
		Read:    !c.unread[n.RecordID] && (c.read[n.RecordID] || readPast(c.seen, n.At)),
	}
	if until, asleep := c.snoozed[n.RecordID]; asleep && live(until, now) {
		mark.Snoozed, mark.SnoozedUntil = true, until
	}
	return mark
}

// readPast reports whether a notice at at sits at or below the seen-through
// position.
//
// THE STREAM HAS TO MATCH, which is what the triple is for: a sequence from a
// dead stream compares as current against a live one, and a founder whose
// stream was recreated would open an inbox in which everything below their old
// position reads as already seen.
func readPast(seen, at Position) bool {
	if seen.Seq == 0 || seen.Stream == "" || seen.Stream != at.Stream {
		return false
	}
	if seen.Generation != at.Generation {
		return seen.Generation > at.Generation
	}
	return seen.Seq >= at.Seq
}

// Seen is the seen-through position the marks are read against: a scan
// covers a notice by it ON THE SAME STREAM, compared as the packed position,
// exactly as [Classifier.Classify] does.
func (c Classifier) Seen() Position { return c.seen }

// Primary reports whether a notice under reason falls in the primary half.
func (c Classifier) Primary(reason string) bool { return c.primary[reason] }

// ReadMarks is every notice the person marked read out of order, sorted, so
// one question is one statement: a map iterates in a random order and the
// arguments would otherwise differ per call.
func (c Classifier) ReadMarks() []string { return sortedKeys(c.read) }

// UnreadMarks is every notice the person marked unread again, sorted for
// [Classifier.ReadMarks]'s reason.
func (c Classifier) UnreadMarks() []string { return sortedKeys(c.unread) }

// Asleep is every notice still snoozed at now, sorted for
// [Classifier.ReadMarks]'s reason — by the test [Classifier.Classify] marks one snoozed
// by, so a notice a scan hides is exactly a notice the page would have marked
// snoozed, and one whose time has come is back under every scope.
func (c Classifier) Asleep(now time.Time) []string {
	out := make([]string, 0, len(c.snoozed))
	for id, until := range c.snoozed {
		if live(until, now) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

// sortedKeys is a set's members in order.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}
