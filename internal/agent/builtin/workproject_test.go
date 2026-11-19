package builtin_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A SEAT MAY DECLARE A TAG AND NOTHING ELSE, which is the one reason this verb
// is in a seat's registry at all: `create_work_item` refuses a label the
// project has not declared, so a seat without it could never use `labels`.
func TestWriteProjectLetsAnySeatDeclareATag(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartRefuses)

	got := callWork(t, reg, worklet.WriteProjectTool, map[string]any{
		"project":  "ENG",
		"tags_add": []any{map[string]any{"slug": "regression"}},
	})
	if got.Failed {
		t.Fatalf("a seat could not declare a tag: %q", got.Output)
	}
	if len(trk.tagEdits) != 1 || len(trk.tagEdits[0].Add) != 1 ||
		trk.tagEdits[0].Add[0].Slug != "regression" {

		t.Fatalf("the tag edit did not reach the writer: %+v", trk.tagEdits)
	}
	// AND THE AUTHORITY IT CARRIES IS THE TRUTH about this seat, never a
	// convenience: the writer is what enforces the gate, and a tool that
	// claimed the policy answer would hand every seat a lead's verbs.
	// THE TABLE IS NEVER ASKED AT ALL for a pure add, which is what makes
	// the verb reachable on a chart that refuses everything.
	if trk.tagAuthority[0].Policy {
		t.Error("a seat the chart refuses was sent as the project's lead")
	}
}

// THE TABLE DECIDES THE LEAD-ONLY FACETS, and a chart that refuses is a
// REFUSAL of the call rather than a write carrying a false authority: the
// decision is taken before the writer is reached, so the tracker's own
// gate is the second line and not the first.
func TestWriteProjectCarriesTheLeadAnswer(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		chart   authz.Chart
		wrote   bool
		refusal string
	}{
		"not the lead": {chartRefuses, false, "didn't run: not allowed"},
		"the lead":     {chartLeads, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			trk := newFakeTracker()
			reg := projectRegistry(t, trk, tc.chart)
			got := callWorkAs(colleagueCaller(), t, reg, worklet.WriteProjectTool,
				map[string]any{
					"project":     "ENG",
					"tags_rename": map[string]any{"api": "Public API"},
				})
			if got.Failed == tc.wrote {
				t.Fatalf("write_project failed = %v, want %v: %q",
					got.Failed, !tc.wrote, got.Output)
			}
			if !tc.wrote {
				if !strings.Contains(got.Output, tc.refusal) {
					t.Errorf("the refusal reads %q", got.Output)
				}
				if len(trk.tagAuthority) != 0 {
					t.Error("a refused rename still reached the writer")
				}
				return
			}
			if !trk.tagAuthority[0].Policy {
				t.Error("the lead's rename did not carry the policy answer, " +
					"so the tracker's own gate refuses it")
			}
		})
	}
}

// ONE ANSWER TO "WHO IS THIS", asked once before either write: a call holding
// both facets must not land the tag half and then be refused the policy half
// on a different answer to the same question.
func TestWriteProjectResolvesTheLeadOnce(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	asked := 0
	reg := projectRegistry(t, trk, countingChart{asked: &asked})
	got := callWorkAs(colleagueCaller(), t, reg, worklet.WriteProjectTool,
		map[string]any{
			"project":          "ENG",
			"tags_add":         []any{map[string]any{"slug": "regression"}},
			"default_assignee": "alice",
		})
	if got.Failed {
		t.Fatalf("write_project failed: %q", got.Output)
	}
	if asked != 1 {
		t.Fatalf("the lead was resolved %d times for one call", asked)
	}
	if len(trk.tagEdits) != 1 || len(trk.projectEdits) != 1 {
		t.Fatalf("a call holding both facets made %d tag writes and %d policy "+
			"writes", len(trk.tagEdits), len(trk.projectEdits))
	}
}

// countingChart counts how many times the project relation is asked.
type countingChart struct {
	projectChart
	asked *int
}

func (c countingChart) LeadsProject(context.Context, string, string) (bool, error) {
	*c.asked++
	return true, nil
}

