package tracker_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func groupOf(t *testing.T, answer tracker.Answer, key string) tracker.Group {
	t.Helper()
	for _, group := range answer.Groups {
		if group.Key == key {
			return group
		}
	}
	var keys []string
	for _, group := range answer.Groups {
		keys = append(keys, group.Key)
	}
	t.Fatalf("the answer carries no group %q, only %v", key, keys)
	return tracker.Group{}
}

// seedBoard files tasks across two statuses and two assignees.
func seedBoard(t *testing.T, r *roundTrip) {
	t.Helper()
	for i, spec := range []struct {
		status   tracker.Status
		assignee string
	}{
		{tracker.StatusTodo, "ana"},
		{tracker.StatusTodo, "ana"},
		{tracker.StatusTodo, "bob"},
		{tracker.StatusInProgress, "bob"},
		{tracker.StatusInProgress, ""},
	} {
		task := newTask("t-" + itoa(i))
		task.Status, task.StatusGroup = spec.status, spec.status.Group()
		task.Assignee = spec.assignee
		if _, err := r.writer.CreateTask(t.Context(), "op-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}
}

// A GROUPED ANSWER IS COLUMNS, and each column's count is over the WHOLE set.
//
// Rows with a group label on each would make the count a property of the PAGE:
// a column with four hundred tasks and a fifty-row page would render twelve,
// and nothing in the answer would say the number was of what happened to fit.
func TestAGroupedAnswerCountsTheWholeColumn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedBoard(t, r)

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status",
		"group_limit": 1, "show_closed": "true",
	})
	if len(answer.Rows) != 0 {
		t.Fatalf("a grouped answer also carried %d flat rows — a caller "+
			"rendering that half would draw a board with no columns",
			len(answer.Rows))
	}
	todo := groupOf(t, answer, string(tracker.StatusTodo))
	if todo.Count != 3 {
		t.Fatalf("the todo column counts %d, want 3 over the whole set",
			todo.Count)
	}
	// ONE ROW CARRIED, THREE COUNTED: the count is not len(rows).
	if len(todo.Rows) != 1 {
		t.Fatalf("the todo column carries %d rows, want the 1 asked for",
			len(todo.Rows))
	}
	if got := groupOf(t, answer, string(tracker.StatusInProgress)); got.Count != 2 {
		t.Fatalf("the in-progress column counts %d, want 2", got.Count)
	}
}

// AN ABSENT VALUE IS ITS OWN COLUMN, labelled.
//
// "Nobody is assigned" is a question a board answers rather than a row it
// hides.
func TestAnUnsetValueIsItsOwnColumn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedBoard(t, r)

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "assignee",
		"show_closed": "true",
	})
	unassigned := groupOf(t, answer, "")
	if unassigned.Count != 1 {
		t.Fatalf("the unassigned column counts %d, want 1", unassigned.Count)
	}
	if unassigned.Label == "" {
		t.Fatal("the unassigned column has no label, so a board draws a " +
			"column with no heading")
	}
	if len(unassigned.Rows) != 1 {
		t.Fatalf("the unassigned column carries %d rows, want the 1 task "+
			"nobody is assigned", len(unassigned.Rows))
	}
}

