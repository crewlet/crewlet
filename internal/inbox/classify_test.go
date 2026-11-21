package inbox_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
)

// A NOTICE READS BY THE POSITION FIRST, THEN BY THE MARKS — and an unread mark
// outranks both.
//
// The read list is pruned to what sits above the position at every write, so
// the lists alone resurface old notices, the position alone misses out-of-order
// reads, and without the override "mark unread" after "mark all read" is
// invisible. The position is a triple: a notice on another stream is never
// covered, and a later generation is above every sequence of an earlier one.
// A snooze is asleep until its instant.
//
// Mutation: drop the unread override, the stream check or the generation
// order from Classify, or read a due snooze as asleep, and a row fails.
func TestANoticeReadsByThePositionThenTheMarks(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	later, lapsed := now.Add(time.Hour), now.Add(-time.Minute)
	at := func(gen uint32, seq uint64) inbox.Position {
		return inbox.Position{Stream: stream, Generation: gen, Seq: seq}
	}
	marks := inbox.Marks[reason]{
		Generation:  stream,
		SeenThrough: at(2, 100),
		Read:        []inbox.Entry{{RecordID: "above-marked-read", Position: at(2, 150).Packed()}},
		Unread:      []inbox.Entry{{RecordID: "below-marked-unread", Position: at(2, 50).Packed()}},
		Snoozed: []inbox.Entry{
			{RecordID: "asleep", Position: at(2, 160).Packed(), Until: &later},
			{RecordID: "due", Position: at(2, 170).Packed(), Until: &lapsed},
		},
	}
	c := marks.Classifier([]reason{"mention"})
	for _, row := range []struct {
		name string
		ref  inbox.Ref
		want inbox.Mark
	}{
		{"below the position", inbox.Ref{RecordID: "below", At: at(2, 99)},
			inbox.Mark{Read: true}},
		{"at the position", inbox.Ref{RecordID: "on-it", At: at(2, 100)},
			inbox.Mark{Read: true}},
		{"above the position", inbox.Ref{RecordID: "above", At: at(2, 101)},
			inbox.Mark{}},
		{"above it and marked read", inbox.Ref{RecordID: "above-marked-read", At: at(2, 150)},
			inbox.Mark{Read: true}},
		{"below it and marked unread", inbox.Ref{RecordID: "below-marked-unread", At: at(2, 50)},
			inbox.Mark{}},
		{"an earlier generation's larger sequence", inbox.Ref{RecordID: "old", At: at(1, 900)},
			inbox.Mark{Read: true}},
		{"a later generation's smaller sequence", inbox.Ref{RecordID: "new", At: at(3, 1)},
			inbox.Mark{}},
		{"on another stream", inbox.Ref{RecordID: "elsewhere",
			At: inbox.Position{Stream: "CREWLET_DEAD_LOG", Generation: 2, Seq: 1}},
			inbox.Mark{}},
		{"asleep", inbox.Ref{RecordID: "asleep", At: at(2, 160)},
			inbox.Mark{Snoozed: true, SnoozedUntil: &later}},
		{"a snooze whose time has come", inbox.Ref{RecordID: "due", At: at(2, 170)},
			inbox.Mark{}},
		{"under a primary reason", inbox.Ref{RecordID: "mentioned", At: at(2, 180),
			Reason: "mention"}, inbox.Mark{Primary: true}},
	} {
		if got := c.Classify(row.ref, now); got != row.want {
			t.Errorf("%s: classified %+v, want %+v", row.name, got, row.want)
		}
	}
}

// THE SCAN AND THE PAGE ARE ONE RULE: the sets a domain filters its scan by
// are exactly what the page is then marked by.
//
// A filter applied after the page was read short-pages it, so each domain
// filters its own scan; a filter disagreeing with the page's marks by one
// notice would hide an awake notice or show a sleeping one. So the sets come
// from the same lookups as the marks, held together here over one record: the
// overrides as stored, the sleeping notices as the clock finds them now.
//
// Mutation: let Asleep keep a due snooze, or drop a set's sort, and this
// fails.
func TestTheScanFiltersByTheMarksThePageCarries(t *testing.T) {
	t.Parallel()
	now := time.Date(2031, 4, 16, 9, 0, 0, 0, time.UTC)
	later, lapsed := now.Add(time.Hour), now.Add(-time.Minute)
	marks := inbox.Marks[reason]{
		SeenThrough: inbox.Position{Stream: stream, Seq: 100},
		Read:        []inbox.Entry{{RecordID: "r-b"}, {RecordID: "r-a"}},
		Unread:      []inbox.Entry{{RecordID: "u-b"}, {RecordID: "u-a"}},
		Snoozed: []inbox.Entry{
			{RecordID: "s-c", Until: &later}, {RecordID: "s-a"},
			{RecordID: "s-b", Until: &lapsed},
		},
	}
	c := marks.Classifier(nil)
	if got, want := c.ReadMarks(), []string{"r-a", "r-b"}; !slices.Equal(got, want) {
		t.Errorf("the read overrides are %v, want %v", got, want)
	}
	if got, want := c.UnreadMarks(), []string{"u-a", "u-b"}; !slices.Equal(got, want) {
		t.Errorf("the unread overrides are %v, want %v", got, want)
	}
	asleep := c.Asleep(now)
	if want := []string{"s-a", "s-c"}; !slices.Equal(asleep, want) {
		t.Errorf("the scan hides %v, want %v — a snooze without an instant, "+
			"and one still ahead", asleep, want)
	}
	for _, e := range marks.Snoozed {
		marked := c.Classify(inbox.Ref{RecordID: e.RecordID,
			At: inbox.Position{Stream: stream, Seq: 200}}, now).Snoozed
		if marked != slices.Contains(asleep, e.RecordID) {
			t.Errorf("%s: the page marks it snoozed = %v, and the scan hides it = %v",
				e.RecordID, marked, !marked)
		}
	}
	if c.Seen() != marks.SeenThrough {
		t.Errorf("the scan covers by %+v, want the stored %+v", c.Seen(), marks.SeenThrough)
	}
}
