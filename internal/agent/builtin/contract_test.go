package builtin_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tools"
)

// everySchema is every registered builtin's advertised schema, by tool.
func everySchema(t *testing.T) map[string]map[string]any {
	t.Helper()
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(fullDeps(t))); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]any{}
	for _, e := range reg.Snapshot().Entries() {
		out[e.Tool.Name()] = e.Tool.Parameters()
	}
	return out
}

// EVERY BUILTIN'S SCHEMA IS ONE THE SURFACE'S CHECK CAN KEEP (ADR-0059): a
// keyword or a type the check does not implement is a promise to the model
// nothing holds a call to.
//
// Mutation: add `"pattern"` to a property and this names it.
func TestEveryBuiltinSchemaIsOneTheCheckKeeps(t *testing.T) {
	t.Parallel()
	schemas := everySchema(t)
	if len(schemas) < 40 {
		t.Fatalf("%d builtins registered, which is not the set", len(schemas))
	}
	for name, schema := range schemas {
		if bad := tools.UnsupportedSchema(schema); len(bad) > 0 {
			t.Errorf("%s advertises what the check cannot keep: %v", name, bad)
		}
	}
}

// declaredTypes is every type each argument name is declared with, at any
// depth, across every builtin's schema.
func declaredTypes(schemas map[string]map[string]any) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	var walk func(s map[string]any)
	walk = func(s map[string]any) {
		props, _ := s["properties"].(map[string]any)
		for key, raw := range props {
			sub, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if out[key] == nil {
				out[key] = map[string]bool{}
			}
			switch typ := sub["type"].(type) {
			case string:
				out[key][typ] = true
			case []any:
				for _, x := range typ {
					if s, ok := x.(string); ok {
						out[key][s] = true
					}
				}
			case nil:
				out[key]["any"] = true
			}
			walk(sub)
		}
		if items, ok := s["items"].(map[string]any); ok {
			walk(items)
		}
		if extra, ok := s["additionalProperties"].(map[string]any); ok {
			walk(extra)
		}
	}
	for _, s := range schemas {
		walk(s)
	}
	return out
}

// THE ADVERTISED TYPE IS THE TYPE THE TOOL READS (ADR-0059): an argument no
// schema declares as a string is never read as free text (argString, or a
// `.(string)` assertion) except to quote it back in a refusal. A field
// advertised as a number, a boolean or a list but read as text is one the
// surface's check would refuse a value of that the tool reads, which is the
// drift this holds the tree to. Read with any helper of argval's, a value
// is read by the same rule the check holds it to.
//
// Mutation: read `limit` with argString in a tool and this names the call.
func TestNoArgumentIsReadAsTextAgainstItsType(t *testing.T) {
	t.Parallel()
	declared := declaredTypes(everySchema(t))
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		var quoting []ast.Node // the fmt calls being walked, whose arguments quote
		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				if len(quoting) > 0 {
					quoting = quoting[:len(quoting)-1]
				}
				return true
			}
			quoting = append(quoting, n)
			key, how := textRead(n)
			if key == "" {
				return true
			}
			types, known := declared[key]
			if !known || types["string"] || types["any"] || inFmt(quoting) {
				return true
			}
			t.Errorf("%s: %s reads %q as text, and every schema declares it %v",
				fset.Position(n.Pos()), how, key, slices.Sorted(maps.Keys(types)))
			return true
		})
	}
}

// textRead is the argument a node reads as text, and how: argString(x, "k")
// or x["k"].(string).
func textRead(n ast.Node) (string, string) {
	switch n := n.(type) {
	case *ast.CallExpr:
		if id, ok := n.Fun.(*ast.Ident); ok && id.Name == "argString" && len(n.Args) == 2 {
			if lit, ok := n.Args[1].(*ast.BasicLit); ok && lit.Kind == token.STRING {
				key, _ := strconv.Unquote(lit.Value)
				return key, "argString"
			}
		}
	case *ast.TypeAssertExpr:
		idx, ok := n.X.(*ast.IndexExpr)
		if typ, isIdent := n.Type.(*ast.Ident); !ok || !isIdent || typ.Name != "string" {
			return "", ""
		}
		if lit, ok := idx.Index.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			key, _ := strconv.Unquote(lit.Value)
			return key, "a .(string) assertion"
		}
	}
	return "", ""
}

// inFmt reports whether the node being walked is an argument of a fmt call:
// a value quoted back in a sentence, not read.
func inFmt(stack []ast.Node) bool {
	for _, n := range stack[:len(stack)-1] {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "fmt" {
				return true
			}
		}
	}
	return false
}
