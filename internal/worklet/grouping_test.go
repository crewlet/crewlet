package worklet_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

func groupOf(t *testing.T, answer worklet.Answer, key string) worklet.Group {
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
	return worklet.Group{}
}

// seedBoard files tasks across two statuses and two assignees.
func seedBoard(t *testing.T, r *roundTrip) {
	t.Helper()
	for i, spec := range []struct {
		status   worklet.Status
		assignee string
	}{
		{worklet.StatusTodo, "ana"},
		{worklet.StatusTodo, "ana"},
		{worklet.StatusTodo, "bob"},
		{worklet.StatusInProgress, "bob"},
		{worklet.StatusInProgress, ""},
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

// A GROUPED ANSWER IS COLUMNS, and each column's count is over the WHOLE set:
// rows labelled with a group would make the count a property of the page.
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
	todo := groupOf(t, answer, string(worklet.StatusTodo))
	if todo.Count != 3 {
		t.Fatalf("the todo column counts %d, want 3 over the whole set",
			todo.Count)
	}
	// ONE ROW CARRIED, THREE COUNTED: the count is not len(rows).
	if len(todo.Rows) != 1 {
		t.Fatalf("the todo column carries %d rows, want the 1 asked for",
			len(todo.Rows))
	}
	if got := groupOf(t, answer, string(worklet.StatusInProgress)); got.Count != 2 {
		t.Fatalf("the in-progress column counts %d, want 2", got.Count)
	}
}

// AN ABSENT VALUE IS ITS OWN COLUMN, labelled: "nobody is assigned" is a
// column a board answers, not rows it hides.
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
		"group": string(worklet.StatusTodo), "show_closed": "true",
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
		"group": string(worklet.StatusTodo), "totals": "tasks:count",
		"show_closed": "true",
	})
	if len(scoped.Totals) != 1 || scoped.Totals[0].Value == nil ||
		*scoped.Totals[0].Value != 3 {
		t.Fatalf("the total over one column is %v, want 3", scoped.Totals)
	}
}

