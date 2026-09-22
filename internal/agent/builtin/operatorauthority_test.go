package builtin_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The operator surface's two identity questions on the WORK tools, which are
// different questions with different answers.
//
// WHO MAY ACT is [builtin.Actor.Record] and [tracker.AuthorKind.Person]: a
// lead relation is between two people in the org chart, and a token is in no
// chart at all — so a lookup asked about the credential matches nobody however
// the company is arranged, and a person acting through their own credential
// carries an authority of their own beside it.
//
// WHOSE RECORD a gesture is about is [builtin.Actor.Record] again — a watch is
// an ADDRESS, and the party registry resolves seats.
//
// WHO WROTE IT is neither, and none of these cases moves it: the author stays
// the token with kind `operator`, because a tracker whose author field is
// chosen by the writer is not an audit trail.

// leadSpy is the project-lead seam, recording WHO it was asked about.
//
// The ARGUMENT is half of what these cases are about: an answer of false is
// indistinguishable between "this person does not lead it" and "the lookup was
// handed a name the chart has never seen".
type leadSpy struct {
	handle, project string
	asked           []string
}

func (s *leadSpy) leads(_ context.Context, actor, project string) bool {
	s.asked = append(s.asked, actor)
	return actor == s.handle && project == s.project
}

// operatorSurface is `/operator/mcp`'s work tools over one fake tracker, one
// actor and one lead lookup — so a case varies the CALLER, which is the only
// thing the authority and the subject of these writes turn on.
func operatorSurface(t *testing.T, trk *fakeTracker,
	actor func(context.Context, *turnctx.Turn) (builtin.Actor, error),
	leads builtin.LeadsProject) *tools.Registry {

	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Units: stubUnits{},
			ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return trk },
			// THE PERSON'S TEAM OWNS ENG AND THE CREDENTIAL OWNS
			// NOTHING, because a home project is resolved from the
			// chart by handle exactly as the lead relation is.
			DefaultProject: func(handle string) string {
				if handle == "jane-founder" {
					return "ENG"
				}
				return ""
			},
			Actor: actor,
		},
		LeadsProject: leads,
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// humanAtTheDashboard is a person writing as themselves — a human seat, with
// no credential behind it.
func humanAtTheDashboard(context.Context, *turnctx.Turn) (builtin.Actor, error) {
	return builtin.Actor{Handle: "jane-founder", Kind: tracker.AuthorHuman}, nil
}

// A BOUND OPERATOR RE-ROUTES THE WORK THEIR SEAT LEADS.
//
// `routing_unit` is gated on leading the project the item is filed in, and the
// gate asked the chart about [builtin.Actor.Handle] — which on `/operator/mcp`
// is the TOKEN's own name. No chart has ever contained one, so the lookup
// answered false for every operator, a bound founder leading the project
// included, and every re-route from a person's own assistant was refused with
// "that is the lead of ENG's decision, not yours" — addressed to the lead.
//
// THE ARGUMENT IS ASSERTED, not only the admission: the person arm below
// admits an operator anyway, so a case reading the outcome alone would pass
// with the lookup still asking about a credential — and `write_project`, whose
// authority the tracker splits into `Lead` and `Operator`, records which of
// the two let a write through.
func TestABoundOperatorRoutesTheWorkTheirSeatLeads(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	spy := &leadSpy{handle: "jane-founder", project: "ENG"}
	reg := operatorSurface(t, trk, boundOperator, spy.leads)

	got := callNoTurn(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "routing_unit": "PLATFORM",
	})
	if got.Failed {
		t.Fatalf("the re-route was refused: %s", got.Output)
	}
	if !slices.Equal(spy.asked, []string{"jane-founder"}) {
		t.Errorf("the chart was asked about %v, want the person the token is "+
			"bound to — asked about the credential the lookup matches nobody, "+
			"and a lead's own decision is recorded as a person's override",
			spy.asked)
	}
	if len(trk.patched) != 1 || trk.patched[0].RoutingUnit == nil {
		t.Fatalf("the re-route wrote %+v, want one routing unit", trk.patched)
	}
	if unit := *trk.patched[0].RoutingUnit; unit != "plat" {
		t.Errorf("the item routes to %q, want the chart's own key", unit)
	}
	if len(trk.kinds) != 1 || trk.kinds[0] != tracker.ChangeRouted {
		t.Errorf("the re-route committed as %v, want %q — the new team's lead "+
			"hears it on that kind alone", trk.kinds, tracker.ChangeRouted)
	}
	// AND THE AUDIT TRAIL IS UNTOUCHED.
	if len(trk.actors) != 1 || trk.actors[0].Handle != "founder" ||
		trk.actors[0].Kind != tracker.AuthorOperator {

		t.Errorf("the re-route is authored by %+v — the author is the "+
			"credential and the kind says it is not a seat", trk.actors)
	}
}

