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
	at := teardownOrder(t)
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

// THE CODE-RETENTION LOOP STOPS BEFORE THE DUTIES ARE GIVEN BACK.
//
// The loop claims its duty afresh every turn, so one still running when the
// teardown releases the node's duties takes the code-retention duty straight
// back — and the node exits holding it, keeping every other node from purging
// for the duty's whole TTL. Every duty loop is stopped before the release; this
// one is held to it from the source, as the committer's order is above.
func TestTheCodeRetentionLoopStopsBeforeTheDutiesAreReleased(t *testing.T) {
	t.Parallel()
	at := teardownOrder(t)
	loop, ok := at["stopCodeRetention"]
	if !ok {
		t.Fatal("the teardown never stops the code-retention duty's loop")
	}
	release, ok := at["releaseDuties"]
	if !ok {
		t.Fatal("the teardown never gives the node's duties back")
	}
	if loop > release {
		t.Errorf("the teardown gives the duties back (statement %d) before it stops the "+
			"code-retention loop (statement %d): the loop's next turn claims the duty "+
			"again, and the node exits holding it", release, loop)
	}
}

// THE ADMISSION IS WITHDRAWN ONLY ONCE EVERY STATE-LOG PUBLISHER HAS STOPPED.
//
// The admission says "this process may be publishing", and it is the one
// thing that keeps a capacity coordinator from taking its exclusion while
// this node runs (statelog.AdmissionBlocks): withdrawn while a loop still
// appends, the coordinator may seal a log this node is still writing — the
// moving quantity the window exists to stop. Every loop the teardown stops
// that appends to a log is named here, each held to precede the withdrawal,
// from the source, as the committer's order is above.
//
// Mutation: move any of them after `e.withdraw(ctx)` and its row goes red.
func TestEveryStateLogPublisherStopsBeforeTheAdmissionIsWithdrawn(t *testing.T) {
	t.Parallel()
	at := teardownOrder(t)
	withdraw, ok := at["withdraw"]
	if !ok {
		t.Fatal("the teardown never withdraws this node's admission")
	}
	for _, c := range []struct{ stop, appends string }{
		{"stopRetention", "the trim's purges and trimmed records"},
		{"stopEmbedding", "the vector log's records"},
		{"stopUsage", "the usage log's records"},
		{"stopCodeRetention", "the code log's purge steps and re-declarations"},
		{"stopCodeRepos", "a code log record for every repository an activation changed"},
	} {
		stop, ok := at[c.stop]
		switch {
		case !ok:
			t.Errorf("the teardown never calls %s", c.stop)
		case stop > withdraw:
			t.Errorf("the teardown withdraws the admission (statement %d) before %s "+
				"(statement %d), which appends %s: a coordinator may take its "+
				"exclusion while this node is still appending", withdraw, c.stop, stop,
				c.appends)
		}
	}
}

// teardownOrder is, by method name, the index of the first top-level
// statement of (*Engine).teardown in run.go that calls a method of the engine
// itself — as a statement of its own or inside one, such as the call an `if`
// statement's initialiser makes.
func teardownOrder(t *testing.T) map[string]int {
	t.Helper()
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
	if len(teardown.Recv.List) != 1 || len(teardown.Recv.List[0].Names) != 1 {
		t.Fatal("(*Engine).teardown names no receiver")
	}
	receiver := teardown.Recv.List[0].Names[0].Name
	at := map[string]int{}
	for i, stmt := range teardown.Body.List {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == receiver {
				if _, seen := at[sel.Sel.Name]; !seen {
					at[sel.Sel.Name] = i
				}
			}
			return true
		})
	}
	return at
}
