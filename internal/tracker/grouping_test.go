package tracker_test

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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

	// EVERY STATUS IS A COLUMN under show_closed, the four nothing was
	// filed under at count 0 — see TestAClosedAxisDrawsEveryColumnTheQueryAdmits
	// — and the order is the declared one, which puts the smaller todo
	// column ahead of the larger in_progress one.
	answer := r.ask(map[string]any{
		"container": "project:ENG", "group_by": "status", "show_closed": "true",
	})
	if got, want := columnKeys(answer), declared(tracker.Statuses); !equalKeys(got, want) {
		t.Fatalf("the board has columns %v, want %v", got, want)
	}
	if answer.Groups[0].Key != string(tracker.StatusTodo) {
		t.Fatalf("the first column is %q, want todo — the columns are in the "+
			"order the status set DECLARES, not the order the counts happen "+
			"to fall in", answer.Groups[0].Key)
	}
	if got, want := columnCounts(answer), []int{1, 3, 0, 0, 0, 0}; !equalCounts(got, want) {
		t.Fatalf("the board counts %v, want %v", got, want)
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
	// ALL FIVE, LOW BEFORE NORMAL, which is the declared order — and the
	// OPPOSITE of the size order, since four tasks are normal and three are
	// low. The three priorities nothing was filed under are columns at zero,
	// because a priority board is the shape of the scale rather than of this
	// week's rows.
	want := declared(tracker.Priorities)
	if !equalKeys(order, want) {
		t.Fatalf("the priority columns are %v, want %v — the declared "+
			"order, not the one this week's work produced", order, want)
	}
	if got, wantCounts := columnCounts(byPriority), []int{0, 3, 4, 0, 0}; !equalCounts(got, wantCounts) {
		t.Fatalf("the priority counts are %v, want %v", got, wantCounts)
	}
}

