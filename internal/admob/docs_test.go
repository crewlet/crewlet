package admob

import (
	"os"
	"strings"
	"testing"
)

// THE DOCS' MATRIX IS THE CATALOG, BOTH WAYS: every method the engine offers
// is a row of docs/integrations/admob.md with its class, and every row names a
// method the engine offers, so a person reads exactly what an agent can do.
//
// Mutation: add an operation to the catalog without its row, or change a
// row's class, and the test names it.
func TestTheDocsMatrixIsTheCatalog(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/integrations/admob.md")
	if err != nil {
		t.Fatal(err)
	}
	_, matrix, ok := strings.Cut(string(raw), "## Every method, and its class")
	if !ok {
		t.Fatal("the page has no method matrix")
	}
	rows := map[string]string{}
	limited := map[string]bool{}
	for _, line := range strings.Split(matrix, "\n") {
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if len(cells) < 6 || !strings.HasPrefix(cells[0], "`") {
			continue
		}
		id := strings.Trim(cells[0], "`")
		rows[id] = cells[3]
		limited[id] = strings.Contains(cells[5], "limited access")
	}
	for _, op := range catalog().Operations {
		class, listed := rows[op.ID]
		want := string(op.Class)
		if len(op.Fields) > 0 {
			want += ", admin by field"
		}
		switch {
		case !listed:
			t.Errorf("%s is offered but not in the docs' matrix", op.ID)
		case class != want:
			t.Errorf("%s is %s, the docs say %q", op.ID, want, class)
		case limited[op.ID] != limitedAccess(op):
			t.Errorf("%s: limited access %v, the docs say %v", op.ID, limitedAccess(op), limited[op.ID])
		}
		delete(rows, op.ID)
	}
	for id := range rows {
		t.Errorf("the docs list %s, which the engine does not offer", id)
	}
}
