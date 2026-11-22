package admob

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
)

// EVERY METHOD IS IN THE CATALOG, EACH CLASSED, AND EVERY TOOL IN ONE CLASS:
// the catalog validates, it lists the coverage matrix's 19 methods, 11 read
// and 8 operate, nothing in AdMob is public, spend or never (AdMob pays the
// publisher, and no method is refused), and admin is reached by exactly the
// three irreversible changes, each a field rule of an operate method.
//
// Mutation: drop a method, class a write read, or give a fourth method an
// admin rule, and this test goes red.
func TestTheCatalogListsEveryMethodInExactlyOneClass(t *testing.T) {
	t.Parallel()
	c, ok := growth.CatalogOf(string(Kind))
	if !ok {
		t.Fatal("the catalog did not register")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	counts := map[growth.Class]int{}
	var admin []string
	for _, op := range c.Operations {
		counts[op.Class]++
		for _, f := range op.Fields {
			if f.Class == growth.ClassAdmin {
				admin = append(admin, op.ID)
			}
		}
	}
	if len(c.Operations) != 19 || counts[growth.ClassRead] != 11 || counts[growth.ClassOperate] != 8 {
		t.Errorf("%d operations by class %v, want 19: 11 read and 8 operate", len(c.Operations), counts)
	}
	if want := []string{"admob.accounts.apps.create", "admob.accounts.mediationGroups.patch",
		"admob.accounts.mediationGroups.mediationAbExperiments.stop"}; !slices.Equal(admin, want) {
		t.Errorf("admin is reached by %v, want %v", admin, want)
	}
	limited := 0
	for _, op := range c.Operations {
		if limitedAccess(op) {
			limited++
		}
	}
	if limited != 10 {
		t.Errorf("%d limited-access methods, want the matrix's 10", limited)
	}
	if len(c.Tools) != 15 {
		t.Errorf("%d tools, want 12 curated and 3 generic", len(c.Tools))
	}
	for _, tool := range c.Tools {
		if tool.Class == growth.ClassPublic || tool.Class == growth.ClassSpend {
			t.Errorf("tool %s is %s; nothing in AdMob publishes or spends", tool.Name, tool.Class)
		}
	}
}

// EVERY ROLE IS EXPLAINED, AND EACH WRITE NAMES WHO MAY MAKE IT: a custom
// role with Inventory management at View and manage makes every write, one at
// View makes none, and the reports need a reports permission.
func TestEveryWriteNamesTheRolesThatMayMakeIt(t *testing.T) {
	t.Parallel()
	c := catalog()
	for _, op := range c.Operations {
		manage := growth.Permits(op, []string{roleInventoryManage})
		view := growth.Permits(op, []string{roleInventoryView})
		switch {
		case op.Class != growth.ClassRead && (!manage || view):
			t.Errorf("%s: View and manage %v, View %v; want only View and manage", op.ID, manage, view)
		case slices.Contains([]string{networkReportOp, mediationReportOp, campaignReportOp}, op.ID) && view:
			t.Errorf("%s is open to a custom role with no reports permission", op.ID)
		}
	}
}