// A LABEL BOARD PUTS ONE TASK ON SEVERAL COLUMNS, and the answer SAYS SO, since
// the counts no longer sum to the answer's own total.
func TestALabelBoardSaysItsColumnsOverlap(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// THE TAGS ARE DECLARED FIRST: a create refuses a label the project does not
	// have (internal/worklet/tags.go).
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
	todo := groupOf(t, answer, string(worklet.StatusTodo))
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

// A SWIMLANE ON A CLOSED SET IS IN THE SET'S DECLARED ORDER, for the reason a
// column is ([TestAClosedSetsColumnsAreInItsDeclaredOrder]). Here a column holds
// more in-progress work than to-do work, so a size order and the declared one
// disagree.
func TestASwimlaneOnAClosedSetIsInItsDeclaredOrder(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)
	for i, status := range []worklet.Status{
		worklet.StatusTodo,
		worklet.StatusInProgress, worklet.StatusInProgress,
		worklet.StatusInProgress,
	} {
		h.seed("ana-"+itoa(i), func(task *worklet.Task) {
			task.Assignee = "ana"
			task.Status, task.StatusGroup = status, status.Group()
		})
	}
	answer := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "assignee",
		"group_by2": "status",
	})
	var lanes []string
	for _, lane := range groupOf(t, answer, "ana").Subgroups {
		lanes = append(lanes, lane.Key)
	}
	want := []string{string(worklet.StatusTodo), string(worklet.StatusInProgress)}
	if !equalKeys(lanes, want) {
		t.Fatalf("ana's lanes are %v, want %v — the order the status set "+
			"DECLARES, not the order the counts happen to fall in", lanes, want)
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
		// THE RESOLUTION REFUSES IT FIRST, before the axis compiles: one refusal for
		// an unresolved ref, whether a filter, a sort, a total or an axis named it.
		"a field nothing resolves": {
			map[string]any{"group_by": "f.nonesuch"}, "names no field"},
	} {
		t.Run(name, func(t *testing.T) {
			params := map[string]any{"container": "project:ENG"}
			for key, value := range tc.params {
				params[key] = value
			}
			q, err := worklet.ParseQuery(worklet.MapParams(params), wednesday, berlin)
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

// GROUPING ON A CUSTOM FIELD LABELS ITS COLUMNS: a dropdown stores the
// option's ID, so without the label beside the key a column is headed by a uuid.
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

// A COLUMN FILTER NARROWS A JOINED AXIS TOO, in its join-free form: the count
// hint and the totals carry no join, so a join-only axis would leave the header
// totalling the whole board while the rows showed one column of it.
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

// A CLOSED SET'S COLUMNS ARE IN ITS DECLARED ORDER, not by size: a size order
// reshuffles as work moves, and the cap would drop whichever column was
// smallest that minute.
func TestAClosedSetsColumnsAreInItsDeclaredOrder(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// MORE in_progress THAN todo, so a size order and the declared one disagree.
	for i, status := range []worklet.Status{
		worklet.StatusTodo,
		worklet.StatusInProgress, worklet.StatusInProgress,
		worklet.StatusInProgress,
	} {
		task := newTask("t-" + itoa(i))
		task.Status, task.StatusGroup = status, status.Group()
		if _, err := r.writer.CreateTask(t.Context(), "op-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	// EVERY STATUS IS A COLUMN under show_closed, the four unused ones at count 0
	// (TestAClosedAxisDrawsEveryColumnTheQueryAdmits), in declared order: the
	// smaller todo column ahead of the larger in_progress one.
	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status", "show_closed": "true",
	})
	if got, want := columnKeys(answer), declared(worklet.Statuses); !equalKeys(got, want) {
		t.Fatalf("the board has columns %v, want %v", got, want)
	}
	if answer.Groups[0].Key != string(worklet.StatusTodo) {
		t.Fatalf("the first column is %q, want todo — the columns are in the "+
			"order the status set DECLARES, not the order the counts happen "+
			"to fall in", answer.Groups[0].Key)
	}
	if got, want := columnCounts(answer), []int{1, 3, 0, 0, 0, 0}; !equalCounts(got, want) {
		t.Fatalf("the board counts %v, want %v", got, want)
	}

	// AND A PRIORITY BOARD TOO, in the order a person reads it.
	for i := range 3 {
		task := newTask("p-" + itoa(i))
		task.Priority = worklet.PriorityLow
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
	// ALL FIVE, LOW BEFORE NORMAL as declared — the OPPOSITE of size order (four
	// normal, three low) — and the three unused priorities as columns at zero: a
	// priority board is the shape of the scale.
	want := declared(worklet.Priorities)
	if !equalKeys(order, want) {
		t.Fatalf("the priority columns are %v, want %v — the declared "+
			"order, not the one this week's work produced", order, want)
	}
	if got, wantCounts := columnCounts(byPriority), []int{0, 3, 4, 0, 0}; !equalCounts(got, wantCounts) {
		t.Fatalf("the priority counts are %v, want %v", got, wantCounts)
	}
}

// A BOARD IS THE SHAPE OF THE PROCESS, NOT OF THIS WEEK'S ROWS: its columns are
// the values the query admits, in declared order, empty ones included, so an
// empty In review is drawn rather than missing. And ONLY those: an open-work
// board draws no Done lane, since "nothing is done" about a set never asked is
// a claim rather than an absence.
func TestAClosedAxisDrawsEveryColumnTheQueryAdmits(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	ask := func(kv map[string]any) worklet.Answer {
		kv["container"] = "project:ENG"
		return r.ask(kv)
	}
	expect := func(what string, answer worklet.Answer, keys []string, counts []int) {
		t.Helper()
		if got := columnKeys(answer); !equalKeys(got, keys) {
			t.Fatalf("%s: the columns are %v, want %v", what, got, keys)
		}
		if got := columnCounts(answer); !equalCounts(got, counts) {
			t.Fatalf("%s: the counts are %v, want %v", what, got, counts)
		}
	}

	// THE DEFAULT SCOPE IS OPEN WORK: the three open statuses, not the finished.
	open := ask(map[string]any{"group_by": "status"})
	expect("open work", open, []string{"todo", "in_progress", "in_review"}, []int{1, 0, 0})
	// AN EMPTY COLUMN CARRIES AN EMPTY LIST ON THE WIRE, never null: every
	// renderer maps a column's rows.
	raw, err := json.Marshal(open.Groups[1])
	if err != nil {
		t.Fatalf("marshal a column: %v", err)
	}
	if !strings.Contains(string(raw), `"rows":[]`) {
		t.Fatalf("an empty column reached the wire as %s, want \"rows\":[]", raw)
	}

	// THE FILTERS THAT NARROW THE PREDICATE NARROW THE BOARD: a group, a negated
	// status, a named status.
	expect("active only", ask(map[string]any{"group_by": "status", "status_group": "active"}),
		[]string{"in_progress", "in_review"}, []int{0, 0})
	expect("not todo", ask(map[string]any{"group_by": "status", "status": "!todo"}),
		[]string{"in_progress", "in_review"}, []int{0, 0})
	expect("todo named", ask(map[string]any{"group_by": "status", "status": "todo"}),
		[]string{"todo"}, []int{1})
	// ASKING FOR FINISHED WORK ADMITS ITS THREE STATUSES.
	expect("everything", ask(map[string]any{"group_by": "status", "show_closed": "true"}),
		declared(worklet.Statuses), []int{1, 0, 0, 0, 0, 0})
	// AND THE OVERDUE ALIAS TAKES THEM OFF AGAIN, whatever show_closed said.
	expect("overdue", ask(map[string]any{
		"group_by": "status", "show_closed": "true", "due": "overdue",
	}), []string{"todo", "in_progress", "in_review"}, []int{0, 0, 0})
	// ONE COLUMN ASKED FOR IS ONE COLUMN ANSWERED, even an empty one: it is the
	// column a reader followed "N more →" into.
	expect("one column", ask(map[string]any{"group_by": "status", "group": "in_review"}),
		[]string{"in_review"}, []int{0})
	// THE GROUP AXIS FOLLOWS THE SAME ADMISSION.
	expect("by group", ask(map[string]any{"group_by": "status_group"}),
		[]string{"not_started", "active"}, []int{1, 0})
	// A PRIORITY BOARD CARRIES THE WHOLE SCALE; a priority filter narrows it.
	expect("by priority", ask(map[string]any{"group_by": "priority"}),
		declared(worklet.Priorities), []int{0, 0, 1, 0, 0})
	expect("two priorities", ask(map[string]any{"group_by": "priority", "priority": "high,urgent"}),
		[]string{"high", "urgent"}, []int{0, 0})
	// AN OPEN AXIS IS LEFT AS IT WAS, the values present and no more: every seat
	// as an empty column is a roster, not a board.
	expect("by assignee", ask(map[string]any{"group_by": "assignee"}), []string{""}, []int{1})
}

func declared[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func columnKeys(answer worklet.Answer) []string {
	out := make([]string, 0, len(answer.Groups))
	for _, group := range answer.Groups {
		out = append(out, group.Key)
	}
	return out
}

func columnCounts(answer worklet.Answer) []int {
	out := make([]int, 0, len(answer.Groups))
	for _, group := range answer.Groups {
		out = append(out, group.Count)
	}
	return out
}

func equalKeys(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func equalCounts(got, want []int) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A MULTI-VALUED CUSTOM FIELD IS A LABEL BOARD, and says so: pinned to its
// first value it would put each task in one column and declare no overlap.
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

// A COLUMN HEADING IS THE OPTION'S NAME, every time: the option map is keyed by
// both slug and name, so inverting it picks either by Go's map iteration.
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

// bulkTasks files n minimal rows straight into the replicated estate: the gate
// below needs twenty thousand, too many for a broker round trip each. The rows
// carry only the columns the gate counts (it never decodes), so `document` is
// empty and they are deliberately not readable as tasks.
func bulkTasks(t *testing.T, r *roundTrip, n int, project string) {
	t.Helper()
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
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

// A GROUPING IS REFUSED ON WHAT IT WOULD SORT, never on which keys are present:
// `status_group=not_started` narrows nothing (every open task is in it), so one
// row past the ceiling asked WITH it is refused all the same.
func TestAGroupingIsRefusedOnTheRowsItWouldSortRatherThanOnAKey(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	bulkTasks(t, r, worklet.GroupByRowCeiling+1, "ENG")

	err := r.askErr(map[string]any{
		"container": "workspace", "group_by": "status",
		"status_group": "not_started",
	})
	if !errors.Is(err, worklet.ErrTooBroad) {
		t.Fatalf("a workspace grouping over %d rows answered %v, want "+
			"ErrTooBroad: a filter every open task satisfies narrows nothing, "+
			"and a gate that accepted it is a gate a caller clears in one "+
			"attempt without making the query any cheaper",
			worklet.GroupByRowCeiling+1, err)
	}
	if !strings.Contains(err.Error(), itoa(worklet.GroupByRowCeiling)) {
		t.Fatalf("the refusal is %q and names no ceiling — a caller cannot "+
			"tell how much narrowing is enough", err)
	}

	// AN ABSENT CONTAINER IS NOT A NARROWER ONE: it adds no predicate, so it scans
	// what `workspace` scans, and a gate keyed on the literal would let it through.
	if err := r.askErr(map[string]any{"group_by": "status"}); !errors.Is(err, worklet.ErrTooBroad) {
		t.Fatalf("a grouping with NO container answered %v, want ErrTooBroad: "+
			"an omitted container adds no predicate and scans the same rows "+
			"as container=workspace", err)
	}
}

// AND THE GATE DOES NOT RUN AT PROJECT SCOPE: there the input is an index range
// on `(project_key, status, rank)`, one project's size, not the company's.
func TestAProjectScopedGroupingIsNotGated(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	bulkTasks(t, r, worklet.GroupByRowCeiling+1, "ENG")
	seedBoard(t, r)

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status", "show_closed": "true",
	})
	if len(answer.Groups) == 0 {
		t.Fatalf("a project-scoped grouping over the same rows drew no columns")
	}
}

// AND A COMPANY UNDER THE CEILING IS SERVED AT WORKSPACE SCOPE, so the case
// above cannot pass against a gate refusing every grouping that names no
// project.
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

// A SECOND AXIS IS BOUNDED BY CELLS, not by either axis alone: one axis is
// 1 + G statements, two are 1 + G × (2 + S) — 4 225 at MaxGroups on both, in
// one read transaction per poll. checkGroupBreadth cannot see it: it measures
// one pass's rows and does not run at project scope, where swimlanes open.
func TestASwimlaneBoardIsBoundedByItsCells(t *testing.T) {
	t.Parallel()
	if worklet.MaxGroupsWithSubgroups*worklet.MaxSubgroups > 256 {
		t.Fatalf("a swimlane board may draw %d cells, which is past the "+
			"budget the caps were derived from",
			worklet.MaxGroupsWithSubgroups*worklet.MaxSubgroups)
	}
	r := newRoundTrip(t)
	seedBoard(t, r)

	// THE COLUMN CAP DROPS when a second axis is asked for. The fixture is too
	// small for the cap to bite, so this asserts the answer carries both axes and
	// reports its own overflow.
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
		if group.SubgroupsDropped < 0 {
			t.Errorf("column %s reports %d dropped lanes",
				group.Key, group.SubgroupsDropped)
		}
	}
	if lanes == 0 {
		t.Fatal("a two-axis board drew no lanes, so the cap assertion above " +
			"is about a shape this reader does not produce")
	}
}

// ---- the relative due bands ------------------------------------------- //

// berlinAt is a wall-clock instant in the COMPANY's own zone, spelled as a
// calendar rather than resolved through [period.Resolve], so a case pinning
// where a band cuts does not move with the resolver. `wednesday` is 16:30 on
// Wednesday 2031-04-16 in Berlin; the Monday-anchored week runs 14–20 April.
func berlinAt(t *testing.T, day, clock string) time.Time {
	t.Helper()
	at, err := time.ParseInLocation("2006-01-02 15:04:05.999999",
		day+" "+clock, berlin)
	if err != nil {
		t.Fatalf("parse %s %s: %v", day, clock, err)
	}
	return at.UTC()
}

// seedDue files one task with a due date and a status group.
func seedDue(h *readHarness, id string, due *time.Time, group worklet.StatusGroup) {
	h.t.Helper()
	h.seed(id, func(task *worklet.Task) {
		task.DueAt = due
		task.StatusGroup = group
		if group == worklet.GroupDone {
			task.Status = worklet.StatusDone
		}
	})
}

// bandOf is the band one task landed in, read back off the answer's columns.
func bandOf(t *testing.T, answer worklet.Answer, id string) (string, bool) {
	t.Helper()
	for _, group := range answer.Groups {
		for _, row := range group.Rows {
			if row.ID == id {
				return group.Key, true
			}
		}
	}
	return "", false
}

// THE BANDS CUT ON THE QUERY'S OWN DAY, AND ON ITS OWN WEEK: the company's day
// start, as the row's `overdue` flag and every `due=` filter are, never the
// browser's midnight or Monday. Every boundary is in Berlin wall-clock, the
// suite's company zone, so a band cut at UTC midnight or over a rolling seven
// days fails here.
func TestTheDueBandsCutOnTheCompanysOwnCalendar(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	// ONE TABLE, SEEDED AND THEN ASSERTED, so an expectation cannot drift from its
	// row.
	cases := []struct {
		id    string
		day   string
		clock string
		group worklet.StatusGroup
		want  string
	}{
		// THE LAST INSTANT BEFORE THE DAY START, twice: the missed band also asks
		// whether the work is still open.
		{"open-yesterday", "2031-04-15", "23:59:59.999999",
			worklet.GroupActive, "overdue"},
		{"done-yesterday", "2031-04-15", "23:59:59.999999",
			worklet.GroupDone, "earlier"},
		// THE DAY START ITSELF is today, not yesterday.
		{"day-start", "2031-04-16", "00:00:00", worklet.GroupNotStarted, "today"},
		// AND THE LAST SECOND OF THE SAME DAY is still today, which a browser cutting
		// on its own midnight gets wrong first.
		{"day-end", "2031-04-16", "23:59:59", worklet.GroupNotStarted, "today"},
		// THE FIRST INSTANT AFTER IT is this week.
		{"tomorrow", "2031-04-17", "00:00:00", worklet.GroupNotStarted, "this_week"},
		// THE LAST INSTANT OF THE WEEK is still this week: the Monday-anchored week
		// ends with Sunday the 20th.
		{"week-end", "2031-04-20", "23:59:59.999999",
			worklet.GroupNotStarted, "this_week"},
		// AND THE FIRST INSTANT AFTER IT is later.
		{"next-monday", "2031-04-21", "00:00:00", worklet.GroupNotStarted, "later"},
		// A FINISHED TASK IS NOT AUTOMATICALLY `earlier`: that band is work past its
		// date, and this one's has not come.
		{"done-ahead", "2031-04-18", "09:00:00", worklet.GroupDone, "this_week"},
		// AND ONE WITH NO DATE: a column, not a row the board hides.
		{"undated", "", "", worklet.GroupNotStarted, ""},
	}
	for _, tc := range cases {
		if tc.day == "" {
			seedDue(h, tc.id, nil, tc.group)
			continue
		}
		due := berlinAt(t, tc.day, tc.clock)
		seedDue(h, tc.id, &due, tc.group)
	}

	answer := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "due:bucket",
		// THE FINISHED WORK HAS TO BE IN THE ANSWER for `earlier` to be reachable: an
		// open-only read leaves that band empty by construction.
		"show_closed": "true",
	})

	for _, tc := range cases {
		got, held := bandOf(t, answer, tc.id)
		if !held {
			t.Errorf("task %s is in no band at all", tc.id)
			continue
		}
		if got != tc.want {
			t.Errorf("task %s bands as %q, want %q — the bands, the "+
				"overdue flag and every due= filter are cut on one "+
				"boundary or a screen showing two of them disagrees",
				tc.id, got, tc.want)
		}
	}

	// THE UNDATED COLUMN IS LABELLED: a board heads it, and "" is no heading.
	undated := groupOf(t, answer, "")
	if undated.Label != "No due date" {
		t.Errorf("the undated band reads %q, want the sixth heading",
			undated.Label)
	}
}

// AND THE OVERDUE BAND IS THE ROW'S OWN OVERDUE FLAG: open AND past its date,
// as [worklet.TaskRow.Overdue] and `due=overdue` mean, from the same instant. A
// band on the date alone would file late-delivered work as still owed.
func TestTheOverdueBandIsTheRowsOverdueFlag(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	yesterday := berlinAt(t, "2031-04-15", "09:00:00")
	seedDue(h, "open-late", &yesterday, worklet.GroupActive)
	seedDue(h, "done-late", &yesterday, worklet.GroupDone)

	answer := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "due:bucket",
		"show_closed": "true",
	})
	for _, group := range answer.Groups {
		for _, row := range group.Rows {
			if wantOverdue := group.Key == "overdue"; row.Overdue != wantOverdue {
				t.Errorf("task %s bands as %q and carries overdue=%v — the "+
					"band and the flag are one predicate or a row "+
					"contradicts its own heading",
					row.ID, group.Key, row.Overdue)
			}
		}
	}
	if _, held := bandOf(t, answer, "done-late"); !held {
		t.Fatal("the finished task is in no band, so this case would pass " +
			"against a reader that never returned it")
	}
	// AND AN OPEN-ONLY READ HAS NO `earlier` BAND: nothing in it is finished.
	open := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "due:bucket",
	})
	for _, group := range open.Groups {
		if group.Key == "earlier" {
			t.Errorf("an open-only answer drew an Earlier band holding %d "+
				"tasks, and nothing open can be in one", group.Count)
		}
	}
}

