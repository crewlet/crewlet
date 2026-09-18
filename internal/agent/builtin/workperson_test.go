package builtin_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// AN OPERATOR WRITING SOMEBODY'S PRIORITIES CARRIES THE AUTHORITY TO DO IT.
//
// This is the wiring that decided whether the `prioritised` wake could fire at
// all. `set_priorities` is registered on the operator MCP alone, and an
// operator's actor is an API TOKEN's name — never a handle in the org chart —
// so the lead lookup can never match it. With the tool sending only `Lead`,
// the writer refused every cross-person write through the only surface that
// exists, and omitting the handle silently wrote a person record for the
// token instead of for a person.
func TestAnOperatorCarriesTheAuthorityToWriteAPersonsPriorities(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind       tracker.AuthorKind
		wantPerson bool
	}{
		"an operator token": {tracker.AuthorOperator, true},
		"a human":           {tracker.AuthorHuman, true},
		// A SEAT CARRIES NEITHER unless it genuinely leads the handle,
		// which the lead seam answers separately.
		"a seat": {tracker.AuthorAgent, false},
	} {
		t.Run(name, func(t *testing.T) {
			person := &personSpy{}
			reg := tools.NewRegistry()
			for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
				Work: builtin.WorkDeps{
					Reader:       newFakeTracker(),
					Writer:       newFakeTracker().as,
					PersonWriter: func(builtin.Actor) builtin.PersonWriter { return person },
					Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
						return builtin.Actor{Handle: "ops", Kind: tc.kind}, nil
					},
				},
			}) {
				if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
					t.Fatalf("register %s: %v", tool.Name(), err)
				}
			}
			got := callWork(t, reg, tracker.SetPrioritiesTool, map[string]any{
				"handle": "alice", "items": []any{"ENG-1"},
			})
			if got.Failed {
				t.Fatalf("set_priorities failed: %q", got.Output)
			}
			if person.authority.Person != tc.wantPerson {
				t.Errorf("authority.Person = %v, want %v — %s writing "+
					"somebody else's queue reaches the writer's gate with "+
					"this and nothing else",
					person.authority.Person, tc.wantPerson, name)
			}
		})
	}
}

// A PRIORITY ENTRY GIVEN AS A KEY IS RESOLVED TO AN ID.
//
// The list is stored as ids and read back by joining on them, and this tool's
// own description invites a key. An unresolved `ENG-1` failed in three places
// at once and reported nothing anywhere: the entry vanished from `my_work`, it
// vanished from `preset=priorities`, and the wake that tells the person their
// queue changed was silently suppressed — while the call answered
// `outcome: applied`.
func TestAPriorityGivenAsAKeyIsResolved(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	person := &personSpy{}
	reg := operatorRegistry(t, trk, person)

	got := callWork(t, reg, tracker.SetPrioritiesTool, map[string]any{
		"handle": "alice", "items": []any{"ENG-1"},
	})
	if got.Failed {
		t.Fatalf("set_priorities failed: %q", got.Output)
	}
	if len(person.priorities) != 1 || person.priorities[0] != "i1" {
		t.Fatalf("the writer was given %v, want the task's id — a key never "+
			"joins, so the entry is stored and then invisible everywhere",
			person.priorities)
	}
}

// AND ONE THAT NAMES NOTHING IS REFUSED rather than stored: a queue entry
// pointing at no task is one the person never sees and nobody is told about.
func TestAPriorityNamingNoTaskIsRefused(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	person := &personSpy{}
	reg := operatorRegistry(t, trk, person)

	got := callWork(t, reg, tracker.SetPrioritiesTool, map[string]any{
		"handle": "alice", "items": []any{"ENG-404"},
	})
	if !got.Failed {
		t.Fatalf("a priority naming no task was stored: %q", got.Output)
	}
	if len(person.priorities) != 0 {
		t.Fatalf("the writer was still called with %v", person.priorities)
	}
}

