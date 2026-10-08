package auxspend_test

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
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY AUXILIARY CALL STATES WHAT IT IS FOR, IN THE SOURCE.
//
// The seam refuses an attribution with no valid purpose at run time, which
// stops a record filed under nothing — and turns a forgotten purpose into a
// model call that never happens, on a path a test may never drive. So the
// rule is held where it is written: every call of the seam's method, anywhere
// in the tree, hands it an attribution whose PURPOSE is a constant a reader
// can find — `types.Aux…`, or `types.AuxCondense(…)` for a compaction — written
// at the call, through `.For(…)` or a learning turn's `.Reflecting(…)`, or as
// the `Purpose` of a `Use` literal.
//
// Two shapes pass a purpose through rather than writing one, and each is held
// to the same rule one frame further out: a seam that FORWARDS the use it was
// handed (a method named Auxiliary passing its own parameter on), and a helper
// named in [purposeForwarders], whose every caller must pass a constant in the
// purpose's position.
func TestEveryAuxiliaryCallStatesItsPurpose(t *testing.T) {
	t.Parallel()
	files := seamFiles(t)
	calls, unstated := auxiliaryCalls(files)
	for _, where := range unstated {
		t.Errorf("%s resolves the auxiliary model without stating a "+
			"purpose constant — write types.Aux… (or types.AuxCondense) at "+
			"the call, so the spend is filed under the line of code that made it", where)
	}
	if calls < 10 {
		// THE GATE COUNTS WHAT IT READ: a walk that found no calls would
		// pass having checked nothing.
		t.Fatalf("found %d calls of the auxiliary seam, want the dozen-odd the tree makes", calls)
	}

	// The forwarders' callers, one frame out.
	for name, position := range purposeForwarders {
		seen := 0
		for _, f := range files {
			ast.Inspect(f.file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || calledName(call) != name || len(call.Args) <= position {
					return true
				}
				seen++
				if !isPurpose(call.Args[position]) {
					t.Errorf("%s: %s forwards its argument %d as an auxiliary call's "+
						"purpose, and this call passes no purpose constant there",
						f.fset.Position(call.Pos()), name, position)
				}
				return true
			})
		}
		if seen == 0 {
			t.Errorf("%s is declared a purpose forwarder and nothing calls it: retire the entry", name)
		}
	}
}

// NOTHING RESOLVES THE AUXILIARY CHAIN BESIDE THE SEAM.
//
// A completion made on a model resolved straight off the phase registry is
// spend that reaches neither the counter nor the record — the leak the seam
// was built to close, back in the most natural spelling there is. The seam
// itself resolves it, and one probe asks whether ANY seat has an auxiliary
// model without making a call on it; nothing else may.
func TestNothingResolvesTheAuxiliaryChainBesideTheSeam(t *testing.T) {
	t.Parallel()
	allowed := map[string]string{
		"internal/engine/auxiliary.go:Auxiliary":          "the seam",
		"internal/engine/learning.go:anySeatHasAuxiliary": "a probe that makes no call",
	}
	found := map[string]bool{}
	for _, f := range seamFiles(t) {
		for _, fn := range funcs(f.file) {
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Head" || len(call.Args) != 2 || !isSelector(call.Args[1], "phase", "Auxiliary") {
					return true
				}
				site := f.rel + ":" + fn.Name.Name
				if _, ok := allowed[site]; ok {
					found[site] = true
					return true
				}
				t.Errorf("%s: %s resolves the auxiliary chain off the phase registry — a "+
					"completion made on it reaches no counter and no record. Resolve it "+
					"through the auxiliary seam, stating the call's attribution",
					f.fset.Position(call.Pos()), fn.Name.Name)
				return true
			})
		}
	}
	for site, why := range allowed {
		if !found[site] {
			t.Errorf("%s (%s) no longer resolves the auxiliary chain: retire the entry", site, why)
		}
	}
}

// auxiliaryCalls is how many calls of the seam's method files make, and where
// each that states no purpose is, with the function it is in.
func auxiliaryCalls(files []parsed) (int, []string) {
	calls := 0
	var unstated []string
	for _, f := range files {
		for _, fn := range funcs(f.file) {
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Auxiliary" || len(call.Args) != 2 {
					return true
				}
				calls++
				if !statesPurpose(call.Args[1], fn) {
					unstated = append(unstated, f.fset.Position(call.Pos()).String()+": "+fn.Name.Name)
				}
				return true
			})
		}
	}
	return calls, unstated
}

// purposeForwarders are the helpers that take a call's purpose as an argument
// and hand it to the seam, by name and the purpose's argument position.
var purposeForwarders = map[string]int{
	// prefetch's one auxiliary call, for its three callers.
	"auxCall": 2,
}

