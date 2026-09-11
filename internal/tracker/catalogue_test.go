package tracker_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

func (r *roundTrip) catalogue(q tracker.CatalogueQuery) tracker.CatalogueAnswer {
	r.t.Helper()
	if q.Level == "" {
		q.Level = statelog.ReadStale
	}
	answer, err := r.reader.Catalogue(r.t.Context(), q)
	if err != nil {
		r.t.Fatalf("Catalogue(%+v): %v", q, err)
	}
	return answer
}

func typeSlugs(in []tracker.TaskType) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		out = append(out, t.Slug)
	}
	return out
}

// A COMPANY HAS A TYPE CATALOGUE BEFORE IT DECLARES ONE.
//
// Every create names a type and is refused if the company does not declare it,
// so a catalogue that started empty would refuse the first task a fresh
// company ever filed. The builtins are what stop that, and they are unioned at
// READ time rather than seeded, because a seed is a write a fresh company has
// to succeed at before it can do anything.
func TestAFreshCompanyAlreadyHasItsTypes(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	got := typeSlugs(r.catalogue(tracker.CatalogueQuery{}).Types)
	for _, slug := range []string{"task", "bug", "epic"} {
		if !containsString(got, slug) {
			t.Fatalf("a fresh company's types are %v, want %s among them", got, slug)
		}
	}
	// AND `task` IS FIRST, because it is what every create that names no
	// type defaults to and a screen that buried it under six bespoke types
	// would hide the one everybody uses.
	if got[0] != "task" {
		t.Fatalf("the types start %v, want task first", got)
	}
}

// A DECLARED TYPE ADDS TO THE BUILTINS, and one sharing a slug RENAMES it.
//
// Replacing the set outright would let a catalogue omit `task` and break every
// create that did not name a type; and a builtin that vanished would leave the
// tasks already filed under it naming nothing.
func TestACompanysTypesAddToTheBuiltins(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteTypes(t.Context(), "op-types", []tracker.TaskType{
		{Slug: "incident", Name: "Incident", Plural: "Incidents"},
		{Slug: "bug", Name: "Defect", Plural: "Defects"},
	}); err != nil {
		t.Fatalf("WriteTypes: %v", err)
	}
	r.drain()

	answer := r.catalogue(tracker.CatalogueQuery{})
	got := typeSlugs(answer.Types)
	for _, slug := range []string{"task", "bug", "epic", "incident"} {
		if !containsString(got, slug) {
			t.Fatalf("the types are %v, want %s among them", got, slug)
		}
	}
	for _, entry := range answer.Types {
		if entry.Slug != "bug" {
			continue
		}
		if entry.Name != "Defect" {
			t.Fatalf("bug is named %q, want the company's own word", entry.Name)
		}
		// THE BUILTIN FLAG SURVIVES THE OVERRIDE: it says "this build
		// ships this slug", which is a fact about the engine rather
		// than about the company's wording.
		if !entry.Builtin {
			t.Fatal("a renamed builtin stopped saying it is one")
		}
	}
}

// A TASK NAMES A TYPE THE COMPANY DECLARES, and the tool has always said so.
//
// Nothing checked it, so any string a model invented became a type — `Bug`,
// `bugfix` and `BUG` filed three different types beside `bug`, and every board
// grouped and filtered on them as if they were real.
func TestATaskCannotInventItsOwnType(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	task := newTask("t-invented")
	task.Type = "bugfix"
	_, err := r.writer.CreateTask(t.Context(), "op-invented", task, nil)
	if err == nil {
		t.Fatal("a task invented its own type")
	}
	if !strings.Contains(err.Error(), "not a task type this company declares") {
		t.Fatalf("the refusal %q does not say what is wrong", err)
	}
	// AND IT NAMES THE SET, because a refusal a model cannot act on is a
	// refusal it retries verbatim.
	if !strings.Contains(err.Error(), "task") || !strings.Contains(err.Error(), "bug") {
		t.Fatalf("the refusal %q does not name the types that exist", err)
	}

	// A BUILTIN STILL WORKS, or the check would refuse everything.
	if _, err := r.writer.CreateTask(t.Context(), "op-ok", newTask("t-ok"), nil); err != nil {
		t.Fatalf("a builtin type was refused: %v", err)
	}
	r.drain()

	// AND SO DOES NAMING NONE. `create_work_item` has always told a model
	// "`task` if you are unsure", so a create that names no type files
	// under it rather than being refused for a type it did not choose.
	bare := newTask("t-bare")
	bare.Type = ""
	result, err := r.writer.CreateTask(t.Context(), "op-bare", bare, nil)
	if err != nil {
		t.Fatalf("a create naming no type was refused: %v", err)
	}
	r.drain()
	detail, err := r.reader.Task(t.Context(), result.Key, tracker.DetailWants{},
		statelog.ReadStale)
	if err != nil {
		t.Fatalf("read it back: %v", err)
	}
	if detail.Task.Type != tracker.DefaultTaskType {
		t.Fatalf("a create naming no type stored %q, want %s — an empty type "+
			"on the row is a task no filter and no board can group",
			detail.Task.Type, tracker.DefaultTaskType)
	}

	// AND AN ARCHIVED TYPE TAKES NO NEW WORK, which is what archiving one
	// is for — the tasks already under it keep it.
	if _, err := r.writer.WriteTypes(t.Context(), "op-archive", []tracker.TaskType{
		{Slug: "spike", Name: "Spike", Archived: true},
	}); err != nil {
		t.Fatalf("WriteTypes: %v", err)
	}
	r.drain()
	spiked := newTask("t-spike")
	spiked.Type = "spike"
	_, err = r.writer.CreateTask(t.Context(), "op-spike", spiked, nil)
	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("an archived type took new work: %v", err)
	}
}

