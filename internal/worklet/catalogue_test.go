package worklet_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

func (r *roundTrip) catalogue(q worklet.CatalogueQuery) worklet.CatalogueAnswer {
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

func typeSlugs(in []worklet.TaskType) []string {
	out := make([]string, 0, len(in))
	for _, t := range in {
		out = append(out, t.Slug)
	}
	return out
}

// A COMPANY HAS A TYPE CATALOGUE BEFORE IT DECLARES ONE.
//
// A create naming an undeclared type is refused, so an empty catalogue would
// refuse a fresh company's first task. The builtins are unioned at READ time
// rather than seeded, because a seed is a write a fresh company would have to
// succeed at before it could do anything.
func TestAFreshCompanyAlreadyHasItsTypes(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	got := typeSlugs(r.catalogue(worklet.CatalogueQuery{}).Types)
	for _, slug := range []string{"task", "bug", "epic"} {
		if !containsString(got, slug) {
			t.Fatalf("a fresh company's types are %v, want %s among them", got, slug)
		}
	}
	// AND `task` IS FIRST: every create naming no type defaults to it, and
	// a screen must not bury it under bespoke types.
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

	if _, err := r.writer.WriteTypes(t.Context(), "op-types", []worklet.TaskType{
		{Slug: "incident", Name: "Incident", Plural: "Incidents"},
		{Slug: "bug", Name: "Defect", Plural: "Defects"},
	}); err != nil {
		t.Fatalf("WriteTypes: %v", err)
	}
	r.drain()

	answer := r.catalogue(worklet.CatalogueQuery{})
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
		// THE BUILTIN FLAG SURVIVES THE OVERRIDE: "this build ships this
		// slug" is a fact about the engine, not the company's wording.
		if !entry.Builtin {
			t.Fatal("a renamed builtin stopped saying it is one")
		}
	}
}