// statesPurpose reports whether a use argument states its purpose at the call.
func statesPurpose(arg ast.Expr, fn *ast.FuncDecl) bool {
	switch a := arg.(type) {
	case *ast.CallExpr:
		sel, ok := a.Fun.(*ast.SelectorExpr)
		if !ok || len(a.Args) != 1 {
			return false
		}
		switch sel.Sel.Name {
		case "For", "Reflecting":
			return isPurpose(a.Args[0]) || forwardedPurpose(a.Args[0], fn)
		}
		return false
	case *ast.CompositeLit:
		for _, elt := range a.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "Purpose" {
				return isPurpose(kv.Value)
			}
		}
		return false
	case *ast.Ident:
		// A SEAM FORWARDING the use it was handed, whole: its own callers
		// are the calls this gate reads.
		return fn.Name.Name == "Auxiliary" && isParam(a.Name, fn)
	}
	return false
}

// forwardedPurpose is a purpose handed in by a declared forwarder's caller.
func forwardedPurpose(arg ast.Expr, fn *ast.FuncDecl) bool {
	ident, ok := arg.(*ast.Ident)
	if !ok {
		return false
	}
	_, declared := purposeForwarders[fn.Name.Name]
	return declared && isParam(ident.Name, fn)
}

// isPurpose is a purpose constant: types.Aux… that is not a stage, or a
// compaction's types.AuxCondense(…).
func isPurpose(e ast.Expr) bool {
	switch v := e.(type) {
	case *ast.SelectorExpr:
		pkg, ok := v.X.(*ast.Ident)
		return ok && pkg.Name == "types" && strings.HasPrefix(v.Sel.Name, "Aux") &&
			!strings.HasPrefix(v.Sel.Name, "AuxStage")
	case *ast.CallExpr:
		return isSelector(v.Fun, "types", "AuxCondense")
	}
	return false
}

func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

func isParam(name string, fn *ast.FuncDecl) bool {
	for _, field := range fn.Type.Params.List {
		for _, n := range field.Names {
			if n.Name == name {
				return true
			}
		}
	}
	return false
}

func calledName(call *ast.CallExpr) string {
	switch f := call.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

func funcs(f *ast.File) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	for _, decl := range f.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
			out = append(out, fn)
		}
	}
	return out
}

type parsed struct {
	rel  string
	fset *token.FileSet
	file *ast.File
}

// seamFiles is what both gates read: the module's files that spell a name
// either matcher compares, parsed ONCE per test binary. The source is the
// build's, so neither gate can see it change, and both only read the trees.
func seamFiles(t *testing.T) []parsed {
	t.Helper()
	files, err := parsedSeamFiles()
	if err != nil {
		t.Fatal(err)
	}
	return files
}

var parsedSeamFiles = sync.OnceValues(func() ([]parsed, error) {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return nil, err
	}
	return parseTree(root)
})

// spellsAuxiliary is what a file must spell to hold anything either gate
// reads: a call of the seam's method, a registry lookup of `phase.Auxiliary`,
// or a call of a declared forwarder. Each is an identifier in the source, and
// an identifier has no escapes (sourcetree.Identifiers).
func spellsAuxiliary() sourcetree.Identifiers {
	names := []string{"Auxiliary"}
	for name := range purposeForwarders {
		names = append(names, name)
	}
	return sourcetree.MustIdentifiers(names...)
}

// parseTree is every non-test Go file of root's internal/ and cmd/ trees —
// the code that runs, which is the code that spends — that spells a name
// either gate reads. Parsing all eleven hundred files to find the thirty that
// do was most of what the two gates cost, twice over.
func parseTree(root string) ([]parsed, error) {
	needs := spellsAuxiliary()
	var out []parsed
	for _, dir := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if !needs.In(src) {
				return nil
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			out = append(out, parsed{rel: filepath.ToSlash(rel), fset: fset, file: file})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", dir, err)
		}
	}
	return out, nil
}

// THE WALK AND THE MATCHER, ON A TREE WHOSE VERDICT IS KNOWN: a seam call
// that states no purpose, one that does, a forwarder's caller, and a file
// naming neither that is not even Go — which the walk must never parse.
func TestTheAuxiliaryWalkParsesWhatCanCallTheSeam(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"internal/a/a.go": "package a\n\nfunc f(e *E, use auxspend.Use) {\n" +
			"\te.Auxiliary(ctx, use)\n" +
			"\te.Auxiliary(ctx, auxspend.Use{}.For(types.AuxSummary))\n}\n",
		"cmd/c/c.go":      "package main\n\nfunc g() { auxCall(ctx, x, types.AuxSummary) }\n",
		"internal/b/b.go": "package b\n\nthis is not Go and names no seam\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	files, err := parseTree(root)
	if err != nil {
		t.Fatalf("the walk parsed a file that names no seam: %v", err)
	}
	var rels []string
	for _, f := range files {
		rels = append(rels, f.rel)
	}
	slices.Sort(rels)
	if want := []string{"cmd/c/c.go", "internal/a/a.go"}; !slices.Equal(rels, want) {
		t.Errorf("parsed %q, want %q", rels, want)
	}
	calls, unstated := auxiliaryCalls(files)
	if calls != 2 || len(unstated) != 1 || !strings.Contains(unstated[0], "a.go:4") {
		t.Errorf("%d calls, unstated %q; want 2, and the one at a.go:4", calls, unstated)
	}
}
