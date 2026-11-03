package worklet

import (
	"context"
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// AN ALL-READ INBOX SCANS NO ROW BELOW THE SEEN-THROUGH POSITION.
//
// ana has read through tracker@1:9000 and marked nothing unread, so an
// `unread` page is ONE seek of tracker_notifications_inbox_idx on her and a
// lower bound — the position itself — and never a walk of every notice she
// holds, the whole retention, filtered row by row to nothing; that is the
// badge's question, asked every minute from every tab. An unread mark below
// the position adds the one statement that seeks what the mark names by the
// table's key and reads no other notice. A read that is not `unread` keeps
// what she has read, so its range is bounded by nothing but her. Planned with
// no statistics, as a deployment plans it ([seededPlanStore]).
//
// Mutation: bound the range by `since` alone whatever the read asks — the
// all-read page's lower bound is 0, red; seek the unread marks through the
// inbox range — the second statement reaches tracker_notifications_inbox_idx,
// red.
func TestAnAllReadInboxScansNoRowBelowTheSeenThroughPosition(t *testing.T) {
	t.Parallel()
	db := seededPlanStore(t, func(context.Context, *sql.Tx, int) error { return nil })
	seen := inbox.Position{Stream: topics.TrackerLogStream, Generation: 1, Seq: 9000}
	read := inboxFilter{unread: true, seen: seen, scope: inbox.SnoozeInclude}
	statements, selects, err := inboxStatements("ana", InboxQuery{}, read, 50)
	if err != nil || !selects || len(statements) != 1 {
		t.Fatalf("an all-read unread page is %d statements, %v, %v; want one", len(statements), selects, err)
	}
	page := statements[0]
	plan := storetest.Plan(t, db, page.sql, page.args)
	shown := strings.Join(plan, "\n")
	// THE BOUND IS IN THE SEEK: the page names no cursor, so the one
	// log_seq term its range can seek on is the lower bound — which the
	// pinned planner prints mirrored, `log_seq<?`, over the index's DESC
	// key.
	if !slices.ContainsFunc(plan, func(line string) bool {
		return strings.Contains(line, "tracker_notifications_inbox_idx (recipient=? AND log_seq")
	}) {
		t.Errorf("an unread page does not seek ana's notices above a lower bound:\n%s", shown)
	}
	if got := page.args[1]; got != int64(seen.Packed()) {
		t.Errorf("an all-read unread page's range starts above %v, want the seen-through position %d — "+
			"every notice below it is read", got, seen.Packed())
	}

	marked := read
	marked.marked = []string{"h-00042"}
	statements, _, err = inboxStatements("ana", InboxQuery{}, marked, 50)
	if err != nil || len(statements) != 2 {
		t.Fatalf("an unread page with a mark below the position is %d statements, %v; want two",
			len(statements), err)
	}
	plan = storetest.Plan(t, db, statements[1].sql, statements[1].args)
	shown = strings.Join(plan, "\n")
	if !storetest.Seeks(plan, "sqlite_autoindex_tracker_notifications_1") ||
		slices.Contains(storetest.IndexesIn(plan), "tracker_notifications_inbox_idx") ||
		storetest.ScansHeap(plan, "tracker_notifications") {
		t.Errorf("the notices an unread mark brings back are not sought by their key alone:\n%s", shown)
	}

	every := read
	every.unread = false
	statements, _, err = inboxStatements("ana", InboxQuery{}, every, 50)
	if err != nil || len(statements) != 1 || strings.Contains(statements[0].sql, "log_seq >") {
		t.Fatalf("a page of every notice is bounded below by %v, %v; want nothing but ana", statements, err)
	}
	plan = storetest.Plan(t, db, statements[0].sql, statements[0].args)
	if slices.ContainsFunc(plan, func(line string) bool { return strings.Contains(line, "AND log_seq") }) {
		t.Errorf("a page of every notice seeks a log_seq bound it never named:\n%s", strings.Join(plan, "\n"))
	}
}
