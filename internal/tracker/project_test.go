package tracker_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/tracker"
)

// TestProjectPolicyIsTheLeads is the authority rule. A project's field
// declarations decide how everybody's work in it is filed, which is not a call
// one seat makes for the team.
func TestProjectPolicyIsTheLeads(t *testing.T) {
	r := newRoundTrip(t)
	who := "alice"
	edit := tracker.ProjectEdit{DefaultAssignee: &who}

	if _, err := r.writer.WriteProject(t.Context(), "op-seat", "ENG", edit,
		tracker.ProjectAuthority{}); err == nil {

		t.Fatal("a seat that does not lead ENG set its default assignee")
	}
	if _, err := r.writer.WriteProject(t.Context(), "op-lead", "ENG", edit,
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("the lead could not set the default assignee: %v", err)
	}
	r.drain()
	if got := r.project(tracker.ProjectDetailQuery{Project: "ENG"}); got.DefaultAssignee != who {
		t.Fatalf("default assignee = %q, want %q", got.DefaultAssignee, who)
	}
}

// TestArchivingAProjectTakesAPerson is the second gate, and it is named
// separately because a lead who hits it DOES have authority over the other
// three facets — "no" would send them looking in the wrong place.
func TestArchivingAProjectTakesAPerson(t *testing.T) {
	r := newRoundTrip(t)
	archived := true
	edit := tracker.ProjectEdit{Archived: &archived}

	_, err := r.writer.WriteProject(t.Context(), "op-lead-archive", "ENG", edit,
		tracker.ProjectAuthority{Lead: true})
	if err == nil {
		t.Fatal("a lead archived a project without a person's credential")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Fatalf("the refusal does not say what is missing: %v", err)
	}
	if _, err := r.writer.WriteProject(t.Context(), "op-operator-archive", "ENG",
		edit, tracker.ProjectAuthority{Lead: true, Operator: true}); err != nil {

		t.Fatalf("an operator could not archive a project: %v", err)
	}
	r.drain()

	// AND THE ARCHIVE IS WHAT IT CLAIMS: no new work is filed into it.
	task := newTask("t-into-archived")
	task.Key = ""
	if _, err := r.writer.CreateTask(t.Context(), "op-into-archived", task, nil); err == nil {
		t.Fatal("an archived project took new work")
	}
}

// TestPolicyVersionMovesOnlyForWhatATaskIsValidatedAgainst protects the stamp
// a task carries: it records which declarations the task was checked against,
// so a default assignee — which nothing is checked against — must not move it,
// and a field declaration must.
func TestPolicyVersionMovesOnlyForWhatATaskIsValidatedAgainst(t *testing.T) {
	r := newRoundTrip(t)
	who := "alice"
	if _, err := r.writer.WriteProject(t.Context(), "op-assignee", "ENG",
		tracker.ProjectEdit{DefaultAssignee: &who},
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("set the default assignee: %v", err)
	}
	r.drain()
	if got := r.project(tracker.ProjectDetailQuery{Project: "ENG"}); got.PolicyStamp != 0 {
		t.Fatalf("the policy stamp moved to %d for a default assignee, which "+
			"no task is validated against", got.PolicyStamp)
	}

	fields := []tracker.FieldDef{{
		ID: "f-sev", Slug: "severity", Name: "Severity", Type: tracker.FieldText,
	}}
	if _, err := r.writer.WriteProject(t.Context(), "op-fields", "ENG",
		tracker.ProjectEdit{Fields: &fields},
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("declare a field: %v", err)
	}
	r.drain()
	if got := r.project(tracker.ProjectDetailQuery{Project: "ENG"}); got.PolicyStamp == 0 {
		t.Fatal("the policy stamp did not move for a field declaration, so a " +
			"task's stamp records a policy it never saw")
	}
}

