package tools_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
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
// internal/ and refuses a failed tool Result that carries no cause — the one
// value its class is read out of ([tools.RefusalOf]).
//
// A WALK OVER THE SOURCE: the failure is a NEW hand-built
// `tools.Result{Failed: true}`, which gives a person's surface no class.
// Checked: a literal that carries a refusal out of a tool call (tool Result,
// the surface's recorded Call, the toolloop's ToolResult) and sets Failed must
// set a non-nil Cause; every `refused(code, msg)` call must name a class. A
// bare `failed(msg)` is the argument refusal and passes. The walk sees that a
// cause is there, not that `err` wraps a [tools.Refusal]: each tool's suite
// holds that.
func TestEveryFirstPartyRefusalIsClassified(t *testing.T) {
	t.Parallel()
	// THROUGH sourcetree, from the module's own internal/: a nested
	// checkout under it is another commit's copy of every file here, and
	// its refusals are not this build's.
	scan := refusals(t, filepath.Join(sourcetree.Root(t), "internal"))
	for _, offence := range scan.offences {
		t.Error(offence)
	}
	// NOT VACUOUS: the builtins, the delegate tool, the discovery
	// meta-tools and the structured submission tool each build at least
	// one classified literal, so a walk that stopped recognising the type
	// would find none rather than pass on zero.
	if scan.classified < 4 {
		t.Fatalf("the walk found %d classified failed Results, want at least "+
			"4 — it no longer recognises the type it is checking", scan.classified)
	}
}

// failedField, causeField and refusedHelper are the names the gate judges a
// refusal by: a carrier literal's two keys, and a package's
// `refused(code, msg)` helper.
const (
	failedField   = "Failed"
	causeField    = "Cause"
	refusedHelper = "refused"
)

// refusalScan is what one walk for refusals found.
type refusalScan struct {
	classified int
	offences   []string
}

// refusals walks every non-test file under root for failed results that
// carry no class.
//
// ONLY A FILE THAT CAN HOLD ONE IS PARSED: a failed carrier sets the keyed
// field Failed, and the helper is called by name, so a file whose bytes spell
// neither identifier holds nothing this judges (sourcetree.Identifiers).
// Parsing every file under internal/ to find the few dozen that do was five
// seconds under the race detector.
func refusals(t *testing.T, root string) refusalScan {
	t.Helper()
	names := sourcetree.MustIdentifiers(failedField, refusedHelper)
	fset := token.NewFileSet()
	var scan refusalScan
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
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
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !names.In(src) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
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
				failed, cause := fieldOf(node, failedField), fieldOf(node, causeField)
				if failed == nil {
					return true
				}
				if _, exempt := unclassifiedByDesign[rel]; exempt {
					return true
				}
				switch {
				case cause == nil:
					scan.offences = append(scan.offences, fmt.Sprintf("%s: a failed tool "+
						"Result with no Cause — state its class (tools.Refusal*, or "+
						"tools.Classify over the error that decided it) so a surface "+
						"that is not a model can act on it", fset.Position(node.Pos())))
				case isNil(cause):
					scan.offences = append(scan.offences, fmt.Sprintf("%s: a failed tool "+
						"Result whose Cause is nil, which reads as unclassified",
						fset.Position(node.Pos())))
				default:
					scan.classified++
				}
			case *ast.CallExpr:
				if name, ok := node.Fun.(*ast.Ident); ok && name.Name == refusedHelper &&
					len(node.Args) > 0 && isEmptyString(node.Args[0]) {
					scan.offences = append(scan.offences, fmt.Sprintf("%s: refused(\"\", …) "+
						"names no class", fset.Position(node.Pos())))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return scan
}

// THE WALK AND THE MATCHER, ON A TREE WHOSE VERDICT IS KNOWN: a failed result
// with no cause, one whose cause is nil, one classified, the helper handed no
// class in a file that names no Failed field — so each identifier is shown to
// admit a file on its own — the third-party bridge that is exempt by design,
// and a file naming neither that is not even Go, which must never be parsed.
func TestTheRefusalWalkFindsAnUnclassifiedFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"builtin/tool.go": "package builtin\n\nfunc f() {\n" +
			"\t_ = tools.Result{Output: \"no\", Failed: true}\n" +
			"\t_ = tools.Result{Failed: true, Cause: nil}\n" +
			"\t_ = tools.Result{Failed: true, Cause: tools.RefusalInvalid}\n}\n",
		"builtin/helper.go": "package builtin\n\nfunc g() { _ = refused(\"\", \"no class\") }\n",
		"mcp/tool.go":       "package mcp\n\nvar _ = Result{Failed: true}\n",
		"other/x.go":        "package other\n\nthis is not Go and names nothing judged\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	scan := refusals(t, root)
	var where []string
	for _, offence := range scan.offences {
		file, _, _ := strings.Cut(offence, ":")
		where = append(where, filepath.ToSlash(file))
	}
	slices.Sort(where)
	want := []string{
		filepath.ToSlash(filepath.Join(root, "builtin/helper.go")),
		filepath.ToSlash(filepath.Join(root, "builtin/tool.go")),
		filepath.ToSlash(filepath.Join(root, "builtin/tool.go")),
	}
	if scan.classified != 1 || !slices.Equal(where, want) {
		t.Errorf("classified %d, offences %q; want 1, two in builtin/tool.go and "+
			"one in builtin/helper.go", scan.classified, scan.offences)
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

// isNil reports whether an expression is the bare identifier nil.
func isNil(expr ast.Expr) bool {
	id, ok := expr.(*ast.Ident)
	return ok && id.Name == "nil"
}

func isEmptyString(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}
