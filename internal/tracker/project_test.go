package tracker_test

import (
	"database/sql"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tracker"
)

// TestProjectPolicyIsTheLeads is the authority rule. A project's field
// declarations and its sprint policy decide how everybody's work in it is
// filed and planned, which is not a call one seat makes for the team.
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

// TestSprintPolicyIsRefusedBeforeItReachesTheDuty. Every one of these is a
// property of the argument, and the duty that would act on a broken policy
// runs on another node where nobody is watching a refusal.
func TestSprintPolicyIsRefusedBeforeItReachesTheDuty(t *testing.T) {
	r := newRoundTrip(t)
	for name, policy := range map[string]tracker.SprintPolicy{
		"no length":        {LengthDays: 0, Ahead: 2},
		"negative":         {LengthDays: -1},
		"past a quarter":   {LengthDays: tracker.MaxSprintDays + 1},
		"too far ahead":    {LengthDays: 14, Ahead: tracker.MaxSprintsAhead + 1},
		"eighth weekday":   {LengthDays: 14, StartWeekday: time.Weekday(7)},
		"a day of minutes": {LengthDays: 14, StartMinutes: 24 * 60},
		"an unknown measure": {LengthDays: 14,
			Measure: tracker.SprintMeasure("hours")},
		"a negative estimate": {LengthDays: 14, PointScale: []float64{1, -2}},
		"a negative capacity": {LengthDays: 14,
			Capacity: map[string]tracker.Capacity{"alice": {Points: -1}}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := r.writer.WriteProject(t.Context(), "op-"+name, "ENG",
				tracker.ProjectEdit{
					Sprints: &tracker.SprintPolicyEdit{Policy: &policy},
				}, tracker.ProjectAuthority{Lead: true})
			if err == nil {
				t.Fatalf("a policy with %s was accepted", name)
			}
		})
	}
	// THE CONTROL, or every case above passes for the wrong reason.
	good := tracker.SprintPolicy{LengthDays: 14, Ahead: 2, StartWeekday: time.Monday}
	if _, err := r.writer.WriteProject(t.Context(), "op-good", "ENG",
		tracker.ProjectEdit{Sprints: &tracker.SprintPolicyEdit{Policy: &good}},
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("a well-formed policy was refused: %v", err)
	}
}

// TestSprintPolicyOffIsNotTheSameAsUnsent is the pointer's whole reason: a
// struct that could not tell "leave this alone" from "set it to nothing" would
// clear a project's field list every time somebody set its default assignee.
func TestSprintPolicyOffIsNotTheSameAsUnsent(t *testing.T) {
	r := newRoundTrip(t)
	policy := tracker.SprintPolicy{LengthDays: 14, Ahead: 1, StartWeekday: time.Monday}
	if _, err := r.writer.WriteProject(t.Context(), "op-on", "ENG",
		tracker.ProjectEdit{Sprints: &tracker.SprintPolicyEdit{Policy: &policy}},
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("turn sprints on: %v", err)
	}
	r.drain()

	// AN UNRELATED EDIT LEAVES IT ALONE.
	who := "alice"
	if _, err := r.writer.WriteProject(t.Context(), "op-unrelated", "ENG",
		tracker.ProjectEdit{DefaultAssignee: &who},
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("set the default assignee: %v", err)
	}
	r.drain()
	if got := r.project(tracker.ProjectDetailQuery{Project: "ENG"}); got.SprintPolicy == nil {
		t.Fatal("an unrelated edit cleared the sprint policy")
	}

	// AND AN EXPLICIT OFF CLEARS IT.
	if _, err := r.writer.WriteProject(t.Context(), "op-off", "ENG",
		tracker.ProjectEdit{Sprints: &tracker.SprintPolicyEdit{}},
		tracker.ProjectAuthority{Lead: true}); err != nil {

		t.Fatalf("turn sprints off: %v", err)
	}
	r.drain()
	if got := r.project(tracker.ProjectDetailQuery{Project: "ENG"}); got.SprintPolicy != nil {
		t.Fatal("an explicit off left the sprint policy in place")
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
