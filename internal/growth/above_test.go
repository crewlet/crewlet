package growth_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
)

// classCatalog is a catalog of one class tool per class, each naming its
// methods by ID.
func classCatalog() growth.Catalog {
	byID := map[string]any{"type": "object", "properties": map[string]any{"operation": map[string]any{"type": "string"}}}
	tool := func(name string, class growth.Class, covers string, ops ...string) growth.Tool {
		return growth.Tool{Name: "class_ads_" + name, Class: class, Operations: ops, Description: "A tool.",
			Parameters: byID, Covers: covers}
	}
	return growth.Catalog{Kind: "class_ads", API: "Class Ads", Version: "v1",
		Operations: []growth.Operation{
			{ID: "ads.list", Method: "GET", Path: "/ads", Class: growth.ClassRead},
			{ID: "ads.pause", Method: "POST", Path: "/ads:pause", Class: growth.ClassOperate},
			{ID: "ads.enable", Method: "POST", Path: "/ads:enable", Class: growth.ClassSpend},
			{ID: "retention.update", Method: "PATCH", Path: "/retention", Class: growth.ClassAdmin},
			{ID: "orphan.delete", Method: "DELETE", Path: "/orphans", Class: growth.ClassAdmin},
			{ID: "billing.charge", Method: "POST", Path: "/charge", Class: growth.ClassNever,
				Reason: "The vendor's terms reserve payments to its own console."},
		},
		Tools: []growth.Tool{
			tool("read", growth.ClassRead, "", "ads.list"),
			tool("operate", growth.ClassOperate, "pauses", "ads.pause"),
			tool("spend", growth.ClassSpend, "enabling", "ads.enable"),
			tool("admin", growth.ClassAdmin, "data retention", "retention.update"),
		},
	}
}

// said is err's refusal words, failing unless it is a refusal of kind.
func said(t *testing.T, err error, kind growth.RefusalKind) string {
	t.Helper()
	var r *growth.Refusal
	if !errors.As(err, &r) || r.Kind != kind {
		t.Fatalf("answered %v, want a %s refusal", err, kind)
	}
	return r.Said
}

// A METHOD OUTSIDE A TOOL IS TOLD APART BY WHERE IT IS: one above the seat's
// level names its class, its tool, its level, its approval and the way to the
// level; one the seat's level reaches names its tool alone; a lane that is off
// is turned on; a never method gives its reason; one no tool lists, and one the
// catalog does not list, say that no level reaches them.
//
// Mutation: answer every method outside a tool alike, or name the raise to a
// seat that holds the level, and one of these reads wrong.
func TestAMethodOutsideAToolIsToldApartByWhereItIs(t *testing.T) {
	t.Parallel()
	c := classCatalog()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	above := said(t, c.Outside("class_ads_operate", "retention.update", growth.LevelOperate), growth.RefusedForbidden)
	for _, want := range []string{"retention.update is admin-class", "class_ads_admin makes it", "at Administer",
		"administrator's approval", "at Manage (operate)", "raise it to Administer", "comment_on_work_item",
		"Settings, Integrations, then the integration's Agents tab", "request_access asks for no level"} {
		if !strings.Contains(above, want) {
			t.Errorf("a method above the seat answered %q, want %q", above, want)
		}
	}
	held := said(t, c.Outside("class_ads_operate", "ads.enable", growth.LevelOperate), growth.RefusedInvalid)
	if !strings.Contains(held, "Call class_ads_spend instead") || strings.Contains(held, "raise") {
		t.Errorf("a method the seat's level reaches answered %q", held)
	}
	off := said(t, c.Outside("class_ads_read", "ads.pause", ""), growth.RefusedForbidden)
	if !strings.Contains(off, "is off, so ask an administrator to turn it on at Manage") {
		t.Errorf("a seat whose lane is off answered %q", off)
	}
	never := said(t, c.Outside("class_ads_admin", "billing.charge", growth.LevelAdminister), growth.RefusedForbidden)
	if !strings.Contains(never, "not offered at any level: The vendor's terms") {
		t.Errorf("a never method answered %q", never)
	}
	for _, id := range []string{"orphan.delete", "ads.nothing"} {
		if got := c.Outside("class_ads_read", id, growth.LevelRead).Error(); !strings.Contains(got, "at any level") ||
			strings.Contains(got, "raise") {
			t.Errorf("%s answered %q, want no level to reach it", id, got)
		}
	}
}

// A CLASS TOOL SAYS WHAT LIES ABOVE IT: every tool of a higher class, with
// what it changes, its level and its approval, and nothing above the top.
//
// Mutation: skip a class, or a tool's Covers, and the operate tool omits the
// admin tool or what it changes.
func TestAClassToolSaysWhatLiesAboveIt(t *testing.T) {
	t.Parallel()
	c := classCatalog()
	operate := c.Above("class_ads_operate")
	for _, want := range []string{"class_ads_spend (spend: enabling, at Manage (operate), each call held",
		"class_ads_admin (admin: data retention, at Administer, each call held for an administrator's approval"} {
		if !strings.Contains(operate, want) {
			t.Errorf("the operate tool says %q, want %q", operate, want)
		}
	}
	if strings.Contains(operate, "class_ads_operate (") || c.Above("class_ads_admin") != "" ||
		c.Above("class_ads_nothing") != "" {
		t.Errorf("a tool named itself or the top tool named something above it: %q", operate)
	}
}

// A CLASS TOOL ABOVE READ SAYS WHAT ITS METHODS CHANGE, so every tool below it
// can name them.
//
// Mutation: drop the rule, and an admin tool with no Covers registers.
func TestAClassToolAboveReadSaysWhatItChanges(t *testing.T) {
	t.Parallel()
	c := classCatalog()
	c.Tools[3].Covers = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "Covers") {
		t.Errorf("an admin class tool with no Covers answered %v", err)
	}
}

// A LEVEL IS NAMED SO EVERY AGENTS TAB FINDS IT: its label, and its written
// name where that differs.
func TestALevelIsNamedByItsLabelAndItsWrittenName(t *testing.T) {
	t.Parallel()
	for level, want := range map[growth.Level]string{growth.LevelRead: "View (read)",
		growth.LevelOperate: "Manage (operate)", growth.LevelAdminister: "Administer"} {
		if got := level.Named(); got != want {
			t.Errorf("%s is named %q, want %q", level, got, want)
		}
	}
}
