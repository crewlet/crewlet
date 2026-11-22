package googleanalytics

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
)

// THE DOCS' MATRIX IS THE CATALOG, BOTH WAYS: every method the engine offers
// is a row of docs/integrations/google-analytics.md with its class, its least
// role and its scope, and every row names a method the engine offers, so a
// person reads exactly what an agent can do and what its identity needs.
//
// Mutation: add an operation to the catalog without its row, or change a
// row's class, its operation's roles or its scope, and the test names it.
func TestTheDocsMatrixIsTheCatalog(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/integrations/google-analytics.md")
	if err != nil {
		t.Fatal(err)
	}
	_, matrix, ok := strings.Cut(string(raw), "## Every method, and its class")
	if !ok {
		t.Fatal("the page has no method matrix")
	}
	rows := map[string][]string{}
	for _, line := range strings.Split(matrix, "\n") {
		cells := strings.Split(strings.Trim(line, "| "), " | ")
		if len(cells) < 6 || !strings.HasPrefix(cells[0], "`") {
			continue
		}
		rows[strings.Trim(cells[0], "`")] = cells
	}
	c := catalog()
	for _, op := range c.Operations {
		cells, listed := rows[op.ID]
		if !listed {
			t.Errorf("%s is offered but not in the docs' matrix", op.ID)
			continue
		}
		class, role, scope := cells[3], cells[4], cells[5]
		switch {
		case len(op.Fields) > 0 && !strings.HasSuffix(class, "by field"):
			t.Errorf("%s is classed by field, the docs say %q", op.ID, class)
		case len(op.Fields) == 0 && class != string(op.Class):
			t.Errorf("%s is %s, the docs say %q", op.ID, op.Class, class)
		}
		if want := leastRole(op); !strings.HasPrefix(role, want) {
			t.Errorf("%s's least role is %s, the docs say %q", op.ID, want, role)
		}
		if want := scopeWords(op); scope != want {
			t.Errorf("%s is minted %s, the docs say %q", op.ID, want, scope)
		}
		delete(rows, op.ID)
	}
	for id := range rows {
		t.Errorf("the docs list %s, which the engine does not offer", id)
	}
}

// leastRole is how the docs' matrix begins op's role: its least role's name,
// "any" for a method any role or identity may call, "none" for one no Google
// identity calls (the Measurement Protocol's, which a stream secret allows).
func leastRole(op growth.Operation) string {
	switch {
	case len(op.Roles) == 0 && len(op.Permissions) == 0:
		return "none"
	case len(op.Roles) == 0:
		return "any"
	}
	for _, r := range roles() {
		if slices.Contains(op.Roles, r.ID) {
			return r.Name
		}
	}
	return "an unknown role"
}

// scopeWords is the docs' matrix's scope of op: its scopes without Google's
// prefix, or the stream secret a Measurement Protocol call carries instead.
func scopeWords(op growth.Operation) string {
	if len(op.Permissions) == 0 {
		return "a stream secret"
	}
	var out []string
	for _, p := range op.Permissions {
		out = append(out, strings.TrimPrefix(p, "https://www.googleapis.com/auth/"))
	}
	return strings.Join(out, ", ")
}
