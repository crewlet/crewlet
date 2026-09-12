package tracker_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// TestTagSlugIsOneSpelling protects what a filter compares. A tag reaches the
// rows through one column and a filter compares it exactly, so every way of
// typing one grouping has to arrive as one slug.
func TestTagSlugIsOneSpelling(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"regression", "regression"},
		{"Regression", "regression"},
		{"  Regression  ", "regression"},
		{"needs design", "needs-design"},
		{"needs  design", "needs-design"},
		{"needs_design", "needs-design"},
		{"needs.design", "needs-design"},
		{"v2-api", "v2-api"},
		{"-leading-", "leading"},
		{"NEEDS — DESIGN", "needs-design"},
		{"!!!", ""},
	} {
		if got := tracker.TagSlug(c.raw); got != c.want {
			t.Errorf("TagSlug(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}

// TestTagSlugNeverTruncates is the other half of the rule at the definition: a
// cut slug is a DIFFERENT tag that looks like the one somebody asked for, so
// the normaliser hands the long one on and the write refuses it by name.
func TestTagSlugNeverTruncates(t *testing.T) {
	long := strings.Repeat("a", 200)
	if got := tracker.TagSlug(long); got != long {
		t.Fatalf("TagSlug cut a long slug to %d characters", len(got))
	}
	if tracker.ValidTagSlug(long) {
		t.Fatal("a 200-character slug passed the grammar, so nothing refuses it")
	}
}

// TestTagAddIsOpenToEverySeat and the two below are the authority rule, which
// is the whole reason the tag set is its own object.
func TestTagAddIsOpenToEverySeat(t *testing.T) {
	r := newRoundTrip(t)
	if _, err := r.writer.WriteTags(t.Context(), "op-add", "ENG",
		tracker.TagEdit{Add: []tracker.Tag{{Slug: "regression"}}},
		tracker.TagAuthority{}); err != nil {

		t.Fatalf("a seat could not declare a tag: %v", err)
	}
	r.drain()
	if got := r.tagSlugs("ENG"); !slices.Equal(got, []string{"regression"}) {
		t.Fatalf("tags = %v, want [regression]", got)
	}
}

func TestTagRenameAndArchiveAreTheLeads(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("api", "apis")

	for _, c := range []struct {
		name string
		edit tracker.TagEdit
	}{
		{"rename", tracker.TagEdit{Rename: map[string]string{"api": "API"}}},
		{"archive", tracker.TagEdit{Archive: []string{"api"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := r.writer.WriteTags(t.Context(), "op-"+c.name, "ENG",
				c.edit, tracker.TagAuthority{})
			if err == nil {
				t.Fatal("a seat that does not lead ENG was allowed to " +
					c.name + " one of its tags")
			}
			// THE REFUSAL NAMES WHO CAN, which is the only part of it a
			// seat can act on.
			if !strings.Contains(err.Error(), "lead") {
				t.Fatalf("the refusal does not name the lead: %v", err)
			}
			if _, err := r.writer.WriteTags(t.Context(), "op-lead-"+c.name,
				"ENG", c.edit, tracker.TagAuthority{Lead: true}); err != nil {

				t.Fatalf("the lead could not %s: %v", c.name, err)
			}
			r.drain()
		})
	}
}

// TestTagArchiveIsOneWay is the rule a field's archive follows, and for the
// same reason: a tag that came back would re-admit work filed against a
// meaning nobody has looked at in a year.
func TestTagArchiveIsOneWayForNewWork(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("legacy")
	if _, err := r.writer.WriteTags(t.Context(), "op-archive", "ENG",
		tracker.TagEdit{Archive: []string{"legacy"}},
		tracker.TagAuthority{Lead: true}); err != nil {

		t.Fatalf("archive: %v", err)
	}
	r.drain()

	task := newTask("t-archived-tag")
	task.Key = ""
	task.Tags = []string{"legacy"}
	_, err := r.writer.CreateTask(t.Context(), "op-create-archived", task, nil)
	if err == nil {
		t.Fatal("a create filed new work under an archived tag")
	}
	if !strings.Contains(err.Error(), "archived") {
		t.Fatalf("the refusal does not say the tag is archived: %v", err)
	}
}

// TestUndeclaredTagIsRefused is the rule that did not exist: any string a
// model invented became a tag row, which is how `Bug`, `bugfix` and `BUG` came
// to sit beside `bug` in the TYPE catalogue before its own check existed.
func TestUndeclaredTagIsRefused(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("regression")

	task := newTask("t-typo")
	task.Key = ""
	task.Tags = []string{"regresion"}
	_, err := r.writer.CreateTask(t.Context(), "op-typo", task, nil)
	if err == nil {
		t.Fatal("a create invented a tag the project never declared")
	}
	// THE REFUSAL CARRIES BOTH WAYS OUT and the near miss, because the
	// caller is usually a model with one round left.
	for _, want := range []string{"regresion", "regression",
		"write_project(tags.add)", "labels_create_missing"} {

		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// TestDeclaredTagIsFiled is the control: the same write with the tag declared
// has to land, or the case above is passing for the wrong reason.
func TestDeclaredTagIsFiled(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("regression")

	task := newTask("t-declared")
	task.Key = ""
	task.Tags = []string{"Regression"}
	if _, err := r.writer.CreateTask(t.Context(), "op-declared", task, nil); err != nil {
		t.Fatalf("create with a declared tag: %v", err)
	}
	r.drain()
	detail, err := r.reader.Task(t.Context(), task.ID, tracker.DetailWants{},
		statelog.ReadSession)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	// NORMALISED ON THE WAY IN, so the row carries the one spelling a
	// filter compares.
	if !slices.Equal(detail.Task.Tags, []string{"regression"}) {
		t.Fatalf("tags = %v, want [regression]", detail.Task.Tags)
	}
}

// TestUpdateRefusesAnUndeclaredTag covers the other write path, which had the
// same hole: a patch's tags replace the set whole.
func TestUpdateRefusesAnUndeclaredTag(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("regression")
	task := r.createTask("A task")

	labels := []string{"invented"}
	_, err := r.writer.UpdateTask(t.Context(), "op-patch-tags", task.ID, "ENG",
		tracker.NoIfMatch, tracker.TaskPatch{Tags: &labels}, nil)
	if err == nil {
		t.Fatal("an update invented a tag the project never declared")
	}
}

// TestEnsureTagsDeclaresOnlyWhatIsMissing is the inline half, and the property
// that matters is that it is ONE append: a record per new tag would contend N
// times on the one subject every seat in the project writes.
func TestEnsureTagsDeclaresOnlyWhatIsMissing(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("regression")

	created, _, err := r.writer.EnsureTags(t.Context(), "op-ensure", "ENG",
		[]string{"regression", "flaky", "perf"})
	if err != nil {
		t.Fatalf("EnsureTags: %v", err)
	}
	if !slices.Equal(created, []string{"flaky", "perf"}) {
		t.Fatalf("created = %v, want [flaky perf]", created)
	}
	r.drain()
	if got := r.tagSlugs("ENG"); !slices.Equal(got, []string{"flaky", "perf", "regression"}) {
		t.Fatalf("tags = %v", got)
	}
	// AND A SECOND PASS DECLARES NOTHING, which is what makes a retry of
	// a whole create free rather than a second record.
	created, _, err = r.writer.EnsureTags(t.Context(), "op-ensure-2", "ENG",
		[]string{"regression", "flaky"})
	if err != nil {
		t.Fatalf("EnsureTags again: %v", err)
	}
	if len(created) != 0 {
		t.Fatalf("a second pass created %v", created)
	}
}

// TestDeclaredIsWhatTheWriteActuallyCreATED — the list a caller is told it
// created comes from inside the snapshot that decided it. A list computed from
// a read before the write names tags somebody else declared a moment earlier,
// which is the ordinary case here rather than a race nobody hits.
func TestDeclaredIsWhatTheWriteActuallyCreated(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("regression")

	got, err := r.writer.WriteTags(t.Context(), "op-mixed", "ENG",
		tracker.TagEdit{Add: []tracker.Tag{
			{Slug: "regression"}, {Slug: "flaky"},
		}}, tracker.TagAuthority{})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !slices.Equal(got.Declared, []string{"flaky"}) {
		t.Fatalf("declared = %v, want only the one that did not exist",
			got.Declared)
	}
}

// TestTagLabelCollisionIsRefused is D43's rule: two tags a person cannot tell
// apart split the work between them at random. It covers BOTH spellings,
// because a label may collide with another tag's label or with its slug.
func TestTagLabelCollisionIsRefused(t *testing.T) {
	r := newRoundTrip(t)
	if _, err := r.writer.WriteTags(t.Context(), "op-seed", "ENG",
		tracker.TagEdit{Add: []tracker.Tag{{Slug: "api", Label: "Public API"}}},
		tracker.TagAuthority{}); err != nil {

		t.Fatalf("seed: %v", err)
	}
	r.drain()

	// EACH CASE ISOLATES ONE SPELLING. The seed's label and slug are
	// deliberately different words, so a collision on one cannot pass for
	// a collision on the other.
	for _, c := range []struct{ name, slug, label string }{
		{"same label, different case", "rest-api", "public api"},
		{"label is another tag's slug", "rest", "API"},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := r.writer.WriteTags(t.Context(), "op-"+c.slug, "ENG",
				tracker.TagEdit{Add: []tracker.Tag{{Slug: c.slug, Label: c.label}}},
				tracker.TagAuthority{})
			if err == nil {
				t.Fatalf("%s was accepted", c.name)
			}
		})
	}
}

// TestTagAddIsIdempotent is the ordinary case two seats meet on one afternoon.
// Refusing the second would make a tag set something every seat has to read
// before it can file anything.
func TestTagAddIsIdempotent(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("regression")
	before := r.tagsVersion("ENG")
	if _, err := r.writer.WriteTags(t.Context(), "op-again", "ENG",
		tracker.TagEdit{Add: []tracker.Tag{{Slug: "regression"}}},
		tracker.TagAuthority{}); err != nil {

		t.Fatalf("a repeated add was refused: %v", err)
	}
	r.drain()
	if after := r.tagsVersion("ENG"); after != before {
		t.Fatalf("tags_version moved %d → %d on a no-op add", before, after)
	}
}

// TestNearTagWarnsAndNeverRefuses is the advisory rule stated at the constant:
// a refusal with no override would block `apis` behind `api` for ever, and a
// lead can merge two tags.
func TestNearTagWarnsAndNeverRefuses(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("api")

	got, err := r.writer.WriteTags(t.Context(), "op-near", "ENG",
		tracker.TagEdit{Add: []tracker.Tag{{Slug: "apis"}}},
		tracker.TagAuthority{})
	if err != nil {
		t.Fatalf("a near-duplicate was REFUSED rather than warned: %v", err)
	}
	r.drain()
	if len(got.Warnings) == 0 {
		t.Fatal("no warning about a slug one edit from an existing tag")
	}
	if !strings.Contains(got.Warnings[0], "api") {
		t.Fatalf("the warning does not name the near tag: %v", got.Warnings)
	}
	if slugs := r.tagSlugs("ENG"); !slices.Equal(slugs, []string{"api", "apis"}) {
		t.Fatalf("the near-duplicate was not declared: %v", slugs)
	}
}

// TestFarTagDoesNotWarn is that case's control: a warning on every add is a
// warning nobody reads.
func TestFarTagDoesNotWarn(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("api")

	got, err := r.writer.WriteTags(t.Context(), "op-far", "ENG",
		tracker.TagEdit{Add: []tracker.Tag{{Slug: "regression"}}},
		tracker.TagAuthority{})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("a tag six edits away warned: %v", got.Warnings)
	}
}

// TestTagsPerTaskIsCapped protects the one bound the plan names for a task's
// own set. A task filed under more groupings than this is not grouped.
func TestTagsPerTaskIsCapped(t *testing.T) {
	r := newRoundTrip(t)
	tags := make([]string, 0, tracker.MaxTagsPerTask+1)
	for i := range tracker.MaxTagsPerTask + 1 {
		tags = append(tags, "tag"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	if _, _, err := r.writer.EnsureTags(t.Context(), "op-many", "ENG", tags); err != nil {
		t.Fatalf("declare: %v", err)
	}
	r.drain()

	task := newTask("t-many-tags")
	task.Key = ""
	task.Tags = tags
	_, err := r.writer.CreateTask(t.Context(), "op-many-create", task, nil)
	if err == nil {
		t.Fatalf("a task carried %d tags and the cap is %d",
			len(tags), tracker.MaxTagsPerTask)
	}
}

// TestTagEditRefusesAnEmptyGesture keeps a record saying nothing off the log.
func TestTagEditRefusesAnEmptyGesture(t *testing.T) {
	r := newRoundTrip(t)
	if _, err := r.writer.WriteTags(t.Context(), "op-empty", "ENG",
		tracker.TagEdit{}, tracker.TagAuthority{Lead: true}); err == nil {

		t.Fatal("an edit that adds, renames and archives nothing was accepted")
	}
	if _, err := r.writer.WriteTags(t.Context(), "op-nokey", "",
		tracker.TagEdit{Add: []tracker.Tag{{Slug: "x"}}},
		tracker.TagAuthority{}); err == nil {

		t.Fatal("an edit naming no project was accepted")
	}
}

// TestRenameKeepsTheSlug is the immutability rule: the slug is what every
// task's row holds, so a rename that moved it would orphan them.
func TestRenameKeepsTheSlug(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("api")
	task := newTask("t-renamed-tag")
	task.Key = ""
	task.Tags = []string{"api"}
	if _, err := r.writer.CreateTask(t.Context(), "op-tagged", task, nil); err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()

	if _, err := r.writer.WriteTags(t.Context(), "op-rename", "ENG",
		tracker.TagEdit{Rename: map[string]string{"api": "Public API"}},
		tracker.TagAuthority{Lead: true}); err != nil {

		t.Fatalf("rename: %v", err)
	}
	r.drain()
	if got := r.tagSlugs("ENG"); !slices.Equal(got, []string{"api"}) {
		t.Fatalf("the rename moved the slug: %v", got)
	}
	detail, err := r.reader.Task(t.Context(), task.ID, tracker.DetailWants{},
		statelog.ReadSession)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if !slices.Equal(detail.Task.Tags, []string{"api"}) {
		t.Fatalf("the task's tags moved with the label: %v", detail.Task.Tags)
	}
}

// TestRenameOfAnUnknownSlugIsRefused, because a rename that silently declared
// the tag would be an add wearing a rename's clothes — and a lead's gesture
// would then create what a seat's typo could not.
func TestRenameOfAnUnknownSlugIsRefused(t *testing.T) {
	r := newRoundTrip(t)
	r.declareTags("api")
	_, err := r.writer.WriteTags(t.Context(), "op-rename-unknown", "ENG",
		tracker.TagEdit{Rename: map[string]string{"nope": "Nope"}},
		tracker.TagAuthority{Lead: true})
	if err == nil {
		t.Fatal("a rename of a tag that does not exist was accepted")
	}
	if errors.Is(err, statelog.ErrExists) {
		t.Fatalf("the refusal is the wrong one: %v", err)
	}
}

// ---- helpers ------------------------------------------------------------ //

// declareTags seeds a project's tag set, so the cases that are not about
// declaring say so in one line.
func (r *roundTrip) declareTags(slugs ...string) {
	r.t.Helper()
	add := make([]tracker.Tag, 0, len(slugs))
	for _, slug := range slugs {
		add = append(add, tracker.Tag{Slug: slug})
	}
	if _, err := r.writer.WriteTags(r.t.Context(), "op-seed-tags", "ENG",
		tracker.TagEdit{Add: add}, tracker.TagAuthority{}); err != nil {

		r.t.Fatalf("seed the tags %v: %v", slugs, err)
	}
	r.drain()
}

// tagsVersion reads the exploded set's stamp, which is what a task's own tag
// rows carry: a set that changed without moving it would leave every task
// carrying the old spelling.
func (r *roundTrip) tagsVersion(project string) int {
	r.t.Helper()
	var version int
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(r.t.Context(),
			`SELECT tags_version FROM tracker_tagsets WHERE project_key = ?`,
			project).Scan(&version)
	}); err != nil {
		r.t.Fatalf("read %s's tags_version: %v", project, err)
	}
	return version
}

// tagSlugs reads a project's declared slugs back out of the rows.
func (r *roundTrip) tagSlugs(project string) []string {
	r.t.Helper()
	detail := r.project(tracker.ProjectDetailQuery{Project: project})
	out := make([]string, 0, len(detail.Tags))
	for _, tag := range detail.Tags {
		out = append(out, tag.Slug)
	}
	slices.Sort(out)
	return out
}