// A BAND IS A COLUMN, so `group=` narrows to it — including the hint: the count
// hint and the totals share the compiled predicate and carry no join, so an
// axis needing a join would leave the header totalling the whole board.
func TestADueBandNarrowsTheWholeQuery(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	today := berlinAt(t, "2031-04-16", "08:00:00")
	alsoToday := berlinAt(t, "2031-04-16", "23:59:59")
	tomorrow := berlinAt(t, "2031-04-17", "10:00:00")
	yesterday := berlinAt(t, "2031-04-15", "10:00:00")
	seedDue(h, "today-a", &today, worklet.GroupNotStarted)
	seedDue(h, "today-b", &alsoToday, worklet.GroupActive)
	seedDue(h, "this-week", &tomorrow, worklet.GroupNotStarted)
	seedDue(h, "overdue", &yesterday, worklet.GroupActive)
	seedDue(h, "undated", nil, worklet.GroupNotStarted)

	answer := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "due:bucket",
		"group": "today",
	})
	if len(answer.Groups) != 1 || answer.Groups[0].Key != "today" {
		var keys []string
		for _, group := range answer.Groups {
			keys = append(keys, group.Key)
		}
		t.Fatalf("group=today drew the columns %v, want only the one asked "+
			"for", keys)
	}
	band := answer.Groups[0]
	got := map[string]bool{}
	for _, row := range band.Rows {
		got[row.ID] = true
	}
	if len(got) != 2 || !got["today-a"] || !got["today-b"] {
		t.Errorf("group=today carries %v, want exactly the two tasks due "+
			"today", got)
	}
	if band.Count != 2 {
		t.Errorf("the today column counts %d, want 2", band.Count)
	}
	// THE HINT IS THE NARROWED SET'S, so a single-column view's header describes
	// that column.
	if answer.TotalHint != 2 {
		t.Errorf("group=today reports a total hint of %d over the whole "+
			"query, want the 2 rows the column holds — the hint and the "+
			"column disagree, so the axis is not narrowing the shared "+
			"predicate", answer.TotalHint)
	}
	// AND THE UNDATED BAND NARROWS BY THE SPELLING every axis's absent value takes.
	undated := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "due:bucket",
		"group": "",
	})
	if undated.TotalHint != 1 {
		t.Errorf("group= (the undated band) reports a hint of %d, want the "+
			"1 task with no due date", undated.TotalHint)
	}
}

