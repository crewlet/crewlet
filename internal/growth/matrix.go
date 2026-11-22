package growth

import (
	"fmt"
	"slices"
	"strings"
)

// Matrix is c's coverage matrix as a Markdown page: every operation with its
// method and path, the states that lower it, whether it can be undone, and
// the roles that may make it, one section per class so a reader (and the
// dashboard's editor, which cuts a long page at its headings) finds a class
// at once. A product's reference page is this, generated (cmd
// internal/growth/matrixdoc) and held to its catalog by a test.
func Matrix(c Catalog) string {
	names := map[string]string{}
	for _, r := range c.Roles {
		names[r.ID] = r.Name
	}
	ops := slices.Clone(c.Operations)
	slices.SortStableFunc(ops, func(a, b Operation) int {
		return strings.Compare(a.Resource+" "+a.Path+" "+a.Method, b.Resource+" "+b.Path+" "+b.Method)
	})
	byClass := map[Class][]Operation{}
	for _, op := range ops {
		byClass[op.Class] = append(byClass[op.Class], op)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# %s operations\n\n", c.API)
	fmt.Fprintf(&b, "Every operation of %s %s as Crewlet offers it, generated from the integration's catalog: "+
		"never edit this page.\n\n", c.API, c.Version)
	b.WriteString("| Class | Operations |\n|---|---|\n")
	for _, cl := range Classes() {
		if n := len(byClass[cl]); n > 0 {
			fmt.Fprintf(&b, "| [%s](#%s) | %d |\n", cl, cl, n)
		} else {
			fmt.Fprintf(&b, "| %s | 0 |\n", cl)
		}
	}
	for _, cl := range Classes() {
		if len(byClass[cl]) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n## %s\n\n| Resource | Operation | Method and path | Lowered while | Roles | Note |\n"+
			"|---|---|---|---|---|---|\n", cl)
		for _, op := range byClass[cl] {
			var lowered []string
			for _, s := range op.States {
				lowered = append(lowered, fmt.Sprintf("%s: %s", s.When, s.Class))
			}
			var note string
			switch {
			case op.Class == ClassNever:
				note = op.Reason
			case op.Danger:
				note = "cannot be undone"
			}
			var roles []string
			for _, r := range op.Roles {
				roles = append(roles, names[r])
			}
			fmt.Fprintf(&b, "| %s | `%s` | `%s %s` | %s | %s | %s |\n", op.Resource, op.ID, op.Method, op.Path,
				strings.Join(lowered, ", "), strings.Join(roles, ", "), note)
		}
	}
	return b.String()
}