// ONE COLUMN PAGES TO ITS END, AND IT STAYS A COLUMN.
//
// A board carries a bounded slice of each column and mints no cursor, so the
// rows past a column's slice are reached by asking for that column on its own.
// That answer stopped at the same slice with no cursor either, so the rest of a
// long column was reachable nowhere. A single cell has one order, so its answer
// carries a cursor that resumes the column's rows — in the query's own order,
// none skipped and none repeated — and it is still the column a board screen
// draws, its count over the whole column on every page.
func TestOneColumnPagesToItsEndAndStaysAColumn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedBoard(t, r)

	// THE ORDER THE PAGES MUST WALK, from the flat query that names the
	// same rows, so a keyset that skipped or repeated a row cannot pass by
	// happening to reach three of them.
	var want []string
	for _, row := range r.ask(map[string]any{
		"container": "project:ENG", "status": string(tracker.StatusTodo),
		"show_closed": "true",
	}).Rows {
		want = append(want, row.ID)
	}
	if len(want) != 3 {
		t.Fatalf("the fixture's todo column holds %v, want three rows", want)
	}

	var walked []string
	cursor := ""
	for page := 0; ; page++ {
		if page > len(want) {
			t.Fatalf("a %d-row column was still paging after %d pages",
				len(want), page)
		}
		params := map[string]any{
			"container": "project:ENG", "group_by": "status",
			"group": string(tracker.StatusTodo), "show_closed": "true",
			"group_limit": 1,
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		answer := r.ask(params)
		if len(answer.Rows) != 0 || len(answer.Groups) != 1 {
			t.Fatalf("naming one column answered %d flat rows and %d columns, "+
				"want that one column — the shape a board screen draws",
				len(answer.Rows), len(answer.Groups))
		}
		column := answer.Groups[0]
		if column.Key != string(tracker.StatusTodo) || column.Count != 3 {
			t.Fatalf("page %d names column %q counting %d, want todo counting "+
				"all 3 of its rows", page, column.Key, column.Count)
		}
		if answer.TotalHint != 3 {
			t.Fatalf("the column's hint is %d, want its own 3", answer.TotalHint)
		}
		for _, row := range column.Rows {
			if row.Status != tracker.StatusTodo {
				t.Fatalf("the todo column answered %s, which is %s", row.ID,
					row.Status)
			}
			walked = append(walked, row.ID)
		}
		if answer.NextCursor == "" {
			break
		}
		cursor = answer.NextCursor
	}
	if !slices.Equal(walked, want) {
		t.Fatalf("paging the todo column reached %v, want %v — every row of "+
			"it, once each, in the query's own order", walked, want)
	}

	// AND THE TOTALS FOLLOW IT, because `group` narrows the predicate
	// rather than filtering the answer afterwards.
	scoped := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status",
		"group": string(tracker.StatusTodo), "totals": "tasks:count",
		"show_closed": "true",
	})
	if len(scoped.Totals) != 1 || scoped.Totals[0].Value == nil ||
		*scoped.Totals[0].Value != 3 {
		t.Fatalf("the total over one column is %v, want 3", scoped.Totals)
	}
}

// ONE COLUMN ANSWERS THE QUESTION A BOARD SCREEN ASKS OF IT.
//
// The dashboard's list, table and timeline ask for one column with the axis,
// the column's key and a per-column bound together, and render the column that
// comes back. The bound is what a single cell's page is — so it is accepted
// there, and the answer is that column rather than a refusal or a list the
// screen has no way to draw.
func TestOneColumnAnswersTheQuestionABoardScreenAsks(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedBoard(t, r)

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status",
		"group": string(tracker.StatusTodo), "group_limit": tracker.GroupRowsMax,
		"limit": 100, "show_closed": "true",
	})
	if len(answer.Groups) != 1 || answer.Groups[0].Key != string(tracker.StatusTodo) {
		t.Fatalf("the screen's one-column request answered %d columns, want "+
			"the todo column alone", len(answer.Groups))
	}
	if got := answer.Groups[0]; got.Count != 3 || len(got.Rows) != 3 {
		t.Fatalf("the todo column counts %d and carries %d rows, want all 3 "+
			"of each", got.Count, len(got.Rows))
	}
	if answer.NextCursor != "" {
		t.Fatalf("a column whose rows all fit its bound minted the cursor %q",
			answer.NextCursor)
	}
}

