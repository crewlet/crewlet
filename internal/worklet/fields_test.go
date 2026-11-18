package worklet_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"

	"github.com/crewlet/crewlet/internal/worklet"
)

// seedFields declares the workspace catalogue these cases filter on.
func seedFields(t *testing.T, r *roundTrip) {
	t.Helper()
	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []worklet.FieldDef{
		{ID: "f-impact", Slug: "impact", Name: "Impact", Type: worklet.FieldDropdown,
			Config: worklet.FieldConfig{Options: []worklet.Option{
				{ID: "o-high", Slug: "high", Name: "High"},
				{ID: "o-low", Slug: "low", Name: "Low"},
			}}},
		{ID: "f-effort", Slug: "effort", Name: "Effort", Type: worklet.FieldNumber},
		{ID: "f-owner", Slug: "owner", Name: "Owning team", Type: worklet.FieldText},
		{ID: "f-ship", Slug: "ship", Name: "Ship by", Type: worklet.FieldDate},
		{ID: "f-urgent", Slug: "urgent", Name: "Urgent", Type: worklet.FieldCheckbox},
		{ID: "f-areas", Slug: "areas", Name: "Areas", Type: worklet.FieldLabels,
			Config: worklet.FieldConfig{Options: []worklet.Option{
				{ID: "o-api", Slug: "api", Name: "API"},
				{ID: "o-ui", Slug: "ui", Name: "UI"},
			}}},
	}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()
}

// seedWithFields files one task carrying the given field values.
func seedWithFields(t *testing.T, r *roundTrip, id string, values map[string]any) {
	t.Helper()
	task := newTask(id)
	task.Fields = map[string]json.RawMessage{}
	for field, value := range values {
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("encode %s: %v", field, err)
		}
		task.Fields[field] = body
	}
	if _, err := r.writer.CreateTask(t.Context(), "op-"+id, task, nil); err != nil {
		t.Fatalf("CreateTask %s: %v", id, err)
	}
	r.drain()
}

// A CUSTOM-FIELD FILTER REACHES THE ROWS: the applier writes
// `tracker_field_values`, and every f.<slug> filter compiles onto them.
func TestACustomFieldFilterReachesTheRows(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)

	seedWithFields(t, r, "t-1", map[string]any{
		"f-impact": "o-high", "f-effort": 8, "f-owner": "platform",
		"f-urgent": true, "f-areas": []string{"o-api", "o-ui"},
		"f-ship": "2031-06-30",
	})
	seedWithFields(t, r, "t-2", map[string]any{
		"f-impact": "o-low", "f-effort": 2, "f-owner": "growth",
		"f-urgent": false, "f-areas": []string{"o-ui"},
		"f-ship": "2031-01-15",
	})
	seedWithFields(t, r, "t-3", nil)
	// Ten and nine: "10" sorts before "9" as text and after it as a
	// number, so only such a pair catches a filter compiled onto the wrong
	// column ([FieldValueColumn]).
	seedWithFields(t, r, "t-ten", map[string]any{"f-effort": 10})
	seedWithFields(t, r, "t-nine", map[string]any{"f-effort": 9})

	for name, tc := range map[string]struct {
		filter string
		want   []string
	}{
		// The slug, the label and the id all resolve to the stored id.
		"a choice by slug":  {"high", []string{"t-1"}},
		"a choice by label": {"High", []string{"t-1"}},
		"a choice by id":    {"o-high", []string{"t-1"}},
		"the other choice":  {"low", []string{"t-2"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := ids(r.ask(map[string]any{
				"container": "project:ENG", "f.impact": tc.filter,
			}))
			assertIDs(t, got, tc.want)
		})
	}

	for name, tc := range map[string]struct {
		key, filter string
		want        []string
	}{
		"a number, exactly": {"f.effort", "8", []string{"t-1"}},
		"a number, above":   {"f.effort", "gt:5", []string{"t-1", "t-ten", "t-nine"}},
		"a number, below":   {"f.effort", "lt:5", []string{"t-2"}},
		// Ten is above nine numerically and below it lexically.
		"ten is above nine":   {"f.effort", "gt:9", []string{"t-ten"}},
		"nine is below ten":   {"f.effort", "lt:10", []string{"t-1", "t-2", "t-nine"}},
		"text, exactly":       {"f.owner", "platform", []string{"t-1"}},
		"text, containing":    {"f.owner", "contains:row", []string{"t-2"}},
		"a checkbox, checked": {"f.urgent", "true", []string{"t-1"}},
		"a checkbox, not":     {"f.urgent", "false", []string{"t-2"}},
		"a date, before":      {"f.ship", "lt:2031-03-01", []string{"t-2"}},
		"a date, after":       {"f.ship", "gt:2031-03-01", []string{"t-1"}},
		// A labels field is N rows: one member matches a task holding others.
		"one label of several": {"f.areas", "api", []string{"t-1"}},
		"a label both hold":    {"f.areas", "ui", []string{"t-1", "t-2"}},
		// Unset is the absence of a row.
		"unset": {"f.effort", "null", []string{"t-3"}},
		"set":   {"f.effort", "not_null", []string{"t-1", "t-2", "t-ten", "t-nine"}},
		"not equal": {"f.owner", "ne:platform",
			[]string{"t-2", "t-3", "t-ten", "t-nine"}},
		// The field resolves by its label too.
		"by the field's own name": {"f.Owning team", "platform", []string{"t-1"}},
	} {
		t.Run(name, func(t *testing.T) {
			got := ids(r.ask(map[string]any{
				"container": "project:ENG", tc.key: tc.filter,
			}))
			assertIDs(t, got, tc.want)
		})
	}
}

