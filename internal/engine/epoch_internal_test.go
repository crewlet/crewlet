package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"
)

// EVERY VENDOR WIRING THE COMPANY DERIVES IS REBUILT ON AN APPLY.
//
// A vendor's parser is built once from the applied company and then held. Two
// halves of what it holds move when a revision lands: the ORG CHART it routes
// by — a lead map, a project or space identity, a seat's own credential — and
// the CREDENTIAL it reads with. A node that kept its boot-time wiring routes
// the new revision's activity by the previous company's org chart, and reads
// with a credential that may have been rotated out from under it.
//
// Both failures are SILENT. A lead-fallback notification looks identical
// whoever it reached, and a 401 on an enrichment read degrades the thread it
// was enriching rather than raising anything.
//
// This is not hypothetical and it is not a typo: `reconcileConfluence` did not
// exist at all. The knowledge base shipped with a lead map built at boot and
// never rebuilt, past four vendors that each had the edge, because nothing
// connected the list of reconcilers to the list of calls. So the two lists are
// derived from the source rather than written down — a reconciler added to the
// package and not to the apply path works perfectly at boot and is discoverable
// only by reading both files.
func TestEveryVendorReconcilerRunsOnApply(t *testing.T) {
	t.Parallel()

	defined := reconcilerMethods(t)
	if len(defined) == 0 {
		t.Fatal("found no reconcile* methods, so this test certifies nothing")
	}
	called := reconcilersCalledBy(t, "Apply")

	for _, name := range defined {
		if !slices.Contains(called, name) {
			t.Errorf("(*Engine).%s exists and the apply path never calls it, so "+
				"that vendor keeps its boot-time wiring for the life of the "+
				"process — routing by an org chart that is no longer running, "+
				"and reading with a credential the revision may have rotated",
				name)
		}
	}
	// The other direction cannot happen — a call to a method that does not
	// exist does not compile — so there is nothing to assert for it.
}

// reconcilerMethods are the `reconcile*` methods declared on *Engine, sorted.
func reconcilerMethods(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, file := range enginePackage(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || !strings.HasPrefix(fn.Name.Name, "reconcile") {
				continue
			}
			if receiverIsEngine(fn) {
				out = append(out, fn.Name.Name)
			}
		}
	}
	slices.Sort(out)
	return out
}

// reconcilersCalledBy are the `reconcile*` methods called from one function,
// however deeply nested in its body.
func reconcilersCalledBy(t *testing.T, function string) []string {
	t.Helper()
	var out []string
	for _, file := range enginePackage(t) {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Name.Name != function || fn.Recv == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				// e.reconcileX(...), and the helper an Apply might
				// delegate to is followed by name below.
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok &&
					strings.HasPrefix(sel.Sel.Name, "reconcile") {
					out = append(out, sel.Sel.Name)
				}
				return true
			})
			// THE APPLY IS SPLIT ACROSS TWO FUNCTIONS: the exported
			// entry point validates and stores, and a helper does the
			// swap. Following one level of delegation is what keeps
			// this test reading the whole path rather than whichever
			// half the calls happen to live in today.
			for _, name := range delegatesOf(fn) {
				out = append(out, reconcilersCalledBy(t, name)...)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// delegatesOf names the package's own methods a function calls, so the walk
// can follow the apply into whichever helper does the swap.
func delegatesOf(fn *ast.FuncDecl) []string {
	var out []string
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		// e.something(...), where e is the receiver rather than a field.
		if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "e" &&
			!strings.HasPrefix(sel.Sel.Name, "reconcile") {
			out = append(out, sel.Sel.Name)
		}
		return true
	})
	return out
}

func receiverIsEngine(fn *ast.FuncDecl) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "Engine"
}

// enginePackage parses this package's own non-test source.
//
// The dispatch is read out of the SOURCE rather than exercised, because a
// method that is never called cannot be observed at runtime — which is the
// same reason the drift is invisible in the first place.
//
// The directory is walked by hand rather than through go/parser's own
// ParseDir, which is deprecated for a reason that applies here: it ignores
// build tags, so a file excluded from the build would still be read and a
// reconciler behind one would be demanded by a test the compiler disagrees
// with. This package has no tagged files, and the walk is the same length
// either way.
func enginePackage(t *testing.T) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the engine package: %v", err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatal("the engine package parsed to nothing, so this test certifies nothing")
	}
	return files
}
