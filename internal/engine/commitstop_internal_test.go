package engine

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"sync/atomic"
	"testing"
	"time"
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
		{"stopObjectCollector", "a barrier on every declaring domain's log at each pass"},
		{"stopCommitter", "a code log record for every batch of pushes peers propose"},
		{"stopServingEstate", "every write and linearizable read a peer routes here"},
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

// THE COMMITTER AND ITS WRITERS ARE CLOSED FIRST AND DRAINED TOGETHER, under
// the teardown's one bound ([stopTogether]): while the committer drains a
// batch a broker election holds, its sibling writer is already closed — a
// write arriving then is answered not serving and taken to the runner-up,
// never admitted and published on a node that is stopping — and the writer's
// publish already in flight finishes inside the bound beside the committer's
// drain, rather than being handed a context the committer's drain had spent.
//
// Mutation: stop them one after another, and the writer is still open while
// the committer drains; close them first and still drain one after another,
// and the writer's publish in flight is handed the context the committer's
// drain spent.
func TestTheCodeWritersAreClosedFirstAndDrainedTogether(t *testing.T) {
	t.Parallel()
	committer := &stoppingWriter{stuck: true, stopped: make(chan struct{})}
	writer := &stoppingWriter{inFlight: make(chan struct{}), stopped: make(chan struct{})}
	const bound = 2 * time.Second
	done := make(chan struct{})
	go func() {
		stopTogether(t.Context(), bound, committer, writer)
		close(done)
	}()

	// CLOSED WHILE THE COMMITTER'S BATCH IS STILL HELD.
	deadline := time.Now().Add(bound / 2)
	for !writer.closed.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("the writer was still open %s into the teardown, while the committer drained", bound/2)
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-committer.stopped:
		t.Fatal("the committer's drain ended before its held batch was released or its bound spent")
	default:
	}

	// ITS PUBLISH IN FLIGHT FINISHES, inside the bound, beside the drain.
	close(writer.inFlight)
	select {
	case <-writer.stopped:
	case <-time.After(bound):
		t.Fatal("the writer's drain did not end once its publish in flight finished")
	}
	if writer.ended != nil {
		t.Fatalf("the writer's publish in flight was cut short (%v): it was handed a spent context", writer.ended)
	}

	<-done
	if !errors.Is(committer.ended, context.DeadlineExceeded) {
		t.Fatalf("the committer's drain ended %v, want the teardown's bound spent on its held batch",
			committer.ended)
	}
}

// stoppingWriter is a writer as the teardown stops it ([closingWriter]): Close
// marks it closed, and Stop closes it and drains — a stuck one until its
// context ends, as a committer whose batch an election holds; another until
// its publish in flight finishes or its context ends — recording how its
// context stood as the drain ended.
type stoppingWriter struct {
	closed   atomic.Bool
	stuck    bool
	inFlight chan struct{}
	stopped  chan struct{}
	ended    error
}

func (w *stoppingWriter) Close() { w.closed.Store(true) }

func (w *stoppingWriter) Stop(ctx context.Context) {
	w.Close()
	if w.stuck {
		<-ctx.Done()
	} else {
		select {
		case <-w.inFlight:
		case <-ctx.Done():
		}
	}
	w.ended = ctx.Err()
	close(w.stopped)
}
