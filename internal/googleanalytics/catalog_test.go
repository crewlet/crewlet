package googleanalytics

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/googlegrowth"
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

// A RULE NAMES A FIELD AS GOOGLE'S JSON DOES, in lowerCamel, since a request is
// classed with every spelling Google takes read as that name: a rule spelled
// otherwise would match no change, and its field would take the highest class.
//
// Mutation: write a rule's field in snake_case, and the test names it.
func TestEveryFieldRuleNamesItsFieldInLowerCamel(t *testing.T) {
	t.Parallel()
	for _, op := range catalog().Operations {
		for _, f := range op.Fields {
			if googlegrowth.LowerCamel(f.Field) != f.Field || strings.Contains(f.Field, "_") {
				t.Errorf("%s's rule names %q, not its lowerCamel name", op.ID, f.Field)
			}
		}
	}
}

// EVERY WRITE NAMES WHAT IT ACTS IN, so the seat's scope holds it: its path's
// property or account, the account or property its body names
// ([bodyScopes]), the stream secret's property for the Measurement Protocol,
// or, for the one write opening a new account, nothing that exists.
//
// Mutation: add a write on a collection with no body scope, or a body scope
// for an operation the catalog lacks, and the test names it.
func TestEveryWriteNamesWhatItActsIn(t *testing.T) {
	t.Parallel()
	c := catalog()
	for _, op := range c.Operations {
		if op.Method == "GET" || op.Reads {
			continue
		}
		pathScoped := strings.Contains(op.Path, "{propertiesId}") || strings.Contains(op.Path, "{accountsId}")
		if !pathScoped && bodyScopes()[op.ID].Target == "" && op.ID != actsInNone && !strings.HasPrefix(op.ID, "mp.") {
			t.Errorf("%s names no property or account a seat's scope can hold", op.ID)
		}
	}
	for id, s := range bodyScopes() {
		op, ok := c.Operation(id)
		if !ok {
			t.Errorf("a body scope names %s, which the catalog does not list", id)
			continue
		}
		pathScoped := strings.Contains(op.Path, "{propertiesId}") || strings.Contains(op.Path, "{accountsId}")
		if (s.Target != "") == pathScoped {
			t.Errorf("%s's body names where it is made only where its path names none", id)
		}
		for _, f := range append([]string{s.Target}, s.Also...) {
			if googlegrowth.LowerCamel(f) != f {
				t.Errorf("%s's body scope names %q, not its lowerCamel name", id, f)
			}
		}
	}
}
