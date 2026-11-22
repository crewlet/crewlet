package searchconsole

import (
	"maps"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
)

// EVERY METHOD IS IN THE CATALOG, EACH CLASSED, AND EVERY TOOL IN ONE CLASS:
// the catalog validates (it registered at init), it lists the matrix's 20
// methods with its counts per class, the retired method alone is never and in
// no tool, and nothing spends or publishes, so no tool of either class exists.
//
// Mutation: class sitemaps.submit as admin, or offer the Mobile-Friendly Test
// on search_console_read, and the counts or the tool sweep name it.
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
	for _, op := range c.Operations {
		counts[op.Class]++
	}
	want := map[growth.Class]int{growth.ClassRead: 10, growth.ClassOperate: 3, growth.ClassAdmin: 6,
		growth.ClassNever: 1}
	if len(c.Operations) != 20 || !maps.Equal(counts, want) {
		t.Errorf("%d operations classed %v, want the matrix's 20 classed %v", len(c.Operations), counts, want)
	}
	offered := map[string]bool{}
	for _, tool := range c.Tools {
		if tool.Class == growth.ClassSpend || tool.Class == growth.ClassPublic || !tool.Class.Callable() {
			t.Errorf("tool %s is %s", tool.Name, tool.Class)
		}
		for _, id := range tool.Operations {
			offered[id] = true
		}
	}
	for _, op := range c.Operations {
		if offered[op.ID] == (op.Class == growth.ClassNever) {
			t.Errorf("%s is %s and offered=%v", op.ID, op.Class, offered[op.ID])
		}
	}
}

// EVERY ROLE IS OFFERED WHAT GOOGLE LETS IT DO, AND NO MORE: a restricted user
// reads, a full user also operates, an owner and a verified owner also ask
// admin acts, and an unverified identity reads its list and asks to verify.
//
// Mutation: give sitemaps.submit to restricted users, and their offer gains
// operate.
func TestEveryRoleIsOfferedWhatGoogleLetsItDo(t *testing.T) {
	t.Parallel()
	want := map[string][]growth.Class{
		roleUnverified:    {growth.ClassRead, growth.ClassAdmin},
		roleRestricted:    {growth.ClassRead, growth.ClassAdmin},
		roleFull:          {growth.ClassRead, growth.ClassOperate, growth.ClassAdmin},
		roleOwner:         {growth.ClassRead, growth.ClassOperate, growth.ClassAdmin},
		roleVerifiedOwner: {growth.ClassRead, growth.ClassOperate, growth.ClassAdmin},
	}
	c, _ := growth.CatalogOf(string(Kind))
	for _, offer := range c.RoleOffers() {
		if !slices.Equal(offer.Classes, want[offer.ID]) {
			t.Errorf("%s is offered %v, want %v", offer.ID, offer.Classes, want[offer.ID])
		}
	}
	for _, op := range []string{"webmasters.sitemaps.submit", "indexing.urlNotifications.publish",
		"siteVerification.webResource.update"} {
		o, _ := c.Operation(op)
		if growth.Permits(o, []string{roleRestricted}) {
			t.Errorf("a restricted user may call %s", op)
		}
	}
	del, _ := c.Operation("siteVerification.webResource.delete")
	if growth.Permits(del, []string{roleOwner}) || !growth.Permits(del, []string{roleVerifiedOwner}) {
		t.Error("relinquishing ownership is not a verified owner's alone")
	}
}