// AND A PERSON'S OWN CREDENTIAL CARRIES THE AUTHORITY WITHOUT ONE.
//
// The second arm, and it is the same pair `write_project` and `set_priorities`
// resolve: a human at the dashboard and an operator token are both a PERSON
// acting through their own credential, which [tracker.AuthorKind.Person] is
// the predicate for. Without it a token nobody bound — a founder who never
// wrote `contact.crewlet_operator_id`, an operator outside the chart — could
// not re-route anything at all, because no ancestor walk can ever reach a
// credential.
func TestAPersonsOwnCredentialMayRerouteWithNoLeadRelation(t *testing.T) {
	t.Parallel()
	for name, actor := range map[string]func(
		context.Context, *turnctx.Turn) (builtin.Actor, error){

		"an unbound token":     unboundOperator,
		"a human at the board": humanAtTheDashboard,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			// THE LOOKUP ANSWERS FALSE FOR EVERYBODY, so what admits
			// the write is the person arm and nothing else.
			spy := &leadSpy{handle: "nobody", project: "NONE"}
			reg := operatorSurface(t, trk, actor, spy.leads)

			if got := callNoTurn(t, reg, builtin.UpdateWorkItemTool, map[string]any{
				"item": "ENG-1", "routing_unit": "PLATFORM",
			}); got.Failed {
				t.Fatalf("%s was refused the re-route: %s", name, got.Output)
			}
			if len(trk.patched) != 1 || trk.patched[0].RoutingUnit == nil {
				t.Fatalf("%s wrote %+v, want one routing unit", name, trk.patched)
			}
		})
	}
}

// AND A SEAT THAT DOES NOT LEAD THE PROJECT IS STILL REFUSED, NAMING IT.
//
// The half a careless fix takes away: pointing somebody else's work at another
// team is a decision about who owns it, and an agent making it for the team is
// the hand-off this gate exists to stop. The refusal has to name the project
// whose lead decides, or there is nobody to ask.
//
// THE LOOKUP IS WIRED AND ANSWERS FALSE, which is the case
// [TestARerouteIsRefusedToASeatThatDoesNotLead] does not cover — that one
// wires none at all, so it cannot say what a wired one was ASKED. A seat is
// its own record, and a gate that started asking about something else here
// would refuse every agent in the company with the message below.
func TestASeatThatDoesNotLeadTheProjectIsRefusedTheReroute(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	spy := &leadSpy{handle: "jane-founder", project: "ENG"}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Units: stubUnits{},
		},
		LeadsProject: spy.leads,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "routing_unit": "PLATFORM",
	})
	if !got.Failed {
		t.Fatal("a seat that leads nothing re-routed the item")
	}
	if !strings.Contains(got.Output, "ENG") {
		t.Errorf("the refusal is %q and does not name the project whose lead "+
			"decides — there is then nobody to ask", got.Output)
	}
	if len(trk.patched) != 0 {
		t.Errorf("the refused re-route still wrote %+v", trk.patched)
	}
	if !slices.Equal(spy.asked, []string{"eng"}) {
		t.Errorf("the chart was asked about %v, want the seat's own handle — "+
			"a seat IS its own record and nothing else", spy.asked)
	}
}

