package adsense

import (
	"os"
	"strings"
	"testing"
)

// THE DOCS' MATRIX IS THE CATALOG, BOTH WAYS: every method the engine offers
// is a row of docs/integrations/adsense.md with its class and whether Google
// answers it only for enabled projects, and every row names a method the
// engine offers, so a person reads exactly what an agent can do.
//
// Mutation: add an operation to the catalog without its row, or change a
// row's class, and the test names it.
func TestTheDocsMatrixIsTheCatalog(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/integrations/adsense.md")
	if err != nil {
		t.Fatal(err)
	}
	_, matrix, ok := strings.Cut(string(raw), "## Every method, and its class")
	if !ok {
		t.Fatal("the page has no method matrix")
	}
	type row struct {
		class, call string
		enabled     bool
	}
	rows := map[string]row{}
	for _, line := range strings.Split(matrix, "\n") {
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if len(cells) < 4 || !strings.HasPrefix(cells[0], "`") {
			continue
		}
		rows[strings.Trim(cells[0], "`")] = row{class: cells[2], call: strings.Trim(cells[1], "`"),
			enabled: strings.Contains(cells[3], "enabled projects")}
	}
	for _, op := range catalog().Operations {
		r, listed := rows[op.ID]
		want := string(op.Class)
		for _, f := range op.Fields {
			want += ", " + string(f.Class) + " by field"
		}
		call := op.Method + " " + op.Path[strings.Index(op.Path, ".com")+len(".com"):]
		switch {
		case !listed:
			t.Errorf("%s is offered but not in the docs' matrix", op.ID)
		case r.class != want:
			t.Errorf("%s is %s, the docs say %q", op.ID, want, r.class)
		case r.call != call:
			t.Errorf("%s is %s, the docs say %q", op.ID, call, r.call)
		case r.enabled != restricted(op):
			t.Errorf("%s: answered only for enabled projects %v, the docs say %v", op.ID, restricted(op), r.enabled)
		}
		delete(rows, op.ID)
	}
	for id := range rows {
		t.Errorf("the docs list %s, which the engine does not offer", id)
	}
}