// A FIELD FILTER NOTHING RESOLVES IS REFUSED, never dropped, which would
// silently show more than the person asked for.
func TestAnUnresolvedFieldFilterIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)

	for name, tc := range map[string]struct{ key, value, want string }{
		"a field nobody declares": {"f.nonesuch", "x", "names no field"},
		"a number that is not one": {"f.effort", "high",
			"is not a number"},
		"a date that is not one": {"f.ship", "soon", "is not one"},
		// The refusal names what the type does admit, so a caller need not
		// guess again.
		"contains on a number": {"f.effort", "contains:8",
			"a number takes eq, ne, lt, lte, gt, gte, range"},
		// An `eq` on a set is refused too: one row per option, it would
		// test membership while reading as equality.
		"eq on a label set": {"f.areas", "eq:api", "a labels takes any, all"},
		"an operator that is not one": {"f.owner", "roughly:platform",
			"not a field comparison"},
	} {
		t.Run(name, func(t *testing.T) {
			q, err := worklet.ParseQuery(worklet.MapParams(map[string]any{
				"container": "project:ENG", tc.key: tc.value,
			}), wednesday, berlin)
			if err != nil {
				t.Fatalf("ParseQuery: %v", err)
			}
			q.Level = "stale"
			_, err = r.reader.Tasks(t.Context(), q, wednesday)
			if err == nil {
				t.Fatal("a filter nothing could resolve was answered")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal %q does not say %q", err, tc.want)
			}
		})
	}
}

// AN ARCHIVED FIELD'S VALUES LEAVE THE FILTERABLE SET AND STAY ON THE TASK:
// the document keeps every value, and `hidden = 1` takes them out of every
// index, so the archive is one-way rather than destructive.
func TestAnArchivedFieldsValuesLeaveTheFilters(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-1", map[string]any{"f-owner": "platform"})

	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "f.owner": "platform",
	})); len(got) != 1 {
		t.Fatalf("the control filter found %v, want the one task", got)
	}

	if _, err := r.writer.WriteFields(t.Context(), "op-archive",
		[]worklet.FieldDef{{
			ID: "f-owner", Slug: "owner", Name: "Owning team",
			Type: worklet.FieldText, Archived: true,
		}}); err != nil {
		t.Fatalf("archive the field: %v", err)
	}
	r.drain()

	// The filter is now unresolvable: a clause on an archived field would
	// match nothing and read as "no task has this".
	q, err := worklet.ParseQuery(worklet.MapParams(map[string]any{
		"container": "project:ENG", "f.owner": "platform",
	}), wednesday, berlin)
	if err != nil {
		t.Fatalf("ParseQuery: %v", err)
	}
	q.Level = "stale"
	if _, err := r.reader.Tasks(t.Context(), q, wednesday); err == nil {
		t.Fatal("an archived field still resolved as a filter")
	}
}

