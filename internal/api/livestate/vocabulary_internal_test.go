package livestate

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	_ "github.com/crewlet/crewlet/internal/events/types"
)

// EVERY TYPE THE PROJECTION BRANCHES ON IS A TYPE THE ENGINE PUBLISHES.
//
// A branch on a name nothing publishes is dead code that reads as coverage:
// this package's own tests synthesise the envelopes they apply, so such a
// branch passes here while being unreachable in production. The projection
// carried three until the retired task-lifecycle types were removed, and the
// seat state they moved was unreachable the whole time. The registry is the
// list of what exists, and internal/engine asserts that everything in it has a
// publisher, so a name here that is not in the registry is a branch nothing
// can reach.
//
// The maps are read directly, and the branches outside them (an `env.Type`
// compared with a literal, or switched on) are read from this package's own
// source, so a new branch is covered without being listed here.
func TestEveryTypeTheProjectionReadsIsRegistered(t *testing.T) {
	t.Parallel()
	known := map[string]bool{}
	for _, typ := range events.RegisteredTypes() {
		known[typ] = true
	}
	for _, group := range []struct {
		what  string
		types []string
	}{
		{"stateEvents", keysOf(stateEvents)},
		{"failureEvents", keysOf(failureEvents)},
		{"providerFailure", []string{providerFailure}},
		{"sandboxEvents", keysOf(sandboxEvents)},
		{"a branch on env.Type", branchedTypes(t)},
	} {
		for _, typ := range group.types {
			if !known[typ] {
				t.Errorf("%s names %q, which no event type declares: the "+
					"branch cannot run, and a reader cannot tell it from one "+
					"whose producer they have not found", group.what, typ)
			}
		}
	}
}

// keysOf is the map's keys, whatever its value type.
func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for key := range m {
		out = append(out, key)
	}
	return out
}

// branchedTypes is every string literal this package's non-test source
// compares an envelope's type against, as `env.Type == "x"` or as a case of
// `switch env.Type`.
func branchedTypes(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	var found []string
	literal := func(expr ast.Expr) {
		if lit, ok := expr.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if value, err := strconv.Unquote(lit.Value); err == nil {
				found = append(found, value)
			}
		}
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.BinaryExpr:
				if n.Op != token.EQL && n.Op != token.NEQ {
					return true
				}
				if isEnvType(n.X) {
					literal(n.Y)
				} else if isEnvType(n.Y) {
					literal(n.X)
				}
			case *ast.SwitchStmt:
				if !isEnvType(n.Tag) {
					return true
				}
				for _, stmt := range n.Body.List {
					if clause, ok := stmt.(*ast.CaseClause); ok {
						for _, expr := range clause.List {
							literal(expr)
						}
					}
				}
			}
			return true
		})
	}
	if len(found) == 0 {
		t.Fatal("no branch on env.Type found, so this half of the test could not fail")
	}
	return found
}

// isEnvType reports whether expr is `env.Type`, the envelope's wire type as
// every Apply path names it.
func isEnvType(expr ast.Expr) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Type" {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == "env"
}