// A BOARD IS THE SHAPE OF THE PROCESS, NOT OF THIS WEEK'S ROWS.
//
// A GROUP BY emits a column per value present, so a company with one task got
// a board with one lane, and nothing on it said whether In review was empty or
// missing. The columns are the values the query itself admits, in the declared
// order, empty ones included — and ONLY those: an open-work board must not
// draw a Done lane the query excluded, because "nothing is done" said about a
// set that was never asked is a claim rather than an absence.
func TestAClosedAxisDrawsEveryColumnTheQueryAdmits(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.CreateTask(t.Context(), "op-1", newTask("t-1"), nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()
	ask := func(kv map[string]any) tracker.Answer {
		kv["container"] = "project:ENG"
		return r.ask(kv)
	}
	expect := func(what string, answer tracker.Answer, keys []string, counts []int) {
		t.Helper()
		if got := columnKeys(answer); !equalKeys(got, keys) {
			t.Fatalf("%s: the columns are %v, want %v", what, got, keys)
		}
		if got := columnCounts(answer); !equalCounts(got, counts) {
			t.Fatalf("%s: the counts are %v, want %v", what, got, counts)
		}
	}

	// THE DEFAULT SCOPE IS OPEN WORK, so the three open statuses are the
	// board and the three finished ones are not on it.
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

	// THE FILTERS THAT NARROW THE PREDICATE NARROW THE BOARD, in the same
	// terms: a group, a negated status, a named status.
	expect("active only", ask(map[string]any{"group_by": "status", "status_group": "active"}),
		[]string{"in_progress", "in_review"}, []int{0, 0})
	expect("not todo", ask(map[string]any{"group_by": "status", "status": "!todo"}),
		[]string{"in_progress", "in_review"}, []int{0, 0})
	expect("todo named", ask(map[string]any{"group_by": "status", "status": "todo"}),
		[]string{"todo"}, []int{1})
	// ASKING FOR FINISHED WORK ADMITS ITS THREE STATUSES.
	expect("everything", ask(map[string]any{"group_by": "status", "show_closed": "true"}),
		declared(tracker.Statuses), []int{1, 0, 0, 0, 0, 0})
	// AND THE OVERDUE ALIAS TAKES THEM OFF AGAIN, whatever show_closed said.
	expect("overdue", ask(map[string]any{
		"group_by": "status", "show_closed": "true", "due": "overdue",
	}), []string{"todo", "in_progress", "in_review"}, []int{0, 0, 0})
	// ONE COLUMN ASKED FOR IS ONE COLUMN ANSWERED, even an empty one: the
	// reader who followed "N more →" into it is looking at that column.
	expect("one column", ask(map[string]any{"group_by": "status", "group": "in_review"}),
		[]string{"in_review"}, []int{0})
	// THE GROUP AXIS FOLLOWS THE SAME ADMISSION.
	expect("by group", ask(map[string]any{"group_by": "status_group"}),
		[]string{"not_started", "active"}, []int{1, 0})
	// A PRIORITY BOARD CARRIES THE WHOLE SCALE, and a priority filter
	// narrows it.
	expect("by priority", ask(map[string]any{"group_by": "priority"}),
		declared(tracker.Priorities), []int{0, 0, 1, 0, 0})
	expect("two priorities", ask(map[string]any{"group_by": "priority", "priority": "high,urgent"}),
		[]string{"high", "urgent"}, []int{0, 0})
	// AN OPEN AXIS IS LEFT AS IT WAS — the values present and no more —
	// because every seat in the company as an empty column is a roster
	// rather than a board.
	expect("by assignee", ask(map[string]any{"group_by": "assignee"}), []string{""}, []int{1})
}

func declared[T ~string](values []T) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

func columnKeys(answer tracker.Answer) []string {
	out := make([]string, 0, len(answer.Groups))
	for _, group := range answer.Groups {
		out = append(out, group.Key)
	}
	return out
}

func columnCounts(answer tracker.Answer) []int {
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

// berlinAt is a wall-clock instant in the COMPANY's own zone.
//
// SPELLED AS A CALENDAR rather than resolved through [tracker.ResolveDate]:
// these cases exist to pin where the bands cut, and a case that asked the
// resolver for its own boundary would move with whatever it was asked to
// hold. `wednesday` is 16:30 on Wednesday 2031-04-16 in Berlin, so the day is
// the 16th and the Monday-anchored week runs 14–20 April.
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
func seedDue(h *readHarness, id string, due *time.Time, group tracker.StatusGroup) {
	h.t.Helper()
	h.seed(id, func(task *tracker.Task) {
		task.DueAt = due
		task.StatusGroup = group
		if group == tracker.GroupDone {
			task.Status = tracker.StatusDone
		}
	})
}

// bandOf is the band one task landed in, read back off the answer's columns.
func bandOf(t *testing.T, answer tracker.Answer, id string) (string, bool) {
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

// THE BANDS CUT ON THE QUERY'S OWN DAY, AND ON ITS OWN WEEK.
//
// This is the whole of why the axis exists. The row's `overdue` flag and every
// `due=` filter are already derived against the company's day start; the bands
// a person reads their day in were cut in the browser, on the browser's
// midnight and the browser's Monday. Two boundaries for one fact, and for
// anybody whose local day differs from the company's they disagree by a whole
// band. Every boundary below is stated in Berlin wall-clock, which is the
// company's zone in this suite, so a band that moved to UTC midnight — or to
// a rolling seven days — fails here rather than on somebody's screen.
func TestTheDueBandsCutOnTheCompanysOwnCalendar(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	// ONE TABLE, SEEDED AND THEN ASSERTED, so a case's expectation cannot
	// drift from the row it was written for.
	cases := []struct {
		id    string
		day   string
		clock string
		group tracker.StatusGroup
		want  string
	}{
		// THE LAST INSTANT BEFORE THE DAY START, twice: the band that
		// means "you missed this" is the one that also asks whether the
		// work is still open.
		{"open-yesterday", "2031-04-15", "23:59:59.999999",
			tracker.GroupActive, "overdue"},
		{"done-yesterday", "2031-04-15", "23:59:59.999999",
			tracker.GroupDone, "earlier"},
		// THE DAY START ITSELF is today, not yesterday.
		{"day-start", "2031-04-16", "00:00:00", tracker.GroupNotStarted, "today"},
		// AND THE LAST SECOND OF THE SAME DAY is still today — the case
		// a browser cutting on its own midnight gets wrong first.
		{"day-end", "2031-04-16", "23:59:59", tracker.GroupNotStarted, "today"},
		// THE FIRST INSTANT AFTER IT is this week.
		{"tomorrow", "2031-04-17", "00:00:00", tracker.GroupNotStarted, "this_week"},
		// THE LAST INSTANT OF THE WEEK is still this week: the week is
		// Monday-anchored, so it ends as Sunday the 20th does.
		{"week-end", "2031-04-20", "23:59:59.999999",
			tracker.GroupNotStarted, "this_week"},
		// AND THE FIRST INSTANT AFTER IT is later.
		{"next-monday", "2031-04-21", "00:00:00", tracker.GroupNotStarted, "later"},
		// A FINISHED TASK IS NOT AUTOMATICALLY `earlier`: that band is
		// work past its date, and this one's date has not come.
		{"done-ahead", "2031-04-18", "09:00:00", tracker.GroupDone, "this_week"},
		// AND ONE WITH NO DATE AT ALL, which is a column rather than a
		// row the board hides. Its empty clock is what says so.
		{"undated", "", "", tracker.GroupNotStarted, ""},
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
		// THE FINISHED WORK HAS TO BE IN THE ANSWER for `earlier` to be
		// reachable at all — it is empty on an open-only read by
		// construction, which is the point of it being its own band.
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

	// THE UNDATED COLUMN IS LABELLED, because a board draws a heading
	// over it and an empty string is not one.
	undated := groupOf(t, answer, "")
	if undated.Label != "No due date" {
		t.Errorf("the undated band reads %q, want the sixth heading",
			undated.Label)
	}
}

// AND THE OVERDUE BAND IS THE ROW'S OWN OVERDUE FLAG.
//
// `overdue` means open AND past its date — which is exactly what
// [tracker.TaskRow.Overdue] and the `due=overdue` filter mean, from the same
// instant. A band derived from the date alone would put work somebody
// delivered late under a heading claiming they still owe it.
func TestTheOverdueBandIsTheRowsOverdueFlag(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	yesterday := berlinAt(t, "2031-04-15", "09:00:00")
	seedDue(h, "open-late", &yesterday, tracker.GroupActive)
	seedDue(h, "done-late", &yesterday, tracker.GroupDone)

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
	// AND AN OPEN-ONLY READ HAS NO `earlier` BAND, because nothing in it
	// can be past its date and finished.
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

// A BAND IS A COLUMN, so `group=` narrows to it — including the hint.
//
// The count hint and the totals share the query's compiled predicate and carry
// no join, so the axis has to be expressible as one join-free expression. An
// axis that needed a join would leave the header adding up the whole board
// while the rows showed a single band of it.
func TestADueBandNarrowsTheWholeQuery(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	today := berlinAt(t, "2031-04-16", "08:00:00")
	alsoToday := berlinAt(t, "2031-04-16", "23:59:59")
	tomorrow := berlinAt(t, "2031-04-17", "10:00:00")
	yesterday := berlinAt(t, "2031-04-15", "10:00:00")
	seedDue(h, "today-a", &today, tracker.GroupNotStarted)
	seedDue(h, "today-b", &alsoToday, tracker.GroupActive)
	seedDue(h, "this-week", &tomorrow, tracker.GroupNotStarted)
	seedDue(h, "overdue", &yesterday, tracker.GroupActive)
	seedDue(h, "undated", nil, tracker.GroupNotStarted)

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
	// THE HINT IS THE NARROWED SET'S, which is what makes the header of a
	// single-column view describe that column.
	if answer.TotalHint != 2 {
		t.Errorf("group=today reports a total hint of %d over the whole "+
			"query, want the 2 rows the column holds — the hint and the "+
			"column disagree, so the axis is not narrowing the shared "+
			"predicate", answer.TotalHint)
	}
	// AND THE UNDATED BAND NARROWS BY THE SAME SPELLING every other axis's
	// absent value takes.
	undated := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "due:bucket",
		"group": "",
	})
	if undated.TotalHint != 1 {
		t.Errorf("group= (the undated band) reports a hint of %d, want the "+
			"1 task with no due date", undated.TotalHint)
	}
}

// THE BANDS HOLD THEIR DECLARED ORDER, and the empty ones are drawn too.
//
// A day is read Overdue, Earlier, Today, This week, Later, No due date. Order
// the columns by size instead and the headings re-shuffle every time work
// moves between them, which is a board nobody can learn the shape of — so the
// fixture makes the LAST drawn band the biggest, and a size-ordered axis fails
// here.
//
// AND THE AXIS IS A CLOSED SET, so it pads: nothing is due this week in the
// fixture and the lane is drawn at nought anyway, because a day with nothing
// in it is a fact about the week rather than a gap in the board. `earlier` is
// the one band that is NOT drawn, and for the reason the admission rule gives
// — it holds work that was finished late, and this answer carries open work
// only, so nothing could have landed in it.
func TestTheDueBandsKeepTheirDeclaredOrder(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	yesterday := berlinAt(t, "2031-04-15", "10:00:00")
	today := berlinAt(t, "2031-04-16", "10:00:00")
	nextMonth := berlinAt(t, "2031-05-20", "10:00:00")
	seedDue(h, "overdue", &yesterday, tracker.GroupActive)
	seedDue(h, "today", &today, tracker.GroupNotStarted)
	for _, id := range []string{"later-a", "later-b", "later-c"} {
		seedDue(h, id, &nextMonth, tracker.GroupNotStarted)
	}
	seedDue(h, "undated", nil, tracker.GroupNotStarted)

	answer := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "due:bucket",
	})
	var drawn []string
	for _, group := range answer.Groups {
		drawn = append(drawn, group.Key)
	}
	// FIVE OF THE SIX, IN THE AXIS'S OWN ORDER — `this_week` empty among
	// them, and `earlier` absent because no finished status is admitted.
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
		// AN EMPTY BAND CARRIES AN EMPTY LIST, never a null: a renderer
		// mapping a column's rows would fall over on exactly the column
		// the padding exists to draw.
		if got.Rows == nil {
			t.Errorf("the %q band carries no row list at all", band.key)
		}
	}
	// AND EACH HEADING IS THE WORD A PERSON READS, not the stored slug.
	for _, want := range []struct{ key, label string }{
		{"overdue", "Overdue"}, {"today", "Today"},
		// THE PADDED ONE TOO: a lane minted from a bare key is headed
		// from the same table as the ones the count statement returned.
		{"this_week", "This week"},
		{"later", "Later"}, {"", "No due date"},
	} {
		if got := groupOf(t, answer, want.key).Label; got != want.label {
			t.Errorf("the %q band reads %q, want %q", want.key, got, want.label)
		}
	}
}

