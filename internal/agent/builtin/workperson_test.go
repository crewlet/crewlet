package builtin_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A SEAT NEVER RE-ORDERS SOMEBODY ELSE'S QUEUE, and a person does.
//
// That bar is the tracker's own, the one thing the authority table does NOT
// decide: [authz.ClassOwnOrLead] admits a lead, but a seat re-ordering a
// colleague's list is a hand-off in disguise, bypassing the guarded take and
// the reassignment budget. The relation half is the table's, so an operator
// (who leads nobody on the chart) must still reach the writer authorized.
func TestASeatNeverReordersSomebodyElsesQueue(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind      worklet.AuthorKind
		wantAgent bool
	}{
		"an operator token": {worklet.AuthorOperator, false},
		"a human":           {worklet.AuthorHuman, false},
		"a seat":            {worklet.AuthorAgent, true},
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
				Authorize: builtin.Decide(chartLeads),
			}) {
				if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
					t.Fatalf("register %s: %v", tool.Name(), err)
				}
			}
			got := callWork(t, reg, worklet.SetPrioritiesTool, map[string]any{
				"handle": "alice", "items": []any{"ENG-1"},
			})
			if got.Failed {
				t.Fatalf("set_priorities failed: %q", got.Output)
			}
			if person.authority.Agent != tc.wantAgent {
				t.Errorf("authority.Agent = %v, want %v — %s writing "+
					"somebody else's queue reaches the writer's gate with "+
					"this and the table's answer, and nothing else",
					person.authority.Agent, tc.wantAgent, name)
			}
			if !person.authority.Authorized {
				t.Error("the table's answer did not reach the writer, so " +
					"every cross-person write is refused at the record")
			}
		})
	}
}

