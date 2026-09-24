package tools_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// unclassifiedByDesign is every file allowed to build a failed Result with no
// class, and why.
//
// ONE ENTRY, and it is the definition of "first-party": the MCP bridge wraps a
// THIRD-PARTY server's failure, whose prose the engine did not write and could
// only classify by guessing. See mcp.Refusal.
var unclassifiedByDesign = map[string]string{
	"mcp/tool.go": "a third-party MCP server's failure is unclassified by design",
}

// TestEveryFirstPartyRefusalIsClassified walks every non-test file under
// internal/ and refuses a failed tool Result that carries no class.
//
// A WALK OVER THE SOURCE rather than a list of tools, because the failure it
// guards against is a NEW refusal: a tool added next month that builds
// `tools.Result{Output: …, Failed: true}` by hand compiles, runs, reads fine to
// the model — and answers a person's write surface with no class at all, which
// that surface can only render as a status it made up. Nothing but a gate
// notices, because every reader that is not the dashboard is a model reading
// the sentence.
//
// Two shapes are checked: a literal of any type that carries a refusal out of
// a tool call — the tool Result, the surface's recorded Call and the
// toolloop's ToolResult — that sets Failed must set Refusal, to something other
// than the empty string; and every call of a package's `refused(code, msg)`
// helper must name a class rather than "". A package's bare `failed(msg)` is
// the argument refusal by definition and passes.
//
// THE SURFACE'S OWN TYPES ARE IN THE WALK because the surface refuses calls
// no tool ever sees — an unknown name, one not offered here, a guard's
// refusal — and a caller dispatching through it reads those as its answer.
// Checking only the tool Result left exactly those unclassified.
func TestEveryFirstPartyRefusalIsClassified(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	classified := 0
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		pkg := file.Name.Name
		ast.Inspect(file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if !isRefusalCarrier(node.Type, pkg) {
					return true
				}
				failed, refusal := fieldOf(node, "Failed"), fieldOf(node, "Refusal")
				if failed == nil {
					return true
				}
				if _, exempt := unclassifiedByDesign[rel]; exempt {
					return true
				}
				switch {
				case refusal == nil:
					t.Errorf("%s: a failed tool Result with no Refusal — "+
						"name its class (tools.Refusal*) so a surface that is "+
						"not a model can act on it", fset.Position(node.Pos()))
				case isEmptyString(refusal):
					t.Errorf("%s: a failed tool Result whose Refusal is \"\", "+
						"which reads as unclassified", fset.Position(node.Pos()))
				default:
					classified++
				}
			case *ast.CallExpr:
				if name, ok := node.Fun.(*ast.Ident); ok && name.Name == "refused" &&
					len(node.Args) > 0 && isEmptyString(node.Args[0]) {
					t.Errorf("%s: refused(\"\", …) names no class",
						fset.Position(node.Pos()))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// NOT VACUOUS: the builtins, the delegate tool, the discovery
	// meta-tools and the structured submission tool each build at least
	// one classified literal, so a walk that stopped recognising the type
	// would find none rather than pass on zero.
	if classified < 4 {
		t.Fatalf("the walk found %d classified failed Results, want at least "+
			"4 — it no longer recognises the type it is checking", classified)
	}
}

// refusalCarriers is every type a failed call leaves a frame in, by the
// package that declares it: the tool Result (spelled through either package
// that exports it), the surface's recorded Call and the loop's ToolResult.
var refusalCarriers = map[string][]string{
	"tools":    {"Result", "Call"},
	"mcp":      {"Result"},
	"toolloop": {"ToolResult"},
}

// isRefusalCarrier reports whether a composite literal's type is one of
// [refusalCarriers], qualified from outside its package or bare inside it.
func isRefusalCarrier(expr ast.Expr, inPkg string) bool {
	switch typ := expr.(type) {
	case *ast.SelectorExpr:
		pkg, ok := typ.X.(*ast.Ident)
		return ok && slices.Contains(refusalCarriers[pkg.Name], typ.Sel.Name)
	case *ast.Ident:
		return slices.Contains(refusalCarriers[inPkg], typ.Name)
	}
	return false
}

// fieldOf is a keyed field's value in a composite literal, or nil.
func fieldOf(lit *ast.CompositeLit, name string) ast.Expr {
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if key, ok := kv.Key.(*ast.Ident); ok && key.Name == name {
			return kv.Value
		}
	}
	return nil
}

func isEmptyString(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}