// AND THE BANDS SURVIVE BEING THE INNER AXIS OF A SWIMLANE BOARD.
//
// The bands are the one axis whose EXPRESSION carries values of its own —
// three instants, four placeholders — and a placeholder binds in textual
// order. As a lane the expression is written into the SELECT of a statement
// that already carries the outer axis's join and the outer column's own
// predicate, so an arrangement that bound the expression's values with the
// join's silently cuts the bands on whatever number happened to be next.
func TestTheDueBandsAreAnInnerAxisToo(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	yesterday := berlinAt(t, "2031-04-15", "10:00:00")
	today := berlinAt(t, "2031-04-16", "10:00:00")
	h.seed("ana-overdue", func(task *tracker.Task) {
		task.DueAt, task.Assignee = &yesterday, "ana"
		task.StatusGroup = tracker.GroupActive
	})
	h.seed("ana-today", func(task *tracker.Task) {
		task.DueAt, task.Assignee = &today, "ana"
	})
	h.seed("bob-today", func(task *tracker.Task) {
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

// THE ABSENT-VALUE COLUMN LOADS LIKE EVERY OTHER ONE.
//
// A board draws "nobody is assigned" as a column with its own count, and
// `group=<value>` is the ONLY way to load a column further — a grouped answer
// mints no cursor, so there is nothing else to page with. Read as a plain
// string, `group=` and an absent `group` were one request, so the one column
// a board could not load was the one holding everything nobody had filled in:
// the ask answered the whole board instead, which is the widest possible
// reading of a narrowing somebody typed. [tracker.Params] already draws that
// line — "a filter set to empty asks for rows with no value" — and this is
// the grammar keeping it.
func TestTheAbsentValueColumnLoadsLikeEveryOther(t *testing.T) {
	t.Parallel()
	h := newReadHarness(t)

	for _, spec := range []struct{ id, assignee string }{
		{"ana-1", "ana"}, {"ana-2", "ana"},
		{"nobody-1", ""}, {"nobody-2", ""},
	} {
		h.seed(spec.id, func(task *tracker.Task) { task.Assignee = spec.assignee })
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
	// AND AN ABSENT `group` IS STILL THE WHOLE BOARD, which is the other
	// half of the same distinction.
	whole := h.ask(map[string]any{
		"container": "project:ENG", "group_by": "assignee",
	})
	if len(whole.Groups) != 2 {
		t.Errorf("an absent group drew %d columns, want both", len(whole.Groups))
	}
}

// AND A BARE `group=` WITH NO AXIS IS STILL REFUSED.
//
// A column filter with no board is a narrowing that would silently answer
// everything, and that is true of the empty spelling exactly as it is of a
// named one — the pointer is what makes the empty one reachable, not what
// makes it legal on its own.
func TestAColumnFilterWithNoAxisIsRefusedEmptyToo(t *testing.T) {
	t.Parallel()
	_, err := tracker.ParseQuery(tracker.MapParams(map[string]any{
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