// operatorRegistry is the operator surface over a fake tracker and a person
// spy, which is the only surface set_priorities is registered on.
func operatorRegistry(t *testing.T, trk *fakeTracker, person builtin.PersonWriter) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader:       trk,
			Writer:       trk.as,
			PersonWriter: func(builtin.Actor) builtin.PersonWriter { return person },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
			},
		},
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

type personSpy struct {
	handle     string
	priorities []string
	authority  tracker.PersonAuthority
	reasons    []tracker.Reason
}

func (p *personSpy) WritePriorities(_ context.Context, _, handle string,
	priorities []string, authority tracker.PersonAuthority) (
	tracker.WriteResult, error) {

	p.handle, p.priorities, p.authority = handle, priorities, authority
	return tracker.WriteResult{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 20},
	}, nil
}

func (p *personSpy) WritePins(_ context.Context, _, _ string, _ []string,
	_ []tracker.Favorite) (
	tracker.WriteResult, error) {

	return tracker.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

func (p *personSpy) WriteInbox(_ context.Context, _, _ string,
	_, _, _ []tracker.InboxEntry, reasons []tracker.Reason,
	_ tracker.Position) (tracker.WriteResult, error) {

	p.reasons = reasons
	return tracker.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

// TestMarkInboxCarriesThePrimarySplit is the finding on the write side.
// [tracker.Person.PrimaryReasons] was validated, stored, replicated and
// reported — and `mark_inbox` passed nil for it on every call, so the one
// writer that could have set it CLEARED it instead. A preference nothing can
// express is storage for a rule nobody wrote.
func TestMarkInboxCarriesThePrimarySplit(t *testing.T) {
	t.Parallel()
	person := &personSpy{}
	reg := personRegistry(t, person)

	got := callWork(t, reg, tracker.MarkInboxTool, map[string]any{
		"primary_reasons": []any{"mention", "asked"},
	})
	if got.Failed {
		t.Fatalf("mark_inbox failed: %q", got.Output)
	}
	if !slices.Equal(person.reasons, []tracker.Reason{
		tracker.ReasonMention, tracker.ReasonAsked,
	}) {
		t.Fatalf("the writer was given %v, want [mention asked] — the split "+
			"has no other producer", person.reasons)
	}

	// AND AN UNKNOWN REASON IS REFUSED NAMING THE SET, rather than
	// written and silently dropped by the validator behind it.
	bad := callWork(t, reg, tracker.MarkInboxTool, map[string]any{
		"primary_reasons": []any{"because-i-said-so"},
	})
	if !bad.Failed {
		t.Fatal("an unknown wake reason was accepted")
	}
	if !strings.Contains(bad.Output, "mention") {
		t.Fatalf("the refusal does not name the reasons that exist: %q",
			bad.Output)
	}
}

// TestWorkInboxIsServedAndNarrows keeps the read verb reachable and its two
// narrowings apart: `reasons` returns fewer rows, the primary split labels
// every row it returns.
func TestWorkInboxIsServedAndNarrows(t *testing.T) {
	t.Parallel()
	reg := personRegistry(t, &personSpy{})

	got := callPlain(t, reg, tracker.WorkInboxTool, map[string]any{
		"handle": "alice",
	})
	if got.Failed {
		t.Fatalf("work_inbox failed: %q", got.Output)
	}
	if !strings.Contains(got.Output, "ENG-1") {
		t.Fatalf("the answer carries no notice: %q", got.Output)
	}

	if missing := callPlain(t, reg, tracker.WorkInboxTool,
		map[string]any{}); !missing.Failed {

		t.Fatal("an inbox read naming nobody was accepted")
	}
	bad := callPlain(t, reg, tracker.WorkInboxTool, map[string]any{
		"handle": "alice", "reasons": []any{"because-i-said-so"},
	})
	if !bad.Failed {
		t.Fatal("an unknown wake reason was accepted")
	}
	if !strings.Contains(bad.Output, "assignee") {
		t.Fatalf("the refusal does not name the reasons that exist: %q",
			bad.Output)
	}
}

// callPlain calls a tool that takes no turn — a read whose authority is the
// surface's rather than a seat's, which is what every operator read is.
func callPlain(t *testing.T, reg *tools.Registry, name string,
	args map[string]any) tools.Result {

	t.Helper()
	entry, held := reg.Lookup(name)
	if !held {
		t.Fatalf("%s is not registered", name)
	}
	got, err := entry.Tool.Call(t.Context(), args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

// personRegistry is the operator surface with the person seams wired.
func personRegistry(t *testing.T, person *personSpy) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader:       newFakeTracker(),
			Writer:       newFakeTracker().as,
			Inbox:        newFakeTracker(),
			PersonWriter: func(builtin.Actor) builtin.PersonWriter { return person },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{
					Handle: "alice", Kind: tracker.AuthorHuman,
				}, nil
			},
		},
	}) {
		// WITH THE HINTS, which is what the operator surface itself
		// serves now — registering without them would make the
		// annotation case below assert nothing.
		if err := reg.RegisterWith(tool, tools.OriginBuiltin,
			builtin.AnnotationsFor(tool.Name())); err != nil {

			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// TestEveryOperatorToolIsAnnotatedDeliberately closes the hole that classified
// three reads as writes. `annotationsFor` is a switch with a FAIL-CLOSED
// default — `ReadOnly: No` with `OpenWorld` unset, which is exactly what
// [mcp.WritesToSharedSurface] reads as TRUE — so a tool added to the operator
// surface and forgotten in the switch is silently reported to every MCP client
// as a write to a surface a human reads, and refused to a sub-agent that was
// granted it. Nothing failed; the tool just stopped working for one caller.
//
// The table is the DECISION, restated where a reviewer sees it: adding a tool
// to the operator surface without deciding this fails here.
func TestEveryOperatorToolIsAnnotatedDeliberately(t *testing.T) {
	t.Parallel()
	shared := map[string]bool{
		// The reads. Asking twice costs a round and changes nothing.
		tracker.ListWorkItemsTool:    false,
		tracker.GetWorkItemTool:      false,
		tracker.GetWorkCatalogueTool: false,
		tracker.ListProjectsTool:     false,
		tracker.DescribeProjectTool:  false,
		tracker.SprintReportTool:     false,
		tracker.TaskActivityTool:     false,
		tracker.MyWorkTool:           false,
		tracker.ListWorkGoalsTool:    false,
		tracker.SearchWorkItemsTool:  false,
		tracker.ListWorkViewsTool:    false,
		tracker.GetPersonTool:        false,
		tracker.WorkInboxTool:        false,

		// The writes everybody sees.
		tracker.CreateWorkItemTool:     true,
		tracker.UpdateWorkItemTool:     true,
		tracker.CommentOnWorkTool:      true,
		tracker.MergeWorkItemTool:      true,
		tracker.WriteProjectTool:       true,
		tracker.WriteWorkGoalTool:      true,
		tracker.WriteWorkCatalogueTool: true,
		tracker.SaveWorkViewTool:       true,
		tracker.ManageSprintTool:       true,
		tracker.RemoveWorkItemTool:     true,
		tracker.RestoreWorkItemTool:    true,

		// A PERSON'S OWN STATE IS NOT A SHARED SURFACE. Each is written
		// only on behalf of the person whose it is, so a second caller
		// cannot be surprised by one — which is the question the flag
		// asks, rather than "does this write".
		tracker.MarkInboxTool: false,
		tracker.SetPinsTool:   false,

		// EXCEPT the one that reaches across people: a lead may set
		// somebody else's queue, and that person sees it.
		tracker.SetPrioritiesTool: true,
	}

	reg := personRegistry(t, &personSpy{})
	snapshot := reg.Snapshot()
	seen := 0
	for _, name := range append(tracker.Tools(), tracker.OperatorOnlyTools()...) {
		entry, held := snapshot.Lookup(name)
		if !held {
			continue
		}
		seen++
		want, classified := shared[name]
		if !classified {
			t.Errorf("%q is on the operator surface and this table does not "+
				"say whether it writes a surface somebody else reads — "+
				"decide, then add it", name)
			continue
		}
		if got := mcp.WritesToSharedSurface(entry.Annotations); got != want {
			t.Errorf("WritesToSharedSurface(%q) = %v, want %v (annotations "+
				"%+v) — the fail-closed default is how a read becomes a write",
				name, got, want, entry.Annotations)
		}
	}
	if seen == 0 {
		t.Fatal("the operator surface registered nothing, so this asserts " +
			"nothing")
	}
}

// sprintSpy records the one call the sprint tool makes.
type sprintSpy struct {
	started int
}

func (s *sprintSpy) StartSprint(context.Context, string, string, int) (
	tracker.WriteResult, error) {

	s.started++
	return tracker.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

func (s *sprintSpy) CloseSprint(context.Context, string, string, int) (
	tracker.WriteResult, error) {

	return tracker.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

func (s *sprintSpy) RolloverSprint(context.Context, string, string, int,
	tracker.RolloverTarget) (int, bool, error) {

	return 0, false, nil
}

// AND AN OPERATOR MANAGES A SPRINT, which is the same hole one tool over.
//
// `manage_sprint` gated on the LEAD lookup alone, and that lookup is asked
// about the actor's handle — which for an operator is an API token's name and
// never a handle in the org chart. So starting or closing a sprint was
// refused for every operator in every company, through the only surface a
// founder has: the capacities a workload screen is read against are declared
// on a policy, and a policy's capacities count only while a sprint is
// RUNNING, so this gate is what stood between the two halves of that screen.
func TestAnOperatorCarriesTheAuthorityToManageASprint(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind      tracker.AuthorKind
		wantStart int
	}{
		"an operator token": {tracker.AuthorOperator, 1},
		"a human":           {tracker.AuthorHuman, 1},
		// A SEAT STILL NEEDS THE LEAD RELATION, which the seam below
		// answers no to for every handle. A sprint decision belongs to
		// whoever plans the team's fortnight.
		"a seat": {tracker.AuthorAgent, 0},
	} {
		t.Run(name, func(t *testing.T) {
			sprints := &sprintSpy{}
			reg := tools.NewRegistry()
			for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
				Work: builtin.WorkDeps{
					// THE SPRINT EXISTS, which this case is not about
					// and every case needs: the tool resolves the
					// sprint before it writes, so a reader answering
					// nothing refuses on the wrong ground and the
					// authority under test is never reached. See
					// worksprints_test.go for what that resolution is.
					Reader:       trackerHoldingSprint("ENG", 1),
					Writer:       newFakeTracker().as,
					SprintWriter: func(builtin.Actor) builtin.SprintWriter { return sprints },
					Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
						return builtin.Actor{Handle: "ops", Kind: tc.kind}, nil
					},
				},
				// THE CHART SAYS NO TO EVERYBODY, which is exactly the
				// state an operator is in: they are not a seat, so
				// there is no handle for it to say yes about.
				LeadsProject: func(context.Context, string, string) bool { return false },
			}) {
				if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
					t.Fatalf("register %s: %v", tool.Name(), err)
				}
			}
			got := callWork(t, reg, tracker.ManageSprintTool, map[string]any{
				"project": "ENG", "action": "start", "sprint": 1,
			})
			if sprints.started != tc.wantStart {
				t.Fatalf("the sprint was started %d times, want %d — %q",
					sprints.started, tc.wantStart, got.Output)
			}
			if tc.wantStart == 0 && !strings.Contains(got.Output, "person's own") {
				t.Errorf("the refusal does not name the other way in: %q",
					got.Output)
			}
		})
	}
}