// ONE LANE PAGES TOO, narrowed on both axes.
//
// `group=` with `subgroup=` names one cell of a swimlane board. Its column is
// that lane's — the count, the rows, the hint — and the lane inside it carries
// the same rows, so the one cursor pages both.
func TestOneLanePagesNarrowedOnBothAxes(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedBoard(t, r)

	var walked []string
	cursor := ""
	for page := 0; ; page++ {
		if page > 2 {
			t.Fatalf("a two-row lane was still paging after %d pages", page)
		}
		params := map[string]any{
			"container": "project:ENG", "group_by": "status",
			"group_by2": "assignee", "group": string(tracker.StatusTodo),
			"subgroup": "ana", "group_limit": 1, "show_closed": "true",
		}
		if cursor != "" {
			params["cursor"] = cursor
		}
		answer := r.ask(params)
		if len(answer.Groups) != 1 || len(answer.Groups[0].Subgroups) != 1 {
			t.Fatalf("naming one lane answered %d columns, want the one "+
				"holding that lane alone", len(answer.Groups))
		}
		column, lane := answer.Groups[0], answer.Groups[0].Subgroups[0]
		if column.Count != 2 || lane.Key != "ana" || lane.Count != 2 ||
			answer.TotalHint != 2 {
			t.Fatalf("ana's todo lane reads a column of %d, a lane %q of %d and "+
				"a hint of %d — want 2 on all three", column.Count, lane.Key,
				lane.Count, answer.TotalHint)
		}
		if len(column.Rows) != 1 || len(lane.Rows) != 1 ||
			column.Rows[0].ID != lane.Rows[0].ID {
			t.Fatalf("page %d carries column rows %v and lane rows %v, want the "+
				"same one row in each", page, column.Rows, lane.Rows)
		}
		row := lane.Rows[0]
		if row.Status != tracker.StatusTodo || row.Assignee != "ana" {
			t.Fatalf("ana's todo lane answered %s, which is %s and %q",
				row.ID, row.Status, row.Assignee)
		}
		walked = append(walked, row.ID)
		if answer.NextCursor == "" {
			break
		}
		cursor = answer.NextCursor
	}
	if len(walked) != 2 || walked[0] == walked[1] {
		t.Fatalf("paging ana's todo lane reached %v, want both of its rows", walked)
	}
}

// A NAMED COLUMN ON A LABEL AXIS IS ONLY THAT COLUMN — ALONE OR WITH ITS LANES.
//
// The column is named in the query's join-free form, which on a label axis
// says only that a task HAS the label — so grouping what it admitted by the
// joined value drew a column for every other label those tasks carry.
func TestANamedColumnOnALabelAxisIsOnlyThatColumn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.declareTags("api", "ui")
	for i, spec := range []struct {
		tags     []string
		assignee string
	}{
		{[]string{"api", "ui"}, "ana"},
		{[]string{"api"}, "bob"},
		{[]string{"ui"}, "bob"},
	} {
		task := newTask("t-" + itoa(i))
		task.Tags, task.Assignee = spec.tags, spec.assignee
		if _, err := r.writer.CreateTask(t.Context(), "op-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	for name, params := range map[string]map[string]any{
		"alone":          {"container": "project:ENG", "group_by": "tag", "group": "api"},
		"with its lanes": {"container": "project:ENG", "group_by": "tag", "group_by2": "assignee", "group": "api"},
	} {
		t.Run(name, func(t *testing.T) {
			answer := r.ask(params)
			if len(answer.Groups) != 1 || answer.Groups[0].Key != "api" {
				var keys []string
				for _, group := range answer.Groups {
					keys = append(keys, group.Key)
				}
				t.Fatalf("the api column drew columns %v, want api alone", keys)
			}
			if got := answer.Groups[0].Count; got != 2 {
				t.Fatalf("the api column counts %d, want the 2 tasks carrying it", got)
			}
			if _, lanes := params["group_by2"]; lanes {
				if got := len(answer.Groups[0].Subgroups); got != 2 {
					t.Fatalf("the api column has %d lanes, want ana's and bob's", got)
				}
			}
		})
	}
}

