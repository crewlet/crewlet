package searchconsole

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// THE DOCS' MATRIX IS THE CATALOG, BOTH WAYS: every method the engine offers
// is a row of docs/integrations/google-search-console.md with its class, its
// least role and its scope, and every row names a method the catalog lists,
// so a person reads exactly what an agent can do. Its resource and call are
// the catalog's too.
//
// Mutation: add an operation to the catalog without its row, or change a
// row's resource, call, class, role or scope, and the test names it.
func TestTheDocsMatrixIsTheCatalog(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../../docs/integrations/google-search-console.md")
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
	for _, op := range catalog().Operations {
		cells, listed := rows[op.ID]
		if !listed {
			t.Errorf("%s is in the catalog but not in the docs' matrix", op.ID)
			continue
		}
		delete(rows, op.ID)
		if cells[1] != op.Resource {
			t.Errorf("%s acts on %s, the docs say %q", op.ID, op.Resource, cells[1])
		}
		if want := "`" + op.Method + " " + pathOf(op.Path) + "`"; cells[2] != want {
			t.Errorf("%s is called as %s, the docs say %s", op.ID, want, cells[2])
		}
		if cells[3] != string(op.Class) {
			t.Errorf("%s is %s, the docs say %q", op.ID, op.Class, cells[3])
		}
		if want := leastRole(op.Roles, op.Class.Callable()); cells[4] != want {
			t.Errorf("%s needs %s, the docs say %q", op.ID, want, cells[4])
		}
		scope := "none"
		if len(op.Permissions) > 0 {
			scope = strings.TrimPrefix(op.Permissions[0], "https://www.googleapis.com/auth/")
		}
		if cells[5] != scope {
			t.Errorf("%s mints %s, the docs say %q", op.ID, scope, cells[5])
		}
	}
	for id := range rows {
		t.Errorf("the docs list %s, which the catalog does not", id)
	}
}

// pathOf is an operation's path after its host, as the matrix writes it.
func pathOf(address string) string {
	_, rest, _ := strings.Cut(address, "://")
	if i := strings.Index(rest, "/"); i >= 0 {
		return rest[i:]
	}
	return rest
}

// leastRole is the name of the least role of ids, as the matrix writes it.
func leastRole(ids []string, callable bool) string {
	switch {
	case !callable:
		return "none"
	case len(ids) == 0:
		return "Any Google account"
	}
	for _, r := range roles() {
		if slices.Contains(ids, r.ID) {
			return r.Name
		}
	}
	return ""
}
