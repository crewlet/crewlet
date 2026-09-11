package tracker_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// seedFields declares the workspace catalogue these cases filter on.
func seedFields(t *testing.T, r *roundTrip) {
	t.Helper()
	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []tracker.FieldDef{
		{ID: "f-impact", Slug: "impact", Name: "Impact", Type: tracker.FieldDropdown,
			Config: tracker.FieldConfig{Options: []tracker.Option{
				{ID: "o-high", Slug: "high", Name: "High"},
				{ID: "o-low", Slug: "low", Name: "Low"},
			}}},
		{ID: "f-effort", Slug: "effort", Name: "Effort", Type: tracker.FieldNumber},
		{ID: "f-owner", Slug: "owner", Name: "Owning team", Type: tracker.FieldText},
		{ID: "f-ship", Slug: "ship", Name: "Ship by", Type: tracker.FieldDate},
		{ID: "f-urgent", Slug: "urgent", Name: "Urgent", Type: tracker.FieldCheckbox},
		{ID: "f-areas", Slug: "areas", Name: "Areas", Type: tracker.FieldLabels,
			Config: tracker.FieldConfig{Options: []tracker.Option{
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

// A CUSTOM-FIELD FILTER REACHES THE ROWS, and it never did.
//
// `tracker_field_values` has four partial indexes, each named in the DDL for a
// different column, the plan test has always registered an `f.impact` query,
// and the applier wrote no row into the table at all: every one of those
// indexes was an index over nothing, and every f.<slug> filter in the grammar
// was parsed, validated and silently dropped.
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
	// TEN AND NINE, because they are the pair that separates a NUMERIC
	// comparison from a lexical one: "10" sorts before "9" as text and
	// after it as a number. Every single-digit fixture passes against a
	// filter compiled onto the wrong column, which is the one mistake
	// [FieldValueColumn] exists to prevent.
	seedWithFields(t, r, "t-ten", map[string]any{"f-effort": 10})
	seedWithFields(t, r, "t-nine", map[string]any{"f-effort": 9})

	for name, tc := range map[string]struct {
		filter string
		want   []string
	}{
		// THE SLUG, THE LABEL AND THE ID all resolve to the same option,
		// because the row holds the id and a person types the word.
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
		// THE PAIR THAT SEPARATES A NUMBER FROM ITS TEXT: ten is above
		// nine numerically and below it lexically.
		"ten is above nine":   {"f.effort", "gt:9", []string{"t-ten"}},
		"nine is below ten":   {"f.effort", "lt:10", []string{"t-1", "t-2", "t-nine"}},
		"text, exactly":       {"f.owner", "platform", []string{"t-1"}},
		"text, containing":    {"f.owner", "contains:row", []string{"t-2"}},
		"a checkbox, checked": {"f.urgent", "true", []string{"t-1"}},
		"a checkbox, not":     {"f.urgent", "false", []string{"t-2"}},
		"a date, before":      {"f.ship", "lt:2031-03-01", []string{"t-2"}},
		"a date, after":       {"f.ship", "gt:2031-03-01", []string{"t-1"}},
		// A LABELS FIELD IS N ROWS, so naming one member matches the
		// task that holds it among others.
		"one label of several": {"f.areas", "api", []string{"t-1"}},
		"a label both hold":    {"f.areas", "ui", []string{"t-1", "t-2"}},
		// UNSET IS THE ABSENCE OF A ROW, which is what the task that
		// set nothing has.
		"unset": {"f.effort", "null", []string{"t-3"}},
		"set":   {"f.effort", "not_null", []string{"t-1", "t-2", "t-ten", "t-nine"}},
		"not equal": {"f.owner", "ne:platform",
			[]string{"t-2", "t-3", "t-ten", "t-nine"}},
		// AND THE FIELD IS RESOLVED BY ITS LABEL TOO.
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

// A FIELD FILTER NOTHING RESOLVES IS REFUSED, never dropped.
//
// A filter nobody parsed is a board showing more than the person asked for,
// silently — which is the finding the unknown-key refusal came from, and a ref
// that resolved to nothing is the same failure one level down.
func TestAnUnresolvedFieldFilterIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)

	for name, tc := range map[string]struct{ key, value, want string }{
		"a field nobody declares": {"f.nonesuch", "x", "names no field"},
		"a number that is not one": {"f.effort", "high",
			"is not a number"},
		"a date that is not one": {"f.ship", "soon", "is not one"},
		"contains on a number":   {"f.effort", "contains:8", "is a text comparison"},
		"an operator that is not one": {"f.owner", "roughly:platform",
			"not a field comparison"},
	} {
		t.Run(name, func(t *testing.T) {
			q, err := tracker.ParseQuery(tracker.MapParams(map[string]any{
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

// AN ARCHIVED FIELD'S VALUES LEAVE THE FILTERABLE SET AND STAY ON THE TASK.
//
// That is what makes the archive one-way rather than destructive: the document
// keeps every value, and `hidden = 1` is what takes them out of every index.
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
		[]tracker.FieldDef{{
			ID: "f-owner", Slug: "owner", Name: "Owning team",
			Type: tracker.FieldText, Archived: true,
		}}); err != nil {
		t.Fatalf("archive the field: %v", err)
	}
	r.drain()

	// THE FILTER IS NOW UNRESOLVABLE, and that is the honest answer: a
	// clause on an archived field would match nothing and read as "no
	// task has this" rather than "this field is gone".
	q, err := tracker.ParseQuery(tracker.MapParams(map[string]any{
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

// THE FIND IS A SUBSTRING OF A KEY OR A TITLE.
//
// `q` was parsed, bounded and never compiled, so every search a seat ran for
// an item it half remembered came back as the UNFILTERED list — which looks
// exactly like a project where everything matches.
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

	// THE KEY MATCHES FROM THE FRONT, which is what stops a bare number
	// matching every key that happens to contain it. `2` unanchored is
	// ENG-2 AND ENG-12 AND ENG-20; anchored it is nothing, because a key
	// starts with its project.
	if got := ids(r.ask(map[string]any{"container": "project:ENG", "q": "2"})); len(got) != 0 {
		t.Fatalf("a bare number matched %v by key — a find for `2` must not "+
			"hand back every key with a 2 in it", got)
	}
	// AND A REAL KEY PREFIX STILL MATCHES, or the anchor would have
	// removed key search rather than narrowed it.
	keys := r.ask(map[string]any{"container": "project:ENG", "q": "ENG-"})
	if len(keys.Rows) != 3 {
		t.Fatalf("a key prefix matched %d tasks, want all three", len(keys.Rows))
	}

	// AND A WILDCARD A CALLER TYPES IS LITERAL, never a pattern: `%` in a
	// find is a person searching for a per-cent sign.
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

// A SORT ON A CUSTOM FIELD ORDERS BY IT, and it never did.
//
// `parseSort` accepts `f.<slug>` and `sortTerms` silently dropped it, so a
// caller's own ordering was answered in the DEFAULT order with nothing
// anywhere saying it had been ignored — the same silence an unparsed filter
// has, one step further along.
func TestASortOnACustomFieldOrdersByIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)

	// TEN AND NINE AGAIN, so a lexical sort and a numeric one disagree.
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

	// AND DESCENDING IS THE OTHER WAY, or the direction was dropped too.
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sort": "-f.effort",
	})); len(got) == 0 || got[0] != "t-ten" {
		t.Fatalf("the descending order is %v, want the largest first", got)
	}

	// A TASK THAT SET NOTHING STILL APPEARS: the join is LEFT, because an
	// inner one would make a sort silently filter.
	seedWithFields(t, r, "t-none", nil)
	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "sort": "f.effort",
	})); !containsString(got, "t-none") {
		t.Fatalf("the order is %v and drops the task with no value — a sort "+
			"that also filters is two things the caller asked for once", got)
	}
}

// A FOREIGN VALUE IS STORED AND NEVER FILTERED ON.
//
// The DDL says so in its own head comment: "every filter and total adds
// `hidden = 0 AND kind <> 'foreign'`". A value mirrored in from a tracker a
// company runs beside this one is there to render, and it is out of every
// predicate because the native operators are defined against the native
// catalogue's declared types.
func TestAForeignValueIsOutOfEveryPredicate(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-1", map[string]any{"f-effort": 8})
	// A REAL TASK carrying no native value of its own, so the only thing
	// that could put it in the answer is the foreign row planted below.
	seedWithFields(t, r, "t-mirrored", nil)

	// The engine has no foreign writer yet, so the row is planted the way
	// a mirror would write it — which is what this case is about: the
	// PREDICATE, not the writer.
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO tracker_field_values
				(task_id, field_id, seq, kind, hidden, num)
			VALUES ('t-mirrored', 'f-effort', 0, ?, 0, 8)`,
			tracker.FieldValueForeign)
		return err
	}); err != nil {
		t.Fatalf("plant a foreign value: %v", err)
	}

	if got := ids(r.ask(map[string]any{
		"container": "project:ENG", "f.effort": "8",
	})); containsString(got, "t-mirrored") {
		t.Fatalf("a foreign value matched a native filter: %v", got)
	}
	// AND IT IS NOT "SET" EITHER, or `not_null` would be a way back in.
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

// A FIELD THAT DOES NOT APPLY TO A TASK'S TYPE IS HIDDEN ON IT.
//
// `AppliesTo` is a declaration about which TYPES carry a field, and a value
// left behind by a re-type is the case it exists for: a `severity` set while
// the task was a bug is still on the document after somebody re-types it to a
// task, and reading only `Archived` left that value filterable, groupable and
// totalled — a live value of a field that is not on the task at all.
func TestAValueOfAFieldThatDoesNotApplyIsHidden(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []tracker.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: tracker.FieldNumber,
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

	// AND A DECLARATION THAT ADDS THE TYPE BRINGS IT BACK, because
	// AppliesTo is not one-way: the settle pass has to move the column in
	// both directions or a widened declaration would never reach the rows
	// written under the narrow one.
	if _, err := r.writer.WriteFields(t.Context(), "op-widen", []tracker.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: tracker.FieldNumber,
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

	// AND NARROWING IT AGAIN TAKES THE VALUE BACK OUT. This is the half a
	// settle pass that only mirrors `Archived` cannot do: nothing rewrites
	// those tasks, so if the catalogue write does not compute the type
	// comparison itself the rows stay live under a declaration that no
	// longer covers them.
	if _, err := r.writer.WriteFields(t.Context(), "op-narrow", []tracker.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: tracker.FieldNumber,
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

// A VALUE FOR A FIELD NOBODY DECLARES IS RECORDED AS FOREIGN, not dropped.
//
// A cross-project move orphans values, and a task that still holds them must
// not read as one that lost them. With no declaration there is no type and so
// no column, which is why the row carries the value's own JSON and is excluded
// from every filter, total and grouping by its kind rather than by its shape.
func TestAValueForAnUndeclaredFieldIsRecordedAsForeign(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-orphan", map[string]any{
		"f-effort": 5, "f-gone": "whatever it was",
	})

	var kind, text string
	var hidden int
	if err := r.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(),
			`SELECT kind, hidden, text FROM tracker_field_values
			 WHERE task_id = 't-orphan' AND field_id = 'f-gone'`).
			Scan(&kind, &hidden, &text)
	}); err != nil {
		t.Fatalf("read the orphaned value: %v — a value whose field nobody "+
			"declares reached no row at all, so nothing can render what the "+
			"task still holds", err)
	}
	if kind != tracker.FieldValueForeign || hidden != 1 {
		t.Fatalf("the orphaned value is kind=%q hidden=%d, want %q and 1",
			kind, hidden, tracker.FieldValueForeign)
	}
	if !strings.Contains(text, "whatever it was") {
		t.Fatalf("the orphaned value's row carries %q, which is not the value "+
			"the document holds", text)
	}
	// AND IT IS NOT A ROW ANY FILTER CAN REACH: `liveFieldValue` excludes
	// it by kind, so the declared field beside it still answers alone.
	answer := r.ask(map[string]any{"container": "project:ENG", "f.effort": "gte:1"})
	if len(answer.Rows) != 1 {
		t.Fatalf("f.effort>=1 matched %d rows, want 1", len(answer.Rows))
	}
}

// A REQUIRED FIELD IS REQUIRED OF THE TASKS IT APPLIES TO, and of no others.
//
// `AppliesTo` names the types a field is carried by. A field required only of
// bugs was enforced on every task in the project, so a plain task filed into a
// project that requires `severity` of its bugs was refused for not setting a
// field that would have been HIDDEN the moment it was set.
func TestARequiredFieldIsRequiredOnlyOfTheTypesItAppliesTo(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []tracker.FieldDef{
		{ID: "f-sev", Slug: "severity", Name: "Severity", Type: tracker.FieldNumber,
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

	// AND IT IS STILL REQUIRED OF A BUG, or this case would pass against a
	// rule that stopped enforcing anything.
	bug := newTask("t-bug")
	bug.Key, bug.Type = "ENG-2", "bug"
	if _, err := r.writer.CreateTask(t.Context(), "op-bug", bug, nil); err == nil {
		t.Fatal("a bug with no severity was accepted, and severity is required of bugs")
	}
}

// AND A FIELD REQUIRED AT THE WORKSPACE IS REQUIRED IN EVERY PROJECT.
//
// Fields are declared in two places and the check read only the project's
// half, so a rule an operator set at the company level was applied to no task
// anywhere — and nothing said it was not being applied.
func TestAWorkspaceRequiredFieldIsEnforced(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-fields", []tracker.FieldDef{
		{ID: "f-team", Slug: "owning_team", Name: "Owning team",
			Type: tracker.FieldText, Required: true},
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

// A VALUE WRITTEN AS AN OPTION'S SLUG IS THE SAME VALUE AS ITS ID.
//
// The query resolved a caller's word to the option's id before comparing and
// the write stored whatever text it was handed, so a task set to `"high"` —
// the slug a person types and the query accepts — stored "high" where every
// filter looked for "o-high". The task was invisible to every filter, grouping
// and total on the field it had just set, with nothing anywhere saying so.
func TestAnOptionResolvesToItsIDOnTheWriteAsWellAsTheRead(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)

	// THREE SPELLINGS OF ONE OPTION: its id, its slug and its name. All
	// three are what a caller may write, and all three must land on the
	// same row.
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

	// AND A MULTI-VALUED FIELD RESOLVES EVERY MEMBER, not just the first.
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
