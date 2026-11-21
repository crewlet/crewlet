package googleads

import (
	"os"
	"strings"
	"testing"
)

// THE DOCS' MATRIX IS THE CATALOG, BOTH WAYS: every method the engine offers
// is a row of docs/integrations/google-ads.md with its class, a method classed
// per call naming its rule, and every row names a method the engine offers.
//
// Mutation: add an operation to the catalog without its row, or change a
// row's class, and the test names it.
func TestTheDocsMatrixIsTheCatalog(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/integrations/google-ads.md")
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
		rule := ruleOf[op.ID]
		switch {
		case !listed:
			t.Errorf("%s is offered but not in the docs' matrix", op.ID)
		case rule != "" && !strings.HasSuffix(class, "by operation ("+rule+")"):
			t.Errorf("%s is classed by %s, the docs say %q", op.ID, rule, class)
		case rule == "" && class != string(op.Class):
			t.Errorf("%s is %s, the docs say %q", op.ID, op.Class, class)
		}
		delete(rows, op.ID)
	}
	for id := range rows {
		t.Errorf("the docs list %s, which the engine does not offer", id)
	}
}
