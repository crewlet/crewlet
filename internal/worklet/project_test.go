package worklet_test

import (
	"database/sql"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/worklet"
)

// TestProjectPolicyIsTheLeads is the authority rule. A project's field
// declarations decide how everybody's work in it is filed, which is not a call
// one seat makes for the team.
func TestProjectPolicyIsTheLeads(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	who := "alice"
	edit := worklet.ProjectEdit{DefaultAssignee: &who}

	_, err := r.writer.WriteProject(t.Context(), "op-seat", "ENG", edit,
		worklet.ProjectAuthority{})
	switch {
	case err == nil:
		t.Fatal("a seat that does not lead ENG set its default assignee")
	case !errors.Is(err, worklet.ErrForbidden):
		// THE CLASS A CALLER BRANCHES ON: without it this refusal read
		// as whatever the caller's default was, and a person's surface
		// offered a retry of something no retry changes.
		t.Fatalf("the refusal is %v, want an ErrForbidden", err)
	}
	if _, err := r.writer.WriteProject(t.Context(), "op-lead", "ENG", edit,
		worklet.ProjectAuthority{Policy: true}); err != nil {

		t.Fatalf("the lead could not set the default assignee: %v", err)
	}
	r.drain()
	if got := r.project(worklet.ProjectDetailQuery{Project: "ENG"}); got.DefaultAssignee != who {
		t.Fatalf("default assignee = %q, want %q", got.DefaultAssignee, who)
	}
}