// THE FIND IS A SUBSTRING OF A KEY OR A TITLE: an uncompiled `q` answers the
// unfiltered list, which looks like a project where everything matches.
func TestTheFindMatchesAKeyOrATitle(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for _, spec := range []struct{ id, title string }{
		{"t-1", "wire the applier"},
		{"t-2", "rewire the publisher"},
		{"t-3", "something else entirely"},
	} {
		task := newTask(spec.id)
		task.Title = spec.title
		if _, err := r.writer.CreateTask(t.Context(), "op-"+spec.id, task, nil); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
		r.drain()
	}

	assertIDs(t, ids(r.ask(map[string]any{
		"container": "project:ENG", "q": "wire",
	})), []string{"t-1", "t-2"})

	// The key matches from the front, so a bare `2` is not ENG-2, ENG-12
	// and ENG-20: a key starts with its project.
	if got := ids(r.ask(map[string]any{"container": "project:ENG", "q": "2"})); len(got) != 0 {
		t.Fatalf("a bare number matched %v by key — a find for `2` must not "+
			"hand back every key with a 2 in it", got)
	}
	// A real key prefix still matches, or the anchor removed key search.
	keys := r.ask(map[string]any{"container": "project:ENG", "q": "ENG-"})
	if len(keys.Rows) != 3 {
		t.Fatalf("a key prefix matched %d tasks, want all three", len(keys.Rows))
	}

	// A wildcard a caller types is literal, never a pattern.
	if got := r.ask(map[string]any{"container": "project:ENG", "q": "%"}); len(got.Rows) != 0 {
		t.Fatalf("a literal %% matched %d tasks, so a caller's own wildcard "+
			"became a pattern", len(got.Rows))
	}
}

func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, id := range want {
		if !containsString(got, id) {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A SORT ON A CUSTOM FIELD ORDERS BY IT, rather than `sortTerms` dropping
// `f.<slug>` and silently answering in the default order.
func TestASortOnACustomFieldOrdersByIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)

	// Ten and nine again, so a lexical sort and a numeric one disagree.
	seedWithFields(t, r, "t-ten", map[string]any{"f-effort": 10})
	seedWithFields(t, r, "t-nine", map[string]any{"f-effort": 9})
	seedWithFields(t, r, "t-one", map[string]any{"f-effort": 1})

	got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sort": "f.effort",
	}))
	want := []string{"t-one", "t-nine", "t-ten"}
	for i, id := range want {
		if i >= len(got) || got[i] != id {
			t.Fatalf("the order is %v, want %v — a field sort that fell back "+
				"to the default would be in creation order", got, want)
		}
	}

	// Descending is the other way, or the direction was dropped.
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sort": "-f.effort",
	})); len(got) == 0 || got[0] != "t-ten" {
		t.Fatalf("the descending order is %v, want the largest first", got)
	}

	// A task that set nothing still appears: the join is LEFT, or a sort
	// would silently filter.
	seedWithFields(t, r, "t-none", nil)
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sort": "f.effort",
	})); !containsString(got, "t-none") {
		t.Fatalf("the order is %v and drops the task with no value — a sort "+
			"that also filters is two things the caller asked for once", got)
	}
}

