package tracker_test

import (
	"database/sql"
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"

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
		// THE REFUSAL NAMES WHAT THE TYPE DOES ADMIT, which is what a
		// caller needs: told only that `contains` is wrong they guess
		// again, and every guess is another round.
		"contains on a number": {"f.effort", "contains:8",
			"a number takes eq, ne, lt, lte, gt, gte, range"},
		// AND AN `eq` ON A SET is refused too, because the value is one
		// row per option: the comparison would ask whether the set
		// CONTAINS one while looking like it asked whether it IS it.
		"eq on a label set": {"f.areas", "eq:api", "a labels takes any, all"},
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

// THE SET OPERATORS ARE WHAT A MULTI-VALUED FIELD IS ACTUALLY ASKED ABOUT.
//
// A `labels` field holds several options at once, stored one row per option,
// so `eq` against it is a question with no useful answer — it matches a task
// whose set contains that option while looking like it asked whether the set
// IS it. `any`, `all`, `not_any` and `not_all` are the four questions there
// are, and until they existed only the first was reachable and only by
// accident.
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
		// ANY is "holds at least one of these".
		"any of two": {"any:api,ui", []string{"both", "api-only", "ui-only"}},
		"any of one": {"any:api", []string{"both", "api-only"}},
		// ALL is "holds every one of these", which is a COUNT over the
		// set rather than an intersection — written as chained clauses
		// it would be one subquery per member for a question one answers.
		"all of two": {"all:api,ui", []string{"both"}},
		"all of one": {"all:ui", []string{"both", "ui-only"}},
		// NOT_ANY is "holds NONE of these", never "some row is not one
		// of these" — the second is true of `both` for either option.
		//
		// AND A TASK THAT SET THE FIELD TO NOTHING HOLDS NONE OF THEM,
		// so it is in: an unset field is the absence of a row, and every
		// negation here is over the membership as a whole rather than
		// over the rows that happen to exist. That is the same reading
		// `ne` takes beside it, and the alternative — silently requiring
		// the field to be set — would make `not_any` and `null` overlap
		// in a way nobody could see.
		"not any": {"not_any:api", []string{"ui-only", "neither"}},
		// NOT_ALL is "is missing at least one of these", which a task
		// with none of them certainly is.
		"not all": {"not_all:api,ui", []string{"api-only", "ui-only", "neither"}},
		// AND THE BARE FORM IS `any`, because a caller writing a value
		// is naming one rather than claiming the set IS it.
		"the bare form": {"api", []string{"both", "api-only"}},
	} {
		t.Run(name, func(t *testing.T) {
			assertIDs(t, ids(r.ask(map[string]any{
				"container": "project:ENG", "f.areas": tc.filter,
			})), tc.want)
		})
	}

	// A SET COMPARISON WITH NOTHING IN IT matches nothing and reads as a
	// field nobody has set, so it is refused rather than answered.
	if err := r.askErr(map[string]any{
		"container": "project:ENG", "f.areas": "any:",
	}); err == nil {
		t.Error("`any:` with no values was answered")
	}
}

// `range` IS TWO BOUNDS IN ONE FILTER, and the pairing is what a caller gets
// wrong: a `gte` alone on a date field is the single most common way to ask
// for "this quarter" and get everything since it.
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

	// STARTSWITH IS A PREFIX, which `contains` cannot express — and the
	// difference is not cosmetic: a prefix is a RANGE over the column and
	// an index can serve it, where a leading `%` can only be scanned.
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

	// `in` IS `any` OVER A SINGLE-VALUED COLUMN, spelled separately
	// because the two read differently on the surfaces they apply to.
	assertIDs(t, ids(r.ask(map[string]any{
		"container": "project:ENG", "f.owner": "in:platform,storage",
	})), []string{"small", "large"})

	// A RANGE WITH ONE END is `gte` or `lte` and says so, rather than
	// silently comparing against the literal "3..".
	if err := r.askErr(map[string]any{
		"container": "project:ENG", "f.effort": "range:3",
	}); err == nil {
		t.Error("a one-ended range was answered")
	}
}

