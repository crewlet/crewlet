package tracker_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// aroundBoard seeds five tasks across three columns, with ranks chosen so the
// board's row order is neither creation order nor id order:
//
//	todo:        t1 (a1)  t2 (a2)
//	in_progress: t3 (a1)
//	in_review:   t5 (a1)  t4 (a3)
//
// and due dates on two of them, for the orders that have to place the undated.
func aroundBoard(t *testing.T) *readHarness {
	t.Helper()
	h := newReadHarness(t)
	due := func(day int) *time.Time {
		at := time.Date(2031, 4, day, 0, 0, 0, 0, time.UTC)
		return &at
	}
	for _, seed := range []struct {
		id     string
		status tracker.Status
		rank   tracker.Rank
		due    *time.Time
	}{
		{"t1", tracker.StatusTodo, "a1", due(30)},
		{"t2", tracker.StatusTodo, "a2", nil},
		{"t3", tracker.StatusInProgress, "a1", due(20)},
		{"t4", tracker.StatusInReview, "a3", nil},
		{"t5", tracker.StatusInReview, "a1", nil},
	} {
		h.seed(seed.id, func(task *tracker.Task) {
			task.Status, task.StatusGroup = seed.status, seed.status.Group()
			task.Rank, task.DueAt = seed.rank, seed.due
		})
	}
	return h
}

// placed is an Around reduced to what a case compares.
type placed struct {
	position   int
	prev, next string
	total      int
}

func placeOf(t *testing.T, h *readHarness, params map[string]any) placed {
	t.Helper()
	got := h.ask(params).Around
	if got == nil {
		t.Fatalf("around=%v is not in the answer to %v", params["around"], params)
	}
	out := placed{total: got.TotalHint}
	if got.Position != nil {
		out.position = *got.Position
	}
	if got.Prev != nil {
		out.prev = *got.Prev
	}
	if got.Next != nil {
		out.next = *got.Next
	}
	return out
}

// A TASK'S NEIGHBOURS ARE THE ONES THE BOARD DRAWS BESIDE IT, READ COLUMN BY
// COLUMN — AND OVER THE WHOLE BOARD, NOT THE ROWS IT HAPPENED TO LOAD.
//
// A task page opened from a board says "3 of 18" and steps to the next card,
// and the next card after the last one in To do is the first one in In
// progress. Each column here carries ONE row (`group_limit=1`), so a peek that
// stepped only through the rows a client held would stop at the first card of
// every column; the engine answers over every row of every column.
func TestAroundFollowsGroupThenRowOrder(t *testing.T) {
	t.Parallel()
	h := aroundBoard(t)
	board := map[string]any{
		"container": "project:ENG", "group_by": "status", "group_limit": "1",
	}
	for _, tc := range []struct {
		task string
		want placed
	}{
		{"t1", placed{1, "", "ENG-t2", 5}},
		// PAST THE COLUMN'S PAGE, and its next card is in the NEXT column.
		{"t2", placed{2, "ENG-t1", "ENG-t3", 5}},
		{"t3", placed{3, "ENG-t2", "ENG-t5", 5}},
		// Rank a1 before a3, so t5 leads its column although t4 was
		// created first — and its previous card is in the column before.
		{"t5", placed{4, "ENG-t3", "ENG-t4", 5}},
		{"t4", placed{5, "ENG-t5", "", 5}},
	} {
		if got := placeOf(t, h, withKey(board, "around", tc.task)); got != tc.want {
			t.Errorf("around=%s on a status board is %+v, want %+v", tc.task,
				got, tc.want)
		}
	}

	// A FLAT ANSWER IS ITS OWN ORDER, and a one-row page is no limit on it:
	// by rank, the ties broken by id, exactly as a cursor would page.
	flat := map[string]any{"container": "project:ENG", "limit": "1"}
	if got, want := placeOf(t, h, withKey(flat, "around", "t3")),
		(placed{2, "ENG-t1", "ENG-t5", 5}); got != want {
		t.Errorf("around=t3 on a flat rank order is %+v, want %+v", got, want)
	}

	// THE UNDATED ARE LAST IN BOTH DIRECTIONS, so the row before the first
	// undated task is the last DATED one — which is where the reversed order
	// has to put its NULLs first rather than trusting either default.
	for _, tc := range []struct {
		sort, task string
		want       placed
	}{
		{"due", "t2", placed{3, "ENG-t1", "ENG-t4", 5}},
		{"due", "t1", placed{2, "ENG-t3", "ENG-t2", 5}},
		// The row before t4 is an UNDATED one, and read backwards the
		// undated come first — so a reversed order that let SQLite put its
		// NULLs last would answer t1, two rows back.
		{"due", "t4", placed{4, "ENG-t2", "ENG-t5", 5}},
		{"-due", "t2", placed{3, "ENG-t3", "ENG-t4", 5}},
		{"-due", "t5", placed{5, "ENG-t4", "", 5}},
	} {
		got := placeOf(t, h, map[string]any{
			"container": "project:ENG", "sort": tc.sort, "around": tc.task,
		})
		if got != tc.want {
			t.Errorf("around=%s under sort=%s is %+v, want %+v", tc.task,
				tc.sort, got, tc.want)
		}
	}

	// AND WITH SWIMLANES THE ORDER IS COLUMN, THEN LANE, THEN ROW — checked
	// against the board's own drawing rather than a second copy of how lanes
	// are ordered, since that order is the answer's to decide.
	lanes := map[string]any{
		"container": "project:ENG", "group_by": "status",
		"group_by2": "priority", "group_limit": "100",
	}
	var drawn, drawnIDs []string
	for _, column := range h.ask(lanes).Groups {
		for _, lane := range column.Subgroups {
			for _, row := range lane.Rows {
				drawn = append(drawn, row.Key)
				drawnIDs = append(drawnIDs, row.ID)
			}
		}
	}
	if len(drawn) != 5 {
		t.Fatalf("the swimlane board draws %v, want all five tasks", drawn)
	}
	for i, key := range drawn {
		want := placed{position: i + 1, total: len(drawn)}
		if i > 0 {
			want.prev = drawn[i-1]
		}
		if i+1 < len(drawn) {
			want.next = drawn[i+1]
		}
		if got := placeOf(t, h, withKey(lanes, "around", drawnIDs[i])); got != want {
			t.Errorf("around=%s on a swimlane board is %+v, want %+v (drawn %v)",
				key, got, want, drawn)
		}
	}
}