// THE BANDS HOLD THEIR DECLARED ORDER (Overdue, Earlier, Today, This week,
// Later, No due date), not size, so the fixture makes the LAST drawn band the
// biggest and a size-ordered axis fails here.
//
// AND THE AXIS IS A CLOSED SET, so it pads: nothing is due this week and that
// lane is drawn at nought. `earlier` alone is not drawn, by the admission rule:
// it holds work finished late, and this answer carries open work only.
func TestTheDueBandsKeepTheirDeclaredOrder(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	yesterday := berlinAt(t, "2031-04-15", "10:00:00")
	today := berlinAt(t, "2031-04-16", "10:00:00")
	nextMonth := berlinAt(t, "2031-05-20", "10:00:00")
	seedDue(h, "overdue", &yesterday, worklet.GroupActive)
	seedDue(h, "today", &today, worklet.GroupNotStarted)
	for _, id := range []string{"later-a", "later-b", "later-c"} {
		seedDue(h, id, &nextMonth, worklet.GroupNotStarted)
	}
	seedDue(h, "undated", nil, worklet.GroupNotStarted)

	answer := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "due:bucket",
	})
	var drawn []string
	for _, group := range answer.Groups {
		drawn = append(drawn, group.Key)
	}
	// FIVE OF THE SIX, IN THE AXIS'S OWN ORDER: `this_week` empty among them,
	// `earlier` absent because no finished status is admitted.
	want := []struct {
		key   string
		count int
	}{
		{"overdue", 1}, {"today", 1}, {"this_week", 0}, {"later", 3}, {"", 1},
	}
	if len(drawn) != len(want) {
		t.Fatalf("the answer drew the bands %v, want %d of them", drawn, len(want))
	}
	for i, band := range want {
		if drawn[i] != band.key {
			t.Fatalf("the answer drew the bands %v, want %v — the declared "+
				"order is the axis's own, and the biggest column is last "+
				"among the ones holding work precisely so a size-ordered "+
				"one fails", drawn, want)
		}
		got := answer.Groups[i]
		if got.Count != band.count {
			t.Errorf("the %q band counts %d, want %d", band.key, got.Count, band.count)
		}
		// AN EMPTY BAND CARRIES AN EMPTY LIST, never null: a renderer mapping a
		// column's rows would fail on exactly the padded column.
		if got.Rows == nil {
			t.Errorf("the %q band carries no row list at all", band.key)
		}
	}
	// AND EACH HEADING IS THE WORD A PERSON READS, not the stored slug.
	for _, want := range []struct{ key, label string }{
		{"overdue", "Overdue"}, {"today", "Today"},
		// THE PADDED ONE TOO: a lane minted from a bare key is headed from the same
		// table as the counted ones.
		{"this_week", "This week"},
		{"later", "Later"}, {"", "No due date"},
	} {
		if got := groupOf(t, answer, want.key).Label; got != want.label {
			t.Errorf("the %q band reads %q, want %q", want.key, got, want.label)
		}
	}
}

