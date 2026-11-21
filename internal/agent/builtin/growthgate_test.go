package builtin_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/tools"
)

// A GROWTH TOOL IS FILED FROM ITS CATALOG: under Marketing and growth, gated
// by the least level its class needs, and listed in the catalogue in its
// catalog's order, with no row written for it here.
//
// Mutation: gate every growth tool growth_read, and the admin tool is offered
// to a read seat.
func TestAGrowthToolIsFiledByItsClasssLevel(t *testing.T) {
	t.Parallel()
	for tool, gate := range map[string]builtin.Gate{
		growthReadTool: builtin.GateGrowthRead, growthOperateTool: builtin.GateGrowthOperate,
		growthSpendTool: builtin.GateGrowthOperate, growthAdminTool: builtin.GateGrowthAdminister,
	} {
		f, ok := builtin.FilingOf(tool)
		if !ok || f.Group != builtin.GroupGrowth || f.Gate != gate {
			t.Errorf("%s filed %+v (%v), want group growth and gate %s", tool, f, ok, gate)
		}
	}
	if !slices.Contains(builtin.Catalogued(), growthAdminTool) {
		t.Error("the catalogue does not list the growth tools")
	}
	if _, ok := builtin.FilingOf("fakeads_nothing"); ok {
		t.Error("a tool no catalog lists was filed")
	}
}

// A SEAT IS OFFERED THE GROWTH TOOLS ITS LANE'S LEVEL ALLOWS, AND NO OTHER,
// through the real gate: the authority's row from the catalog, over the org
// chart's reading of the seat's level. A seat at read is offered the read
// tool and withheld the operate, spend and admin tools; a seat whose lane is
// off is withheld all four.
//
// Mutation: drop the catalog's row from the authority, or grant a growth tool
// by its product alone, and the read seat's offer goes wrong.
func TestASeatIsOfferedTheGrowthToolsItsLevelAllows(t *testing.T) {
	t.Parallel()
	_, o, _ := sshFixture(t)
	o.AgentSeatByHandle("sre").GrowthLevels = map[string]string{"fakeads": string(growth.LevelRead)}
	chart := orgchart.Of(o)
	reg := tools.NewRegistry()
	product := &fakeAds{levels: map[string]growth.Level{"sre": growth.LevelRead}}
	if _, err := builtin.Register(reg, builtin.Deps{Growth: builtin.GrowthDeps{
		Products: []builtin.GrowthProduct{product}, Requesters: healthRequesters(), Chart: chart,
		Audit: &growthAudit{}}}); err != nil {
		t.Fatal(err)
	}
	snapshot := reg.Snapshot()
	every := []string{growthReadTool, growthOperateTool, growthSpendTool, growthAdminTool}
	for _, c := range []struct {
		seat     string
		withheld []string
	}{
		{"sre", []string{growthOperateTool, growthSpendTool, growthAdminTool}},
		{"dev", every},
	} {
		got := slices.DeleteFunc(builtin.Withheld(asSeat(c.seat), builtin.Decide(chart), snapshot),
			func(tool string) bool { return !slices.Contains(every, tool) })
		if want := slices.Sorted(slices.Values(c.withheld)); !slices.Equal(got, want) {
			t.Errorf("%s is withheld the growth tools %v, want %v", c.seat, got, want)
		}
	}
}
