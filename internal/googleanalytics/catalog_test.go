package googleanalytics

import (
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
)

// EVERY METHOD IS IN THE CATALOG, EACH CLASSED, AND EVERY TOOL IN ONE CLASS:
// the catalog validates (it registered at init), it lists every method of the
// coverage matrix, and nothing in Google Analytics is offered at public or
// never, which no method of it is.
func TestTheCatalogListsEveryMethodInExactlyOneClass(t *testing.T) {
	t.Parallel()
	c, ok := growth.CatalogOf(string(Kind))
	if !ok {
		t.Fatal("the catalog did not register")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if n := len(c.Operations); n != 193 {
		t.Errorf("%d operations, want the matrix's 191 API methods and 2 Measurement Protocol calls", n)
	}
	counts := map[growth.Class]int{}
	for _, op := range c.Operations {
		counts[op.Class]++
		if op.Class == growth.ClassPublic || op.Class == growth.ClassNever {
			t.Errorf("%s is %s; no Google Analytics method publishes, and none is refused", op.ID, op.Class)
		}
	}
	for _, tool := range c.Tools {
		if !tool.Class.Callable() {
			t.Errorf("tool %s is %s", tool.Name, tool.Class)
		}
	}
	if counts[growth.ClassRead] == 0 || counts[growth.ClassOperate] == 0 || counts[growth.ClassAdmin] == 0 {
		t.Errorf("classes %v", counts)
	}
}