// AND THE BANDS SURVIVE BEING THE INNER AXIS OF A SWIMLANE BOARD: theirs is the
// one axis expression with values of its own, and placeholders bind in textual
// order. As a lane it sits in a SELECT already carrying the outer axis's join
// and predicate, so binding its values with the join's would cut the bands on
// whatever number came next.
func TestTheDueBandsAreAnInnerAxisToo(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	yesterday := berlinAt(t, "2031-04-15", "10:00:00")
	today := berlinAt(t, "2031-04-16", "10:00:00")
	h.seed("ana-overdue", func(task *worklet.Task) {
		task.DueAt, task.Assignee = &yesterday, "ana"
		task.StatusGroup = worklet.GroupActive
	})
	h.seed("ana-today", func(task *worklet.Task) {
		task.DueAt, task.Assignee = &today, "ana"
	})
	h.seed("bob-today", func(task *worklet.Task) {
		task.DueAt, task.Assignee = &today, "bob"
	})

	answer := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "assignee",
		"group_by2": "due:bucket",
	})
	ana := groupOf(t, answer, "ana")
	var lanes []string
	for _, lane := range ana.Subgroups {
		lanes = append(lanes, lane.Key)
	}
	if len(lanes) != 2 || lanes[0] != "overdue" || lanes[1] != "today" {
		t.Fatalf("ana's lanes are %v, want overdue then today", lanes)
	}
	for _, lane := range ana.Subgroups {
		if len(lane.Rows) != 1 {
			t.Errorf("ana's %q lane carries %d rows, want 1",
				lane.Key, len(lane.Rows))
		}
	}
	bob := groupOf(t, answer, "bob")
	if len(bob.Subgroups) != 1 || bob.Subgroups[0].Key != "today" {
		t.Errorf("bob's lanes are %v, want only today", bob.Subgroups)
	}
}

