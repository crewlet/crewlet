package builtin_test

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A PERSON THE IDENTITY DIRECTORY BINDS TO A SEAT ACTS AS THAT SEAT on every
// work tool the operator surface serves — as its AUTHOR, as the ADDRESS a
// gesture about them lands on, and as the party a lead relation is asked
// about.
//
// Each of these was once asked about the credential in the person's hand, and
// each answered about a name no chart has ever contained: a re-route refused a
// lead leading the project, a watch put a token in the watcher set where every
// later wake dropped it, a comment left the ask it answered open, a create
// found no home project, and `preset=my_queue` read the token's queue. Here
// the conversion is [builtin.PrincipalActor] over [iam.ActorFor], so the seat
// is the author and there is no second name to reconcile — which is exactly
// what these cases hold, through the served tools rather than the conversion
// alone, so a tool that reached for the login instead goes red.

// seatChart is a chart in which ONE seat leads ONE project, recording every
// actor the project relation was asked about — because a refusal alone is
// indistinguishable between "this person does not lead it" and "the chart was
// asked about a name it has never seen".
type seatChart struct {
	seat, project string
	asked         []string
}

func (c *seatChart) Leads(context.Context, string, string) (bool, error) {
	return false, nil
}

func (c *seatChart) LeadsProject(_ context.Context, actor, project string) (bool, error) {
	c.asked = append(c.asked, actor)
	return actor == c.seat && project == c.project, nil
}

func (c *seatChart) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, nil
}

func (c *seatChart) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
}

func (c *seatChart) LeadsAnyone(context.Context, string) (bool, error) {
	return false, nil
}

// janesChart is the company these cases run in: the seat `jane-founder` leads
// ENG, and nobody else leads anything.
func janesChart() *seatChart { return &seatChart{seat: "jane-founder", project: "ENG"} }

// boundSurface is the operator's catalogue as the engine wires it: the
// request's principal is the actor, and the chart decides.
func boundSurface(t *testing.T, trk *fakeTracker, chart *seatChart) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Units: stubUnits{},
			ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return trk },
			// THE SEAT'S TEAM OWNS ENG AND THE LOGIN OWNS NOTHING, because
			// a home project is resolved from the chart by handle exactly
			// as the lead relation is.
			DefaultProject: func(handle string) string {
				if handle == "jane-founder" {
					return "ENG"
				}
				return ""
			},
			Actor: builtin.PrincipalActor,
		},
		Authorize: builtin.Decide(chart),
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// asJane calls a tool as a person signed in as `jane.founder`, whom the
// directory binds to the seat `jane-founder`, holding the ordinary write grant
// and NOT the admin one — so every relation below is the chart's to answer.
func asJane(t *testing.T, reg *tools.Registry, name string,
	args map[string]any) tools.Result {

	t.Helper()
	jane := iam.Principal{
		ID: uuid.New(), Login: "jane.founder", Kind: iam.KindPerson,
		Seat: "jane-founder", Stage: iam.StageActive,
		Colleague: iam.ColleagueWrite,
		Grants:    []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite},
	}
	entry, ok := reg.Lookup(name)
	if !ok {
		t.Fatalf("%s is not registered", name)
	}
	callable, ok := entry.Tool.(tools.SeatCallable)
	if !ok {
		t.Fatalf("%s is not seat-callable", name)
	}
	got, err := callable.CallForTurn(iam.WithPrincipal(t.Context(), jane), nil, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return got
}

// A BOUND PERSON RE-ROUTES THE WORK THEIR SEAT LEADS, and the chart is asked
// about the seat.
func TestABoundPersonRoutesTheWorkTheirSeatLeads(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	chart := janesChart()
	got := asJane(t, boundSurface(t, trk, chart), builtin.UpdateWorkItemTool,
		map[string]any{"item": "ENG-1", "routing_unit": "PLATFORM"})
	if got.Failed {
		t.Fatalf("the lead's re-route was refused: %s", got.Output)
	}
	if !slices.Equal(chart.asked, []string{"jane-founder"}) {
		t.Errorf("the chart was asked about %v, want the seat the person is "+
			"bound to", chart.asked)
	}
	if len(trk.actors) != 1 || trk.actors[0].Handle != "jane-founder" ||
		trk.actors[0].Kind != tracker.AuthorHuman ||
		trk.actors[0].OperatorID != "jane.founder" {

		t.Errorf("the re-route is authored by %+v, want the seat as a human "+
			"with the login they acted under beside it", trk.actors)
	}
}