// A FOREIGN VALUE IS STORED AND NEVER FILTERED ON, as the DDL's head comment
// says ("every filter and total adds `hidden = 0 AND kind <> 'foreign'`"): it
// is there to render, and the native operators are defined against the native
// catalogue's declared types.
func TestAForeignValueIsOutOfEveryPredicate(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-1", map[string]any{"f-effort": 8})
	// A task with no native value: only the foreign row below could put it
	// in the answer.
	seedWithFields(t, r, "t-mirrored", nil)

	// No foreign writer exists, so the row is planted as a mirror would
	// write it: this case is about the predicate, not the writer.
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO tracker_field_values
				(task_id, field_id, seq, kind, hidden, num)
			VALUES ('t-mirrored', 'f-effort', 0, ?, 0, 8)`,
			worklet.FieldValueForeign)
		return err
	}); err != nil {
		t.Fatalf("plant a foreign value: %v", err)
	}

	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "f.effort": "8",
	})); containsString(got, "t-mirrored") {
		t.Fatalf("a foreign value matched a native filter: %v", got)
	}
	// Nor is it "set", or `not_null` would be a way back in.
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "f.effort": "not_null",
	})); containsString(got, "t-mirrored") {
		t.Fatalf("a foreign value counted as the field being set: %v", got)
	}
	total := totalOf(t, r.ask(map[string]any{
		"container": "project:ENG", "totals": "f.effort:sum",
	}), "f.effort:sum")
	if total.Value == nil || *total.Value != 8 {
		t.Fatalf("the total is %v, want 8 — a foreign value was added into a "+
			"native sum", total.Value)
	}
}

// A FIELD THAT DOES NOT APPLY TO A TASK'S TYPE IS HIDDEN ON IT: a `severity`
// set on a bug stays on the document after a re-type to a task, and must leave
// every filter, grouping and total: hiding follows `AppliesTo`, not only
// `Archived`.
func TestAValueOfAFieldThatDoesNotApplyIsHidden(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []worklet.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldNumber,
			AppliesTo: []string{"bug"}},
	}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()

	bug := newTask("t-bug")
	bug.Type = "bug"
	bug.Fields = map[string]json.RawMessage{"f-sev": json.RawMessage(`3`)}
	if _, err := r.writer.CreateTask(t.Context(), "op-bug", bug, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	plain := newTask("t-plain")
	plain.Type = "task"
	plain.Fields = map[string]json.RawMessage{"f-sev": json.RawMessage(`9`)}
	if _, err := r.writer.CreateTask(t.Context(), "op-plain", plain, nil); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	r.drain()

	answer := r.ask(map[string]any{
		"container": "project:ENG", "f.severity": "gte:1",
	})
	if len(answer.Rows) != 1 || answer.Rows[0].ID != "t-bug" {
		t.Fatalf("f.severity>=1 matched %d rows (%v), want only the bug: a "+
			"value of a field the task's type does not carry is not a live "+
			"value of that field", len(answer.Rows), ids(answer))
	}

	// A declaration adding the type brings it back: the settle pass moves
	// the column both ways, or a widened declaration never reaches old rows.
	if _, err := r.writer.WriteFields(t.Context(), "op-widen", []worklet.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldNumber,
			AppliesTo: []string{"bug", "task"}},
	}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()
	widened := r.ask(map[string]any{
		"container": "project:ENG", "f.severity": "gte:1",
	})
	if len(widened.Rows) != 2 {
		t.Fatalf("f.severity>=1 matched %d rows after the declaration added "+
			"`task`, want 2", len(widened.Rows))
	}

	// Narrowing it again takes the value back out: nothing rewrites those
	// tasks, so the catalogue write must compute the type comparison itself.
	if _, err := r.writer.WriteFields(t.Context(), "op-narrow", []worklet.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldNumber,
			AppliesTo: []string{"bug"}},
	}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()
	narrowed := r.ask(map[string]any{
		"container": "project:ENG", "f.severity": "gte:1",
	})
	if len(narrowed.Rows) != 1 || narrowed.Rows[0].ID != "t-bug" {
		t.Fatalf("f.severity>=1 matched %d rows (%v) after the declaration "+
			"dropped `task`, want only the bug", len(narrowed.Rows), ids(narrowed))
	}
}

// A VALUE FOR A FIELD NOBODY DECLARES IS RECORDED AS FOREIGN, not dropped: a
// cross-project move orphans values the task still holds. With no type there is
// no column, so the row carries the value's JSON and is excluded from every
// filter, total and grouping by its kind.
func TestAValueForAnUndeclaredFieldIsRecordedAsForeign(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-orphan", map[string]any{
		"f-effort": 5, "f-gone": "whatever it was",
	})

	var kind, text string
	var hidden int
	if err := r.db.Tx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT kind, hidden, text FROM tracker_field_values
			 WHERE task_id = 't-orphan' AND field_id = 'f-gone'`).
			Scan(&kind, &hidden, &text)
	}); err != nil {
		t.Fatalf("read the orphaned value: %v — a value whose field nobody "+
			"declares reached no row at all, so nothing can render what the "+
			"task still holds", err)
	}
	if kind != worklet.FieldValueForeign || hidden != 1 {
		t.Fatalf("the orphaned value is kind=%q hidden=%d, want %q and 1",
			kind, hidden, worklet.FieldValueForeign)
	}
	if !strings.Contains(text, "whatever it was") {
		t.Fatalf("the orphaned value's row carries %q, which is not the value "+
			"the document holds", text)
	}
	// No filter reaches it: `liveFieldValue` excludes it by kind.
	answer := r.ask(map[string]any{"container": "project:ENG", "f.effort": "gte:1"})
	if len(answer.Rows) != 1 {
		t.Fatalf("f.effort>=1 matched %d rows, want 1", len(answer.Rows))
	}
}

