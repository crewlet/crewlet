package builtin_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A CREATE STORES THE CHART'S OWN KEY, not the string the model typed.
//
// A model names a team the way it remembers it — the name, the id, in
// whatever case — and `filed_unit` is written once and never rewritten. So a
// tool that stored the argument verbatim left one team's work under as many
// spellings as its colleagues have ways of writing it, each of them a filter
// the others miss, and the id a founder added to survive a rename went
// unwritten by the surface that files most of the company's work.
func TestACreateStoresTheUnitsOwnKey(t *testing.T) {
	t.Parallel()
	for _, typed := range []string{"platform", "Platform", "PLAT", "plat"} {
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Units: stubUnits{},
		})
		got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
			"title": "ship the planner", "project": "ENG", "unit": typed,
		})
		if got.Failed {
			t.Fatalf("create with unit=%q failed: %s", typed, got.Output)
		}
		if len(trk.created) != 1 {
			t.Fatalf("created %d tasks, want 1", len(trk.created))
		}
		task := trk.created[0]
		if task.FiledUnit != "plat" || task.RoutingUnit != "plat" {
			t.Errorf("unit=%q filed %q and routed %q, want the chart's own "+
				"key on both", typed, task.FiledUnit, task.RoutingUnit)
		}
	}
}

// AND A TEAM THE CHART DOES NOT HAVE IS REFUSED BY NAME, rather than stored
// as a unit nothing can route to or filter on.
func TestACreateRefusesAUnitTheChartDoesNotHave(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Units: stubUnits{},
	})
	got := callWork(t, reg, builtin.CreateWorkItemTool, map[string]any{
		"title": "ship the planner", "project": "ENG", "unit": "Legal",
	})
	if !got.Failed {
		t.Fatal("a unit nobody has was accepted")
	}
	if !strings.Contains(got.Output, "Legal") {
		t.Errorf("the refusal is %q and does not name the team", got.Output)
	}
	if len(trk.created) != 0 {
		t.Errorf("the refused create still wrote %+v", trk.created)
	}
}

// A RE-ROUTE STORES THE KEY FOR THE SAME REASON: the wake resolves the stored
// value back to a lead, and a spelling the chart did not choose is one a
// rename walks away from.
func TestARerouteStoresTheUnitsOwnKey(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Units: stubUnits{},
		},
		LeadsProject: leadAlways,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	got := callWork(t, reg, builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "routing_unit": "PLATFORM",
	})
	if got.Failed {
		t.Fatalf("the re-route failed: %s", got.Output)
	}
	if len(trk.patched) != 1 || trk.patched[0].RoutingUnit == nil {
		t.Fatalf("the re-route wrote %+v, want one routing unit", trk.patched)
	}
	if unit := *trk.patched[0].RoutingUnit; unit != "plat" {
		t.Errorf("the item routes to %q, want the chart's own key", unit)
	}
}

// GET_WORK_ITEM READS WITH THE CHART, so its answer carries the team's NAME
// beside the key the row holds.
//
// A model acts on the key — it is what a `unit=` filter takes, and that filter
// takes the name too — but a model also writes PROSE about the item it just
// read, into a comment, a chat message or a hand-off, and "filed into eng" is
// a sentence about a slug nobody outside the config file has seen. It is also
// the only way this surface can say a team has LEFT the chart, which is the
// difference between a stale unit and a typo.
func TestGetWorkItemReadsWithTheChart(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Units: stubUnits{},
	})
	if got := callWork(t, reg, builtin.GetWorkItemTool,
		map[string]any{"item": "ENG-1"}); got.Failed {
		t.Fatalf("get_work_item failed: %s", got.Output)
	}
	if trk.wants.Units == nil {
		t.Error("get_work_item read the item with no chart, so a model is " +
			"handed a unit key with no name and every team reads as one the " +
			"chart has lost")
	}
}

// viewRegistry is the operator surface the two view tools are registered on,
// with the chart behind it.
func viewRegistry(t *testing.T, trk *fakeTracker, views builtin.ViewWriter) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Units: stubUnits{},
			ViewWriter: func(builtin.Actor) builtin.ViewWriter { return views },
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

// viewSpy captures the view a save wrote.
type viewSpy struct{ saved []tracker.View }

func (s *viewSpy) WriteView(_ context.Context, _ string, view tracker.View) (
	tracker.WriteResult, error) {

	s.saved = append(s.saved, view)
	return tracker.WriteResult{Outcome: statelog.OutcomeApplied}, nil
}

// A VIEW'S UNIT CONTAINER IS STORED UNDER THE TEAM'S KEY.
//
// A container is an ADDRESS, and this tool already upper-cases a project key
// for exactly that reason. A unit is the kind with two spellings, so a view
// saved as `unit:Engineering` sat in a different strip from one saved as
// `unit:eng` — and nothing on either strip says the other exists.
func TestASavedViewsUnitContainerIsStoredUnderTheKey(t *testing.T) {
	t.Parallel()
	for _, typed := range []string{"unit:Platform", "unit:plat", "unit:PLATFORM"} {
		trk := newFakeTracker()
		views := &viewSpy{}
		reg := viewRegistry(t, trk, views)
		got := callPlain(t, reg, tracker.SaveWorkViewTool, map[string]any{
			"container": typed, "name": "Ours", "type": string(tracker.ViewList),
		})
		if got.Failed {
			t.Fatalf("saving a view in %s failed: %s", typed, got.Output)
		}
		if len(views.saved) != 1 {
			t.Fatalf("saved %d views, want 1", len(views.saved))
		}
		container := views.saved[0].Container
		if container.Kind != tracker.ContainerUnit || container.ID != "plat" {
			t.Errorf("%s was stored as %+v, want the unit container keyed "+
				"plat", typed, container)
		}
	}
}

// AND THE STRIP IS READ WITH THE CHART BEHIND IT, which is what lets a strip
// asked for under one spelling carry the views saved under the other.
func TestAViewStripIsReadWithTheChart(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := viewRegistry(t, trk, &viewSpy{})
	if got := callPlain(t, reg, tracker.ListWorkViewsTool,
		map[string]any{"container": "unit:Platform"}); got.Failed {
		t.Fatalf("listing a unit's strip failed: %s", got.Output)
	}
	if trk.viewQuery.Units == nil {
		t.Error("list_work_views passed no chart, so a strip asked for by a " +
			"team's id carries none of the views saved under its name")
	}
}
