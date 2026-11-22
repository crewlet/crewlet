package googleplay

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// THE DOCS' MATRIX IS THE CATALOG, BOTH WAYS: every method the engine offers
// is a row of docs/integrations/google-play.md with its class and the class
// each state or field rule gives it, and every row names a method the engine
// offers, so a person reads exactly what an agent can do.
//
// Mutation: add an operation to the catalog without its row, or change a
// row's class or a state rule, and the test names it.
func TestTheDocsMatrixIsTheCatalog(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/integrations/google-play.md")
	if err != nil {
		t.Fatal(err)
	}
	_, matrix, ok := strings.Cut(string(raw), "## Every method, and its class")
	if !ok {
		t.Fatal("the page has no method matrix")
	}
	rows := map[string]string{}
	for _, line := range strings.Split(matrix, "\n") {
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if len(cells) < 4 || !strings.HasPrefix(cells[0], "`") {
			continue
		}
		rows[strings.Trim(cells[0], "`")] = cells[3]
	}
	for _, op := range catalog().Operations {
		class, listed := rows[op.ID]
		want := string(op.Class)
		for _, s := range op.States {
			want += fmt.Sprintf(", %s while %s", s.Class, s.When)
		}
		for _, f := range op.Fields {
			want += fmt.Sprintf(", %s for %s", f.Class, f.Field)
		}
		switch {
		case !listed:
			t.Errorf("%s is offered but not in the docs' matrix", op.ID)
		case class != want:
			t.Errorf("%s is %q, the docs say %q", op.ID, want, class)
		}
		delete(rows, op.ID)
	}
	for id := range rows {
		t.Errorf("the docs list %s, which the engine does not offer", id)
	}
}
