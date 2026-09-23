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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A SEAT MAY DECLARE A TAG AND NOTHING ELSE, which is the one reason this verb
// is in a seat's registry at all: `create_work_item` refuses a label the
// project has not declared, so a seat without it could never use `labels`.
func TestWriteProjectLetsAnySeatDeclareATag(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartRefuses)

	got := callWork(t, reg, tracker.WriteProjectTool, map[string]any{
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
// decision is taken before the writer is reached now, so the tracker's own
// gate is the second line and not the first.
func TestWriteProjectCarriesTheLeadAnswer(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		chart   authz.Chart
		wrote   bool
		refusal string
	}{
		"not the lead": {chartRefuses, false, "refused"},
		"the lead":     {chartLeads, true, ""},
	} {
		t.Run(name, func(t *testing.T) {
			trk := newFakeTracker()
			reg := projectRegistry(t, trk, tc.chart)
			got := callWorkAs(t, reg, colleagueCaller(), tracker.WriteProjectTool,
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
	got := callWorkAs(t, reg, colleagueCaller(), tracker.WriteProjectTool,
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

	callWork(t, reg, tracker.WriteProjectTool, map[string]any{
		"project": "ENG", "default_assignee": "",
	})
	if len(trk.projectEdits) != 1 || trk.projectEdits[0].DefaultAssignee == nil {
		t.Fatal("an empty default assignee was read as absent, so a project " +
			"cannot be put back into triage")
	}
	// THE ARCHIVE FACET IS A PERSON'S, which is what the human-only bar
	// on [authz.ActionProjectArchive] says — see
	// [TestAnAgentNeverArchivesAProject].
	callWorkAs(t, reg, personHolding(iam.AllGrants...),
		tracker.WriteProjectTool, map[string]any{
			"project": "ENG", "archived": true,
		})
	if len(trk.projectEdits) != 2 || trk.projectEdits[1].DefaultAssignee != nil {
		t.Fatal("an unsent default assignee was read as an empty one, so an " +
			"unrelated edit clears it")
	}
}

// AN AGENT NEVER ARCHIVES A PROJECT, and the bar is the TABLE's now rather
// than a kind the tool reported.
//
// It used to be `ProjectAuthority.Operator`, filled from "the actor is not a
// seat" — which was the same fact the policy gate accepted as a SUBSTITUTE for
// the lead relation, so a person's own credential was authority over every
// project in the company. [authz] states it once, as a human-only bar on the
// archive verb alone, and a seat that leads the project is refused by it while
// keeping every other facet.
func TestAnAgentNeverArchivesAProject(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartLeads)
	got := callWork(t, reg, tracker.WriteProjectTool, map[string]any{
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
	if got := callWork(t, reg, tracker.WriteProjectTool, map[string]any{
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
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
			},
		},
		Authorize: builtin.Decide(chartLeads),
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	entry, _ := opReg.Lookup(tracker.WriteProjectTool)
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

// A NEAR-DUPLICATE WARNING REACHES THE CALLER. It is advisory by design, so
// the only thing that makes it useful is that the model actually sees it.
func TestWriteProjectCarriesTheTagWarnings(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.tagWarnings = []string{"apis is within a typo of api"}
	reg := projectRegistry(t, trk, chartRefuses)

	got := callWork(t, reg, tracker.WriteProjectTool, map[string]any{
		"project": "ENG", "tags_add": []any{map[string]any{"slug": "apis"}},
	})
	if got.Failed {
		t.Fatalf("write_project failed: %q", got.Output)
	}
	if !strings.Contains(got.Output, "within a typo") {
		t.Fatalf("the warning never reached the caller: %q", got.Output)
	}
}

// A CALL THAT CHANGES NOTHING IS REFUSED rather than publishing two records
// that say nothing, and the refusal lists what it could have taken.
func TestWriteProjectRefusesAnEmptyCall(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartLeads)
	got := callWork(t, reg, tracker.WriteProjectTool, map[string]any{"project": "ENG"})
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
	got := callWork(t, reg, tracker.WriteProjectTool, map[string]any{
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
	if got := callWork(t, unowned, tracker.WriteProjectTool, map[string]any{
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
			got := callWork(t, reg, tracker.CreateWorkItemTool, tc.args)
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
	got := callWork(t, reg, tracker.CreateWorkItemTool, map[string]any{
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
	callWork(t, reg, tracker.CreateWorkItemTool, map[string]any{
		"title": "A task", "labels": []any{"regression"},
		"labels_create_missing": true,
	})
	if len(trk.opIDs) < 2 || !strings.HasPrefix(trk.opIDs[0], "tags-") {
		t.Fatalf("the first write was %v, want the tag declare", trk.opIDs)
	}
}

// THE VERB IS DECLARED DESTRUCTIVE, and the facet that makes it so is the one
// a caller is most likely to reach for by accident rather than the one it is
// named after: `fields` REPLACES a project's declarations, so a call sending a
// short list retires every field it left out. The flag asks whether a call can
// undo somebody else's work, and this one can.
//
// Asserted here rather than left to the classification table, because that
// table only reads [mcp.WritesToSharedSurface] — which the fail-closed DEFAULT
// arm already answers true. Without this, the arm could be deleted outright
// and every suite would stay green.
func TestWriteProjectIsDeclaredDestructive(t *testing.T) {
	t.Parallel()
	reg := projectRegistry(t, newFakeTracker(), chartRefuses)
	entry, ok := reg.Snapshot().Lookup(tracker.WriteProjectTool)
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
// every case in this file turns on.
//
// THE OTHER TWO RELATIONS REFUSE rather than mirroring this one, on
// [authz.Chart.LeadsUnit]'s own rule: a fixture that folded them would agree
// with a rule asking either, and the whole point of three methods is that a
// rule asking the wrong one is caught.
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

// chartLeads and chartRefuses are the two answers a chart that CAN answer
// gives.
var (
	chartLeads   = projectChart{leads: true}
	chartRefuses = projectChart{}
)

// chartUnknown is a chart that could not answer — a node booting, applying a
// revision, or behind the chart log. It is a fixture rather than an error
// literal at the call sites because the outcome it drives is the one the
// two-valued seam could not express at all.
var chartUnknown = projectChart{err: errors.New("this node is behind the chart log")}

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

// A CHART THAT COULD NOT ANSWER REFUSES THE WRITE, AND SAYS WHY.
//
// The outcome the two-valued seam had no way to express. `person` alone is
// enough for the operator half of this authority, so a chart that could not
// answer would land a lead's edit as an operator's — or, for a seat, refuse
// it naming the lead relation rather than the lag. A node holds no company
// while it is booting, while it is installing a revision, and for as long as
// it is behind the chart log, and none of those is "you do not lead this".
//
// TOLD TO TRY AGAIN, which is the difference that matters to whoever is
// holding the keyboard: they try again and it works, rather than going to
// find an authority they already hold.
func TestWriteProjectRefusesWhenTheChartCannotAnswer(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := projectRegistry(t, trk, chartUnknown)
	got := callWorkAs(t, reg, colleagueCaller(), tracker.WriteProjectTool,
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
	for _, want := range []string{"could not decide", "chart"} {
		if !strings.Contains(got.Output, want) {
			t.Errorf("the refusal does not say %q:\n%s", want, got.Output)
		}
	}
}