// A BOUND OPERATOR'S WATCH IS THEIR SEAT'S.
//
// `watch: true` is a gesture about ONE person, and it carried
// [builtin.Actor.Handle] — the token's own name on this surface. So a founder
// following an item through their assistant put a CREDENTIAL in the watcher
// set: it rendered as a colleague on the item and in the `watchers` history
// delta, their own seat was not following the item, and every later wake
// dropped the entry against the roster, which knows seats.
//
// THE DELTA IS ASSERTED BESIDE THE PATCH, because that is where it is read: a
// watcher change announces itself, and the wake's own snapshot is what names
// who is on the set now.
func TestABoundOperatorsWatchIsWrittenUnderTheirSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := operatorSurface(t, trk, boundOperator, leadAlways)

	if got := callNoTurn(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "watch": true,
	}); got.Failed {
		t.Fatalf("the watch failed: %s", got.Output)
	}
	if len(trk.patched) != 1 || trk.patched[0].Watch == nil {
		t.Fatalf("the watch wrote %+v, want one gesture", trk.patched)
	}
	if got := trk.patched[0].Watch.Handle; got != "jane-founder" {
		t.Errorf("the watch is recorded for %q, want the person the token is "+
			"bound to — a credential in the watcher set renders as a colleague "+
			"and is dropped at every wake", got)
	}
	if len(trk.notified) != 1 || trk.notified[0] == nil {
		t.Fatalf("the watch announced %+v", trk.notified)
	}
	if watchers := trk.notified[0].Snapshot.Watchers; !slices.Contains(
		watchers, "jane-founder") || slices.Contains(watchers, "founder") {

		t.Errorf("the `watchers` delta names %v — the history row shows the "+
			"credential where a colleague's handle goes", watchers)
	}
	// AND THE AUDIT TRAIL IS UNTOUCHED: who wrote it is the other
	// question, and its answer is still the token.
	if len(trk.actors) != 1 || trk.actors[0].Handle != "founder" {
		t.Errorf("the watch is authored by %+v, want the credential", trk.actors)
	}
}

// AND AN UNBOUND TOKEN WATCHES AS ITSELF, exactly as before: an operator
// outside the org chart is an ordinary state, and it holds its own record.
func TestAnUnboundOperatorWatchesUnderItsOwnID(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := operatorSurface(t, trk, unboundOperator, leadAlways)

	if got := callNoTurn(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "watch": true,
	}); got.Failed {
		t.Fatalf("the watch failed: %s", got.Output)
	}
	if len(trk.patched) != 1 || trk.patched[0].Watch == nil ||
		trk.patched[0].Watch.Handle != "ci" {

		t.Errorf("an unbound token's watch wrote %+v, want its own id",
			trk.patched)
	}
}

// AND SO IS THE WATCH A COMMENT LEAVES, AND THE ASK IT ANSWERS.
//
// Three gestures in one call, and all three are about the PERSON rather than
// the credential: the commenter is subscribed automatically, the `answers`
// inference asks who the open ask was addressed to — and an ask names a seat —
// and the warning about waking an assignee who is not being asked anything
// compares the caller against that assignee.
//
// Under the token's own name a bound founder answering the question put to
// their seat stamped nothing (the ask stayed open on the board), stopped
// following the thread they had just joined, and was warned that they were
// waking themselves.
func TestABoundOperatorsCommentActsAsTheirSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	// THE ITEM IS THE PERSON'S OWN, which is what the warning compares
	// against.
	item := trk.tasks["ENG-1"]
	item.Task.Assignee = "jane-founder"
	trk.tasks["ENG-1"] = item
	reg := operatorSurface(t, trk, boundOperator, leadAlways)

	got := callNoTurn(t, reg, builtin.CommentOnWorkTool, map[string]any{
		"item": "ENG-1", "body": "shipping this today",
	})
	if got.Failed {
		t.Fatalf("the comment failed: %s", got.Output)
	}
	if trk.threadQuery.Author != "jane-founder" {
		t.Errorf("the `answers` inference was asked about %q, want the person "+
			"the token is bound to — an ask is addressed to a seat, so under "+
			"the credential's name it matches nothing and the ask stays open",
			trk.threadQuery.Author)
	}
	if len(trk.patched) != 1 || trk.patched[0].Watch == nil ||
		trk.patched[0].Watch.Handle != "jane-founder" {

		t.Errorf("the comment subscribed %+v, want the person the token is "+
			"bound to", trk.patched)
	}
	if strings.Contains(got.Output, "is woken by this") {
		t.Errorf("the comment answered %q — the assignee IS the caller, and a "+
			"warning that they are waking somebody else names them to "+
			"themselves", got.Output)
	}
}

