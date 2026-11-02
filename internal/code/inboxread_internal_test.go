package code

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN ALL-READ INBOX SCANS NO ROW BELOW THE SEEN-THROUGH POSITION: founder has
// read through code@1:9000 and marked nothing unread, so an `unread` read's
// range starts at that position and seeks no notice below it; an unread mark
// below it is the one notice sought there, by its key. A `since` above the
// position bounds the range by itself; a read that is not `unread` keeps every
// notice, read or not, so its range starts at `since` alone; and a position
// on another stream covers no code notice, so it bounds nothing.
//
// Mutation: start the range at `since` whatever the read asks — the all-read
// inbox walks every notice founder holds, red.
func TestAnAllReadInboxScansNoRowBelowTheSeenThroughPosition(t *testing.T) {
	t.Parallel()
	seen := inbox.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9000}
	since := statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 9500}
	elsewhere := inbox.Position{Stream: topics.TrackerLogStream, Generation: 1, Seq: 9000}
	unreadAgain := inbox.Entry{RecordID: NoticeID("record-1", "change-1"), Position: 4000}
	for name, tc := range map[string]struct {
		seen   inbox.Position
		unread []inbox.Entry
		q      InboxQuery
		floor  int64
		marked string
	}{
		"an all-read inbox": {seen: seen, q: InboxQuery{Unread: true}, floor: int64(seen.Packed()),
			marked: "[]"},
		"an unread mark below the position": {seen: seen, unread: []inbox.Entry{unreadAgain},
			q: InboxQuery{Unread: true}, floor: int64(seen.Packed()), marked: `[["record-1","change-1"]]`},
		"since above the position": {seen: seen, q: InboxQuery{Unread: true, Since: since},
			floor: since.Packed(), marked: "[]"},
		"every notice": {seen: seen, q: InboxQuery{}, floor: 0, marked: "[]"},
		"a position on another stream": {seen: elsewhere, q: InboxQuery{Unread: true}, floor: 0,
			marked: "[]"},
	} {
		marks := inbox.Marks[Reason]{SeenThrough: tc.seen, Unread: tc.unread}
		tc.q.Handle, tc.q.Snoozed = "founder", inbox.SnoozeInclude
		scan, selects, err := inboxScanOf(ReadQuery{Kind: ReadInbox, Inbox: &tc.q},
			marks.Classifier(DefaultPrimaryReasons), time.Now())
		if err != nil || !selects {
			t.Fatalf("%s: inboxScanOf = %v, %v", name, selects, err)
		}
		if got := scan.floor(); got != tc.floor {
			t.Errorf("%s: the range starts above %d, want %d", name, got, tc.floor)
		}
		if scan.marked != tc.marked {
			t.Errorf("%s: the scan seeks the unread marks %s, want %s", name, scan.marked, tc.marked)
		}
	}
}