// PRESENCE DECIDES, never the value. `default_assignee: ""` means triage and
// omitting it means leave it alone, and a reader that could not tell them
// apart would clear a project's default every time somebody declared a field.
func TestWriteProjectTellsAbsentFromEmpty(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartLeads)

	callWork(t, reg, worklet.WriteProjectTool, map[string]any{
		"project": "ENG", "default_assignee": "",
	})
	if len(trk.projectEdits) != 1 || trk.projectEdits[0].DefaultAssignee == nil {
		t.Fatal("an empty default assignee was read as absent, so a project " +
			"cannot be put back into triage")
	}
	// THE ARCHIVE FACET IS A PERSON'S, which is what the human-only bar
	// on [authz.ActionProjectArchive] says — see
	// [TestAnAgentNeverArchivesAProject].
	callWorkAs(personHolding(iam.AllGrants...), t, reg,
		worklet.WriteProjectTool, map[string]any{
			"project": "ENG", "archived": true,
		})
	if len(trk.projectEdits) != 2 || trk.projectEdits[1].DefaultAssignee != nil {
		t.Fatal("an unsent default assignee was read as an empty one, so an " +
			"unrelated edit clears it")
	}
}

// AN AGENT NEVER ARCHIVES A PROJECT, and the bar is the TABLE's ([authz]'s
// human-only bar on the archive verb alone), never a kind the tool reports: a
// seat that leads the project is refused it while keeping every other facet.
func TestAnAgentNeverArchivesAProject(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartLeads)
	got := callWork(t, reg, worklet.WriteProjectTool, map[string]any{
		"project": "ENG", "archived": true,
	})
	if !got.Failed {
		t.Fatal("a seat that leads ENG archived it")
	}
	if len(trk.projectEdits) != 0 {
		t.Fatal("the refused archive still reached the writer")
	}

	// AND THE SAME SEAT KEEPS THE POLICY, which is what makes the bar a
	// property of the VERB rather than of the caller.
	if got := callWork(t, reg, worklet.WriteProjectTool, map[string]any{
		"project": "ENG", "default_assignee": "alice",
	}); got.Failed {
		t.Fatalf("the lead could not set the default assignee: %q", got.Output)
	}

	// AND A PERSON'S OWN CREDENTIAL DOES ARCHIVE, through the same table.
	operator := newFakeTracker()
	opReg := tools.NewRegistry()
	if _, err := builtin.Register(opReg, builtin.Deps{
		Work: builtin.WorkDeps{
			Reader: operator, Writer: operator.as,
			ProjectWriter:  func(builtin.Actor) builtin.ProjectWriter { return operator },
			DefaultProject: func(string) string { return "ENG" },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: worklet.AuthorOperator}, nil
			},
		},
		Authorize: builtin.Decide(chartLeads),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	entry, _ := opReg.Lookup(worklet.WriteProjectTool)
	if got, err := entry.Tool.Call(personHolding(iam.AllGrants...),
		map[string]any{"project": "ENG", "archived": true}); err != nil {

		t.Fatalf("write_project: %v", err)
	} else if got.Failed {
		t.Fatalf("a person could not archive a project: %q", got.Output)
	}
	if !operator.projectAuthority[0].Archive {
		t.Error("the archive answer did not reach the writer, so nothing can " +
			"ever archive a project")
	}
}

