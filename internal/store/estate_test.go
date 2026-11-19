package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"
)

// NOTHING ISSUES A STATEMENT ON THE REPLICATED ESTATE'S POOL.
//
// The replicated estate's database ([store.ReplicatedHandle.DB],
// [store.DB.ReplicatedDB]) answers [store.ErrNoEstate] when the estate is not
// open (no `data`, an adoption between rename and reopen, after Close); `SQL()`
// on the answer is a NIL `*sql.DB` dereferenced inside database/sql, taking the
// process down. Callers use Read or Tx on the handle.
// STRUCTURAL, because a behavioural test would have to close the estate at the
// instant a loop issues a statement: per top-level declaration the walk records
// each name bound to `.DB()`, `.ReplicatedDB()` or `.OpenReplicated(…)` and
// reports its `.SQL()` in the same declaration, closures included. Use in
// another declaration is beyond it; a shadowed name is reported, the safe side.
func TestNothingIssuesAStatementOnTheReplicatedPool(t *testing.T) {
	t.Parallel()

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN. A guard asserting an
	// absence passes identically when the thing is absent and when the
	// guard has gone inert.
	for _, positive := range []string{
		"db, err := h.DB()\nrows, err := db.SQL().QueryContext(ctx, q)",
		"replicated, err := node.ReplicatedDB()\nconn := replicated.SQL()",
		"replicated, _ := node.OpenReplicated(ctx, logs)\ngo func() { _ = replicated.SQL() }()",
		"var live, err = s.deps.Estate.DB()\n_ = live.SQL()",
		"x.replicated, err = node.ReplicatedDB()\n_ = x.replicated.SQL()",
	} {
		if !reachesTheReplicatedPool(t, positive) {
			t.Errorf("control: %q reaches the replicated estate's pool and the "+
				"matcher did not flag it", positive)
		}
	}
	for _, negative := range []string{
		"db, err := h.DB()\nerr = db.Read(ctx, fn)",
		// The NODE's own file, which the node's Open owns for its life.
		"rows, err := node.SQL().QueryContext(ctx, q)",
		"db, err := h.DB()\n_ = other.SQL()",
		// A test helper's two-argument lookup is not the store's.
		"db := c.ReplicatedDB(t, h)\n_ = db.SQL()",
	} {
		if reachesTheReplicatedPool(t, negative) {
			t.Errorf("control: %q was flagged, so this guard fails on the "+
				"correct shape", negative)
		}
	}

	files := moduleTree(t)
	var found []string
	for _, f := range files {
		for _, decl := range f.file.Decls {
			for _, call := range poolsTaken(decl) {
				found = append(found, f.rel+":"+
					strconv.Itoa(f.fset.Position(call.Pos()).Line))
			}
		}
	}
	slices.Sort(found)
	for _, site := range found {
		t.Errorf("%s issues a statement on the replicated estate's pool, which "+
			"is NIL whenever the estate is not open — an adoption's rename, a "+
			"node without `data`, or a shutdown makes it so — and the "+
			"statement panics inside database/sql rather than answering "+
			"ErrNoEstate. Go through Read or Tx on the handle", site)
	}
	t.Logf("parsed %d files", len(files))
}

// poolsTaken is every `.SQL()` in one declaration on a name the same
// declaration bound to the replicated estate's database.
func poolsTaken(decl ast.Node) []*ast.CallExpr {
	bound := map[string]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		var lhs, rhs []ast.Expr
		switch v := n.(type) {
		case *ast.AssignStmt:
			lhs, rhs = v.Lhs, v.Rhs
		case *ast.ValueSpec:
			for _, name := range v.Names {
				lhs = append(lhs, name)
			}
			rhs = v.Values
		default:
			return true
		}
		if len(rhs) != 1 || len(lhs) == 0 || !answersReplicatedDB(rhs[0]) {
			return true
		}
		if key, ok := exprKey(lhs[0]); ok {
			bound[key] = true
		}
		return true
	})
	var out []*ast.CallExpr
	ast.Inspect(decl, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "SQL" {
			return true
		}
		if key, ok := exprKey(sel.X); ok && bound[key] {
			out = append(out, call)
		}
		return true
	})
	return out
}

// answersReplicatedDB reports whether an expression is a call that answers the
// replicated estate's own database.
func answersReplicatedDB(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "DB", "ReplicatedDB":
		return len(call.Args) == 0
	case "OpenReplicated":
		return true
	}
	return false
}

// exprKey renders a name or a chain of field selections as the text it is
// written as, which is what two mentions of one variable share.
func exprKey(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name, v.Name != "_"
	case *ast.SelectorExpr:
		x, ok := exprKey(v.X)
		return x + "." + v.Sel.Name, ok
	}
	return "", false
}

// reachesTheReplicatedPool parses one snippet as a function body and reports
// whether the walk flags it.
func reachesTheReplicatedPool(t *testing.T, body string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "control.go",
		"package p\nfunc f() {\n"+body+"\n}\n", parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("control %q does not parse: %v", body, err)
	}
	return len(poolsTaken(file.Decls[0])) > 0
}
