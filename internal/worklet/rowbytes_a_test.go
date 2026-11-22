package worklet_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// askWithin is [readHarness.ask] under ctx, which carries the bytes a read's
// rows are held to ([statelog.WithAnswerBytes]).
func askWithin(ctx context.Context, h *readHarness, kv map[string]any) worklet.Answer {
	h.t.Helper()
	q, err := worklet.ParseQuery(worklet.MapParams(kv), wednesday, berlin)
	if err != nil {
		h.t.Fatalf("ParseQuery(%v): %v", kv, err)
	}
	q.Level = statelog.ReadStale
	answer, err := h.reader.Tasks(ctx, q, wednesday)
	if err != nil {
		h.t.Fatalf("Tasks(%v): %v", kv, err)
	}
	return answer
}

// twoRowsOf is a bound two of the answer's rows fit and three do not: twice
// the heaviest row as the wire encodes it, the rows being near-equal.
func twoRowsOf(t *testing.T, rows []worklet.TaskRow) int {
	t.Helper()
	heaviest := 0
	for _, row := range rows {
		raw, err := json.Marshal(row)
		if err != nil {
			t.Fatalf("encode %s: %v", row.ID, err)
		}
		heaviest = max(heaviest, len(raw))
	}
	return 2 * heaviest
}

// A TASK LIST PAGES AT THE ANSWER'S BYTES, AND A BOARD'S COLUMN SAYS IT HOLDS
// MORE.
//
// Five tasks under a bound two of their rows fit: each flat page is cut short
// of its limit, its cursor resumes after the last row kept, and paging on
// reads every task once in the unbounded order. A board's column holds the
// two and keeps its count over all five, the signal a board loads the rest by.
//
// Mutations, each red: drop holdPage from Tasks and the first page holds all
// five; mint the cut page's cursor from the refused row and paging skips one
// task per page; drop holdGroups and the column holds all five.
func TestATaskListPagesAtTheAnswersBytes(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		h.seed(id, func(task *worklet.Task) {
			task.Title = strings.Repeat("x", worklet.MaxTitle-1) + id
		})
	}
	whole := h.ask(map[string]any{"container": "project:ENG"})
	if len(whole.Rows) != 5 || whole.NextCursor != "" {
		t.Fatalf("the unbounded list holds %d rows (cursor %q), want all 5",
			len(whole.Rows), whole.NextCursor)
	}
	ctx := statelog.WithAnswerBytes(t.Context(), twoRowsOf(t, whole.Rows))

	var got []string
	cursor, pages := "", 0
	for {
		params := map[string]any{"container": "project:ENG"}
		if cursor != "" {
			params["cursor"] = cursor
		}
		page := askWithin(ctx, h, params)
		pages++
		if len(page.Rows) > 2 {
			t.Errorf("page %d holds %d rows, want at most the 2 the bound fits",
				pages, len(page.Rows))
		}
		got = append(got, ids(page)...)
		if cursor = page.NextCursor; cursor == "" {
			break
		}
		if pages > len(whole.Rows) {
			t.Fatal("the cursor never ran out")
		}
	}
	if want := ids(whole); !slices.Equal(got, want) {
		t.Errorf("paging at the bytes read %v, want %v", got, want)
	}
	if pages != 3 {
		t.Errorf("five tasks came back in %d pages, want 3 of at most two", pages)
	}

	board := askWithin(ctx, h, map[string]any{
		"container": "project:ENG", "group_by": "status",
	})
	for _, group := range board.Groups {
		if group.Key != string(worklet.StatusTodo) {
			continue
		}
		if group.Count != 5 || len(group.Rows) != 2 {
			t.Errorf("the todo column holds %d rows of a count of %d, want 2 of 5",
				len(group.Rows), group.Count)
		}
		return
	}
	t.Fatalf("the board has no todo column: %+v", board.Groups)
}