// A BOUND OPERATOR FILES WORK INTO THEIR OWN TEAM, AND FOLLOWS IT.
//
// Two defaults resolved from the chart by handle, and the token's own name is
// in no chart: the home project, which made `create_work_item` with no
// `project` refuse a founder with "your team owns none" while their team owned
// one — and the reporter's own watch, which put a credential in the set.
//
// `reporter` is NOT one of them and is asserted here for that reason: it is
// attribution, and it stays the token.
func TestABoundOperatorsCreateUsesTheirSeatsTeam(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := operatorSurface(t, trk, boundOperator, leadAlways)

	got := callNoTurn(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "decide the pricing page", "assignee": "cto",
	})
	if got.Failed {
		t.Fatalf("the create failed: %s", got.Output)
	}
	if len(trk.created) != 1 {
		t.Fatalf("the create wrote %+v", trk.created)
	}
	task := trk.created[0]
	if task.Project != "ENG" {
		t.Errorf("the item was filed into %q, want the project the person's "+
			"own team owns — resolved against the credential the chart "+
			"answers nothing and the call is refused for having no default",
			task.Project)
	}
	if !slices.Contains(task.Watchers, "jane-founder") ||
		slices.Contains(task.Watchers, "founder") {

		t.Errorf("the item is watched by %v — a credential in the set renders "+
			"as a colleague and is dropped at every later wake, so the person "+
			"who filed the work never hears about it again", task.Watchers)
	}
	if task.Reporter != "founder" {
		t.Errorf("the item is reported by %q — attribution is the other "+
			"question and its answer is the credential", task.Reporter)
	}
}

// AND A PERSONAL READ ASKS ABOUT BOTH OF THAT PERSON'S NAMES.
//
// `list_work_items` expands `preset=my_queue`, `assignee=me` and the rest
// against a VIEWER, and it built one out of the bare author — so a founder's
// assistant asking what they hold was answered about the TOKEN: a party no
// colleague has ever assigned anything to, in a home container the chart does
// not give a credential. `my_work` already asked as the party; this is the
// same question through the other tool.
func TestABoundOperatorsListIsExpandedForThePerson(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := operatorSurface(t, trk, boundOperator, leadAlways)

	if got := callNoTurn(t, reg, builtin.ListWorkItemsTool,
		map[string]any{"project": "ENG"}); got.Failed {

		t.Fatalf("the list failed: %s", got.Output)
	}
	if got := trk.viewer.Handle; got != "jane-founder" {
		t.Errorf("the list was expanded for %q, want the person the token "+
			"names", got)
	}
	if got := trk.viewer.OperatorID; got != "founder" {
		t.Errorf("the viewer carries alias %q, want the credential — the rows "+
			"this person left before the binding are filed under it", got)
	}
	if got := trk.viewer.Project; got != "ENG" {
		t.Errorf("the viewer's home container is %q, want the person's own "+
			"team's project", got)
	}
}

// A BOUND OPERATOR'S PROJECT EDIT IS ASKED ABOUT THE PERSON TOO.
//
// The other lead gate, and the one whose answer is RECORDED: the tracker
// splits this authority into `Lead` and `Operator`, so a lookup asked about
// the credential matched nobody and every founder's edit fell through to the
// person arm — a lead's own decision about their team's fields written down as
// an operator's override.
func TestABoundOperatorsProjectEditIsAskedAboutTheirSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	spy := &leadSpy{handle: "jane-founder", project: "ENG"}
	reg := operatorSurface(t, trk, boundOperator, spy.leads)

	if got := callNoTurn(t, reg, tracker.WriteProjectTool, map[string]any{
		"project": "ENG", "default_assignee": "cto",
	}); got.Failed {
		t.Fatalf("the project edit failed: %s", got.Output)
	}
	if !slices.Equal(spy.asked, []string{"jane-founder"}) {
		t.Errorf("the chart was asked about %v, want the person the token is "+
			"bound to", spy.asked)
	}
	if len(trk.projectAuthority) != 1 || !trk.projectAuthority[0].Lead {
		t.Errorf("the edit was written under %+v, want one carrying the lead "+
			"relation the person actually holds", trk.projectAuthority)
	}
}