// A URL IS A VALUE, NOT AN OPERATOR CALL.
//
// Cutting on the first colon read the SCHEME of every URL as an operator, so
// a `url` field could not be filtered by its value at all — and the refusal
// named `https` as though the caller had typed a comparison.
func TestAUrlValueIsNotReadAsAnOperator(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-url", []tracker.FieldDef{
		{ID: "f-home", Slug: "home", Name: "Homepage", Type: tracker.FieldURL},
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

	// AND A TYPO IS STILL REFUSED. The `//` is the whole exception, so
	// anything else with a colon is carried through as an operator and
	// named in the refusal rather than silently answered as a value
	// nobody holds.
	if err := r.askErr(map[string]any{
		"container": "project:ENG", "f.home": "roughly:example",
	}); err == nil {
		t.Error("a typo'd operator was answered as a value")
	}
}

// `me` IS RESOLVED BY THE SURFACE, before the query is parsed.
//
// A saved view carrying `me` means whoever opens it, which is only true if the
// substitution happens per read — and a query that reached the compiler with
// the literal would match the tasks whose people field holds somebody called
// "me".
func TestTheViewerKeywordIsResolvedPerRead(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-people", []tracker.FieldDef{
		{ID: "f-rev", Slug: "reviewers", Name: "Reviewers", Type: tracker.FieldPeople},
	}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()
	seedWithFields(t, r, "hers", map[string]any{"f-rev": []string{"ana"}})
	seedWithFields(t, r, "his", map[string]any{"f-rev": []string{"bob"}})

	for viewer, want := range map[string]string{"ana": "hers", "bob": "his"} {
		q, err := r.reader.ExpandedQuery(t.Context(), map[string]any{
			"container": "project:ENG", "f.reviewers": "me",
		}, tracker.Viewer{Handle: viewer}, wednesday, berlin)
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

	// AND WITH NOBODY it is refused naming the key, never answered: a
	// read that cannot say whose "me" it is would answer everybody's.
	if _, err := r.reader.ExpandedQuery(t.Context(), map[string]any{
		"container": "project:ENG", "f.reviewers": "me",
	}, tracker.Viewer{}, wednesday, berlin); err == nil {
		t.Error("f.reviewers=me with no viewer was answered")
	}
}

// A DETAIL READ ANSWERS IN WORDS, not in field identifiers.
//
// The document keys values by field ID and carries opaque JSON per entry,
// which is the right shape for a record and the wrong one for an answer: the
// WRITE side takes an id, a slug or a name interchangeably, and the read side
// handed back the one of the three a caller cannot interpret without spending
// a second call on the catalogue.
//
// The two non-live states travel with it, because only a reader can be told
// about them: `hidden` is a value whose declaration was archived — kept on the
// task, out of every filter — and a value whose field this company declares
// nowhere stays on the document with no row at all, which is a third thing
// again and reported as exactly that.
func TestADetailReadNamesEachFieldAndItsState(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedFields(t, r)
	seedWithFields(t, r, "t-1", map[string]any{
		"f-effort":  3,
		"f-ghostly": "a field nobody declared",
	})

	// AND ONE OF THEM IS ARCHIVED AFTER THE FACT, which is the state the
	// archive's one-way rule creates and nothing could read back.
	if _, err := r.writer.WriteFields(t.Context(), "op-archive", []tracker.FieldDef{
		{ID: "f-effort", Slug: "effort", Name: "Effort",
			Type: tracker.FieldNumber, Archived: true},
	}); err != nil {
		t.Fatalf("archive the field: %v", err)
	}
	r.drain()

	detail, err := r.reader.Task(t.Context(), "t-1",
		tracker.DetailWants{Fields: true}, statelog.ReadStale)
	if err != nil {
		t.Fatalf("Task: %v", err)
	}
	if len(detail.Fields) != 2 {
		t.Fatalf("the read answered %d field values for a task carrying two: %+v",
			len(detail.Fields), detail.Fields)
	}
	byID := map[string]tracker.FieldValue{}
	for _, value := range detail.Fields {
		byID[value.ID] = value
	}

	effort := byID["f-effort"]
	if effort.Slug != "effort" || effort.Name != "Effort" {
		t.Errorf("the declared value came back as %+v — a caller reading an "+
			"id it cannot interpret has to spend a second call on the "+
			"catalogue to write the same value back", effort)
	}
	if effort.Type != tracker.FieldNumber {
		t.Errorf("the value's type is %q and its declaration says %q",
			effort.Type, tracker.FieldNumber)
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