// A PRIORITY ENTRY GIVEN AS A KEY IS RESOLVED TO AN ID: the list is stored as
// ids and joined on them, so an unresolved key would vanish from `my_work` and
// `preset=priorities` and suppress the wake while the call answered `applied`.
func TestAPriorityGivenAsAKeyIsResolved(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	person := &personSpy{}
	reg := operatorRegistry(t, trk, person)

	got := callWork(t, reg, worklet.SetPrioritiesTool, map[string]any{
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

	got := callWork(t, reg, worklet.SetPrioritiesTool, map[string]any{
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
				return builtin.Actor{Handle: "ops", Kind: worklet.AuthorOperator}, nil
			},
		},
		Authorize: builtin.Decide(chartLeads),
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

type personSpy struct {
	// handle is WHOSE record the last write named, and opID its operation:
	// a mark written under the wrong name is one nobody ever sees.
	handle     string
	opID       string
	priorities []string
	ifMatch    *uint64
	authority  worklet.PersonAuthority
	inbox      inbox.Gesture[worklet.Reason]
	seen       statelog.Position
	pins       worklet.PinGesture

	// actor is who the writer was resolved FOR: the record's subject is the
	// person and its author the credential, and a case asserting only one
	// would pass a fix that let a caller write as anybody.
	actor builtin.Actor
}

func (p *personSpy) WritePriorities(_ context.Context, opID, handle string,
	priorities []string, ifMatch *uint64, authority worklet.PersonAuthority) (
	worklet.WriteResult, error) {

	p.handle, p.opID = handle, opID
	p.priorities, p.ifMatch, p.authority = priorities, ifMatch, authority
	return worklet.WriteResult{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 20},
	}, nil
}

func (p *personSpy) WritePins(_ context.Context, opID, handle string,
	gesture worklet.PinGesture, authority worklet.PersonAuthority) (
	worklet.WriteResult, error) {

	p.handle, p.opID, p.pins, p.authority = handle, opID, gesture, authority
	return worklet.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

func (p *personSpy) MarkInbox(_ context.Context, opID, handle string, seen statelog.Position,
	gesture inbox.Gesture[worklet.Reason], authority worklet.PersonAuthority) (
	worklet.WriteResult, error) {

	p.handle, p.opID, p.inbox, p.seen, p.authority = handle, opID, gesture, seen, authority
	return worklet.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

// TestMarkInboxCarriesThePrimarySplit holds `mark_inbox` to passing
// [worklet.Person.PrimaryReasons] through: it is the split's only writer, so
// passing nil would clear it on every call.
func TestMarkInboxCarriesThePrimarySplit(t *testing.T) {
	t.Parallel()
	person := &personSpy{}
	reg := personRegistry(t, person)

	got := callWork(t, reg, worklet.MarkInboxTool, map[string]any{
		"primary_reasons": []any{"mention", "asked"},
	})
	if got.Failed {
		t.Fatalf("mark_inbox failed: %q", got.Output)
	}
	if person.inbox.PrimaryReasons == nil || !slices.Equal(*person.inbox.PrimaryReasons,
		[]worklet.Reason{worklet.ReasonMention, worklet.ReasonAsked}) {
		t.Fatalf("the writer was given %v, want [mention asked] — the split "+
			"has no other producer", person.inbox.PrimaryReasons)
	}

	// An unknown reason is refused naming the set, not dropped silently
	// by the validator behind it.
	bad := callWork(t, reg, worklet.MarkInboxTool, map[string]any{
		"primary_reasons": []any{"because-i-said-so"},
	})
	if !bad.Failed {
		t.Fatal("an unknown wake reason was accepted")
	}
	if want := "The reasons are: " + words.And(worklet.Reasons) + "."; !strings.Contains(bad.Output, want) {
		t.Fatalf("the refusal does not name the reasons that exist as %q: %q",
			want, bad.Output)
	}
}

// TestAMarkCarriesTheCallersFloor holds `mark_inbox`'s `seen` to the writer:
// the decide may run on a data node behind the one the inbox was read from, so
// without the caller's floor a notice they were shown is refused as unknown.
// None is the zero position, which waits for nothing; a malformed one, or one
// on another log, is refused before anything is asked.
func TestAMarkCarriesTheCallersFloor(t *testing.T) {
	t.Parallel()
	person := &personSpy{}
	reg := personRegistry(t, person)

	floor := statelog.Position{Stream: topics.TrackerLogStream, Generation: 1, Seq: 12}
	if got := callWork(t, reg, worklet.MarkInboxTool, map[string]any{
		"read": []any{"r-1"}, "seen": floor.String(),
	}); got.Failed {
		t.Fatalf("a mark carrying its floor failed: %q", got.Output)
	}
	if person.seen != floor {
		t.Fatalf("the mark seen at %s reached the writer seen at %s", floor, person.seen)
	}
	if got := callWork(t, reg, worklet.MarkInboxTool, map[string]any{
		"read": []any{"r-2"},
	}); got.Failed {
		t.Fatalf("a mark carrying no floor failed: %q", got.Output)
	}
	if !person.seen.IsZero() {
		t.Fatalf("the mark naming no floor reached the writer seen at %s, want the zero position",
			person.seen)
	}
	for _, bad := range []string{"not-a-position", topics.CodeLogStream + "@1:12"} {
		person.opID = ""
		refused := callWork(t, reg, worklet.MarkInboxTool, map[string]any{
			"read": []any{"r-3"}, "seen": bad,
		})
		if !refused.Failed || tools.RefusalOf(refused) != tools.RefusalInvalid {
			t.Errorf("a mark seen at %q answered %+v, want it refused invalid", bad, refused)
		}
		if person.opID != "" {
			t.Errorf("a mark seen at %q reached the writer", bad)
		}
	}
}

// TestWorkInboxIsServedAndNarrows keeps the read verb reachable and its two
// narrowings apart: `reasons` returns fewer rows, the primary split labels
// every row it returns.
func TestWorkInboxIsServedAndNarrows(t *testing.T) {
	t.Parallel()
	reg := personRegistry(t, &personSpy{})

	got := callPlain(t, reg, worklet.WorkInboxTool, map[string]any{
		"handle": "alice",
	})
	if got.Failed {
		t.Fatalf("work_inbox failed: %q", got.Output)
	}
	if !strings.Contains(got.Output, "ENG-1") {
		t.Fatalf("the answer carries no notice: %q", got.Output)
	}

	if missing := callPlain(t, reg, worklet.WorkInboxTool,
		map[string]any{}); !missing.Failed {

		t.Fatal("an inbox read naming nobody was accepted")
	}
	bad := callPlain(t, reg, worklet.WorkInboxTool, map[string]any{
		"handle": "alice", "reasons": []any{"because-i-said-so"},
	})
	if !bad.Failed {
		t.Fatal("an unknown wake reason was accepted")
	}
	if want := "The reasons are: " + words.And(worklet.Reasons) + "."; !strings.Contains(bad.Output, want) {
		t.Fatalf("the refusal does not name the reasons that exist as %q: %q",
			want, bad.Output)
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
	got, err := entry.Tool.Call(everyGrant(), args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

// personRegistry is the operator surface with the person seams wired, acting
// as the human seat `alice` and with no chart behind it.
func personRegistry(t *testing.T, person *personSpy) *tools.Registry {
	t.Helper()
	return personSurface(t, newFakeTracker(), person, func(
		context.Context, *turnctx.Turn) (builtin.Actor, error) {

		return builtin.Actor{Handle: "alice", Kind: worklet.AuthorHuman}, nil
	})
}

// personSurface is that surface over one fake tracker and one actor — so a case
// can vary the CALLER, which is the only thing the subject of these writes
// depends on. A nil actor takes the turn's seat, as a seat's registry does.
func personSurface(t *testing.T, trk *fakeTracker, person *personSpy,
	actor func(context.Context, *turnctx.Turn) (builtin.Actor, error)) *tools.Registry {

	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk,
			Writer: trk.as,
			Inbox:  trk,
			PersonWriter: func(a builtin.Actor) builtin.PersonWriter {
				// The attribution: see personSpy.actor.
				person.actor = a
				return person
			},
			Actor: actor,
		},
		Authorize: builtin.Decide(chartLeads),
	}) {
		// With the hints the operator surface serves, or the annotation
		// case below asserts nothing.
		if err := reg.RegisterWith(tool, tools.OriginBuiltin,
			builtin.AnnotationsFor(tool.Name())); err != nil {

			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// TestEveryOperatorToolIsAnnotatedDeliberately makes the table below the
// decision: `annotationsFor`'s default is FAIL-CLOSED (`ReadOnly: No`,
// `OpenWorld` unset), which [mcp.WritesToSharedSurface] reads as TRUE, so a
// tool missing from its switch is silently reported to every MCP client as a
// write to a surface a human reads, and refused to a sub-agent granted it.
func TestEveryOperatorToolIsAnnotatedDeliberately(t *testing.T) {
	t.Parallel()
	shared := map[string]bool{
		// The reads. Asking twice costs a round and changes nothing.
		worklet.ListWorkItemsTool:    false,
		worklet.GetWorkItemTool:      false,
		worklet.GetWorkCatalogueTool: false,
		worklet.ListProjectsTool:     false,
		worklet.DescribeProjectTool:  false,
		worklet.TaskActivityTool:     false,
		worklet.MyWorkTool:           false,
		worklet.SearchWorkItemsTool:  false,
		worklet.ListWorkViewsTool:    false,
		worklet.GetPersonTool:        false,
		worklet.WorkInboxTool:        false,

		// The writes everybody sees.
		worklet.CreateWorkItemTool:     true,
		worklet.UpdateWorkItemTool:     true,
		worklet.CommentOnWorkTool:      true,
		worklet.MergeWorkItemTool:      true,
		worklet.WriteProjectTool:       true,
		worklet.WriteWorkCatalogueTool: true,
		worklet.SaveWorkViewTool:       true,
		worklet.RemoveWorkItemTool:     true,
		worklet.RestoreWorkItemTool:    true,
		worklet.MoveWorkItemTool:       true,

		// A person's own state is not a shared surface: written only for
		// its owner, it surprises no second caller, which is what the
		// flag asks rather than "does this write".
		worklet.MarkInboxTool: false,
		worklet.SetPinsTool:   false,

		// EXCEPT the one that reaches across people: a lead may set
		// somebody else's queue, and that person sees it.
		worklet.SetPrioritiesTool: true,
	}

	reg := personRegistry(t, &personSpy{})
	snapshot := reg.Snapshot()
	seen := 0
	for _, name := range append(worklet.Tools(), worklet.OperatorOnlyTools()...) {
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

// THE PERSON TOOLS TAKE GESTURES, and their schemas say so: a caller builds
// its call from the schema, so `mark_inbox` offers the gesture's names (beside
// every operator write's `op_id`) and nothing of a replace-the-lists shape,
// under which one "mark this read" erased every other mark.
func TestThePersonToolSchemasTakeGestures(t *testing.T) {
	t.Parallel()
	reg := personRegistry(t, &personSpy{})
	snapshot := reg.Snapshot()
	props := func(name string) map[string]any {
		entry, held := snapshot.Lookup(name)
		if !held {
			t.Fatalf("%s is not registered", name)
		}
		return entry.Tool.Parameters()["properties"].(map[string]any)
	}

	marks := props(worklet.MarkInboxTool)
	var names []string
	for name := range marks {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"op_id", "primary_reasons", "read", "read_through", "seen", "snooze",
		"unread", "unsnooze"}; !slices.Equal(names, want) {
		t.Errorf("mark_inbox takes %v, want %v", names, want)
	}
	entry, _ := snapshot.Lookup(worklet.MarkInboxTool)
	if strings.Contains(entry.Tool.Description(), "REPLACES") {
		t.Error("mark_inbox still describes itself as replacing the lists")
	}
	for _, key := range []string{"views", "favorites"} {
		change, ok := props(worklet.SetPinsTool)[key].(map[string]any)
		if !ok || change["type"] != "object" {
			t.Errorf("set_pins' %s is %v, want a change object", key, change)
			continue
		}
		for _, verb := range []string{"add", "remove", "set"} {
			if _, held := change["properties"].(map[string]any)[verb]; !held {
				t.Errorf("set_pins' %s takes no %q", key, verb)
			}
		}
	}
	if _, held := props(worklet.SetPrioritiesTool)["if_match"]; !held {
		t.Error("set_priorities takes no if_match, so a reorder from a stale " +
			"screen replaces the newer list")
	}
}

// AND THE ARGUMENTS BECOME THE GESTURE, field for field — including the two
// absences that mean something: no `primary_reasons` leaves the person's
// choice alone where an empty one takes the default back, and no `if_match`
// writes unconditionally where a zero one means "nobody has written this yet".
func TestThePersonToolsPassTheGestureThrough(t *testing.T) {
	t.Parallel()
	person := &personSpy{}
	reg := personRegistry(t, person)

	got := callWork(t, reg, worklet.MarkInboxTool, map[string]any{
		"read":         []any{"r-1"},
		"snooze":       []any{map[string]any{"record_id": "r-2", "until": "2031-05-01T09:00:00Z"}},
		"read_through": "CREWLET_TRACKER_LOG@1:7",
	})
	if got.Failed {
		t.Fatalf("mark_inbox failed: %q", got.Output)
	}
	g := person.inbox
	switch {
	case !slices.Equal(g.Read, []string{"r-1"}):
		t.Errorf("read = %v", g.Read)
	case len(g.Snooze) != 1 || g.Snooze[0].RecordID != "r-2" ||
		g.Snooze[0].Until.Format("2006-01-02") != "2031-05-01":
		t.Errorf("snooze = %+v", g.Snooze)
	case g.ReadThrough == nil || g.ReadThrough.Generation != 1 || g.ReadThrough.Seq != 7:
		t.Errorf("read_through = %+v", g.ReadThrough)
	case g.PrimaryReasons != nil:
		t.Errorf("an absent primary_reasons became %v, which would clear the "+
			"person's choice", *g.PrimaryReasons)
	}
	if got := callWork(t, reg, worklet.MarkInboxTool, map[string]any{
		"primary_reasons": []any{},
	}); got.Failed || person.inbox.PrimaryReasons == nil {
		t.Errorf("an empty primary_reasons did not reach the writer as a "+
			"choice: %q", got.Output)
	}
	if got := callWork(t, reg, worklet.MarkInboxTool, map[string]any{}); !got.Failed {
		t.Error("a mark_inbox naming nothing was accepted")
	}

	// A bare list where a change belongs is refused naming the shape, not
	// read as the whole strip.
	bare := callWork(t, reg, worklet.SetPinsTool, map[string]any{"views": []any{"v-1"}})
	if !bare.Failed || !strings.Contains(bare.Output, "add") {
		t.Errorf("a bare list of views answered %q", bare.Output)
	}
	if got := callWork(t, reg, worklet.SetPinsTool, map[string]any{
		"views":     map[string]any{"remove": []any{"v-1"}},
		"favorites": map[string]any{"add": []any{map[string]any{"kind": "project", "id": "ENG"}}},
	}); got.Failed {
		t.Fatalf("set_pins failed: %q", got.Output)
	}
	if p := person.pins; !slices.Equal(p.Views.Remove, []string{"v-1"}) ||
		p.Views.Set != nil || len(p.Favorites.Add) != 1 {
		t.Errorf("the pin gesture reached the writer as %+v", p)
	}

	for _, c := range []struct {
		args map[string]any
		want *uint64
	}{
		{map[string]any{"items": []any{"ENG-1"}}, nil},
		{map[string]any{"items": []any{"ENG-1"}, "if_match": float64(0)}, new(uint64)},
	} {
		person.ifMatch = nil
		if got := callWork(t, reg, worklet.SetPrioritiesTool, c.args); got.Failed {
			t.Fatalf("set_priorities failed: %q", got.Output)
		}
		if (person.ifMatch == nil) != (c.want == nil) ||
			(c.want != nil && *person.ifMatch != *c.want) {
			t.Errorf("if_match %v reached the writer as %v", c.args["if_match"], person.ifMatch)
		}
	}
	if got := callWork(t, reg, worklet.SetPrioritiesTool, map[string]any{
		"items": []any{"ENG-1"}, "if_match": float64(-1),
	}); !got.Failed {
		t.Error("a negative if_match was accepted")
	}
}

// THE SNOOZED SCOPE REACHES THE READER, DEFAULTS TO HIDING, AND REFUSES A
// SCOPE IT DOES NOT KNOW. The reader refuses the zero value, so the tool is
// what makes "my inbox" mean "not what I put off" — and a misspelt scope read
// as the default would answer "what did I put off" with the whole inbox.
func TestWorkInboxTakesTheSnoozedScope(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := personSurface(t, trk, &personSpy{}, func(
		context.Context, *turnctx.Turn) (builtin.Actor, error) {

		return builtin.Actor{Handle: "alice", Kind: worklet.AuthorHuman}, nil
	})

	if got := callPlain(t, reg, worklet.WorkInboxTool, map[string]any{
		"handle": "alice",
	}); got.Failed {
		t.Fatalf("work_inbox failed: %q", got.Output)
	}
	if trk.inboxQuery.Snoozed != inbox.SnoozeExclude {
		t.Errorf("no `snoozed` reached the reader as %q, want %q",
			trk.inboxQuery.Snoozed, inbox.SnoozeExclude)
	}
	if got := callPlain(t, reg, worklet.WorkInboxTool, map[string]any{
		"handle": "alice", "snoozed": "only",
	}); got.Failed {
		t.Fatalf("work_inbox failed: %q", got.Output)
	}
	if trk.inboxQuery.Snoozed != inbox.SnoozeOnly {
		t.Errorf("snoozed=only reached the reader as %q", trk.inboxQuery.Snoozed)
	}
	bad := callPlain(t, reg, worklet.WorkInboxTool, map[string]any{
		"handle": "alice", "snoozed": "bogus",
	})
	if !bad.Failed || !strings.Contains(bad.Output, "include") {
		t.Fatalf("snoozed=bogus answered %q, want a refusal naming the scopes",
			bad.Output)
	}
}