// A TASK NAMES A TYPE THE COMPANY DECLARES: unchecked, `Bug`, `bugfix` and
// `BUG` would be three more types beside `bug`, every board grouping and
// filtering on them as if they were real.
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
	// AND IT NAMES THE SET: a refusal a model cannot act on is one it
	// retries verbatim.
	if !strings.Contains(err.Error(), "task") || !strings.Contains(err.Error(), "bug") {
		t.Fatalf("the refusal %q does not name the types that exist", err)
	}

	// A BUILTIN STILL WORKS, or the check would refuse everything.
	if _, err := r.writer.CreateTask(t.Context(), "op-ok", newTask("t-ok"), nil); err != nil {
		t.Fatalf("a builtin type was refused: %v", err)
	}
	r.drain()

	// AND SO DOES NAMING NONE: `create_work_item` tells a model "`task` if
	// you are unsure", so a create naming no type files under it.
	bare := newTask("t-bare")
	bare.Type = ""
	result, err := r.writer.CreateTask(t.Context(), "op-bare", bare, nil)
	if err != nil {
		t.Fatalf("a create naming no type was refused: %v", err)
	}
	r.drain()
	detail, err := r.reader.Task(t.Context(), result.Key, worklet.DetailWants{},
		statelog.Freshness{Level: statelog.ReadStale})
	if err != nil {
		t.Fatalf("read it back: %v", err)
	}
	if detail.Task.Type != worklet.DefaultTaskType {
		t.Fatalf("a create naming no type stored %q, want %s — an empty type "+
			"on the row is a task no filter and no board can group",
			detail.Task.Type, worklet.DefaultTaskType)
	}

	// AND AN ARCHIVED TYPE TAKES NO NEW WORK; tasks already under it keep it.
	if _, err := r.writer.WriteTypes(t.Context(), "op-archive", []worklet.TaskType{
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

// A FIELD'S ARCHIVE IS ONE-WAY, enforced at the declaration: a field back
// under its old id would silently re-admit values validated against a
// definition nobody has seen for a year.
func TestAFieldsArchiveCannotBeUndone(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	field := worklet.FieldDef{
		ID: "f-1", Slug: "impact", Name: "Impact", Type: worklet.FieldText,
	}
	if _, err := r.writer.WriteFields(t.Context(), "op-f1",
		[]worklet.FieldDef{field}); err != nil {
		t.Fatalf("WriteFields: %v", err)
	}
	r.drain()

	archived := field
	archived.Archived = true
	if _, err := r.writer.WriteFields(t.Context(), "op-f2",
		[]worklet.FieldDef{archived}); err != nil {
		t.Fatalf("archive it: %v", err)
	}
	r.drain()

	_, err := r.writer.WriteFields(t.Context(), "op-f3", []worklet.FieldDef{field})
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
		[]worklet.FieldDef{archived, restored}); err != nil {
		t.Fatalf("a new declaration was refused: %v", err)
	}
}

// THE POLICY VERSION MOVES ON EVERY FIELDS EDIT, derived rather than sent: a
// task's policy stamp records it, and a version writers chose is one two
// writers can choose alike.
func TestAFieldsEditMovesThePolicyVersion(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	var last int
	for i, slug := range []string{"one", "two", "three"} {
		if _, err := r.writer.WriteFields(t.Context(), "op-"+slug,
			[]worklet.FieldDef{{
				ID: "f-" + slug, Slug: slug, Name: slug, Type: worklet.FieldText,
			}}); err != nil {
			t.Fatalf("WriteFields: %v", err)
		}
		r.drain()
		got := r.catalogue(worklet.CatalogueQuery{}).PolicyVersion
		if got != i+1 {
			t.Fatalf("after %d edits the policy version is %d, want %d",
				i+1, got, i+1)
		}
		last = got
	}
	// AND A TYPES EDIT DOES NOT MOVE IT: separate subjects, so an unrelated
	// edit does not invalidate every task's policy stamp.
	if _, err := r.writer.WriteTypes(t.Context(), "op-types",
		[]worklet.TaskType{{Slug: "incident", Name: "Incident"}}); err != nil {
		t.Fatalf("WriteTypes: %v", err)
	}
	r.drain()
	if got := r.catalogue(worklet.CatalogueQuery{}).PolicyVersion; got != last {
		t.Fatalf("a types edit moved the field policy version to %d, was %d",
			got, last)
	}
}

// A CATALOGUE NOTHING COULD BE VALIDATED AGAINST IS REFUSED.
func TestACatalogueThatCouldNotValidateIsRefused(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for name, tc := range map[string]struct {
		types  []worklet.TaskType
		fields []worklet.FieldDef
		want   string
	}{
		"a type with no slug": {
			types: []worklet.TaskType{{Name: "Nameless"}}, want: "has no slug"},
		"a slug that is not one": {
			types: []worklet.TaskType{{Slug: "Not A Slug", Name: "x"}},
			want:  "is not a slug"},
		"two types under one slug": {
			types: []worklet.TaskType{
				{Slug: "incident", Name: "One"}, {Slug: "incident", Name: "Two"},
			}, want: "twice"},
		"a field with no id": {
			fields: []worklet.FieldDef{{Slug: "impact", Name: "Impact",
				Type: worklet.FieldText}}, want: "has no id"},
		"two fields under one slug": {
			fields: []worklet.FieldDef{
				{ID: "a", Slug: "impact", Name: "A", Type: worklet.FieldText},
				{ID: "b", Slug: "impact", Name: "B", Type: worklet.FieldText},
			}, want: "would resolve to whichever row"},
		"a field type that is not one": {
			fields: []worklet.FieldDef{{ID: "a", Slug: "impact", Name: "A",
				Type: "colour"}}, want: "not a field type"},
		"options on a type that has none": {
			fields: []worklet.FieldDef{{ID: "a", Slug: "impact", Name: "A",
				Type: worklet.FieldText, Config: worklet.FieldConfig{
					Options: []worklet.Option{{ID: "o", Slug: "high", Name: "High"}},
				}}}, want: "only dropdown"},
		"two options under one slug": {
			fields: []worklet.FieldDef{{ID: "a", Slug: "impact", Name: "A",
				Type: worklet.FieldDropdown, Config: worklet.FieldConfig{
					Options: []worklet.Option{
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

	// THE CONTROLS, or every case above would pass against verbs that
	// refused everything.
	if _, err := r.writer.WriteTypes(t.Context(), "op-control-types",
		[]worklet.TaskType{{Slug: "incident", Name: "Incident"}}); err != nil {
		t.Fatalf("the control type catalogue was refused: %v", err)
	}
	if _, err := r.writer.WriteFields(t.Context(), "op-control-fields",
		[]worklet.FieldDef{{ID: "a", Slug: "impact", Name: "Impact",
			Type: worklet.FieldDropdown, Config: worklet.FieldConfig{
				Options: []worklet.Option{{ID: "o1", Slug: "high", Name: "High"}},
			}}}); err != nil {
		t.Fatalf("the control field catalogue was refused: %v", err)
	}

	// AND A READ WITH NO LEVEL IS REFUSED, like every other read here.
	if _, err := r.reader.Catalogue(t.Context(), worklet.CatalogueQuery{}); err == nil {
		t.Fatal("a catalogue read with no read level was answered")
	}
}

// A FIELD RECORDS WHO DECLARED IT, and the WRITE decides that, not the
// document: these writes are WHOLE POST-STATES, so a document carrying its own
// provenance would let the next edit re-attribute a field somebody else
// declared, with nothing downstream able to tell.
func TestAFieldRecordsWhoDeclaredIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteFields(t.Context(), "op-declare",
		[]worklet.FieldDef{{ID: "f1", Slug: "impact", Name: "Impact",
			Type: worklet.FieldText}}); err != nil {
		t.Fatalf("declare a field: %v", err)
	}
	r.drain()
	declared := r.catalogue(worklet.CatalogueQuery{}).Fields[0]
	if declared.CreatedBy != "ana" {
		t.Errorf("created_by = %q, want the writer's own actor — a catalogue "+
			"that serves the column and never writes it tells every reader "+
			"nobody declared anything", declared.CreatedBy)
	}
	if !declared.CreatedAt.Equal(wednesday) {
		t.Errorf("created_at = %v, want the write's own clock %v",
			declared.CreatedAt, wednesday)
	}

	// A LATER EDIT KEEPS THEM, whatever the document says: this write
	// carries somebody else's name on the SAME field id.
	r.at = wednesday.Add(24 * time.Hour)
	if _, err := r.writer.WriteFields(t.Context(), "op-edit",
		[]worklet.FieldDef{
			{ID: "f1", Slug: "impact", Name: "Blast radius",
				Type: worklet.FieldText, CreatedBy: "mallory",
				CreatedAt: r.at},
			{ID: "f2", Slug: "effort", Name: "Effort", Type: worklet.FieldNumber},
		}); err != nil {
		t.Fatalf("edit the catalogue: %v", err)
	}
	r.drain()
	after := map[string]worklet.FieldDef{}
	for _, field := range r.catalogue(worklet.CatalogueQuery{}).Fields {
		after[field.ID] = field
	}
	if got := after["f1"]; got.CreatedBy != "ana" || !got.CreatedAt.Equal(wednesday) {
		t.Errorf("the edited field is now %q at %v — a later write "+
			"re-attributed a declaration somebody else made",
			got.CreatedBy, got.CreatedAt)
	}
	// AND A FIELD THE SNAPSHOT DID NOT HOLD IS NEW: this write's own actor
	// and clock.
	if got := after["f2"]; got.CreatedBy != "ana" || !got.CreatedAt.Equal(r.at) {
		t.Errorf("the new field was stamped %q at %v, want the second write's "+
			"own clock %v", got.CreatedBy, got.CreatedAt, r.at)
	}
}

// AND A RESUBMITTED DECLARATION IS STILL A NO-OP.
//
// A form submits every control, so a field list mostly arrives unchanged; a
// provenance stamp applied AFTER the comparison would make each differ by the
// two facts a caller never sends, and publish a record saying nothing.
func TestResubmittingAProjectsFieldsWritesNothing(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	fields := []worklet.FieldDef{{ID: "f1", Slug: "impact", Name: "Impact",
		Type: worklet.FieldText}}
	authority := worklet.ProjectAuthority{Policy: true}

	if _, err := r.writer.WriteProject(t.Context(), "op-fields", "ENG",
		worklet.ProjectEdit{Fields: &fields}, authority); err != nil {
		t.Fatalf("declare the project's fields: %v", err)
	}
	r.drain()
	first := r.project(worklet.ProjectDetailQuery{Project: "ENG"}).Version

	// THE SAME LIST AS FIRST SENT, without the facts the write stamped, as
	// a second form submission looks.
	again := []worklet.FieldDef{{ID: "f1", Slug: "impact", Name: "Impact",
		Type: worklet.FieldText}}
	if _, err := r.writer.WriteProject(t.Context(), "op-fields-again", "ENG",
		worklet.ProjectEdit{Fields: &again}, authority); err != nil {
		t.Fatalf("resubmit the project's fields: %v", err)
	}
	r.drain()
	if got := r.project(worklet.ProjectDetailQuery{Project: "ENG"}).Version; got != first {
		t.Errorf("the project moved from version %d to %d on a resubmission "+
			"that changed nothing", first, got)
	}
}

// ARCHIVED ENTRIES ARE OUT OF A CATALOGUE READ UNLESS ASKED FOR: a form must
// not offer a type nobody may file under, and an audit must see what retired.
func TestAnArchivedEntryIsOutOfTheDefaultCatalogue(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteTypes(t.Context(), "op-types", []worklet.TaskType{
		{Slug: "incident", Name: "Incident"},
		{Slug: "retired", Name: "Retired", Archived: true},
	}); err != nil {
		t.Fatalf("WriteTypes: %v", err)
	}
	r.drain()

	if got := typeSlugs(r.catalogue(worklet.CatalogueQuery{}).Types); containsString(got, "retired") {
		t.Fatalf("an archived type is in the default catalogue: %v", got)
	}
	got := typeSlugs(r.catalogue(worklet.CatalogueQuery{Archived: true}).Types)
	if !containsString(got, "retired") {
		t.Fatalf("an archived type is unreachable even when asked for: %v", got)
	}
}

// THE COMPANY'S OWN DECLARATIONS RIDE BESIDE THE EFFECTIVE SET, archived ones
// included whatever the read asked: a write of `types` replaces exactly
// these, so a screen that sent back the effective set would copy every
// builtin into the company's words, and one that sent back the live types
// alone would drop a retired declaration.
func TestACatalogueReadCarriesTheDeclarationsAWriteReplaces(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	declared := []worklet.TaskType{
		{Slug: "bug", Name: "Defect"},
		{Slug: "incident", Name: "Incident"},
		{Slug: "retired", Name: "Retired", Archived: true},
	}
	if _, err := r.writer.WriteTypes(t.Context(), "op-declared", declared); err != nil {
		t.Fatalf("WriteTypes: %v", err)
	}
	r.drain()

	answer := r.catalogue(worklet.CatalogueQuery{})
	if got := typeSlugs(answer.DeclaredTypes); !slices.Equal(got, []string{"bug", "incident", "retired"}) {
		t.Fatalf("declared types = %v, want exactly the company's three, the archived one included", got)
	}
	for _, d := range answer.DeclaredTypes {
		if d.Builtin {
			t.Errorf("declared type %q says builtin: the flag is the effective set's, and "+
				"sent back it would be written as the company's", d.Slug)
		}
	}
	if got := typeSlugs(answer.Types); !containsString(got, "task") {
		t.Errorf("the effective set lost the builtins: %v", got)
	}
}

// A MILESTONE IS A TASK: a committed date is an item with an owner, a due date
// and blockers, so everything the tracker does to a task is what it needs. A
// separate object would be a second thing to file, route, notify about and
// report on, differing from a task only in its name.
func TestAMilestoneIsABuiltinType(t *testing.T) {
	t.Parallel()
	slugs := map[string]worklet.TaskType{}
	for _, taskType := range worklet.BuiltinTypes() {
		slugs[taskType.Slug] = taskType
	}
	milestone, held := slugs["milestone"]
	if !held {
		t.Fatal("nothing can be filed as a milestone — the builtins are " +
			"what a company has before it declares anything, so a type that " +
			"is not among them is one a fresh company cannot use")
	}
	if !milestone.Builtin {
		t.Error("the milestone type does not say it is a builtin, so a " +
			"catalogue screen cannot tell it from one the company chose")
	}
	if milestone.Description == "" || milestone.Plural == "" {
		t.Errorf("the milestone type is %+v — a model chooses a type BY its "+
			"description, exactly as it chooses a status", milestone)
	}
}