// TestArchivingAProjectTakesAPerson is the second gate, and it is named
// separately because a lead who hits it DOES have authority over the other
// three facets — "no" would send them looking in the wrong place.
func TestArchivingAProjectTakesAPerson(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	archived := true
	edit := worklet.ProjectEdit{Archived: &archived}

	_, err := r.writer.WriteProject(t.Context(), "op-lead-archive", "ENG", edit,
		worklet.ProjectAuthority{Policy: true})
	if err == nil {
		t.Fatal("a lead the table refused the archive archived a project")
	}
	// WHAT IS MISSING is the person: the archive takes the policy's own
	// relation AND a principal that is not an agent, so the lead refused
	// here is a lead acting as an agent.
	if !strings.Contains(err.Error(), "as a person rather than as an agent") {
		t.Fatalf("the refusal does not say what is missing: %v", err)
	}
	if _, err := r.writer.WriteProject(t.Context(), "op-operator-archive", "ENG",
		edit, worklet.ProjectAuthority{Policy: true, Archive: true}); err != nil {

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
	t.Parallel()
	r := newRoundTrip(t)
	who := "alice"
	if _, err := r.writer.WriteProject(t.Context(), "op-assignee", "ENG",
		worklet.ProjectEdit{DefaultAssignee: &who},
		worklet.ProjectAuthority{Policy: true}); err != nil {

		t.Fatalf("set the default assignee: %v", err)
	}
	r.drain()
	if got := r.project(worklet.ProjectDetailQuery{Project: "ENG"}); got.PolicyStamp != 0 {
		t.Fatalf("the policy stamp moved to %d for a default assignee, which "+
			"no task is validated against", got.PolicyStamp)
	}

	fields := []worklet.FieldDef{{
		ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldText,
	}}
	if _, err := r.writer.WriteProject(t.Context(), "op-fields", "ENG",
		worklet.ProjectEdit{Fields: &fields},
		worklet.ProjectAuthority{Policy: true}); err != nil {

		t.Fatalf("declare a field: %v", err)
	}
	r.drain()
	if got := r.project(worklet.ProjectDetailQuery{Project: "ENG"}); got.PolicyStamp == 0 {
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
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteFields(t.Context(), "op-workspace",
		[]worklet.FieldDef{{
			ID: "f-impact", Slug: "impact", Name: "Impact", Type: worklet.FieldText,
		}, {
			ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldText,
		}}); err != nil {

		t.Fatalf("declare the workspace's fields: %v", err)
	}
	r.drain()

	fields := []worklet.FieldDef{{
		ID: "f-sev", Slug: "urgency", Name: "Urgency",
		Type: worklet.FieldDropdown,
		Config: worklet.FieldConfig{Options: []worklet.Option{
			{ID: "o-1", Slug: "sev1", Name: "Sev 1"},
			{ID: "o-2", Slug: "sev2", Name: "Sev 2"},
		}},
	}}
	if _, err := r.writer.WriteProject(t.Context(), "op-fields", "ENG",
		worklet.ProjectEdit{Fields: &fields},
		worklet.ProjectAuthority{Policy: true}); err != nil {

		t.Fatalf("declare: %v", err)
	}
	r.drain()

	got := r.project(worklet.ProjectDetailQuery{Project: "ENG"})
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
	shorter := []worklet.FieldDef{}
	if _, err := r.writer.WriteProject(t.Context(), "op-clear", "ENG",
		worklet.ProjectEdit{Fields: &shorter},
		worklet.ProjectAuthority{Policy: true}); err != nil {

		t.Fatalf("clear the project's fields: %v", err)
	}
	r.drain()

	got = r.project(worklet.ProjectDetailQuery{Project: "ENG"})
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
func fieldSlugs(groups []worklet.FieldGroup) []string {
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
func optionSlugs(groups []worklet.FieldGroup, id string) []string {
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
	if err := r.db.Read(r.t.Context(), func(tx *sql.Tx) error {
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
	t.Parallel()
	r := newRoundTrip(t)
	field := worklet.FieldDef{
		ID: "f-sev", Slug: "severity", Name: "Severity", Type: worklet.FieldText,
	}
	declare := func(f worklet.FieldDef, op string) error {
		fields := []worklet.FieldDef{f}
		_, err := r.writer.WriteProject(t.Context(), op, "ENG",
			worklet.ProjectEdit{Fields: &fields},
			worklet.ProjectAuthority{Policy: true})
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
	t.Parallel()
	r := newRoundTrip(t)
	who := "alice"
	edit := worklet.ProjectEdit{DefaultAssignee: &who}
	if _, err := r.writer.WriteProject(t.Context(), "op-first", "ENG", edit,
		worklet.ProjectAuthority{Policy: true}); err != nil {

		t.Fatalf("first: %v", err)
	}
	r.drain()
	before := r.project(worklet.ProjectDetailQuery{Project: "ENG"}).Version

	if _, err := r.writer.WriteProject(t.Context(), "op-second", "ENG", edit,
		worklet.ProjectAuthority{Policy: true}); err != nil {

		t.Fatalf("second: %v", err)
	}
	r.drain()
	if after := r.project(worklet.ProjectDetailQuery{Project: "ENG"}).Version; after != before {
		t.Fatalf("a repeated edit moved the version %d → %d", before, after)
	}
}

// TestProjectEditRefusesAnEmptyGesture, and names where the chart-owned fields
// actually come from — which is the thing a caller reaching for them needs.
func TestProjectEditRefusesAnEmptyGesture(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	_, err := r.writer.WriteProject(t.Context(), "op-empty", "ENG",
		worklet.ProjectEdit{}, worklet.ProjectAuthority{Policy: true})
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
	t.Parallel()
	r := newRoundTrip(t)
	who := "alice"
	_, err := r.writer.WriteProject(t.Context(), "op-nope", "NOPE",
		worklet.ProjectEdit{DefaultAssignee: &who},
		worklet.ProjectAuthority{Policy: true})
	if err == nil {
		t.Fatal("an edit to a project that does not exist created one")
	}
	if !strings.Contains(err.Error(), "org chart") {
		t.Fatalf("the refusal does not say where a project comes from: %v", err)
	}
}

// THE TWO FACETS ARE TWO ANSWERS, and the split is what makes the second one
// mean anything.
//
// Both fields used to be INPUTS to one policy — `Lead` from a chart lookup and
// `Operator` from "the actor is not a seat" — joined here as `!Lead &&
// !Operator`. Under that reading a person's own credential was authority over
// every project's policy in the company, whether or not they led it and
// whether or not they held any grant: the archive gate below wanted an
// operator, and the policy gate accepted the same fact as a substitute for the
// relation. Now each facet carries the answer [authz.Decide] gave for ITS own
// verb, and holding one says nothing about the other.
func TestEachProjectFacetTakesItsOwnAnswer(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	who := "alice"
	edit := worklet.ProjectEdit{DefaultAssignee: &who}

	// THE ARCHIVE ANSWER DOES NOT UNLOCK THE POLICY, which is the hole
	// the old join had: a person who may take a project out of
	// circulation is not thereby the person who says how its work is
	// filed.
	if _, err := r.writer.WriteProject(t.Context(), "op-archive-only", "ENG",
		edit, worklet.ProjectAuthority{Archive: true}); err == nil {

		t.Fatal("an archive answer set the project's default assignee")
	}

	if _, err := r.writer.WriteProject(t.Context(), "op-policy", "ENG", edit,
		worklet.ProjectAuthority{Policy: true}); err != nil {

		t.Fatalf("a caller the table admitted could not set ENG's policy: %v", err)
	}
	r.drain()
	if got := r.project(worklet.ProjectDetailQuery{Project: "ENG"}); got.DefaultAssignee != who {
		t.Fatalf("default assignee = %q, want %q", got.DefaultAssignee, who)
	}

	// AND THE POLICY ANSWER DOES NOT UNLOCK THE ARCHIVE, which is the
	// other direction: a project nobody can file into again is a company
	// decision and never an agent's, so the table asks a second verb with
	// a human-only bar on it.
	archived := true
	if _, err := r.writer.WriteProject(t.Context(), "op-policy-archive", "ENG",
		worklet.ProjectEdit{Archived: &archived},
		worklet.ProjectAuthority{Policy: true}); err == nil {

		t.Fatal("a policy answer archived the project")
	}

	// AND THE REFUSAL STILL NAMES BOTH WAYS IN, for the caller that is
	// neither: "ask the lead" is the useful answer, and so is the permission
	// that overrides the relation — admin, the table's admin path. It named
	// "a person's own" credential once, which is a way in this build does
	// not have: being a person is no authority over a project here.
	_, err := r.writer.WriteProject(t.Context(), "op-seat", "ENG", edit,
		worklet.ProjectAuthority{})
	if err == nil {
		t.Fatal("a caller that neither leads ENG nor holds admin set its policy")
	}
	if !strings.Contains(err.Error(), "lead") ||
		!strings.Contains(err.Error(), "an admin") {
		t.Errorf("the refusal does not name both ways in: %v", err)
	}
}

// AND A TAG IS THE SAME GATE ONE OBJECT OVER: adding one is open to every
// colleague, and renaming or archiving one changes the word on every task
// already filed under it.
func TestOnlyThePolicyAnswerRenamesATag(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	if _, err := r.writer.WriteTags(t.Context(), "op-add", "ENG",
		worklet.TagEdit{Add: []worklet.Tag{{Slug: "api", Label: "API"}}},
		worklet.TagAuthority{}); err != nil {

		t.Fatalf("adding a tag is open to every seat: %v", err)
	}
	r.drain()

	rename := worklet.TagEdit{Rename: map[string]string{"api": "Interface"}}
	if _, err := r.writer.WriteTags(t.Context(), "op-seat-rename", "ENG",
		rename, worklet.TagAuthority{}); err == nil {

		t.Fatal("a caller the table did not admit renamed one of ENG's tags")
	}
	if _, err := r.writer.WriteTags(t.Context(), "op-operator-rename", "ENG",
		rename, worklet.TagAuthority{Policy: true}); err != nil {

		t.Fatalf("a caller the table admitted could not rename a tag: %v", err)
	}
}

// roster is a chart seam holding exactly the seats it lists, resolving each
// name it is given to the handle beside it — which may be ANOTHER handle, the
// way the colleague match's later tiers answer a departed seat's name.
type roster map[string]string

func (c roster) ResolveSeat(ref string) (string, bool) {
	handle, held := c[ref]
	return handle, held
}

// TestUnassignedWorkLandsOnTheProjectsDefaultAssignee is the setting doing what
// it says. `default_assignee` is "who unassigned work lands on": the lead sets
// it, the project read serves it and the New task sheet names it under an
// empty Assignee field — and nothing applied it, so every task filed without
// an assignee went to triage whatever the lead had chosen.
//
// FOUR CASES, each a way the rule could be half right: a create naming nobody
// is filed to the default, and its WAKE names them too (or the recipient rule
// routes it as triage, to the lead, while it sits on somebody else's queue);
// a named assignee is left alone; and a default that no longer names a seat
// on the chart — including one the colleague match would turn into somebody
// ELSE — is not applied, and says so.
func TestUnassignedWorkLandsOnTheProjectsDefaultAssignee(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	bo := "bo"
	if _, err := r.writer.WriteProject(t.Context(), "op-default", "ENG",
		worklet.ProjectEdit{DefaultAssignee: &bo},
		worklet.ProjectAuthority{Policy: true}); err != nil {

		t.Fatalf("set the default assignee: %v", err)
	}
	r.drain()

	file := func(id, assignee string) worklet.WriteResult {
		t.Helper()
		task := newTask(id)
		task.Key, task.Assignee, task.Watchers = "", assignee, []string{"ana"}
		if assignee != "" {
			task.Watchers = append(task.Watchers, assignee)
		}
		notify := worklet.Wake{Kind: worklet.ChangeCreated, After: task}.Notify(nil)
		got, err := r.writer.CreateTask(t.Context(), "op-"+id, task, notify)
		if err != nil {
			t.Fatalf("file %s: %v", id, err)
		}
		r.drain()
		return got
	}

	// NOBODY NAMED: the project's default, on the row, the answer and the
	// wake alike.
	got := file("t-nobody", "")
	if got.Assignee != "bo" {
		t.Errorf("the create reported it filed to %q, want the default %q", got.Assignee, "bo")
	}
	row := r.task(t, "t-nobody").Task
	if row.Assignee != "bo" {
		t.Errorf("the task was filed to %q, want the project's default %q", row.Assignee, "bo")
	}
	if !slices.Contains(row.Watchers, "bo") || !slices.Contains(row.Watchers, "ana") {
		t.Errorf("watchers %v: the default assignee follows what they hold, "+
			"beside the reporter", row.Watchers)
	}
	wakes := r.wakes(t)
	wake := wakes[len(wakes)-1]
	if wake.Snapshot.Assignee != "bo" {
		t.Errorf("the wake names %q as the assignee, want %q", wake.Snapshot.Assignee, "bo")
	}
	if moved := wake.Fields["assignee"]; moved.To != "bo" {
		t.Errorf("the wake's assignee delta is %+v, want it to name %q", moved, "bo")
	}
	woken := false
	for _, c := range worklet.Candidates(wake) {
		if c.Handle == "bo" && c.Reason == worklet.ReasonAssignee {
			woken = true
		}
	}
	if !woken {
		t.Errorf("the default assignee is not woken as the assignee: %+v",
			worklet.Candidates(wake))
	}

	// A NAMED ASSIGNEE IS LEFT ALONE.
	if got := file("t-named", "cy"); got.Assignee != "cy" ||
		r.task(t, "t-named").Task.Assignee != "cy" {

		t.Errorf("a create naming cy was filed to %q", r.task(t, "t-named").Task.Assignee)
	}

	// A DEFAULT THE CHART NO LONGER HOLDS goes to triage, and says why —
	// and so does one the chart would answer with somebody else.
	for name, seats := range map[string]roster{
		"departed":      {"cy": "cy"},
		"somebody else": {"cy": "cy", "bo": "bob"},
	} {
		r.writer.World = seats
		id := "t-" + strings.ReplaceAll(name, " ", "-")
		got := file(id, "")
		r.writer.World = nil
		if row := r.task(t, id).Task; row.Assignee != "" || got.Assignee != "" {
			t.Errorf("%s: filed to %q (reported %q), want triage", name, row.Assignee, got.Assignee)
		}
		if !slices.Contains(got.Caveats, caveat.DefaultAssigneeGone("ENG", "bo")) {
			t.Errorf("%s: the writer was not told the default was skipped: %+v", name, got.Caveats)
		}
	}
}
