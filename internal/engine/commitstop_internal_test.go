package engine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// THE COMMITTER STOPS BEFORE THE ESTATE STOPS ANSWERING.
//
// Withdrawing the estate subject cancels every answer in flight before it
// waits for them — the queue's contract for every Serve — and a proposal still
// queued in the committer is one of them: answered with its own cancellation,
// which the router hands back to the receiving node as the push's FINAL
// answer, so a person's push fails with `context canceled`, possibly after its
// batch landed. A committer stopped first answers every queued proposal "not
// serving" — the runner-up decides it under the same push id — and finishes
// the batch in flight with its real outcome before anything is cancelled. (A
// withdrawn subject costs no wait either way: an ask finds no responder at
// once.) The teardown is one function read by nobody twice, so its order is
// held here, from the source: the behavioural form, a peer's queued proposal
// decided by the runner-up during a stop, needs a fleet.
func TestTheCommitterStopsBeforeTheEstateStopsAnswering(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "run.go", nil, 0)
	if err != nil {
		t.Fatalf("parse run.go: %v", err)
	}
	var teardown *ast.FuncDecl
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == "teardown" && fn.Recv != nil {
			teardown = fn
		}
	}
	if teardown == nil {
		t.Fatal("run.go declares no (*Engine).teardown")
	}
	at := map[string]int{}
	for i, stmt := range teardown.Body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			continue
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if _, seen := at[sel.Sel.Name]; !seen {
				at[sel.Sel.Name] = i
			}
		}
	}
	committer, ok := at["stopCommitter"]
	if !ok {
		t.Fatal("the teardown never stops the committer")
	}
	estate, ok := at["stopServingEstate"]
	if !ok {
		t.Fatal("the teardown never stops serving the estate")
	}
	if committer > estate {
		t.Errorf("the teardown stops serving the estate (statement %d) before it stops the "+
			"committer (statement %d): the withdrawal cancels every proposal still queued, "+
			"and each is answered with that cancellation rather than taken to the "+
			"runner-up", estate, committer)
	}
}