// TestAProjectsOwnFieldsReachItsReaders is what the declaration is FOR: a
// seat asks its project what it may file, and the answer is the workspace's
// vocabulary with the project's own declarations laid over it. A project
// declaration of an id the workspace also declares SHADOWS it and is named as
// shadowed, because that is what a field halfway through a move between the
// two scopes looks like and a reader that silently picked one would report
// whichever it read second.
func TestAProjectsOwnFieldsReachItsReaders(t *testing.T) {
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-workspace",
		[]tracker.FieldDef{{
			ID: "f-impact", Slug: "impact", Name: "Impact", Type: tracker.FieldText,
		}, {
			ID: "f-sev", Slug: "severity", Name: "Severity", Type: tracker.FieldText,
		}}); err != nil {

		t.Fatalf("declare the workspace's fields: %v", err)
	}
	r.drain()

	fields := []tracker.FieldDef{{
		ID: "f-sev", Slug: "urgency", Name: "Urgency",
		Type: tracker.FieldDropdown,
		Config: tracker.FieldConfig{Options: []tracker.Option{
			{ID: "o-1", Slug: "sev1", Name: "Sev 1"},
			{ID: "o-2", Slug: "sev2", Name: "Sev 2"},
		}},
	}}
	if _, err := r.writer.WriteProject(t.Context(), "op-fields", "ENG",
		tracker.ProjectEdit{Fields: &fields},
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("declare: %v", err)
	}
	r.drain()

	got := r.project(tracker.ProjectDetailQuery{Project: "ENG"})
	if !slices.Equal(got.Shadowed, []string{"f-sev"}) {
		t.Fatalf("the shadowed ids are %v, want [f-sev]", got.Shadowed)
	}
	declared := fieldSlugs(got.Fields)
	if !slices.Equal(declared, []string{"impact", "urgency"}) {
		t.Fatalf("the project declares %v, want [impact urgency]", declared)
	}
	if options := optionSlugs(got.Fields, "f-sev"); !slices.Equal(
		options, []string{"sev1", "sev2"}) {

		t.Fatalf("the project's field carries options %v", options)
	}

	// AND THE PROJECT'S CLEAR LEAVES THE WORKSPACE'S ALONE, which is what
	// makes the two scopes independent: a project's edit that cleared the
	// workspace's declarations would retire the company's whole vocabulary.
	shorter := []tracker.FieldDef{}
	if _, err := r.writer.WriteProject(t.Context(), "op-clear", "ENG",
		tracker.ProjectEdit{Fields: &shorter},
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("clear the project's fields: %v", err)
	}
	r.drain()

	got = r.project(tracker.ProjectDetailQuery{Project: "ENG"})
	if len(got.Shadowed) != 0 {
		t.Fatalf("a cleared project still shadows %v", got.Shadowed)
	}
	if declared := fieldSlugs(got.Fields); !slices.Equal(
		declared, []string{"impact", "severity"}) {

		t.Fatalf("a project's clear reached the workspace's fields: %v", declared)
	}
}

// fieldSlugs is every slug the answer offers, across its groups and sorted, so
// a comparison does not depend on how the reader grouped them.
func fieldSlugs(groups []tracker.FieldGroup) []string {
	var out []string
	for _, group := range groups {
		for _, field := range group.Fields {
			out = append(out, field.Slug)
		}
	}
	slices.Sort(out)
	return out
}

// optionSlugs is one declared field's option slugs, in declaration order.
func optionSlugs(groups []tracker.FieldGroup, id string) []string {
	for _, group := range groups {
		for _, field := range group.Fields {
			if field.ID != id {
				continue
			}
			out := make([]string, 0, len(field.Config.Options))
			for _, option := range field.Config.Options {
				out = append(out, option.Slug)
			}
			return out
		}
	}
	return nil
}

func (r *roundTrip) strings(query string, args ...any) []string {
	r.t.Helper()
	var out []string
	if err := r.db.Replicated().Read(r.t.Context(), func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(r.t.Context(), query, args...)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	}); err != nil {
		r.t.Fatalf("read rows: %v", err)
	}
	return out
}