// A BOARD OF SEVERAL CELLS REFUSES A CURSOR.
//
// A keyset cursor resumes after one row in one order, and a board of several
// cells is several lists: applied to each, it would cut every column at a row
// from another, and ignored — which the grouped read once did — it handed a
// pager its first page again, for ever, with nothing saying so. One column's
// LANES are several cells too.
func TestABoardOfSeveralCellsRefusesACursor(t *testing.T) {
	t.Parallel()
	for name, params := range map[string]map[string]any{
		"a board": {"group_by": "status", "cursor": "abc"},
		"one column's lanes": {"group_by": "status", "group_by2": "assignee",
			"group": "todo", "cursor": "abc"},
	} {
		t.Run(name, func(t *testing.T) {
			params["container"] = "project:ENG"
			_, err := tracker.ParseQuery(tracker.MapParams(params), wednesday, berlin)
			if err == nil {
				t.Fatalf("%v was accepted", params)
			}
			if !strings.Contains(err.Error(), "no single order to resume after") {
				t.Fatalf("the refusal %q does not say why a board has no cursor", err)
			}
		})
	}
}

// A LABEL BOARD PUTS ONE TASK ON SEVERAL COLUMNS, and the answer SAYS SO.
//
// A task with three labels is on three columns, which is what a label board
// is. What it changes is that the counts no longer sum to the answer's own
// total — so a reader is told rather than left to conclude the answer is
// wrong.
func TestALabelBoardSaysItsColumnsOverlap(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// THE TAGS ARE DECLARED FIRST, because a create refuses a label the
	// project does not have — see internal/tracker/tags.go.
	r.declareTags("urgent", "api")

	task := newTask("t-1")
	task.Tags = []string{"urgent", "api"}
	if _, err := r.writer.CreateTask(t.Context(), "op-1", task, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "tag",
	})
	if !answer.GroupsOverlap {
		t.Fatal("a tag board did not say its columns overlap, so the counts " +
			"read as an answer that does not add up")
	}
	if len(answer.Groups) != 2 {
		t.Fatalf("one task with two labels made %d columns, want 2",
			len(answer.Groups))
	}
	for _, group := range answer.Groups {
		if group.Count != 1 || len(group.Rows) != 1 {
			t.Fatalf("column %s has %d counted and %d carried, want 1 of each",
				group.Key, group.Count, len(group.Rows))
		}
	}
}

// A SECOND AXIS IS SWIMLANES INSIDE EACH COLUMN.
func TestASecondAxisIsSubgroups(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedBoard(t, r)

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status",
		"group_by2": "assignee", "show_closed": "true",
	})
	todo := groupOf(t, answer, string(tracker.StatusTodo))
	if len(todo.Subgroups) != 2 {
		t.Fatalf("the todo column has %d swimlanes, want ana's and bob's",
			len(todo.Subgroups))
	}
	byKey := map[string]int{}
	for _, sub := range todo.Subgroups {
		byKey[sub.Key] = sub.Count
	}
	if byKey["ana"] != 2 || byKey["bob"] != 1 {
		t.Fatalf("the todo column's swimlanes are %v, want ana 2 and bob 1",
			byKey)
	}
}

// A GROUPING NOTHING COULD MEAN IS REFUSED.
func TestAGroupingThatMeansNothingIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for name, tc := range map[string]struct {
		params map[string]any
		want   string
	}{
		"an axis that is not one": {
			map[string]any{"group_by": "colour"}, "not a grouping"},
		"a column with no axis": {
			map[string]any{"group": "todo"}, "without group_by"},
		"a swimlane with no second axis": {
			map[string]any{"group_by": "status", "subgroup": "ana"},
			"without group_by2"},
		"a column bound with no board": {
			map[string]any{"group_limit": 5}, "no group_by was passed"},
		"two axes that are one": {
			map[string]any{"group_by": "status", "group_by2": "status"},
			"same as the first"},
		"a second axis with no first": {
			map[string]any{"group_by2": "status"}, "without group_by"},
		// THE RESOLUTION REFUSES IT FIRST, before the axis is
		// compiled — one refusal for a ref nothing resolved, whether it
		// was a filter, a sort, a total or an axis that named it.
		"a field nothing resolves": {
			map[string]any{"group_by": "f.nonesuch"}, "names no field"},
	} {
		t.Run(name, func(t *testing.T) {
			params := map[string]any{"container": "project:ENG"}
			for key, value := range tc.params {
				params[key] = value
			}
			q, err := tracker.ParseQuery(tracker.MapParams(params), wednesday, berlin)
			if err != nil {
				if !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("the refusal %q does not say %q", err, tc.want)
				}
				return
			}
			q.Level = statelog.ReadStale
			_, err = r.reader.Tasks(t.Context(), q, wednesday)
			if err == nil {
				t.Fatal("a grouping that means nothing was answered")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal %q does not say %q", err, tc.want)
			}
		})
	}
}

