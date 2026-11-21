package builtin_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
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