// TestAProjectFieldArchiveIsOneWay is the workspace catalogue's own rule, one
// level down: a field that came back with its old id would silently re-admit
// values validated against a definition nobody has seen for a year.
func TestAProjectFieldArchiveIsOneWay(t *testing.T) {
	r := newRoundTrip(t)
	field := tracker.FieldDef{
		ID: "f-sev", Slug: "severity", Name: "Severity", Type: tracker.FieldText,
	}
	declare := func(f tracker.FieldDef, op string) error {
		fields := []tracker.FieldDef{f}
		_, err := r.writer.WriteProject(t.Context(), op, "ENG",
			tracker.ProjectEdit{Fields: &fields},
			tracker.ProjectAuthority{Lead: true})
		r.drain()
		return err
	}
	if err := declare(field, "op-declare"); err != nil {
		t.Fatalf("declare: %v", err)
	}
	archived := field
	archived.Archived = true
	if err := declare(archived, "op-archive"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if err := declare(field, "op-unarchive"); err == nil {
		t.Fatal("an archived project field was brought back under its own id")
	}
}

// TestARepeatedProjectEditWritesNothing keeps a form that submits every
// control from publishing a record per submit.
func TestARepeatedProjectEditWritesNothing(t *testing.T) {
	r := newRoundTrip(t)
	who := "alice"
	edit := tracker.ProjectEdit{DefaultAssignee: &who}
	if _, err := r.writer.WriteProject(t.Context(), "op-first", "ENG", edit,
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("first: %v", err)
	}
	r.drain()
	before := r.project(tracker.ProjectDetailQuery{Project: "ENG"}).Version

	if _, err := r.writer.WriteProject(t.Context(), "op-second", "ENG", edit,
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("second: %v", err)
	}
	r.drain()
	if after := r.project(tracker.ProjectDetailQuery{Project: "ENG"}).Version; after != before {
		t.Fatalf("a repeated edit moved the version %d → %d", before, after)
	}
}

// TestProjectEditRefusesAnEmptyGesture, and names where the chart-owned fields
// actually come from — which is the thing a caller reaching for them needs.
func TestProjectEditRefusesAnEmptyGesture(t *testing.T) {
	r := newRoundTrip(t)
	_, err := r.writer.WriteProject(t.Context(), "op-empty", "ENG",
		tracker.ProjectEdit{}, tracker.ProjectAuthority{Lead: true})
	if err == nil {
		t.Fatal("an edit that sets nothing was accepted")
	}
	if !strings.Contains(err.Error(), "org chart") {
		t.Fatalf("the refusal does not say where the name and unit come "+
			"from: %v", err)
	}
}

// TestProjectEditNamesAnUnknownProject rather than creating one: a project is
// derived from the chart, and creating it on demand is exactly the shape that
// produces two counters and two ENG-1s.
func TestProjectEditNamesAnUnknownProject(t *testing.T) {
	r := newRoundTrip(t)
	who := "alice"
	_, err := r.writer.WriteProject(t.Context(), "op-nope", "NOPE",
		tracker.ProjectEdit{DefaultAssignee: &who},
		tracker.ProjectAuthority{Lead: true})
	if err == nil {
		t.Fatal("an edit to a project that does not exist created one")
	}
	if !strings.Contains(err.Error(), "org chart") {
		t.Fatalf("the refusal does not say where a project comes from: %v", err)
	}
}

// AN OPERATOR IS NOT A SEAT, and the gate that admitted only the lead refused
// the one actor the whole operator surface exists for.
//
// It was reachable rather than theoretical: `/operator/mcp` resolves the lead
// from the ORG CHART by handle and an operator token carries its own name
// rather than a seat's, so the lookup answered false and `write_project` was
// refused for every operator in every company — including the founder
// declaring the fields a project files work under. The archive gate below it
// already expected an operator, which is what made the hole visible: a person
// could take a project out of circulation and could not say how its work was
// filed.
func TestAPersonsOwnCredentialIsAuthorityOverAProjectsPolicy(t *testing.T) {
	r := newRoundTrip(t)
	who := "alice"
	edit := tracker.ProjectEdit{DefaultAssignee: &who}

	if _, err := r.writer.WriteProject(t.Context(), "op-operator", "ENG", edit,
		tracker.ProjectAuthority{Operator: true}); err != nil {

		t.Fatalf("an operator who does not lead ENG could not set its "+
			"policy: %v", err)
	}
	r.drain()
	if got := r.project(tracker.ProjectDetailQuery{Project: "ENG"}); got.DefaultAssignee != who {
		t.Fatalf("default assignee = %q, want %q", got.DefaultAssignee, who)
	}
	// AND THE REFUSAL STILL NAMES BOTH WAYS IN, for the seat that is
	// neither: "ask the lead" is the useful answer, and so is "or a person".
	_, err := r.writer.WriteProject(t.Context(), "op-seat", "ENG", edit,
		tracker.ProjectAuthority{})
	if err == nil {
		t.Fatal("a seat that neither leads ENG nor holds a person's " +
			"credential set its policy")
	}
	if !strings.Contains(err.Error(), "person's own") {
		t.Errorf("the refusal does not name the other way in: %v", err)
	}
}

// AND THE SAME FOR A TAG, which is the same gate one object over: renaming or
// archiving a tag changes the word on every task already filed under it.
func TestAPersonsOwnCredentialMayRenameATag(t *testing.T) {
	r := newRoundTrip(t)
	if _, err := r.writer.WriteTags(t.Context(), "op-add", "ENG",
		tracker.TagEdit{Add: []tracker.Tag{{Slug: "api", Label: "API"}}},
		tracker.TagAuthority{}); err != nil {

		t.Fatalf("adding a tag is open to every seat: %v", err)
	}
	r.drain()

	rename := tracker.TagEdit{Rename: map[string]string{"api": "Interface"}}
	if _, err := r.writer.WriteTags(t.Context(), "op-seat-rename", "ENG",
		rename, tracker.TagAuthority{}); err == nil {

		t.Fatal("a seat that neither leads ENG nor holds a person's " +
			"credential renamed one of its tags")
	}
	if _, err := r.writer.WriteTags(t.Context(), "op-operator-rename", "ENG",
		rename, tracker.TagAuthority{Operator: true}); err != nil {

		t.Fatalf("an operator could not rename a tag: %v", err)
	}
}