// GROUPING ON A CUSTOM FIELD LABELS ITS COLUMNS.
//
// A dropdown's stored value is the option's ID, so a board grouped on one
// would draw columns headed by uuids without the label beside the key.
func TestGroupingOnACustomFieldLabelsItsColumns(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-1", map[string]any{"f-impact": "o-high"})
	seedWithFields(t, r, "t-2", map[string]any{"f-impact": "o-high"})
	seedWithFields(t, r, "t-3", map[string]any{"f-impact": "o-low"})

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "f.impact",
	})
	high := groupOf(t, answer, "o-high")
	if high.Count != 2 {
		t.Fatalf("the high column counts %d, want 2", high.Count)
	}
	if !strings.EqualFold(high.Label, "high") && !strings.EqualFold(high.Label, "High") {
		t.Fatalf("the high column is headed %q — a board grouped on a "+
			"dropdown must not draw a column headed by an option id",
			high.Label)
	}
}

// A COLUMN FILTER NARROWS A JOINED AXIS TOO, in its join-free form.
//
// A tag or a custom field reaches its value through a join, and the count hint
// and the totals carry no join — so an axis expressed only as one would leave a
// header adding up the whole board while the rows showed one column of it.
func TestAColumnFilterOnAJoinedAxisNarrowsTheTotals(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	r.declareTags("api", "ui")

	for id, spec := range map[string]struct {
		impact string
		points float64
		tags   []string
	}{
		"t-1": {"o-high", 5, []string{"api"}},
		"t-2": {"o-high", 3, []string{"api"}},
		"t-3": {"o-low", 100, []string{"ui"}},
	} {
		task := newTask(id)
		task.Points = spec.points
		task.Tags = spec.tags
		task.Fields = map[string]json.RawMessage{
			"f-impact": json.RawMessage(`"` + spec.impact + `"`),
		}
		if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	// A CUSTOM FIELD'S COLUMN.
	byField := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "f.impact",
		"group": "o-high", "totals": "points:sum,tasks:count",
	})
	if len(byField.Groups) != 1 || byField.Groups[0].Count != 2 {
		t.Fatalf("naming one field column answered %v", byField.Groups)
	}
	if got := totalOf(t, byField, "points:sum"); got.Value == nil || *got.Value != 8 {
		t.Fatalf("the total over one field column is %v, want 8 — the third "+
			"task's 100 is on another column", got.Value)
	}
	if byField.TotalHint != 2 {
		t.Fatalf("the hint over one field column is %d, want 2", byField.TotalHint)
	}

	// AND A TAG'S.
	byTag := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "tag",
		"group": "api", "totals": "points:sum",
	})
	if got := totalOf(t, byTag, "points:sum"); got.Value == nil || *got.Value != 8 {
		t.Fatalf("the total over one tag column is %v, want 8", got.Value)
	}
}

