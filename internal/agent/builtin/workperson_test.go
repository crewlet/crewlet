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
	// handle is WHOSE record the last write named, and opID the operation
	// it wrote under. Both on every verb, because the subject of a person
	// write is the whole of what this file is about: a mark written under
	// the wrong name is a mark nobody ever sees.
	handle     string
	opID       string
	priorities []string
	ifMatch    *uint64
	authority  tracker.PersonAuthority
	inbox      tracker.InboxGesture
	pins       tracker.PinGesture

	// actor is who the surface resolved the writer FOR, which is the other
	// half: the record's subject is the person and its author is still
	// the credential, and a case asserting one without the other would
	// pass on a fix that let a caller write as anybody.
	actor builtin.Actor
}

func (p *personSpy) WritePriorities(_ context.Context, opID, handle string,
	priorities []string, ifMatch *uint64, authority tracker.PersonAuthority) (
	tracker.WriteResult, error) {

	p.handle, p.opID = handle, opID
	p.priorities, p.ifMatch, p.authority = priorities, ifMatch, authority
	return tracker.WriteResult{
		Outcome:  statelog.OutcomeApplied,
		Position: statelog.Position{Stream: "S", Generation: 1, Seq: 20},
	}, nil
}

func (p *personSpy) WritePins(_ context.Context, opID, handle string,
	gesture tracker.PinGesture) (tracker.WriteResult, error) {

	p.handle, p.opID, p.pins = handle, opID, gesture
	return tracker.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

func (p *personSpy) MarkInbox(_ context.Context, opID, handle string,
	gesture tracker.InboxGesture) (tracker.WriteResult, error) {

	p.handle, p.opID, p.inbox = handle, opID, gesture
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
	if person.inbox.PrimaryReasons == nil || !slices.Equal(*person.inbox.PrimaryReasons,
		[]tracker.Reason{tracker.ReasonMention, tracker.ReasonAsked}) {
		t.Fatalf("the writer was given %v, want [mention asked] — the split "+
			"has no other producer", person.inbox.PrimaryReasons)
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

// personRegistry is the operator surface with the person seams wired, acting
// as the human seat `alice` and with no chart behind it.
func personRegistry(t *testing.T, person *personSpy) *tools.Registry {
	t.Helper()
	return personSurface(t, newFakeTracker(), person, func(
		context.Context, *turnctx.Turn) (builtin.Actor, error) {

		return builtin.Actor{Handle: "alice", Kind: tracker.AuthorHuman}, nil
	}, nil)
}

// personSurface is that surface over one fake tracker, one actor and one party
// lookup — so a case can vary the CALLER, which is the only thing the subject
// of these writes depends on.
func personSurface(t *testing.T, trk *fakeTracker, person *personSpy,
	actor func(context.Context, *turnctx.Turn) (builtin.Actor, error),
	party func(string) tracker.Party) *tools.Registry {

	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk,
			Writer: trk.as,
			Inbox:  trk,
			Party:  party,
			PersonWriter: func(a builtin.Actor) builtin.PersonWriter {
				// THE ACTOR THE WRITER WAS RESOLVED FOR, which is
				// what carries the attribution: the record's
				// subject is the person and its author is still
				// the credential, and a case that asserted only
				// the first would pass on a fix that let a caller
				// write as anybody.
				person.actor = a
				return person
			},
			Actor: actor,
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

// boundOperator is what `operator.WorkActor` builds for a token a company bound
// to a human seat with `contact.crewlet_operator_id`: the credential in the
// author field, the kind saying it is not a seat, and the PERSON it names in
// [builtin.Actor.Seat].
func boundOperator(context.Context, *turnctx.Turn) (builtin.Actor, error) {
	return builtin.Actor{
		Handle: "founder", Kind: tracker.AuthorOperator,
		OperatorID: "founder", Seat: "jane-founder",
	}, nil
}

// unboundOperator is a token nobody is bound to — a pipeline, an operator
// outside the org chart. An ordinary state, and not an error.
func unboundOperator(context.Context, *turnctx.Turn) (builtin.Actor, error) {
	return builtin.Actor{
		Handle: "ci", Kind: tracker.AuthorOperator, OperatorID: "ci",
	}, nil
}

// boundParties is the chart lookup the operator surface wires: this company
// bound `founder` to `jane-founder` and nobody else.
func boundParties(handle string) tracker.Party {
	if handle == "jane-founder" {
		return tracker.Party{Handle: handle, OperatorID: "founder"}
	}
	return tracker.PartyOf(handle)
}

// A BOUND OPERATOR'S OWN MARKS AND PINS ARE THE PERSON'S, AND THE RECORD
// STILL NAMES THE TOKEN.
//
// These two verbs passed `actor.Handle` as the record's SUBJECT, and through
// `/operator/mcp` that is the TOKEN's id. So a founder whose assistant marked
// their inbox read wrote a whole second person record called `founder`, while
// their own screen — which asks under the seat their colleagues assign to —
// showed an inbox where nothing had ever been read, a strip with none of their
// pins, and a queue nobody had ever ordered.
//
// It is not attribution: the author field is a separate, correct concern, and
// a tracker whose author is chosen by the writer is not an audit trail. It is
// WHOSE STATE the document holds, and that is the person.
//
// BOTH VERBS IN ONE CASE, because they are the two that carry a subject with
// no handle argument to correct it — one of them fixed and the other left
// would look exactly like this from the screen that is still half empty.
func TestABoundOperatorsOwnStateIsWrittenUnderTheirSeat(t *testing.T) {
	t.Parallel()
	for name, call := range map[string]struct {
		tool string
		args map[string]any
		want string
	}{
		"pins":  {tracker.SetPinsTool, map[string]any{"views": map[string]any{"add": []any{"v-1"}}}, "pins-jane-founder-"},
		"inbox": {tracker.MarkInboxTool, map[string]any{"read": []any{"r-1"}}, "inbox-jane-founder-"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			person := &personSpy{}
			reg := personSurface(t, newFakeTracker(), person,
				boundOperator, boundParties)

			if got := callNoTurn(t, reg, call.tool, call.args); got.Failed {
				t.Fatalf("%s failed: %q", call.tool, got.Output)
			}
			if person.handle != "jane-founder" {
				t.Errorf("%s wrote %q's record, want the person the token is "+
					"bound to — written under the credential it is a second "+
					"record nothing this person opens will ever read",
					call.tool, person.handle)
			}
			// AND THE OPERATION ID FOLLOWS THE SUBJECT, because it is
			// what the ledger collapses a redelivery against: keyed on
			// one name while the record is keyed on another, two people's
			// writes share a scope.
			if !strings.HasPrefix(person.opID, call.want) {
				t.Errorf("%s wrote under operation %q, want one scoped to %q",
					call.tool, person.opID, call.want)
			}
			// AND THE AUDIT TRAIL IS UNTOUCHED.
			if person.actor.Handle != "founder" ||
				person.actor.Kind != tracker.AuthorOperator ||
				person.actor.OperatorID != "founder" {

				t.Errorf("%s is authored by %+v — the author is the credential "+
					"and the kind says it is not a seat", call.tool, person.actor)
			}
		})
	}
}

// AND AN UNBOUND TOKEN WRITES UNDER ITS OWN ID, EXACTLY AS BEFORE.
//
// Leaving a token unbound is ordinary — an operator outside the org chart, a
// pipeline — and it acts as itself rather than being refused. This is the half
// a careless fix breaks: a lookup that answered the first seat, or refused,
// would take a working surface away from every company that never wrote the
// binding.
func TestAnUnboundOperatorStillWritesItsOwnRecord(t *testing.T) {
	t.Parallel()
	person := &personSpy{}
	reg := personSurface(t, newFakeTracker(), person, unboundOperator, boundParties)

	if got := callNoTurn(t, reg, tracker.SetPinsTool, map[string]any{
		"views": map[string]any{"add": []any{"v-1"}},
	}); got.Failed {
		t.Fatalf("set_pins failed: %q", got.Output)
	}
	if person.handle != "ci" {
		t.Errorf("an unbound token wrote %q's record, want its own", person.handle)
	}
	// AND SO DOES ITS PRIORITY LIST, which is the verb that resolves an
	// omitted handle: the caller's own identity is not a typo, and running
	// it through the roster refused an operator their own list by name.
	if got := callNoTurn(t, reg, tracker.SetPrioritiesTool, map[string]any{
		"items": []any{"ENG-1"},
	}); got.Failed {
		t.Fatalf("set_priorities for an unbound token failed: %q", got.Output)
	}
	if person.handle != "ci" {
		t.Errorf("set_priorities wrote %q's list, want the caller's own",
			person.handle)
	}
}

// A BOUND OPERATOR OMITTING THE HANDLE ORDERS THE PERSON'S QUEUE.
//
// `set_priorities` defaulted to `actor.Handle` too, so a founder saying "these
// three first" put them on a list belonging to their credential — invisible in
// `my_work`, invisible under `preset=priorities`, and with the `prioritised`
// wake routed to a handle nobody holds.
func TestABoundOperatorsOwnQueueIsTheirSeats(t *testing.T) {
	t.Parallel()
	person := &personSpy{}
	trk := newFakeTracker()
	reg := personSurface(t, trk, person, boundOperator, boundParties)

	if got := callNoTurn(t, reg, tracker.SetPrioritiesTool, map[string]any{
		"items": []any{"ENG-1"},
	}); got.Failed {
		t.Fatalf("set_priorities failed: %q", got.Output)
	}
	if person.handle != "jane-founder" {
		t.Errorf("the list was written for %q, want the person the token names",
			person.handle)
	}
}

// AND MY WORK ANSWERS FOR BOTH OF THAT PERSON'S NAMES.
//
// The read side is the mirror of the same defect: asked about the bare handle,
// a founder's assistant got the TOKEN's day — seven blocks over rows their
// colleagues had filed against the seat, and not one of them matched. The
// alias behind it is what still reaches the rows their own earlier writes left
// under the credential.
func TestMyWorkThroughAnOperatorAnswersForTheirSeat(t *testing.T) {
	t.Parallel()
	person := &personSpy{}
	trk := newFakeTracker()
	reg := personSurface(t, trk, person, boundOperator, boundParties)

	if got := callNoTurn(t, reg, tracker.MyWorkTool, nil); got.Failed {
		t.Fatalf("my_work failed: %q", got.Output)
	}
	if got := trk.myWorkQuery.Who.Handles(); !slices.Equal(got,
		[]string{"jane-founder", "founder"}) {

		t.Errorf("my_work asked about %v, want the seat first and the "+
			"credential behind it", got)
	}
	// AND A SEAT IS STILL A PARTY OF ONE, which is every in-engine caller.
	seat := newFakeTracker()
	own := personSurface(t, seat, &personSpy{}, func(
		context.Context, *turnctx.Turn) (builtin.Actor, error) {

		return builtin.Actor{Handle: "ana", Kind: tracker.AuthorAgent}, nil
	}, boundParties)
	if got := callNoTurn(t, own, tracker.MyWorkTool, nil); got.Failed {
		t.Fatalf("my_work for a seat failed: %q", got.Output)
	}
	if got := seat.myWorkQuery.Who.Handles(); !slices.Equal(got, []string{"ana"}) {
		t.Errorf("a seat's my_work asked about %v, want its one handle", got)
	}
}

// AND A PERSONAL READ ABOUT SOMEBODY ELSE CARRIES *THEIR* TWO NAMES.
//
// Whose two names these are is a fact about the SEAT, resolved from the chart,
// never the credential in the caller's own hand: an operator reading a
// report's inbox is answered about that report. Both verbs take the handle as
// an argument, so neither could get it from the actor.
func TestTheHandleArgumentIsResolvedToThatPersonsParty(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := personSurface(t, trk, &personSpy{}, unboundOperator, boundParties)

	if got := callPlain(t, reg, tracker.GetPersonTool, map[string]any{
		"handle": "jane-founder",
	}); got.Failed {
		t.Fatalf("get_person failed: %q", got.Output)
	}
	if got := trk.personQuery.Who.Handles(); !slices.Equal(got,
		[]string{"jane-founder", "founder"}) {

		t.Errorf("get_person asked about %v, want the seat and the credential "+
			"bound to it — the record may be filed under either", got)
	}
	if got := callPlain(t, reg, tracker.WorkInboxTool, map[string]any{
		"handle": "jane-founder",
	}); got.Failed {
		t.Fatalf("work_inbox failed: %q", got.Output)
	}
	if got := trk.inboxQuery.Who.Handles(); !slices.Equal(got,
		[]string{"jane-founder", "founder"}) {

		t.Errorf("work_inbox asked about %v — the applier writes one row per "+
			"recipient the record named, so a change that reached this person "+
			"under their credential is a row the seat alone never sees", got)
	}
	// AND A SURFACE WITH NO CHART ANSWERS THE HANDLE ALONE, which is the
	// honest state for a build that has not loaded one and the whole truth
	// for every agent seat.
	bare := newFakeTracker()
	none := personSurface(t, bare, &personSpy{}, unboundOperator, nil)
	if got := callPlain(t, none, tracker.GetPersonTool, map[string]any{
		"handle": "jane-founder",
	}); got.Failed {
		t.Fatalf("get_person with no chart failed: %q", got.Output)
	}
	if got := bare.personQuery.Who.Handles(); !slices.Equal(got,
		[]string{"jane-founder"}) {

		t.Errorf("with no chart get_person asked about %v", got)
	}
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
		tracker.TaskActivityTool:     false,
		tracker.MyWorkTool:           false,
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
		tracker.WriteWorkCatalogueTool: true,
		tracker.SaveWorkViewTool:       true,
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

// THE PERSON TOOLS TAKE GESTURES, and their schemas say so.
//
// `mark_inbox` used to take all three lists and the watermark and REPLACE each
// — its own description told a caller to read first because an omitted list
// was CLEARED — so one "mark this read" from a screen erased every other mark.
// The schema is what a caller builds its call from, so it is asserted here:
// the gesture's six names and nothing of the old shape.
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

	inbox := props(tracker.MarkInboxTool)
	var names []string
	for name := range inbox {
		names = append(names, name)
	}
	slices.Sort(names)
	if want := []string{"primary_reasons", "read", "read_through", "snooze",
		"unread", "unsnooze"}; !slices.Equal(names, want) {
		t.Errorf("mark_inbox takes %v, want %v", names, want)
	}
	entry, _ := snapshot.Lookup(tracker.MarkInboxTool)
	if strings.Contains(entry.Tool.Description(), "REPLACES") {
		t.Error("mark_inbox still describes itself as replacing the lists")
	}
	for _, key := range []string{"views", "favorites"} {
		change, ok := props(tracker.SetPinsTool)[key].(map[string]any)
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
	if _, held := props(tracker.SetPrioritiesTool)["if_match"]; !held {
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

	got := callWork(t, reg, tracker.MarkInboxTool, map[string]any{
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
	if got := callWork(t, reg, tracker.MarkInboxTool, map[string]any{
		"primary_reasons": []any{},
	}); got.Failed || person.inbox.PrimaryReasons == nil {
		t.Errorf("an empty primary_reasons did not reach the writer as a "+
			"choice: %q", got.Output)
	}
	if got := callWork(t, reg, tracker.MarkInboxTool, map[string]any{}); !got.Failed {
		t.Error("a mark_inbox naming nothing was accepted")
	}

	// A BARE LIST WHERE A CHANGE BELONGS is refused naming the shape,
	// rather than read as the whole strip — which is the old replace.
	bare := callWork(t, reg, tracker.SetPinsTool, map[string]any{"views": []any{"v-1"}})
	if !bare.Failed || !strings.Contains(bare.Output, "add") {
		t.Errorf("a bare list of views answered %q", bare.Output)
	}
	if got := callWork(t, reg, tracker.SetPinsTool, map[string]any{
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
		if got := callWork(t, reg, tracker.SetPrioritiesTool, c.args); got.Failed {
			t.Fatalf("set_priorities failed: %q", got.Output)
		}
		if (person.ifMatch == nil) != (c.want == nil) ||
			(c.want != nil && *person.ifMatch != *c.want) {
			t.Errorf("if_match %v reached the writer as %v", c.args["if_match"], person.ifMatch)
		}
	}
	if got := callWork(t, reg, tracker.SetPrioritiesTool, map[string]any{
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
	reg := personSurface(t, trk, &personSpy{}, unboundOperator, nil)

	if got := callPlain(t, reg, tracker.WorkInboxTool, map[string]any{
		"handle": "alice",
	}); got.Failed {
		t.Fatalf("work_inbox failed: %q", got.Output)
	}
	if trk.inboxQuery.Snoozed != tracker.SnoozeExclude {
		t.Errorf("no `snoozed` reached the reader as %q, want %q",
			trk.inboxQuery.Snoozed, tracker.SnoozeExclude)
	}
	if got := callPlain(t, reg, tracker.WorkInboxTool, map[string]any{
		"handle": "alice", "snoozed": "only",
	}); got.Failed {
		t.Fatalf("work_inbox failed: %q", got.Output)
	}
	if trk.inboxQuery.Snoozed != tracker.SnoozeOnly {
		t.Errorf("snoozed=only reached the reader as %q", trk.inboxQuery.Snoozed)
	}
	bad := callPlain(t, reg, tracker.WorkInboxTool, map[string]any{
		"handle": "alice", "snoozed": "bogus",
	})
	if !bad.Failed || !strings.Contains(bad.Output, "include") {
		t.Fatalf("snoozed=bogus answered %q, want a refusal naming the scopes",
			bad.Output)
	}
}