// A REQUIRED FIELD IS REQUIRED OF THE TASKS IT APPLIES TO, and of no others:
// a plain task is not refused for a `severity` its type would hide.
func TestARequiredFieldIsRequiredOnlyOfTheTypesItAppliesTo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []worklet.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldNumber,
			Required: true, AppliesTo: []string{"bug"}},
	}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()

	plain := newTask("t-plain")
	plain.Type = "task"
	if _, err := r.writer.CreateTask(t.Context(), "op-plain", plain, nil); err != nil {
		t.Fatalf("a plain task was refused for a field required only of bugs: %v", err)
	}
	r.drain()

	// Still required of a bug, or this would pass with no rule at all.
	bug := newTask("t-bug")
	bug.Key, bug.Type = "ENG-2", "bug"
	if _, err := r.writer.CreateTask(t.Context(), "op-bug", bug, nil); err == nil {
		t.Fatal("a bug with no severity was accepted, and severity is required of bugs")
	}
}

// AND A FIELD REQUIRED AT THE WORKSPACE IS REQUIRED IN EVERY PROJECT: the
// check reads both places fields are declared, not only the project's.
func TestAWorkspaceRequiredFieldIsEnforced(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []worklet.FieldDef{
		{ID: "f-team", Slug: "owning_team", Name: "Owning team",
			Type: worklet.FieldText, Required: true},
	}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()

	if _, err := r.writer.CreateTask(t.Context(), "op-bare", newTask("t-bare"), nil); err == nil {
		t.Fatal("a task with no owning_team was accepted, and the workspace " +
			"catalogue requires it of every task in every project")
	}

	withIt := newTask("t-set")
	withIt.Key = "ENG-2"
	withIt.Fields = map[string]json.RawMessage{"f-team": json.RawMessage(`"platform"`)}
	if _, err := r.writer.CreateTask(t.Context(), "op-set", withIt, nil); err != nil {
		t.Fatalf("a task that sets the required field was refused: %v", err)
	}
}

// A VALUE WRITTEN AS AN OPTION'S SLUG IS THE SAME VALUE AS ITS ID: the write
// resolves through [OptionIDs] as the read does, or a task set to `"high"` is
// invisible to every filter looking for "o-high".
func TestAnOptionResolvesToItsIDOnTheWriteAsWellAsTheRead(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)

	// Three spellings of one option (id, slug, name) land on one row.
	seedWithFields(t, r, "by-id", map[string]any{"f-impact": "o-high"})
	seedWithFields(t, r, "by-slug", map[string]any{"f-impact": "high"})
	seedWithFields(t, r, "by-name", map[string]any{"f-impact": "High"})

	got := ids(r.ask(map[string]any{
		"container": "project:ENG", "f.impact": "high",
	}))
	if len(got) != 3 {
		t.Fatalf("f.impact=high answers %v, want all three — one written by "+
			"id, one by slug and one by name are one value", got)
	}

	// A multi-valued field resolves every member, not just the first.
	seedWithFields(t, r, "areas", map[string]any{
		"f-areas": []string{"api", "o-ui"},
	})
	for _, value := range []string{"api", "ui"} {
		if found := ids(r.ask(map[string]any{
			"container": "project:ENG", "f.areas": value,
		})); len(found) != 1 {
			t.Errorf("f.areas=%s answers %v, want the task that set it", value, found)
		}
	}
}