// A CLOSED SET'S COLUMNS ARE IN ITS DECLARED ORDER, not by size.
//
// A status board whose columns re-shuffled every time work moved between them
// is a board nobody can learn the shape of — and, worse, one where the cap
// would drop whichever column happened to be smallest that minute.
func TestAClosedSetsColumnsAreInItsDeclaredOrder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// MORE in_progress THAN todo, so a size ordering and a declared one
	// disagree.
	for i, status := range []tracker.Status{
		tracker.StatusTodo,
		tracker.StatusInProgress, tracker.StatusInProgress,
		tracker.StatusInProgress,
	} {
		task := newTask("t-" + itoa(i))
		task.Status, task.StatusGroup = status, status.Group()
		if _, err := r.writer.CreateTask(t.Context(), "op-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status", "show_closed": "true",
	})
	if len(answer.Groups) != 2 {
		t.Fatalf("the board has %d columns, want 2", len(answer.Groups))
	}
	if answer.Groups[0].Key != string(tracker.StatusTodo) {
		t.Fatalf("the first column is %q, want todo — the columns are in the "+
			"order the status set DECLARES, not the order the counts happen "+
			"to fall in", answer.Groups[0].Key)
	}

	// AND A PRIORITY BOARD TOO, whose order is the one a person reads it
	// in rather than the one this week's work produced.
	for i := range 3 {
		task := newTask("p-" + itoa(i))
		task.Priority = tracker.PriorityLow
		if _, err := r.writer.CreateTask(t.Context(), "op-p-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}
	byPriority := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "priority", "show_closed": "true",
	})
	var order []string
	for _, group := range byPriority.Groups {
		order = append(order, group.Key)
	}
	// LOW BEFORE NORMAL, which is the declared order — and the OPPOSITE
	// of the size order, since four tasks are normal and three are low.
	want := []string{string(tracker.PriorityLow), string(tracker.PriorityNormal)}
	if len(order) != len(want) {
		t.Fatalf("the priority columns are %v, want %v", order, want)
	}
	for i, key := range want {
		if order[i] != key {
			t.Fatalf("the priority columns are %v, want %v — the declared "+
				"order, not the one this week's work produced", order, want)
		}
	}
}

// A MULTI-VALUED CUSTOM FIELD IS A LABEL BOARD, and says so.
//
// Pinned to its first value it would show every task under one column and
// declare no overlap — a board that is quietly wrong rather than one that is
// differently shaped.
func TestAMultiValuedFieldBoardPutsATaskOnEveryColumnItChose(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-both", map[string]any{"f-areas": []string{"o-api", "o-ui"}})
	seedWithFields(t, r, "t-one", map[string]any{"f-areas": []string{"o-ui"}})

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "f.areas",
	})
	if !answer.GroupsOverlap {
		t.Fatal("a labels board did not declare its columns overlap")
	}
	if len(answer.Groups) != 2 {
		t.Fatalf("a labels board has %d columns, want 2", len(answer.Groups))
	}
	byKey := map[string]int{}
	for _, group := range answer.Groups {
		byKey[group.Key] = group.Count
	}
	if byKey["o-api"] != 1 || byKey["o-ui"] != 2 {
		t.Fatalf("the columns count %v, want api 1 and ui 2 — a task with two "+
			"values belongs on both", byKey)
	}
}

// A COLUMN HEADING IS THE OPTION'S NAME, every time.
//
// Inverting the two-key option map picked the slug or the name by Go's
// randomised iteration, so one node answered two requests with two headings.
func TestAColumnHeadingIsTheSameOnEveryRequest(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-1", map[string]any{"f-impact": "o-high"})

	want := ""
	for i := range 12 {
		answer := r.ask(map[string]any{
			"container": "project:ENG", "group_by": "f.impact",
		})
		got := groupOf(t, answer, "o-high").Label
		if i == 0 {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("the heading was %q and is now %q — a board's column "+
				"heading must not depend on map iteration", want, got)
		}
	}
	if want != "High" {
		t.Fatalf("the heading is %q, want the option's NAME — a slug is what "+
			"somebody types and a name is what they read", want)
	}
}