// THE ABSENT-VALUE COLUMN LOADS LIKE EVERY OTHER ONE: `group=<value>` is the
// only way to load a column further (a grouped answer mints no cursor), so a
// bare `group=` asks for the no-value column, never the whole board — the
// line [worklet.Params] draws: "a filter set to empty asks for rows with no
// value".
func TestTheAbsentValueColumnLoadsLikeEveryOther(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	for _, spec := range []struct{ id, assignee string }{
		{"ana-1", "ana"}, {"ana-2", "ana"},
		{"nobody-1", ""}, {"nobody-2", ""},
	} {
		h.seed(spec.id, func(task *worklet.Task) { task.Assignee = spec.assignee })
	}

	answer := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "assignee", "group": "",
	})
	if len(answer.Groups) != 1 || answer.Groups[0].Key != "" {
		var keys []string
		for _, group := range answer.Groups {
			keys = append(keys, group.Key)
		}
		t.Fatalf("group= drew the columns %v, want only the unassigned one — "+
			"a named-but-empty group is a request for the column with no "+
			"value, not an absent filter", keys)
	}
	if got := answer.Groups[0].Count; got != 2 {
		t.Errorf("the unassigned column counts %d, want the 2 tasks nobody "+
			"holds", got)
	}
	if answer.TotalHint != 2 {
		t.Errorf("group= reports a hint of %d over the whole query, want 2 — "+
			"the hint shares the narrowed predicate or a header adds up the "+
			"whole board while the rows show one column of it", answer.TotalHint)
	}
	// AND AN ABSENT `group` IS STILL THE WHOLE BOARD.
	whole := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "assignee",
	})
	if len(whole.Groups) != 2 {
		t.Errorf("an absent group drew %d columns, want both", len(whole.Groups))
	}
}

