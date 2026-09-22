package auth_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// NO CALL SITE DISCARDS THE UNKNOWN ARM, asserted against the SOURCE because
// nothing a running handler does can show it.
//
// # The failure this exists for
//
// [iam.From] answers three-valued, and `p, _ := iam.From(ctx)` compiles,
// reads naturally and is wrong in exactly one case: the zero principal it
// hands back covers both "nobody presented a credential" and "this node could
// not tell". The first is a 401 about the caller. The second is a 503 about
// the node — and answered as the first, it tells everybody holding a perfectly
// good credential that theirs is invalid, for as long as the identity estate
// is unreachable, which is precisely how a company gets taught to go and reset
// working passwords during an outage.
//
// Nineteen sites in this tree wrote `operator, _ :=` against the two-valued
// reader this replaced, and every one of them did it for a good reason: the
// prefix guard had already answered. The discarded half was a bool then. It is
// the unknown arm now, and the reason it was safe to discard is gone.
//
// # Why a walk rather than a convention
//
// Because the convention already failed once, at nineteen sites, silently.
// [auth.Caller] is the shape that cannot be got wrong — it writes the refusal
// and returns nothing to drop — and this is what stops the two-valued habit
// coming back beside it.
func TestNoCallSiteDiscardsTheUnknownArm(t *testing.T) {
	t.Parallel()
	// EVERY PACKAGE THAT READS A PRINCIPAL OFF A REQUEST, named rather
	// than discovered: a walk that globbed the tree would quietly stop
	// covering a package somebody moved, and the point of this gate is
	// that it cannot stop covering anything quietly.
	roots := []string{
		"..", "../auth", "../chartapi", "../configapi", "../opsmcp",
		"../queries", "../secretsapi", "../setupapi", "../stream",
		"../../e2e",
	}
	walked, found := 0, 0
	for _, root := range roots {
		files, err := filepath.Glob(filepath.Join(root, "*.go"))
		if err != nil {
			t.Fatalf("glob %s: %v", root, err)
		}
		for _, name := range files {
			fset := token.NewFileSet()
			parsed, err := parser.ParseFile(fset, name, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", name, err)
			}
			walked++
			ast.Inspect(parsed, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || len(assign.Rhs) != 1 || len(assign.Lhs) != 2 {
					return true
				}
				if !isFromCall(assign.Rhs[0]) {
					return true
				}
				found++
				if blank(assign.Lhs[1]) {
					t.Errorf("%s:%d discards iam.From's resolution: "+
						"`unknown` then reads as `anonymous`, and a caller "+
						"this node could not CHECK is told their credential "+
						"is invalid. Use auth.Caller, which has no second "+
						"value to drop",
						name, fset.Position(assign.Pos()).Line)
				}
				return true
			})
		}
	}
	// THE CONTROLS. A walk that parsed nothing, or that found no call to
	// the function it is about, passes every assertion above and protects
	// nothing — which is how a gate reading a path that had moved
	// certified a whole rewrite elsewhere in this tree while reporting a
	// pass.
	if walked < 40 {
		t.Fatalf("walked %d files; the walk is not reading the API surface", walked)
	}
	if found < 3 {
		t.Fatalf("found %d calls to iam.From; the walk is not finding them", found)
	}
}

// isFromCall reports whether an expression is `iam.From(...)`.
func isFromCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "From" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "iam"
}

// blank reports whether an expression is the blank identifier.
func blank(e ast.Expr) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == "_"
}

// AND THE ONE DELIBERATE EXCEPTION IS NAMED, rather than left for a reader to
// find and wonder about.
//
// internal/api/chartapi's `writerFor` reads the principal with the resolution
// discarded, and it is correct there for a reason no walk can see: the guard
// beside it has already refused an unknown one with a 503, so a request
// reaching that function carries an answer. Asking again would be a second
// decision about one request, and the two would drift the day somebody changed
// one of them.
//
// It is in a package this walk covers, so the walk would flag it — which is
// why it does not write `iam.From` at all: it goes through the surface's own
// seam. This case pins that, so the exception cannot quietly become a direct
// call again.
func TestTheChartWriterGoesThroughItsOwnSeam(t *testing.T) {
	t.Parallel()
	source, err := parser.ParseFile(token.NewFileSet(), "../chartapi/write.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ast.Inspect(source, func(n ast.Node) bool {
		expr, ok := n.(ast.Expr)
		if ok && isFromCall(expr) {
			t.Error("chartapi/write.go calls iam.From directly; the surface's " +
				"own Principal seam is what the guard beside it already decided " +
				"against, and two readings of one request eventually disagree")
		}
		return true
	})
	// The control: the file this points at is the one that holds the
	// writer, and a path that moved would otherwise certify nothing.
	if !strings.Contains(renderDecls(source), "writerFor") {
		t.Fatal("chartapi/write.go no longer declares writerFor; this case is " +
			"reading the wrong file")
	}
}

// renderDecls is the names of a file's top-level declarations, for a control
// that has to know it is reading the right file.
func renderDecls(file *ast.File) string {
	var names []string
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok {
			names = append(names, fn.Name.Name)
		}
	}
	return strings.Join(names, " ")
}
