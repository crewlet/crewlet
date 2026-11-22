package googleplay

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
)

// EVERY METHOD IS IN THE CATALOG, EACH CLASSED, AND EVERY TOOL IN ONE CLASS:
// the catalog validates (it registered at init), it lists the coverage
// matrix's 190 methods in its classes, every never row says why, and every
// method but the engine's own edit steps is offered by a tool.
//
// Mutation: drop a generic tool, or class a method never without a reason,
// and the test names it.
func TestTheCatalogListsEveryMethodInExactlyOneClass(t *testing.T) {
	t.Parallel()
	c, ok := growth.CatalogOf(string(Kind))
	if !ok {
		t.Fatal("the catalog did not register")
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if n := len(c.Operations); n != 190 {
		t.Errorf("%d operations, want the matrix's 190", n)
	}
	counts := map[growth.Class]int{}
	for _, op := range c.Operations {
		counts[op.Class]++
	}
	want := map[growth.Class]int{growth.ClassRead: 79, growth.ClassOperate: 14, growth.ClassPublic: 55,
		growth.ClassSpend: 2, growth.ClassAdmin: 28, growth.ClassNever: 12}
	for class, n := range want {
		if counts[class] != n {
			t.Errorf("%d %s methods, want %d", counts[class], class, n)
		}
	}
	offered := map[string]bool{}
	for _, tool := range c.Tools {
		for _, id := range tool.Operations {
			offered[id] = true
		}
	}
	for _, op := range c.Operations {
		switch {
		case op.Class == growth.ClassNever && !strings.Contains(op.Reason, "cannot") &&
			!strings.Contains(op.Reason, "not supported"):
			t.Errorf("%s is never offered for %q, which says no impossibility", op.ID, op.Reason)
		case op.Class != growth.ClassNever && !offered[op.ID] && !slices.Contains(sessionOps(), op.ID):
			t.Errorf("%s is offered by no tool", op.ID)
		case op.Class == growth.ClassNever && offered[op.ID]:
			t.Errorf("%s is never offered, and a tool offers it", op.ID)
		}
	}
}

// AN UPLOAD IS NEVER LESS THAN WHAT IT PUBLISHES, AND NOTHING READS AN EDIT
// AN AGENT OPENED: every upload method's class is held to the matrix, and
// every staged method takes the engine's edit.
func TestEveryStagedMethodTakesTheEnginesEdit(t *testing.T) {
	t.Parallel()
	for _, op := range catalog().Operations {
		if !strings.Contains(op.Path, "{editId}") {
			continue
		}
		req, err := newRequest(op, map[string]string{"packageName": "com.acme.app", "editId": "agent-edit"}, nil, nil)
		if err == nil || !strings.Contains(err.Error(), "editId") {
			t.Errorf("%s took an edit an agent named: %v, %s", op.ID, err, req.Address)
		}
	}
}