// AND A BARE `group=` WITH NO AXIS IS STILL REFUSED: a column filter with no
// board would silently answer everything, empty or named.
func TestAColumnFilterWithNoAxisIsRefusedEmptyToo(t *testing.T) {
	t.Parallel()
	_, err := worklet.ParseQuery(worklet.MapParams(map[string]any{
		"container": "project:ENG", "group": "",
	}), wednesday, berlin)
	if err == nil {
		t.Fatal("group= with no group_by was accepted, so a request for one " +
			"column answers the whole set")
	}
	if !strings.Contains(err.Error(), "group_by") {
		t.Errorf("the refusal is %q and does not name the key that is "+
			"missing", err)
	}
}

// TWO CUSTOM FIELDS MAKE A SWIMLANE BOARD: a lane is read in one statement with
// its column's join, so the lane axis joins `tracker_field_values` under its
// own alias; one alias for both is a statement the engine refuses.
func TestTwoFieldAxesDrawASwimlaneBoard(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-1", map[string]any{
		"f-impact": "o-high", "f-areas": []string{"o-api"}})
	seedWithFields(t, r, "t-2", map[string]any{
		"f-impact": "o-high", "f-areas": []string{"o-ui"}})
	seedWithFields(t, r, "t-3", map[string]any{
		"f-impact": "o-low", "f-areas": []string{"o-ui"}})

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "f.impact",
		"group_by2": "f.areas",
	})
	high := groupOf(t, answer, "o-high")
	lanes := map[string]int{}
	for _, lane := range high.Subgroups {
		lanes[lane.Key] = lane.Count
	}
	if lanes["o-api"] != 1 || lanes["o-ui"] != 1 || len(lanes) != 2 {
		t.Fatalf("the high column's lanes are %v, want one task each in api "+
			"and ui", lanes)
	}
}

