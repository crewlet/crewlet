package authz_test

import (
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
			{ID: "users.create", Method: "POST", Path: "/users", Class: growth.ClassAdmin},
		},
		Tools: []growth.Tool{
			{Name: "testgrowth_report", Class: growth.ClassRead, Operations: []string{"report"},
				Description: "Read a report.", Parameters: object},
			{Name: "testgrowth_pause", Class: growth.ClassOperate, Operations: []string{"pause"},
				Description: "Pause.", Parameters: object},
			{Name: "testgrowth_add_user", Class: growth.ClassAdmin, Operations: []string{"users.create"},
				Description: "Add a user.", Parameters: object},
		},
	})
}

// A GROWTH TOOL IS DECIDED BY ITS PRODUCT'S GRANT AT ITS CLASS'S LEVEL, with
// no row written for it: an agent whose role carries the product's read grant
// may call its read tool and no other, and a tool no catalog lists is no
// verb at all.
//
// Mutation: answer a growth tool by its product alone, ignoring the level,
// and the read seat pauses.
func TestAGrowthToolIsDecidedByItsProductsGrantAtItsLevel(t *testing.T) {
	t.Parallel()
	c := nimbus()
	c.granted[[2]string{"sre", string(authz.GrowthSeatGrant("testgrowth", growth.LevelRead))}] = true
	for _, tc := range []struct {
		tool  string
		grant authz.SeatGrant
		want  bool
	}{
		{"testgrowth_report", "testgrowth_read", true},
		{"testgrowth_pause", "testgrowth_operate", false},
		{"testgrowth_add_user", "testgrowth_administer", false},
	} {
		d := authz.Decide(t.Context(), seat("sre"), authz.Action(tc.tool), authz.Object{}, c, decidedAt)
		if d.Allowed != tc.want || d.SeatGrant != tc.grant {
			t.Errorf("%s: allowed %v by grant %q, want %v by %q (%+v)", tc.tool, d.Allowed, d.SeatGrant,
				tc.want, tc.grant, d)
		}
	}
	if g, ok := authz.SeatGrantOf(authz.Action("testgrowth_nothing")); ok || g != "" {
		t.Errorf("a tool no catalog lists has grant %q", g)
	}
	if !authz.GrowthSeatGrant("testgrowth", growth.LevelOperate).Valid() || authz.SeatGrant("nosuch_read").Valid() {
		t.Error("a growth grant is valid only for a registered product")
	}
}
