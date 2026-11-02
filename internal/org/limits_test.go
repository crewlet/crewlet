package org

import (
	"errors"
	"reflect"
	"testing"
)

// limitedOrg is a division whose lead turns `search` off and holds `tracker`'s
// close_items at approve, a team inside it holding close_items at blocked and
// delete_items at approve, a seat in that team holding delete_items at approve
// again and read_items at approve, a person in the team, and a seat at the root.
func limitedOrg() *Organization {
	return normalized(&Organization{
		Name: "Acme",
		Roles: []*Role{
			{Name: "CEO", DeclaredHandle: "ceo"},
		},
		Units: []*Unit{{
			Name: "Engineering", ID: "eng",
			MCPLimits: MCPLimits{
				"search":  {Enabled: Off()},
				"tracker": {Tools: map[string]ToolLimit{"close_items": ToolApprove}},
			},
			Roles: []*Role{{Name: "Architect", DeclaredHandle: "arch"}},
			Children: []*Unit{{
				Name: "Backend", ID: "backend",
				MCPLimits: MCPLimits{
					"tracker": {Tools: map[string]ToolLimit{"close_items": ToolBlocked, "delete_items": ToolApprove}},
				},
				Roles: []*Role{
					{Name: "Dev", DeclaredHandle: "dev", MCPLimits: MCPLimits{
						"tracker": {Tools: map[string]ToolLimit{"delete_items": ToolApprove, "read_items": ToolApprove}},
					}},
					{Name: "Pat", DeclaredHandle: "pat", Kind: KindHuman},
				},
			}},
		}},
	})
}

// AN AGENT SEAT IS HELD TO THE STRICTEST LIMIT ON THE WAY DOWN TO IT, at any
// depth, each part named by the block nearest the seat that holds it there: a
// division's off reaches a seat two units down, a team's blocked beats the
// division's approve, and the seat's own approve, which agrees with its
// team's, is named as the seat's own. A person is held to nothing, and a seat
// at the root to its own block alone.
//
// Mutation: fold the blocks outermost first, or keep the looser level, or stop
// the walk at the seat's own unit, and this is red.
func TestASeatIsHeldToTheStrictestLimitOnTheWayDownToIt(t *testing.T) {
	t.Parallel()
	o := limitedOrg()
	want := map[string]ServerLimit{
		"search": {Off: true, OffBy: "eng"},
		"tracker": {Tools: map[string]LimitedTool{
			"close_items":  {Level: ToolBlocked, By: "backend"},
			"delete_items": {Level: ToolApprove, By: ""},
			"read_items":   {Level: ToolApprove, By: ""},
		}},
	}
	if got := o.Role("dev").Limits; !reflect.DeepEqual(got, want) {
		t.Errorf("dev is held to %+v, want %+v", got, want)
	}
	if got := o.Role("arch").LimitOn("tracker"); !reflect.DeepEqual(got, ServerLimit{
		Tools: map[string]LimitedTool{"close_items": {Level: ToolApprove, By: "eng"}}}) {
		t.Errorf("arch is held to %+v on tracker, want close_items at approve by eng alone", got)
	}
	if got := o.Role("pat").Limits; got != nil {
		t.Errorf("a person is held to %+v, want nothing: a human seat runs no tools", got)
	}
	if got := o.Role("ceo").Limits; got != nil {
		t.Errorf("a seat at the root is held to %+v, want nothing", got)
	}
	if level, ok := o.Role("dev").LimitOn("tracker").Tool("list_items"); ok {
		t.Errorf("a tool no block names is held at %s", level)
	}
	var nobody *Role
	if !nobody.LimitOn("tracker").IsZero() {
		t.Error("a seat the organization does not have is held to something")
	}
}

// NORMALISING TWICE HOLDS EVERY SEAT TO THE SAME LIMITS: a stored revision is
// built again on every apply, and a pass that compounded the first would hold
// a seat to a limit it no longer carries.
func TestLimitsResolveTheSameOnEveryPass(t *testing.T) {
	t.Parallel()
	o := limitedOrg()
	first := o.Role("dev").Limits
	o.Role("dev").MCPLimits = nil
	o.Normalize()
	o.Normalize()
	if got := o.Role("dev").Limits; got["tracker"].Tools["read_items"].Level != "" {
		t.Errorf("dropping the seat's own block left its read_items limit: %+v", got)
	}
	if got := o.Role("dev").LimitOn("tracker").Tools["delete_items"]; got != (LimitedTool{Level: ToolApprove, By: "backend"}) {
		t.Errorf("the team's own delete_items limit is %+v after the seat's went, want approve by backend", got)
	}
	if first["tracker"].Tools["read_items"].Level != ToolApprove {
		t.Error("a later pass rewrote what an earlier pass resolved: the maps are shared")
	}
}

// A LEVEL THIS BUILD CANNOT READ HOLDS A TOOL BACK AS FAR AS ANY CAN. A limit
// only narrows, so a word a later build wrote is never read as looser than
// blocked; `allow`, which a limit cannot say, is read the same way.
func TestALimitThisBuildCannotReadBlocksTheTool(t *testing.T) {
	t.Parallel()
	o := normalized(&Organization{Name: "Acme", Roles: []*Role{{
		Name: "Dev", DeclaredHandle: "dev", MCPLimits: MCPLimits{"tracker": {Tools: map[string]ToolLimit{
			"close_items": "ask_twice", "list_items": "allow",
		}}},
	}}})
	for _, tool := range []string{"close_items", "list_items"} {
		if level, _ := o.Role("dev").LimitOn("tracker").Tool(tool); level != ToolBlocked {
			t.Errorf("%s is held at %q, want blocked", tool, level)
		}
	}
	if ToolLimit("allow").Valid() || ToolLimit("ask_twice").Valid() || !ToolBlocked.Valid() {
		t.Error("the closed set is not approve and blocked")
	}
	if !ToolBlocked.Stricter(ToolApprove) || ToolApprove.Stricter(ToolBlocked) || ToolApprove.Stricter(ToolApprove) {
		t.Error("blocked is not the stricter of the two")
	}
}

// A PERSON'S SEAT CARRIES NO LIMITS: it runs no tools, so a block there would
// read as a limit and hold nothing.
func TestAHumanSeatIsRefusedLimits(t *testing.T) {
	t.Parallel()
	r := &Role{Name: "Pat", Kind: KindHuman, MCPLimits: MCPLimits{"tracker": {Enabled: Off()}}}
	err := r.Validate()
	var seatErr *SeatError
	if !errors.Is(err, ErrHumanSeatField) || !errors.As(err, &seatErr) ||
		!reflect.DeepEqual(seatErr.Field, []any{"mcp_limits"}) {
		t.Fatalf("a human seat's mcp_limits = %v, want ErrHumanSeatField at mcp_limits", err)
	}
}