// A PARENT COLUMN IS HEADED BY ITS PARENT, never by the uuid it is keyed on:
// the parent's key and title, read off its own row, which a filter admitting
// only subtasks still names.
func TestAParentColumnIsHeadedByItsParent(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	for i, spec := range []struct{ id, key, title string }{
		{"epic", "ENG-1", "Rotate the build keys"},
		{"sub-a", "ENG-2", "Revoke the old key"},
		{"sub-b", "ENG-3", "Mint the new key"},
		{"lone", "ENG-4", "Retry PXE boot"},
	} {
		task := newTask(spec.id)
		task.Key, task.Title = spec.key, spec.title
		if _, err := r.writer.CreateTask(t.Context(), "op-create-"+itoa(i), task, nil); err != nil {
			t.Fatalf("CreateTask %s: %v", spec.id, err)
		}
		r.drain()
	}
	epic := "epic"
	for _, id := range []string{"sub-a", "sub-b"} {
		if _, err := r.writer.UpdateTask(t.Context(), "op-adopt-"+id, id, "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Parent: &epic},
			worklet.ChangeReparented, nil); err != nil {
			t.Fatalf("adopt %s: %v", id, err)
		}
		r.drain()
	}

	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "parent", "subtasks": "separate",
		// ONLY THE SUBTASKS: the parent's row is not in the answer, yet heads its
		// column.
		"has_parent": "true",
	})
	column := groupOf(t, answer, "epic")
	if column.Label != "ENG-1 · Rotate the build keys" {
		t.Fatalf("the epic's column is headed %q, want its key and title",
			column.Label)
	}
	if column.Count != 2 {
		t.Fatalf("the epic's column counts %d, want its 2 subtasks", column.Count)
	}

	// AND AS THE SECOND AXIS: a band of a band is headed the same way.
	answer = r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status", "group_by2": "parent",
		"subtasks": "separate",
	})
	todo := groupOf(t, answer, string(worklet.StatusTodo))
	var headed bool
	for _, lane := range todo.Subgroups {
		if lane.Key == "epic" {
			headed = lane.Label == "ENG-1 · Rotate the build keys"
		}
	}
	if !headed {
		t.Fatalf("the epic's lane is not headed by its key and title: %+v", todo.Subgroups)
	}
}