// A NEAR-DUPLICATE CAVEAT REACHES BOTH READERS: it is advisory, so useful only
// if seen — by a model in the answer's one top-level `warnings` list, by a
// person through the result's caveats. Nested under the `tags` half, the act
// route could not take it off a person's receipt.
func TestWriteProjectCarriesTheTagWarnings(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	near := caveat.TagNearDuplicate("ENG", "apis", []string{"api"})
	trk.tagWarnings = []caveat.Caveat{near}
	reg := projectRegistry(t, trk, chartRefuses)

	got := callWork(t, reg, worklet.WriteProjectTool, map[string]any{
		"project": "ENG", "tags_add": []any{map[string]any{"slug": "apis"}},
	})
	if got.Failed {
		t.Fatalf("write_project failed: %q", got.Output)
	}
	var answer struct {
		Warnings []string       `json:"warnings"`
		Tags     map[string]any `json:"tags"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(answer.Warnings, []string{near.Agent()}) {
		t.Fatalf("the model was told %q, want the caveat's own words", answer.Warnings)
	}
	if _, nested := answer.Tags["warnings"]; nested {
		t.Fatal("the warning is nested under the tags half as well")
	}
	if len(got.Caveats) != 1 || got.Caveats[0] != near {
		t.Fatalf("the result carries %+v, want the caveat for a person's surface", got.Caveats)
	}
}

// A CALL THAT CHANGES NOTHING IS REFUSED rather than publishing two records
// that say nothing, and the refusal lists what it could have taken.
func TestWriteProjectRefusesAnEmptyCall(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartLeads)
	got := callWork(t, reg, worklet.WriteProjectTool, map[string]any{"project": "ENG"})
	if !got.Failed {
		t.Fatal("a call setting nothing was accepted")
	}
	if !strings.Contains(got.Output, "tags_add") {
		t.Fatalf("the refusal does not name the facets: %q", got.Output)
	}
	if len(trk.tagEdits)+len(trk.projectEdits) != 0 {
		t.Fatal("an empty call still wrote")
	}
}

// THE SEAT'S OWN PROJECT IS THE DEFAULT, exactly as it is for the three
// project READS beside this verb.
func TestWriteProjectFallsBackToTheSeatsOwnProject(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistryIn(t, trk, chartRefuses, "ENG")
	got := callWork(t, reg, worklet.WriteProjectTool, map[string]any{
		"tags_add": []any{map[string]any{"slug": "regression"}},
	})
	if got.Failed {
		t.Fatalf("write_project with no project failed: %q", got.Output)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if answer["project"] != "ENG" {
		t.Fatalf("wrote to %v, want the seat's own project", answer["project"])
	}

	// AND A SEAT WHOSE UNIT OWNS NONE IS REFUSED naming the lookup rather
	// than writing to an empty key.
	unowned := projectRegistry(t, newFakeTracker(), chartRefuses)
	if got := callWork(t, unowned, worklet.WriteProjectTool, map[string]any{
		"tags_add": []any{map[string]any{"slug": "regression"}},
	}); !got.Failed {
		t.Fatal("a seat with no project wrote to an empty key")
	}
}

// `labels_create_missing` IS AN OPT-IN, and the opt-in is the whole rule: a
// seat that MEANT to declare a grouping and a seat that typo'd both send one
// unknown label, and declaring whichever arrived fills a board's filter strip
// with every misspelling anybody ever typed.
func TestLabelsAreDeclaredOnlyWhenTheCallerSaysSo(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args    map[string]any
		ensured bool
	}{
		"not asked": {map[string]any{
			"title": "A task", "labels": []any{"regression"},
		}, false},
		"asked": {map[string]any{
			"title": "A task", "labels": []any{"regression"},
			"labels_create_missing": true,
		}, true},
		"asked with no labels": {map[string]any{
			"title": "A task", "labels_create_missing": true,
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			trk := newFakeTracker()
			reg := projectRegistryIn(t, trk, chartRefuses, "ENG")
			got := callWork(t, reg, worklet.CreateWorkItemTool, tc.args)
			if got.Failed {
				t.Fatalf("create failed: %q", got.Output)
			}
			if declared := len(trk.ensured) > 0; declared != tc.ensured {
				t.Fatalf("declared = %v, want %v", declared, tc.ensured)
			}
			if tc.ensured && trk.ensuredIn[0] != "ENG" {
				t.Fatalf("declared into %q, want the task's own project",
					trk.ensuredIn[0])
			}
		})
	}
}

// AND WHAT IT CREATED IS IN THE ANSWER, so a caller that set the flag by habit
// still sees a typo now rather than on a board three weeks later.
func TestADeclaredLabelIsReportedBack(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistryIn(t, trk, chartRefuses, "ENG")
	got := callWork(t, reg, worklet.CreateWorkItemTool, map[string]any{
		"title": "A task", "labels": []any{"regresion"},
		"labels_create_missing": true,
	})
	if got.Failed {
		t.Fatalf("create failed: %q", got.Output)
	}
	var answer struct {
		Created []string `json:"labels_created"`
	}
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !slices.Contains(answer.Created, "regresion") {
		t.Fatalf("labels_created = %v, want the typo it just declared",
			answer.Created)
	}
}

// THE DECLARE RUNS BEFORE THE TASK, because the create refuses a label the
// project has not declared: doing it after would file the task that already
// failed, and a model would see a refusal for a tag it had just asked for.
func TestLabelsAreDeclaredBeforeTheTaskIsFiled(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistryIn(t, trk, chartRefuses, "ENG")
	callWork(t, reg, worklet.CreateWorkItemTool, map[string]any{
		"title": "A task", "labels": []any{"regression"},
		"labels_create_missing": true,
	})
	if len(trk.opIDs) < 2 || !strings.HasPrefix(opName(trk.opIDs[0]), "tags-") {
		t.Fatalf("the first write was %v, want the tag declare", trk.opIDs)
	}
}

// A REFUSED CALL DECLARES NO LABEL. The declare is the call's first PUBLISH, so
// it comes after every check that can still refuse (ask, decision, blockers,
// re-route), or every refused retry adds a label nobody uses. And an update
// that cannot write its dependency change writes NOTHING, or the caller is told
// "failed" about an item whose fields did change.
func TestARefusedWriteDeclaresNoLabel(t *testing.T) {
	t.Parallel()
	labels := func(args map[string]any) map[string]any {
		args["labels"] = []any{"regression"}
		args["labels_create_missing"] = true
		return args
	}
	for name, tc := range map[string]struct {
		tool string
		args map[string]any
	}{
		"create with a decision asked of nobody": {worklet.CreateWorkItemTool,
			labels(map[string]any{"title": "Which?", "decision": decisionArg()})},
		"create with a malformed decision": {worklet.CreateWorkItemTool,
			labels(map[string]any{"title": "Which?", "ask": "pm",
				"decision": map[string]any{"question": "Which?", "options": []any{}}})},
		"create waiting on an item that does not exist": {worklet.CreateWorkItemTool,
			labels(map[string]any{"title": "A task", "waiting_on": []any{"ENG-99"}})},
		"create waiting on work with no dependency writer": {worklet.CreateWorkItemTool,
			labels(map[string]any{"title": "A task", "waiting_on": []any{"ENG-2"}})},
		"update re-routed by somebody who does not lead the project": {
			worklet.UpdateWorkItemTool,
			labels(map[string]any{"item": "ENG-1", "routing_unit": "platform"})},
		"update waiting on work with no dependency writer": {worklet.UpdateWorkItemTool,
			labels(map[string]any{"item": "ENG-1",
				"waiting_on": map[string]any{"add": []any{"ENG-2"}}})},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A COLLEAGUE OVER A CHART THAT SAYS THEY LEAD NOTHING, never
			// [everyGrant]: admin admits a re-route before
			// the chart is asked, and a case run as it asserts nothing
			// about the one refusal that turns on who leads the project.
			trk := newFakeTracker()
			reg := projectRegistryIn(t, trk, chartRefuses, "ENG")
			got := callWorkAs(colleagueCaller(), t, reg, tc.tool, tc.args)
			if !got.Failed {
				t.Fatalf("the call was accepted: %s", got.Output)
			}
			if len(trk.ensured) != 0 {
				t.Errorf("a refused call declared %v", trk.ensured)
			}
			if len(trk.created) != 0 || len(trk.patched) != 0 {
				t.Errorf("a refused call wrote: %d creates, %d patches",
					len(trk.created), len(trk.patched))
			}
		})
	}
}

// THE VERB IS DECLARED DESTRUCTIVE because `fields` REPLACES a project's
// declarations: a short list retires every field it left out, undoing
// somebody else's work. Asserted here because the classification table only
// reads [mcp.WritesToSharedSurface], which the fail-closed DEFAULT arm already
// answers true: without this, deleting the arm would leave every suite green.
func TestWriteProjectIsDeclaredDestructive(t *testing.T) {
	t.Parallel()
	reg := projectRegistry(t, newFakeTracker(), chartRefuses)
	entry, ok := reg.Snapshot().Lookup(worklet.WriteProjectTool)
	if !ok {
		t.Fatal("write_project is not registered")
	}
	if entry.Annotations.Destructive != mcp.Yes {
		t.Errorf("write_project is annotated %+v — a verb that can retire "+
			"every field a project declares has to say so, or a sub-agent "+
			"guard reading the flag cannot tell it from a comment",
			entry.Annotations)
	}
	if entry.Annotations.OpenWorld != mcp.Yes {
		t.Errorf("write_project is annotated %+v — a declared tag is a filter "+
			"on everybody's board", entry.Annotations)
	}
}

// ---- helpers ------------------------------------------------------------ //

// projectChart answers the project relation and nothing else, which is what
// every case in this file turns on. THE OTHER TWO RELATIONS REFUSE
// ([authz.Chart.LeadsUnit]'s rule), so a rule asking the wrong one is caught.
type projectChart struct {
	leads bool
	err   error
}

func (c projectChart) Leads(context.Context, string, string) (bool, error) {
	return false, nil
}

func (c projectChart) LeadsProject(context.Context, string, string) (bool, error) {
	return c.leads, c.err
}

func (c projectChart) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, nil
}

func (c projectChart) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
}

// LeadsAnyone, the person relation's "anybody at all", refuses for the reason
// [projectChart.Leads] does.
func (c projectChart) LeadsAnyone(context.Context, string) (bool, error) {
	return false, nil
}

// MemberOf answers that nobody sits in any unit: no case here shares an
// artifact with a team.
func (projectChart) MemberOf(context.Context, string, string) (bool, error) {
	return false, nil
}

// SeatGranted answers that no role carries a grant: no project verb names one.
func (projectChart) SeatGranted(context.Context, string, authz.SeatGrant) (bool, error) {
	return false, nil
}

// chartLeads and chartRefuses are the two answers a chart that CAN answer
// gives.
var (
	chartLeads   = projectChart{leads: true}
	chartRefuses = projectChart{}
)

// chartUnknown is a chart that could not answer — a node booting, or one
// running no company yet.
var chartUnknown = projectChart{err: errors.New("this node runs no company yet")}

func projectRegistry(t *testing.T, trk *fakeTracker, chart authz.Chart) *tools.Registry {
	return projectRegistryIn(t, trk, chart, "")
}

func projectRegistryIn(t *testing.T, trk *fakeTracker, chart authz.Chart,
	project string) *tools.Registry {

	t.Helper()
	reg := tools.NewRegistry()
	deps := builtin.Deps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as,
			ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return trk },
		},
		Authorize: builtin.Decide(chart),
	}
	if project != "" {
		deps.Work.DefaultProject = func(string) string { return project }
	}
	if _, err := builtin.Register(reg, gated(deps)); err != nil {
		t.Fatalf("register: %v", err)
	}
	return reg
}

// A CHART THAT COULD NOT ANSWER REFUSES THE WRITE, AND SAYS WHY: a node holds
// no company while booting or installing a revision, which is not "you do not
// lead this". Since `person` alone grants the operator half, answering anyway
// would land a lead's edit as an operator's, or refuse a seat naming the lead
// relation rather than the lag. TOLD TO TRY AGAIN, so the caller retries rather
// than hunting for an authority they already hold.
func TestWriteProjectRefusesWhenTheChartCannotAnswer(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartUnknown)
	got := callWorkAs(colleagueCaller(), t, reg, worklet.WriteProjectTool,
		map[string]any{
			"project":     "ENG",
			"tags_rename": map[string]any{"api": "Public API"},
		})
	if !got.Failed {
		t.Fatalf("a chart that could not answer decided anyway: %q", got.Output)
	}
	if len(trk.tagAuthority) != 0 {
		t.Errorf("the write landed with authority %+v, decided without the "+
			"one relation it turns on", trk.tagAuthority[0])
	}
	for _, want := range []string{"can't check access right now", "no company yet"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, got.Output)
		}
	}
}

// THE TARGET DATE TRAVELS AS SENT, AND NULL CLEARS IT. Presence decides: a
// call silent on the target keeps it, null or "" clears it. The tracker
// coerces the value, and an instant stored as its day is surfaced, since
// `applied` alone would read as stored verbatim.
func TestWriteProjectCarriesTheTargetDate(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args map[string]any
		want *string
	}{
		"absent leaves it alone": {map[string]any{"default_assignee": ""}, nil},
		"a date is carried":      {map[string]any{"target_date": " 2026-12-18 "}, new("2026-12-18")},
		"null clears it":         {map[string]any{"target_date": nil}, new("")},
		"empty clears it":        {map[string]any{"target_date": ""}, new("")},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := projectRegistry(t, trk, chartLeads)
			tc.args["project"] = "ENG"
			if got := callWork(t, reg, worklet.WriteProjectTool, tc.args); got.Failed {
				t.Fatalf("write_project failed: %q", got.Output)
			}
			if len(trk.projectEdits) != 1 {
				t.Fatalf("%d policy writes, want one", len(trk.projectEdits))
			}
			got := trk.projectEdits[0].TargetDate
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("the target reached the writer as %v, want %v", got, tc.want)
			}
		})
	}

	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartLeads)
	if got := callWork(t, reg, worklet.WriteProjectTool, map[string]any{
		"project": "ENG", "target_date": 20261218,
	}); !got.Failed || !strings.Contains(got.Output, "target_date") {
		t.Errorf("a number for a date answered %+v, want a failure naming the "+
			"argument", got)
	}

	trk.projectWarnings = []caveat.Caveat{caveat.TargetDateTruncated(
		"2026-12-18T20:00:00Z", "2026-12-19", "Asia/Tokyo")}
	got := callWork(t, reg, worklet.WriteProjectTool, map[string]any{
		"project": "ENG", "target_date": "2026-12-18T20:00:00Z",
	})
	if got.Failed || !strings.Contains(got.Output, "2026-12-19") {
		t.Errorf("a truncated target answered %q, want the warning carried", got.Output)
	}
}

// THE STUCK WINDOW TRAVELS AS SENT, AND NULL OR ZERO CLEARS IT. Presence
// decides, as for the target date; the tracker refuses a whole number outside
// its bounds, and anything that is not a whole number is refused here by name.
func TestWriteProjectCarriesTheStuckWindow(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args map[string]any
		want *int
	}{
		"absent leaves it alone": {map[string]any{"default_assignee": ""}, nil},
		"hours are carried":      {map[string]any{"stuck_after_hours": 24}, new(24)},
		"null clears it":         {map[string]any{"stuck_after_hours": nil}, new(0)},
		"zero clears it":         {map[string]any{"stuck_after_hours": 0}, new(0)},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			reg := projectRegistry(t, trk, chartLeads)
			tc.args["project"] = "ENG"
			if got := callWork(t, reg, worklet.WriteProjectTool, tc.args); got.Failed {
				t.Fatalf("write_project failed: %q", got.Output)
			}
			if len(trk.projectEdits) != 1 {
				t.Fatalf("%d policy writes, want one", len(trk.projectEdits))
			}
			got := trk.projectEdits[0].StuckAfterHours
			if (got == nil) != (tc.want == nil) || (got != nil && *got != *tc.want) {
				t.Fatalf("the window reached the writer as %v, want %v", got, tc.want)
			}
		})
	}
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartLeads)
	if got := callWork(t, reg, worklet.WriteProjectTool, map[string]any{
		"project": "ENG", "stuck_after_hours": "a day",
	}); !got.Failed || !strings.Contains(got.Output, "stuck_after_hours") {
		t.Errorf("words for a window answered %+v, want a failure naming the argument", got)
	}
}
