package authz_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/growth"
)

func init() {
	object := map[string]any{"type": "object"}
	growth.Register(growth.Catalog{
		Kind: "testgrowth", API: "Test Growth API", Version: "v1",
		Operations: []growth.Operation{
			{ID: "report", Method: "GET", Path: "/report", Class: growth.ClassRead},
			{ID: "pause", Method: "POST", Path: "/pause", Class: growth.ClassOperate},
			{ID: "ads.publish", Method: "POST", Path: "/ads:publish", Class: growth.ClassPublic},
			{ID: "budget.raise", Method: "POST", Path: "/budget:raise", Class: growth.ClassSpend},
			{ID: "users.create", Method: "POST", Path: "/users", Class: growth.ClassAdmin},
		},
		Tools: []growth.Tool{
			{Name: "testgrowth_report", Class: growth.ClassRead, Operations: []string{"report"},
				Description: "Read a report.", Parameters: object},
			{Name: "testgrowth_pause", Class: growth.ClassOperate, Operations: []string{"pause"},
				Description: "Pause.", Parameters: object},
			{Name: "testgrowth_publish", Class: growth.ClassPublic, Operations: []string{"ads.publish"},
				Description: "Publish an ad.", Parameters: object},
			{Name: "testgrowth_raise_budget", Class: growth.ClassSpend, Operations: []string{"budget.raise"},
				Description: "Raise a budget.", Parameters: object},
			{Name: "testgrowth_add_user", Class: growth.ClassAdmin, Operations: []string{"users.create"},
				Description: "Add a user.", Parameters: object},
		},
	})
}

// A GROWTH TOOL IS DECIDED BY ITS PRODUCT'S GRANT AT ITS CLASS'S LEVEL, with
// no row written for it: a seat at read may call the read tool alone, one at
// operate also publishes and spends (each act then waits for an
// administrator), one at administer calls every tool, and a tool no catalog
// lists is no verb at all. A seat carries every grant up to its level
// (internal/authz/orgchart).
//
// Mutation: answer a growth tool by its product alone, ignoring the level,
// and the read seat pauses; ask public or spend at administer, and the
// operate seat cannot publish; leave the catalogs out of Actions, and no walk
// sees the tools.
func TestAGrowthToolIsDecidedByItsProductsGrantAtItsLevel(t *testing.T) {
	t.Parallel()
	c := nimbus()
	upTo := map[string]growth.Level{"sre": growth.LevelRead, "ops": growth.LevelOperate, "lead": growth.LevelAdminister}
	for seat, top := range upTo {
		for _, l := range growth.Levels() {
			c.granted[[2]string{seat, string(authz.GrowthSeatGrant("testgrowth", l))}] = true
			if l == top {
				break
			}
		}
	}
	tools := []struct {
		tool  string
		grant authz.SeatGrant
	}{
		{"testgrowth_report", "testgrowth_read"},
		{"testgrowth_pause", "testgrowth_operate"},
		{"testgrowth_publish", "testgrowth_operate"},
		{"testgrowth_raise_budget", "testgrowth_operate"},
		{"testgrowth_add_user", "testgrowth_administer"},
	}
	allowed := map[string][]string{
		"sre":  {"testgrowth_report"},
		"ops":  {"testgrowth_report", "testgrowth_pause", "testgrowth_publish", "testgrowth_raise_budget"},
		"lead": {"testgrowth_report", "testgrowth_pause", "testgrowth_publish", "testgrowth_raise_budget", "testgrowth_add_user"},
	}
	for _, tc := range tools {
		if !slices.Contains(authz.Actions(), authz.Action(tc.tool)) {
			t.Errorf("%s is not among the verbs every walk covers", tc.tool)
		}
	}
	for s, want := range allowed {
		for _, tc := range tools {
			d := authz.Decide(t.Context(), seat(s), authz.Action(tc.tool), authz.Object{}, c, decidedAt)
			if d.Allowed != slices.Contains(want, tc.tool) || d.SeatGrant != tc.grant {
				t.Errorf("%s: %s allowed %v by grant %q, want %v by %q (%+v)", s, tc.tool, d.Allowed,
					d.SeatGrant, slices.Contains(want, tc.tool), tc.grant, d)
			}
		}
	}
	if g, ok := authz.SeatGrantOf(authz.Action("testgrowth_nothing")); ok || g != "" {
		t.Errorf("a tool no catalog lists has grant %q", g)
	}
	if !authz.GrowthSeatGrant("testgrowth", growth.LevelOperate).Valid() || authz.SeatGrant("nosuch_read").Valid() {
		t.Error("a growth grant is valid only for a registered product")
	}
}
