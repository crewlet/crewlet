package auth_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// NO CALL SITE DISCARDS THE UNKNOWN ARM, asserted against the SOURCE because
// nothing a running handler does can show it.
//
// [iam.From] answers three-valued, and `p, _ := iam.From(ctx)` compiles but
// is wrong in one case: the zero principal covers both "nobody presented a
// credential" (401, about the caller) and "this node could not tell" (503,
// about the node). Answered as the first, everybody with a good credential is
// told theirs is invalid while the identity estate is unreachable.
// [auth.Caller] writes the refusal and returns nothing to drop; this walk, not
// a convention, stops the two-valued habit coming back beside it.
func TestNoCallSiteDiscardsTheUnknownArm(t *testing.T) {
	t.Parallel()
	// EVERY PACKAGE THAT READS A PRINCIPAL OFF A REQUEST, named rather
	// than discovered: a walk that globbed the tree would quietly stop
	// covering a package somebody moved, and the point of this gate is
	// that it cannot stop covering anything quietly.
	roots := []string{
		"internal/api", "internal/api/assistantapi", "internal/api/auth", "internal/api/configapi",
		"internal/api/operator", "internal/api/queries", "internal/api/secretsapi",
		"internal/api/setupapi", "internal/api/stream", "internal/e2e",
	}
	module := sourcetree.Root(t)
	walked, found := 0, 0
	for _, root := range roots {
		files, err := filepath.Glob(filepath.Join(module, filepath.FromSlash(root), "*.go"))
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