// A BOUND PERSON'S WATCH IS THEIR SEAT'S, and so is the watch a comment leaves
// and the ask it answers.
func TestABoundPersonsGesturesLandOnTheirSeat(t *testing.T) {
	t.Parallel()

	t.Run("watch", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		if got := asJane(t, boundSurface(t, trk, janesChart()),
			builtin.UpdateWorkItemTool,
			map[string]any{"item": "ENG-1", "watch": true}); got.Failed {

			t.Fatalf("the watch failed: %s", got.Output)
		}
		if len(trk.patched) != 1 || trk.patched[0].Watch == nil ||
			trk.patched[0].Watch.Handle != "jane-founder" {

			t.Errorf("the watch wrote %+v, want the seat — a login in the "+
				"watcher set is dropped at every later wake", trk.patched)
		}
	})

	t.Run("comment", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		// THE ITEM IS THE PERSON'S OWN, which is what the warning about
		// waking an assignee compares against.
		item := trk.tasks["ENG-1"]
		item.Task.Assignee = "jane-founder"
		trk.tasks["ENG-1"] = item
		got := asJane(t, boundSurface(t, trk, janesChart()),
			builtin.CommentOnWorkTool,
			map[string]any{"item": "ENG-1", "body": "shipping this today"})
		if got.Failed {
			t.Fatalf("the comment failed: %s", got.Output)
		}
		if trk.threadQuery.Author != "jane-founder" {
			t.Errorf("the `answers` inference was asked about %q, want the "+
				"seat — an ask is addressed to one", trk.threadQuery.Author)
		}
		if len(trk.patched) != 1 || trk.patched[0].Watch == nil ||
			trk.patched[0].Watch.Handle != "jane-founder" {

			t.Errorf("the comment subscribed %+v, want the seat", trk.patched)
		}
		if strings.Contains(got.Output, "is woken by this") {
			t.Errorf("the comment warned the assignee about themselves: %s",
				got.Output)
		}
	})
}

// A BOUND PERSON FILES WORK INTO THEIR SEAT'S TEAM, REPORTS IT AS THEIR SEAT,
// AND FOLLOWS IT.
func TestABoundPersonsCreateIsTheirSeats(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	got := asJane(t, boundSurface(t, trk, janesChart()), builtin.CreateWorkItemTool,
		map[string]any{"title": "decide the pricing page", "assignee": "cto"})
	if got.Failed {
		t.Fatalf("the create failed: %s", got.Output)
	}
	if len(trk.created) != 1 {
		t.Fatalf("the create wrote %+v", trk.created)
	}
	task := trk.created[0]
	if task.Project != "ENG" {
		t.Errorf("the item was filed into %q, want the seat's own team's project",
			task.Project)
	}
	if task.Reporter != "jane-founder" {
		t.Errorf("the item is reported by %q, want the seat", task.Reporter)
	}
	if !slices.Contains(task.Watchers, "jane-founder") ||
		slices.Contains(task.Watchers, "jane.founder") {

		t.Errorf("the item is watched by %v, want the seat and never the login",
			task.Watchers)
	}
}

// AND A PERSONAL READ IS EXPANDED FOR THE SEAT: `preset=my_queue` is what the
// seat holds, in the seat's own container.
func TestABoundPersonsListIsExpandedForTheirSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	if got := asJane(t, boundSurface(t, trk, janesChart()),
		builtin.ListWorkItemsTool, map[string]any{"project": "ENG"}); got.Failed {

		t.Fatalf("the list failed: %s", got.Output)
	}
	if trk.viewer.Handle != "jane-founder" || trk.viewer.Project != "ENG" {
		t.Errorf("the list was expanded for %+v, want the seat and its team's "+
			"project", trk.viewer)
	}
}

// A BOUND PERSON'S PROJECT EDIT IS DECIDED ON THEIR SEAT, and the authority
// the tracker records is the lead's.
func TestABoundPersonsProjectEditIsDecidedOnTheirSeat(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	chart := janesChart()
	if got := asJane(t, boundSurface(t, trk, chart), tracker.WriteProjectTool,
		map[string]any{"project": "ENG", "default_assignee": "cto"}); got.Failed {

		t.Fatalf("the project edit failed: %s", got.Output)
	}
	if !slices.Contains(chart.asked, "jane-founder") ||
		slices.Contains(chart.asked, "jane.founder") {

		t.Errorf("the chart was asked about %v, want the seat", chart.asked)
	}
	if len(trk.projectAuthority) != 1 || !trk.projectAuthority[0].Policy {
		t.Errorf("the edit was written under %+v, want the lead's policy "+
			"authority", trk.projectAuthority)
	}
}