// THE SET OPERATORS ARE WHAT A MULTI-VALUED FIELD IS ACTUALLY ASKED ABOUT:
// `any`, `all`, `not_any` and `not_all`, each over the field's one row per
// option.
func TestTheSetOperatorsOverALabelField(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "both", map[string]any{"f-areas": []string{"api", "ui"}})
	seedWithFields(t, r, "api-only", map[string]any{"f-areas": []string{"api"}})
	seedWithFields(t, r, "ui-only", map[string]any{"f-areas": []string{"ui"}})
	seedWithFields(t, r, "neither", map[string]any{"f-effort": 3})

	for name, tc := range map[string]struct {
		filter string
		want   []string
	}{
		// any is "holds at least one of these".
		"any of two": {"any:api,ui", []string{"both", "api-only", "ui-only"}},
		"any of one": {"any:api", []string{"both", "api-only"}},
		// all is "holds every one of these", a count over the set.
		"all of two": {"all:api,ui", []string{"both"}},
		"all of one": {"all:ui", []string{"both", "ui-only"}},
		// not_any is "holds none of these", never "some row is not one of
		// these", which `both` meets for either option. A task with the field
		// unset holds none, so it is in: every negation is over the whole
		// membership, as `ne` is, or `not_any` would silently require a value.
		"not any": {"not_any:api", []string{"ui-only", "neither"}},
		// not_all is "is missing at least one of these".
		"not all": {"not_all:api,ui", []string{"api-only", "ui-only", "neither"}},
		// The bare form is `any` ([defaultFieldOp]).
		"the bare form": {"api", []string{"both", "api-only"}},
	} {
		t.Run(name, func(t *testing.T) {
			assertIDs(t, ids(r.ask(map[string]any{
				"container": "project:ENG", "f.areas": tc.filter,
			})), tc.want)
		})
	}

	// An empty set comparison is refused: it would match nothing.
	if err := r.askErr(map[string]any{
		"container": "project:ENG", "f.areas": "any:",
	}); err == nil {
		t.Error("`any:` with no values was answered")
	}
}

// `range` IS TWO BOUNDS IN ONE FILTER ([fieldValueRange]); `startswith` and
// `in` are the text comparisons beside it.
func TestRangeAndStartsWithAndIn(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "small", map[string]any{"f-effort": 2, "f-owner": "platform"})
	seedWithFields(t, r, "middle", map[string]any{"f-effort": 5, "f-owner": "plat-api"})
	seedWithFields(t, r, "large", map[string]any{"f-effort": 9, "f-owner": "storage"})

	assertIDs(t, ids(r.ask(map[string]any{
		"container": "project:ENG", "f.effort": "range:3..8",
	})), []string{"middle"})

	// startswith is a prefix, which `contains` cannot express.
	assertIDs(t, ids(r.ask(map[string]any{
		"container": "project:ENG", "f.owner": "startswith:plat",
	})), []string{"small", "middle"})
	assertIDs(t, ids(r.ask(map[string]any{
		"container": "project:ENG", "f.owner": "contains:lat",
	})), []string{"small", "middle"})
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "f.owner": "startswith:lat",
	})); len(got) != 0 {
		t.Errorf("startswith:lat answers %v — it is a PREFIX, and `lat` is in "+
			"the middle of both", got)
	}

	// `in` is `any` over a single-valued column ([FieldOpIn]).
	assertIDs(t, ids(r.ask(map[string]any{
		"container": "project:ENG", "f.owner": "in:platform,storage",
	})), []string{"small", "large"})

	// A range with one end is refused naming `gte` or `lte`, rather than
	// comparing against the literal "3..".
	if err := r.askErr(map[string]any{
		"container": "project:ENG", "f.effort": "range:3",
	}); err == nil {
		t.Error("a one-ended range was answered")
	}
}

// A URL IS A VALUE, NOT AN OPERATOR CALL: its scheme is not read as an
// operator.
func TestAUrlValueIsNotReadAsAnOperator(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-url", []worklet.FieldDef{
		{ID: "f-home", Slug: "home", Name: "Homepage", Type: worklet.FieldURL},
	}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()
	seedWithFields(t, r, "site", map[string]any{
		"f-home": "https://example.com/docs",
	})

	assertIDs(t, ids(r.ask(map[string]any{
		"container": "project:ENG", "f.home": "https://example.com/docs",
	})), []string{"site"})

	// A typo is still refused: the `//` is the whole exception, so any
	// other colon is an operator, named in the refusal.
	if err := r.askErr(map[string]any{
		"container": "project:ENG", "f.home": "roughly:example",
	}); err == nil {
		t.Error("a typo'd operator was answered as a value")
	}
}