// bulkTasks files n minimal rows straight into the replicated estate.
//
// DIRECTLY RATHER THAN THROUGH THE WRITER, because what the gate below is
// about is a corpus of twenty thousand and a broker round trip per task is a
// test nobody would ever run. The rows carry exactly the columns the gate's
// own count reads — it counts and never decodes — so `document` is empty and
// the rows are deliberately not readable as tasks.
func bulkTasks(t *testing.T, r *roundTrip, n int, project string) {
	t.Helper()
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			WITH RECURSIVE n(i) AS (
				SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?
			)
			INSERT INTO tracker_tasks
				(id, key, project_key, root_id, type, title, status,
				 status_group, rank, created_at, updated_at, version, document)
			SELECT 'bulk-' || i, ? || '-' || (100000 + i), ?, 'bulk-' || i,
			       'task', 'bulk ' || i, 'todo', 'not_started', 'a0',
			       1, 1, 1, x''
			FROM n`, n, project, project)
		return err
	}); err != nil {
		t.Fatalf("file %d rows: %v", n, err)
	}
}

// A GROUPING IS REFUSED ON WHAT IT WOULD SORT, never on which keys are present.
//
// The gate this replaces asked for "a narrowing filter — a status_group, an
// assignee, a unit or a date bound", and `status_group=not_started`
// satisfies it while narrowing nothing: every open task is already in it. So
// the case files one row past the ceiling, asks WITH that filter, and expects
// the refusal anyway — which is the whole difference between a gate on a name
// and a gate on a count.
func TestAGroupingIsRefusedOnTheRowsItWouldSortRatherThanOnAKey(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	bulkTasks(t, r, tracker.GroupByRowCeiling+1, "ENG")

	err := r.askErr(map[string]any{
		"container": "workspace", "group_by": "status",
		"status_group": "not_started",
	})
	if !errors.Is(err, tracker.ErrTooBroad) {
		t.Fatalf("a workspace grouping over %d rows answered %v, want "+
			"ErrTooBroad: a filter every open task satisfies narrows nothing, "+
			"and a gate that accepted it is a gate a caller clears in one "+
			"attempt without making the query any cheaper",
			tracker.GroupByRowCeiling+1, err)
	}
	if !strings.Contains(err.Error(), itoa(tracker.GroupByRowCeiling)) {
		t.Fatalf("the refusal is %q and names no ceiling — a caller cannot "+
			"tell how much narrowing is enough", err)
	}

	// AN ABSENT CONTAINER IS NOT A NARROWER ONE. It adds no predicate at
	// all, so it scans exactly what `workspace` scans, and a gate spelled
	// against the literal key would let the identical query straight
	// through.
	if err := r.askErr(map[string]any{"group_by": "status"}); !errors.Is(err, tracker.ErrTooBroad) {
		t.Fatalf("a grouping with NO container answered %v, want ErrTooBroad: "+
			"an omitted container adds no predicate and scans the same rows "+
			"as container=workspace", err)
	}
}

// AND THE GATE DOES NOT RUN AT PROJECT SCOPE, because there the input is an
// index range on `(project_key, status, rank)` whose width is one project's
// own size rather than the company's.
func TestAProjectScopedGroupingIsNotGated(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	bulkTasks(t, r, tracker.GroupByRowCeiling+1, "ENG")
	seedBoard(t, r)

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status", "show_closed": "true",
	})
	if len(answer.Groups) == 0 {
		t.Fatalf("a project-scoped grouping over the same rows drew no columns")
	}
}

// AND A COMPANY UNDER THE CEILING IS SERVED AT WORKSPACE SCOPE, which is what
// stops the case above from passing against a gate that simply refuses every
// grouping that names no project.
func TestAWorkspaceGroupingUnderTheCeilingIsServed(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedBoard(t, r)

	answer := r.ask(map[string]any{
		"container": "workspace", "group_by": "status", "show_closed": "true",
	})
	if len(answer.Groups) == 0 {
		t.Fatalf("a workspace grouping over five tasks drew no columns — the " +
			"breadth gate is refusing on scope rather than on the row count")
	}
}

// A SECOND AXIS IS BOUNDED BY CELLS, not by either axis alone.
//
// One axis is 1 + G statements; a second repeats that pattern inside every
// column, so it is 1 + G × (2 + S). At MaxGroups on both that is 4 225 ordered
// statements with two joins each, inside one read transaction, for one board
// poll — and checkGroupBreadth cannot see it, because that gate measures the
// row count of a single pass and does not run at project scope, which is
// exactly where a swimlane board is opened.
func TestASwimlaneBoardIsBoundedByItsCells(t *testing.T) {
	t.Parallel()
	if tracker.MaxGroupsWithSubgroups*tracker.MaxSubgroups > 256 {
		t.Fatalf("a swimlane board may draw %d cells, which is past the "+
			"budget the caps were derived from",
			tracker.MaxGroupsWithSubgroups*tracker.MaxSubgroups)
	}
	r := newRoundTrip(t)
	seedBoard(t, r)

	// THE COLUMN CAP DROPS when a second axis is asked for, so a board
	// with lanes draws fewer columns than the same board without them.
	// The fixture is small, so what this asserts is that the answer still
	// carries both axes and reports its own overflow rather than that the
	// cap bit here.
	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status",
		"group_by2": "assignee", "show_closed": "true",
	})
	if len(answer.Groups) == 0 {
		t.Fatal("a two-axis board drew no columns")
	}
	var lanes int
	for _, group := range answer.Groups {
		lanes += len(group.Subgroups)
		// THE FLAG AND THE CAP AGREE. A column that reports itself cut
		// must actually be at the bound, and one at the bound with more
		// behind it must report itself cut — the count this replaced
		// could only ever read 1, so nothing here could check it.
		if group.SubgroupsTruncated && len(group.Subgroups) != tracker.MaxSubgroups {
			t.Errorf("column %s says its lanes were cut but carries %d of %d",
				group.Key, len(group.Subgroups), tracker.MaxSubgroups)
		}
	}
	if lanes == 0 {
		t.Fatal("a two-axis board drew no lanes, so the cap assertion above " +
			"is about a shape this reader does not produce")
	}
}

// AN OVERFLOWING BOARD SAYS SO, AND THE FLAG AGREES WITH THE BOUND.
//
// This reported a NUMBER that could only ever be 1. `groupCounts` selects
// `LIMIT limit+1` — one row past the bound, as evidence — and the overflow was
// `len(out) - limit` over that, so a board with two hundred assignee columns
// told its reader "1 more column did not fit". A wrong number stated as a fact
// is worse than the silence the rule was written against, because a reader
// acts on it: the dashboard printed it verbatim and the tool answer carried it
// to a model.
func TestAnOverflowingBoardSaysSoRatherThanCountingWrong(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// One status column with more lanes than MaxSubgroups, which is the
	// cheap way to overflow groupCounts: the same function serves both
	// axes, so the column path is the same assertion at MaxGroups.
	lanes := tracker.MaxSubgroups + 4
	for i := range lanes {
		task := newTask(fmt.Sprintf("g%02d", i))
		task.Status, task.StatusGroup = tracker.StatusTodo, tracker.StatusTodo.Group()
		task.Assignee = fmt.Sprintf("seat%02d", i)
		if _, err := r.writer.CreateTask(t.Context(), "op-g"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status",
		"group_by2": "assignee", "group_limit": 1,
	})
	todo := groupOf(t, answer, string(tracker.StatusTodo))
	if !todo.SubgroupsTruncated {
		t.Fatalf("a column with %d lanes drew %d and reported nothing, so it "+
			"reads as a company with %d people",
			lanes, len(todo.Subgroups), len(todo.Subgroups))
	}
	// THE PAGE STAYS AT THE BOUND: the evidence row is evidence, never an
	// answer. Without this the flag could be right while the board quietly
	// carried one lane more than it is allowed to.
	if len(todo.Subgroups) != tracker.MaxSubgroups {
		t.Errorf("the column carries %d lanes, want the bound of %d",
			len(todo.Subgroups), tracker.MaxSubgroups)
	}

	// AND A BOARD THAT FITS SAYS NOTHING, so the flag is not simply always
	// on — which is the mutation a bool invites where a count did not.
	small := newRoundTrip(t)
	seedBoard(t, small)
	fits := small.ask(map[string]any{
		"container": "project:ENG", "group_by": "status",
		"group_by2": "assignee", "group_limit": 1, "show_closed": "true",
	})
	for _, group := range fits.Groups {
		if group.SubgroupsTruncated {
			t.Errorf("column %s reports itself cut at %d lanes",
				group.Key, len(group.Subgroups))
		}
	}
	if fits.GroupsTruncated {
		t.Error("a five-task board reports its columns cut")
	}
}