// projectSpy records the edit the project tool built.
type projectSpy struct {
	edit tracker.ProjectEdit
	tags tracker.TagEdit
}

func (p *projectSpy) WriteProject(_ context.Context, _, _ string,
	edit tracker.ProjectEdit, _ tracker.ProjectAuthority) (
	tracker.WriteResult, error) {

	p.edit = edit
	return tracker.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

func (p *projectSpy) WriteTags(_ context.Context, _, _ string,
	edit tracker.TagEdit, _ tracker.TagAuthority) (tracker.WriteResult, error) {

	p.tags = edit
	return tracker.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

func (p *projectSpy) EnsureTags(context.Context, string, string, []string) (
	[]string, []string, error) {

	return nil, nil, nil
}

// A SPRINT CAPACITY HAD NO PRODUCER, so the field the whole comparison rests
// on could only ever be written by a test.
//
// `SprintPolicy.Capacity` is validated by `checkSprintPolicy`, read by the
// sprint report's `AssigneeFigures` and read again by the workload — and
// `sprintPolicyArg` read nine fields and not that one, nor did the schema
// declare it. So the validation loop ran over an always-empty map, and both
// readers answered "nobody declared a capacity" in every company that has ever
// run this engine. `point_scale` is the same hole beside it.
func TestASprintPolicyCanDeclareItsCapacities(t *testing.T) {
	t.Parallel()
	project := &projectSpy{}
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader:        newFakeTracker(),
			Writer:        newFakeTracker().as,
			ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return project },
			Actor: func(context.Context, *turnctx.Turn) (builtin.Actor, error) {
				return builtin.Actor{Handle: "ops", Kind: tracker.AuthorOperator}, nil
			},
		},
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	got := callWork(t, reg, tracker.WriteProjectTool, map[string]any{
		"project": "ENG",
		"sprints": map[string]any{
			"length_days": 14,
			"measure":     "points",
			"capacity": map[string]any{
				"ada": map[string]any{"points": 8.0},
				"bo":  map[string]any{"estimate_min": 600.0},
				// A MALFORMED ENTRY IS SKIPPED rather than failing the
				// whole edit: the policy carries nine other fields and
				// losing them over one bad capacity is the worse answer.
				"broken": "not an object",
			},
			"point_scale": []any{1.0, 2.0, 3.0, 5.0, 8.0},
		},
	})
	if got.Failed {
		t.Fatalf("write_project failed: %q", got.Output)
	}
	policy := project.edit.Sprints
	if policy == nil || policy.Policy == nil {
		t.Fatal("the edit carries no sprint policy at all")
	}
	if len(policy.Policy.Capacity) != 2 {
		t.Fatalf("the policy declares %+v, want the two well-formed seats — "+
			"a field nothing can set is a feature only a test has",
			policy.Policy.Capacity)
	}
	if got := policy.Policy.Capacity["ada"].Points; got != 8 {
		t.Errorf("ada's capacity is %v points, want 8", got)
	}
	if got := policy.Policy.Capacity["bo"].EstimateMin; got != 600 {
		t.Errorf("bo's capacity is %v minutes, want 600 — the other measure "+
			"is not a second field nobody reads", got)
	}
	if !slices.Equal(policy.Policy.PointScale, []float64{1, 2, 3, 5, 8}) {
		t.Errorf("the point scale is %v, want the one that was sent — the "+
			"same hole beside the capacity", policy.Policy.PointScale)
	}
	// AN ABSENT CAPACITY IS NIL, never an empty map, and BOTH ways of
	// arriving at absent are tested — the key that is not there, and the
	// object whose every entry was malformed. They reach different guards,
	// and the second is the one a caller actually produces: a client
	// sending a capacity it built from a form with nothing valid in it.
	if capacityOf(t, reg, project, map[string]any{"length_days": 14}) != nil {
		t.Error("a policy that names no capacities carries an empty map")
	}
	if got := capacityOf(t, reg, project, map[string]any{
		"length_days": 14,
		"capacity":    map[string]any{"ada": "not an object", "bo": 7.0},
	}); got != nil {
		t.Errorf("a capacity with nothing well-formed in it is %v, want "+
			"nothing — an empty map says somebody declared capacities and "+
			"named nobody, which is a different claim", got)
	}
}

// capacityOf runs one more write_project and hands back the capacity it built.
func capacityOf(t *testing.T, reg *tools.Registry, project *projectSpy,
	sprints map[string]any) map[string]tracker.Capacity {

	t.Helper()
	if got := callWork(t, reg, tracker.WriteProjectTool, map[string]any{
		"project": "ENG", "sprints": sprints,
	}); got.Failed {
		t.Fatalf("write_project failed: %q", got.Output)
	}
	return project.edit.Sprints.Policy.Capacity
}