// `me` IS RESOLVED BY THE SURFACE, before the query is parsed, per read: a
// saved view carrying `me` means whoever opens it.
func TestTheViewerKeywordIsResolvedPerRead(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-people", []worklet.FieldDef{
		{ID: "f-rev", Slug: "reviewers", Name: "Reviewers", Type: worklet.FieldPeople},
	}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()
	seedWithFields(t, r, "hers", map[string]any{"f-rev": []string{"ana"}})
	seedWithFields(t, r, "his", map[string]any{"f-rev": []string{"bob"}})

	for viewer, want := range map[string]string{"ana": "hers", "bob": "his"} {
		q, err := r.reader.ExpandedQuery(t.Context(), map[string]any{
			"container": "project:ENG", "f.reviewers": "me",
		}, worklet.Viewer{Handle: viewer}, wednesday, berlin)
		if err != nil {
			t.Fatalf("ExpandedQuery(%s): %v", viewer, err)
		}
		q.Level = statelog.ReadStale
		answer, err := r.reader.Tasks(t.Context(), q, wednesday)
		if err != nil {
			t.Fatalf("Tasks(%s): %v", viewer, err)
		}
		got := ids(answer)
		if len(got) != 1 || got[0] != want {
			t.Errorf("f.reviewers=me for %s answers %v, want [%s] — one saved "+
				"query has to mean the reader rather than whoever saved it",
				viewer, got, want)
		}
	}

	// With no viewer it is refused naming the key, never answered as
	// everybody's.
	if _, err := r.reader.ExpandedQuery(t.Context(), map[string]any{
		"container": "project:ENG", "f.reviewers": "me",
	}, worklet.Viewer{}, wednesday, berlin); err == nil {
		t.Error("f.reviewers=me with no viewer was answered")
	}
}

// A DETAIL READ ANSWERS IN WORDS, not in field identifiers, so a caller needs
// no second call on the catalogue. It reports both non-live states: `hidden`
// (the declaration was archived; kept on the task, out of every filter) and a
// value whose field nothing declares (on the document, with no row).
func TestADetailReadNamesEachFieldAndItsState(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-1", map[string]any{
		"f-effort":  3,
		"f-ghostly": "a field nobody declared",
	})

	// One is archived after the fact, the state the one-way archive creates.
	if _, err := r.writer.WriteFields(t.Context(), "op-archive", []worklet.FieldDef{
		{ID: "f-effort", Slug: "effort", Name: "Effort",
			Type: worklet.FieldNumber, Archived: true},
	}); err != nil {
		t.Fatalf("archive the field: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), "t-1",
		worklet.DetailWants{Fields: true}, statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if len(detail.Fields) != 2 {
		t.Fatalf("the read answered %d field values for a task carrying two: %+v",
			len(detail.Fields), detail.Fields)
	}
	byID := map[string]worklet.FieldValue{}
	for _, value := range detail.Fields {
		byID[value.ID] = value
	}

	effort := byID["f-effort"]
	if effort.Slug != "effort" || effort.Name != "Effort" {
		t.Errorf("the declared value came back as %+v — a caller reading an "+
			"id it cannot interpret has to spend a second call on the "+
			"catalogue to write the same value back", effort)
	}
	if effort.Type != worklet.FieldNumber {
		t.Errorf("the value's type is %q and its declaration says %q",
			effort.Type, worklet.FieldNumber)
	}
	if !effort.Hidden {
		t.Error("an archived field's value is not reported hidden, so it " +
			"renders beside a live one and somebody acts on a rule this " +
			"company stopped using")
	}

	ghost := byID["f-ghostly"]
	if !ghost.Undeclared {
		t.Errorf("a value for a field declared nowhere came back as %+v — it "+
			"is on the record and has no row, and an answer that dropped it "+
			"would disagree with the task itself", ghost)
	}
	if ghost.Slug != "" {
		t.Errorf("an undeclared value named a slug %q it cannot have", ghost.Slug)
	}
}
