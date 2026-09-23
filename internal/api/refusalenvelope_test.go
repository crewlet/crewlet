package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// THE SETTINGS SURFACES REFUSE IN THE ENVELOPE, AND A 503 SAYS WHEN.
//
// `/config` and `/secrets` wrote their own `{"error": code}` objects — a code
// and nothing a person could read — at some thirty sites, `/setup` declared a
// dozen codes of its own with no sentence behind any of them (two of them
// second spellings of codes the vocabulary already held), and a dozen 503s
// across these surfaces went out through the ordinary refusal writer with no
// Retry-After, so a client was told "come back" and not when.
//
// Each of those shapes is visible in the SOURCE and nowhere else — a route
// that builds its own body answers correctly on every case somebody thought
// to test — so this walks the source:
//
//   - a map literal carrying an "error" key is a hand-built envelope, which
//     has no `message` and no guarantee its code is on the table;
//   - an httpjson.Code(...) conversion is a code minted outside the table,
//     which [httpjson.Code.Message] answers with nothing;
//   - http.StatusServiceUnavailable named anywhere is a 503 written without
//     httpjson.Unavailable / UnavailableWith, the only writers that set the
//     header and the envelope together.
func TestTheSettingsSurfacesRefuseInTheEnvelope(t *testing.T) {
	t.Parallel()
	surfaces := []string{"configapi", "secretsapi", "chartapi", "setupapi", "workapi"}
	walked := 0
	for _, dir := range surfaces {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			path := filepath.Join(dir, name)
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", path, err)
			}
			walked++
			ast.Inspect(file, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.KeyValueExpr:
					if lit, ok := node.Key.(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if key, _ := strconv.Unquote(lit.Value); key == "error" {
							t.Errorf("%s: a hand-built refusal body — answer through "+
								"httpjson.Fail/FailWith/FailWithFields so it carries "+
								"the code's sentence", fset.Position(node.Pos()))
						}
					}
				case *ast.CallExpr:
					if isSelector(node.Fun, "httpjson", "Code") {
						t.Errorf("%s: a code minted outside internal/api/httpjson's "+
							"table, which carries no sentence — declare it there",
							fset.Position(node.Pos()))
					}
				case *ast.SelectorExpr:
					if isSelector(node, "http", "StatusServiceUnavailable") {
						t.Errorf("%s: a 503 written without httpjson.Unavailable "+
							"or UnavailableWith, which set the Retry-After and the "+
							"envelope together", fset.Position(node.Pos()))
					}
				}
				return true
			})
		}
	}
	// THE CONTROL: a walk that read nothing would pass on every tree.
	if walked < len(surfaces) {
		t.Fatalf("walked %d source files across %d surfaces, so the walk "+
			"certifies nothing", walked, len(surfaces))
	}
}

// isSelector reports whether expr is pkg.name.
func isSelector(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}