// A FIELD'S ARCHIVE IS ONE-WAY, and the declaration is where that is enforced.
//
// A field that came back with its old id would silently re-admit values
// validated against a definition nobody has seen for a year.
func TestAFieldsArchiveCannotBeUndone(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	field := tracker.FieldDef{
		ID: "f-1", Slug: "impact", Name: "Impact", Type: tracker.FieldText,
	}
	if _, err := r.writer.WriteFields(t.Context(), "op-f1",
		[]tracker.FieldDef{field}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()

	archived := field
	archived.Archived = true
	if _, err := r.writer.WriteFields(t.Context(), "op-f2",
		[]tracker.FieldDef{archived}); err != nil {
		t.Fatalf("archive it: %v", err)
	}
	r.drain()

	_, err := r.writer.WriteFields(t.Context(), "op-f3", []tracker.FieldDef{field})
	if err == nil {
		t.Fatal("an archived field came back under its own id")
	}
	if !errors.Is(err, statelog.ErrConflict) {
		t.Fatalf("the refusal is %v, want an ErrConflict a caller can branch on", err)
	}
	if !strings.Contains(err.Error(), "new id") {
		t.Fatalf("the refusal %q does not say how to restore the field", err)
	}

	// A NEW ID IS THE WAY BACK, and it works.
	restored := field
	restored.ID = "f-2"
	if _, err := r.writer.WriteFields(t.Context(), "op-f4",
		[]tracker.FieldDef{archived, restored}); err != nil {
		t.Fatalf("a new declaration was refused: %v", err)
	}
}

// THE POLICY VERSION MOVES ON EVERY FIELDS EDIT, derived rather than sent.
//
// It is what a task's policy stamp records having validated against, so a
// version a writer chose is one two writers can choose alike.
func TestAFieldsEditMovesThePolicyVersion(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	var last int
	for i, slug := range []string{"one", "two", "three"} {
		if _, err := r.writer.WriteFields(t.Context(), "op-"+slug,
			[]tracker.FieldDef{{
				ID: "f-" + slug, Slug: slug, Name: slug, Type: tracker.FieldText,
			}}); err != nil {
			t.Fatalf("WriteFields: %v", err)
		}
		r.drain()
		got := r.catalogue(tracker.CatalogueQuery{}).PolicyVersion
		if got != i+1 {
			t.Fatalf("after %d edits the policy version is %d, want %d",
				i+1, got, i+1)
		}
		last = got
	}
	// AND A TYPES EDIT DOES NOT MOVE IT: the two are separate subjects
	// precisely so an unrelated edit does not invalidate every task's
	// policy stamp.
	if _, err := r.writer.WriteTypes(t.Context(), "op-types",
		[]tracker.TaskType{{Slug: "incident", Name: "Incident"}}); err != nil {
		t.Fatalf("WriteTypes: %v", err)
	}
	r.drain()
	if got := r.catalogue(tracker.CatalogueQuery{}).PolicyVersion; got != last {
		t.Fatalf("a types edit moved the field policy version to %d, was %d",
			got, last)
	}
}

// A CATALOGUE NOTHING COULD BE VALIDATED AGAINST IS REFUSED.
func TestACatalogueThatCouldNotValidateIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for name, tc := range map[string]struct {
		types  []tracker.TaskType
		fields []tracker.FieldDef
		want   string
	}{
		"a type with no slug": {
			types: []tracker.TaskType{{Name: "Nameless"}}, want: "has no slug"},
		"a slug that is not one": {
			types: []tracker.TaskType{{Slug: "Not A Slug", Name: "x"}},
			want:  "is not a slug"},
		"two types under one slug": {
			types: []tracker.TaskType{
				{Slug: "incident", Name: "One"}, {Slug: "incident", Name: "Two"},
			}, want: "twice"},
		"a field with no id": {
			fields: []tracker.FieldDef{{Slug: "impact", Name: "Impact",
				Type: tracker.FieldText}}, want: "has no id"},
		"two fields under one slug": {
			fields: []tracker.FieldDef{
				{ID: "a", Slug: "impact", Name: "A", Type: tracker.FieldText},
				{ID: "b", Slug: "impact", Name: "B", Type: tracker.FieldText},
			}, want: "would resolve to whichever row"},
		"a field type that is not one": {
			fields: []tracker.FieldDef{{ID: "a", Slug: "impact", Name: "A",
				Type: "colour"}}, want: "not a field type"},
		"options on a type that has none": {
			fields: []tracker.FieldDef{{ID: "a", Slug: "impact", Name: "A",
				Type: tracker.FieldText, Config: tracker.FieldConfig{
					Options: []tracker.Option{{ID: "o", Slug: "high", Name: "High"}},
				}}}, want: "only dropdown"},
		"two options under one slug": {
			fields: []tracker.FieldDef{{ID: "a", Slug: "impact", Name: "A",
				Type: tracker.FieldDropdown, Config: tracker.FieldConfig{
					Options: []tracker.Option{
						{ID: "o1", Slug: "high", Name: "High"},
						{ID: "o2", Slug: "high", Name: "Higher"},
					},
				}}}, want: "declares option"},
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			if tc.types != nil {
				_, err = r.writer.WriteTypes(t.Context(), "op-"+name, tc.types)
			} else {
				_, err = r.writer.WriteFields(t.Context(), "op-"+name, tc.fields)
			}
			if err == nil {
				t.Fatal("a catalogue nothing could validate against was saved")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal %q does not say %q", err, tc.want)
			}
		})
	}

	// THE CONTROLS, without which every case above would pass against
	// verbs that refused everything.
	if _, err := r.writer.WriteTypes(t.Context(), "op-control-types",
		[]tracker.TaskType{{Slug: "incident", Name: "Incident"}}); err != nil {
		t.Fatalf("the control type catalogue was refused: %v", err)
	}
	if _, err := r.writer.WriteFields(t.Context(), "op-control-fields",
		[]tracker.FieldDef{{ID: "a", Slug: "impact", Name: "Impact",
			Type: tracker.FieldDropdown, Config: tracker.FieldConfig{
				Options: []tracker.Option{{ID: "o1", Slug: "high", Name: "High"}},
			}}}); err != nil {
		t.Fatalf("the control field catalogue was refused: %v", err)
	}

	// AND A READ WITH NO LEVEL IS REFUSED, like every other read here.
	if _, err := r.reader.Catalogue(t.Context(), tracker.CatalogueQuery{}); err == nil {
		t.Fatal("a catalogue read with no read level was answered")
	}
}

// ARCHIVED ENTRIES ARE OUT OF A CATALOGUE READ UNLESS ASKED FOR.
//
// A form offering choices must not offer a type nobody may file under, and a
// screen auditing the catalogue must be able to see what was retired.
func TestAnArchivedEntryIsOutOfTheDefaultCatalogue(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteTypes(t.Context(), "op-types", []tracker.TaskType{
		{Slug: "incident", Name: "Incident"},
		{Slug: "retired", Name: "Retired", Archived: true},
	}); err != nil {
		t.Fatalf("WriteTypes: %v", err)
	}
	r.drain()

	if got := typeSlugs(r.catalogue(tracker.CatalogueQuery{}).Types); containsString(got, "retired") {
		t.Fatalf("an archived type is in the default catalogue: %v", got)
	}
	got := typeSlugs(r.catalogue(tracker.CatalogueQuery{Archived: true}).Types)
	if !containsString(got, "retired") {
		t.Fatalf("an archived type is unreachable even when asked for: %v", got)
	}
}
