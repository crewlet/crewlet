package tracker_test

import (
	"encoding/json"
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

// A COLUMN FILTER NARROWS THE WHOLE QUERY, which is how a board loads one
// column further — and why a grouped answer mints no cursor.
func TestAColumnFilterNarrowsToThatColumn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedBoard(t, r)

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status",
		"group": string(tracker.StatusTodo), "show_closed": "true",
	})
	if len(answer.Groups) != 1 {
		t.Fatalf("naming one column answered %d of them", len(answer.Groups))
	}
	if answer.Groups[0].Count != 3 {
		t.Fatalf("the named column counts %d, want 3", answer.Groups[0].Count)
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

// A LABEL BOARD PUTS ONE TASK ON SEVERAL COLUMNS, and the answer SAYS SO.
//
// A task with three labels is on three columns, which is what a label board
// is. What it changes is that the counts no longer sum to the answer's own
// total — so a reader is told rather than left to conclude the answer is
// wrong.
func TestALabelBoardSaysItsColumnsOverlap(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

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
