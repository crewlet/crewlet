package store_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// NOTHING ISSUES A STATEMENT ON A PARTITION'S POOL.
//
// A partition's database is reached for the length of one operation —
// [store.PartitionHandle.DB], [store.DB.PartitionDB] — and it answers
// [store.ErrNoEstate] rather than a database whenever the partition is not
// open: a partition this node does not hold, one an adoption holds closed
// between its rename and its reopen, and every one after the node's Close. A
// caller that goes on to `SQL()` on what it was answered has taken the pool
// itself, and on the nil a failed lookup leaves behind that is a NIL
// `*sql.DB`: the statement does not answer an error, it dereferences nil three
// frames inside database/sql and takes the process down.
//
// Six call sites had that shape when every node held one replicated file, and
// the one that fired did so in the trim's own tick, which runs on a timer
// against an estate a shutdown is closing:
//
//	database/sql.(*DB).conn(0x0, …)
//	tracker.Evictions(…)
//	engine.(*retention).tombstones(…)
//
// Every one of them now goes through Read or Tx on the handle, which answer
// ErrNoEstate. A caller in flight then reaches a closed partition honestly, and
// every one of these readers already had a branch for an unreadable one.
//
// A STRUCTURAL TEST, because the defect is structural: a behavioural one would
// have to close a partition underneath a running loop at the instant it issues
// a statement, which is a race a test can only lose reliably.
//
// # What it follows, and how far
//
// A partition's database is answered alongside an error, so it can never be
// chained into `.SQL()`: it is always bound to a name first. The walk records,
// per top-level declaration, every name bound to what `.DB()`,
// `.PartitionDB(…)` or `.OpenPartition(…)` answered, and reports that name's
// `.SQL()` anywhere in the same declaration — a closure inside it included,
// which is where a goroutine outliving the check would take it. A name bound in
// one declaration and used in another is beyond it, and a shadowed one is
// reported: the safe direction, for the reason the applier gate gives.
func TestNothingIssuesAStatementOnAPartitionsPool(t *testing.T) {
	t.Parallel()

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN. A guard asserting an
	// absence passes identically when the thing is absent and when the
	// guard has gone inert.
	for _, positive := range []string{
		"db, err := h.DB()\nrows, err := db.SQL().QueryContext(ctx, q)",
		"part, err := node.PartitionDB(name)\nconn := part.SQL()",
		"part, _ := node.OpenPartition(ctx, f)\ngo func() { _ = part.SQL() }()",
		"var live, err = s.deps.Partition.DB()\n_ = live.SQL()",
		"x.part, err = node.PartitionDB(name)\n_ = x.part.SQL()",
	} {
		if !reachesAPartitionsPool(t, positive) {
			t.Errorf("control: %q reaches a partition's pool and the matcher "+
				"did not flag it", positive)
		}
	}
	for _, negative := range []string{
		"db, err := h.DB()\nerr = db.Read(ctx, fn)",
		// The NODE's own file, which the node's Open owns for its life.
		"rows, err := node.SQL().QueryContext(ctx, q)",
		"db, err := h.DB()\n_ = other.SQL()",
	} {
		if reachesAPartitionsPool(t, negative) {
			t.Errorf("control: %q was flagged, so this guard fails on the "+
				"correct shape", negative)
		}
	}

	root := sourcetree.Root(t)
	files := parseTree(t, root, "internal", "cmd")
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
		t.Errorf("%s issues a statement on a partition's pool, which is NIL "+
			"whenever the partition is not open — an adoption's rename, a "+
			"partition this node does not hold, or a shutdown makes it so — "+
			"and the statement panics inside database/sql rather than "+
			"answering ErrNoEstate. Go through Read or Tx on the handle", site)
	}
	t.Logf("parsed %d files", len(files))
}

// poolsTaken is every `.SQL()` in one declaration on a name the same
// declaration bound to a partition's database.
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
		if len(rhs) != 1 || len(lhs) == 0 || !answersPartitionDB(rhs[0]) {
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

// answersPartitionDB reports whether an expression is a call that answers a
// partition's own database.
func answersPartitionDB(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "DB":
		return len(call.Args) == 0
	case "PartitionDB":
		return len(call.Args) == 1
	case "OpenPartition":
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

// reachesAPartitionsPool parses one snippet as a function body and reports
// whether the walk flags it.
func reachesAPartitionsPool(t *testing.T, body string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "control.go",
		"package p\nfunc f() {\n"+body+"\n}\n", parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("control %q does not parse: %v", body, err)
	}
	return len(poolsTaken(file.Decls[0])) > 0
}