// A TASK THE ANSWER DOES NOT HOLD IS NULL, NEVER A REFUSAL.
//
// Somebody opens a task that has just finished, from an open-work board: it is
// not on the board any more, and the page should drop "3 of 18" rather than
// fail its read. The same for a key that names nothing at all — a link to a
// task since purged.
func TestAroundAbsentKeyIsNullNotAnError(t *testing.T) {
	t.Parallel()
	h := aroundBoard(t)
	finished(h, "gone", tracker.StatusDone, wednesday)

	for _, key := range []string{"ENG-NOTHING", "gone"} {
		answer := h.ask(map[string]any{"container": "project:ENG", "around": key})
		if answer.Around != nil {
			t.Errorf("around=%s answered %+v, want null — the task is not in "+
				"this answer", key, *answer.Around)
		}
		if len(answer.Rows) != 5 {
			t.Errorf("around=%s changed the answer itself: %d rows", key,
				len(answer.Rows))
		}
	}
	// A CLOSED_SINCE BOARD DOES HOLD IT, which is the difference the scope
	// makes — the same task, the same key, a different question.
	since := h.ask(map[string]any{
		"container": "project:ENG", "closed_since": "sow", "around": "gone",
	})
	if since.Around == nil {
		t.Error("around=gone on a board that holds this week's finished " +
			"work answered null")
	}
}

// A PRIORITY LIST PAGES IN ITS OWN ORDER.
//
// The list's order used to be restored per page — SQL read a page, minted a
// keyset cursor on its last row, and the page was re-sorted by the list — so
// with a page smaller than the list, the top of somebody's priorities could
// land on page two. And the place `around=` reports is the list's order too,
// since that is the order the rows are drawn in.
func TestAPriorityListPagesInItsOwnOrder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		filedTask(t, r, id)
	}
	// Reverse id order, so a page in SQL's own order is visibly wrong.
	list := []string{"d", "c", "b", "a"}
	if _, err := r.writer.WritePriorities(t.Context(), "op-prio", "ana", list,
		nil, tracker.PersonAuthority{}); err != nil {
		t.Fatalf("WritePriorities: %v", err)
	}
	r.drain()

	var seen []string
	cursor := ""
	for range 4 {
		params := map[string]any{
			"container": "project:ENG", "priorities": "ana", "limit": "2",
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		answer := r.ask(params)
		seen = append(seen, ids(answer)...)
		if cursor = answer.NextCursor; cursor == "" {
			break
		}
	}
	if len(seen) != 4 || seen[0] != "d" || seen[1] != "c" || seen[2] != "b" ||
		seen[3] != "a" {
		t.Fatalf("a four-item list paged two at a time answers %v, want %v — "+
			"the list's order across pages, not within each", seen, list)
	}

	whole := r.ask(map[string]any{
		"container": "project:ENG", "priorities": "ana", "around": "b",
	})
	keyOf := map[string]string{}
	for _, row := range whole.Rows {
		keyOf[row.ID] = row.Key
	}
	around := whole.Around
	if around == nil || around.Position == nil || *around.Position != 3 ||
		around.Prev == nil || *around.Prev != keyOf["c"] ||
		around.Next == nil || *around.Next != keyOf["a"] {
		t.Errorf("around=b on the list is %+v, want third, between c (%s) and "+
			"a (%s)", around, keyOf["c"], keyOf["a"])
	}
}
