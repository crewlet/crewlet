package engine

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// THE COMMITTER STOPS BEFORE THE ESTATE STOPS ANSWERING.
//
// Withdrawing the estate subject cancels every answer in flight (the queue's
// Serve contract), a proposal queued in the committer among them, and the
// router hands that cancellation back as the push's FINAL answer (`context
// canceled`), possibly after its batch landed. A committer stopped first answers queued proposals
// "not serving" (the runner-up decides them under the same push id) and
// finishes its batch in flight. Held from the source: the behavioural form
// needs a fleet.
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
// The loop claims its duty afresh every turn, so one still running after the
// release takes the duty back and the node exits holding it, keeping every
// other node from purging for the duty's whole TTL.
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
// The admission is what keeps a capacity coordinator from taking its exclusion
// while this node may publish (statelog.AdmissionBlocks): withdrawn while a
// loop still appends, the coordinator may seal a log this node is writing.
// Every teardown loop that appends to a log is named below.
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

// THE SANDBOX IS STOPPED ONLY BY THE TEARDOWN, AND THE TEARDOWN COMES ONLY AFTER
// THE DRAIN.
//
// The sandbox coordinator holds the taker lease until its last keep ends,
// through a drain that gives presence up first while captures still upload: a
// peer settles a capture whose taker's lease is gone as never kept, and the
// turn is tombstoned before its last session lands. Held from the source: the
// behavioural form needs a fleet.
//
// Mutation: stop the sandbox from the drain, or from anywhere but the teardown,
// or tear down before draining in Stop, and a check below goes red.
func TestTheSandboxIsStoppedOnlyByTheTeardownAfterTheDrain(t *testing.T) {
	t.Parallel()
	if _, ok := teardownOrder(t)["stopSandbox"]; !ok {
		t.Fatal("the teardown never stops the sandbox")
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("list the engine's files: %v", err)
	}
	fset := token.NewFileSet()
	var stop *ast.FuncDecl
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			switch {
			case engineMethod(fn, "teardown"):
				continue
			case engineMethod(fn, "Stop"):
				stop = fn
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "stopSandbox" {
					t.Errorf("%s stops the sandbox outside the teardown: there it may run before the drain "+
						"has ended, and give the taker lease back under keeps still uploading",
						fset.Position(sel.Pos()))
				}
				return true
			})
		}
	}
	if stop == nil {
		t.Fatal("the engine declares no (*Engine).Stop")
	}
	drained, torn := -1, -1
	for i, stmt := range stop.Body.List {
		ast.Inspect(stmt, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch {
			case sel.Sel.Name == "Drain" && drained < 0:
				drained = i
			case sel.Sel.Name == "teardown" && torn < 0:
				torn = i
			}
			return true
		})
	}
	switch {
	case drained < 0:
		t.Error("(*Engine).Stop never drains")
	case torn < 0:
		t.Error("(*Engine).Stop never tears down")
	case torn <= drained:
		t.Errorf("(*Engine).Stop tears down (statement %d) before it drains (statement %d): the sandbox's "+
			"taker lease would be given back while the drain's keeps still upload", torn, drained)
	}
}

// engineMethod reports whether fn is the method name of *Engine.
func engineMethod(fn *ast.FuncDecl, name string) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 || fn.Name.Name != name {
		return false
	}
	star, ok := fn.Recv.List[0].Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	ident, ok := star.X.(*ast.Ident)
	return ok && ident.Name == "Engine"
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
// the teardown's one bound ([stopTogether]): while the committer drains a batch
// an election holds, the writer is already closed (a new write goes to the
// runner-up), and its publish in flight finishes inside the bound rather than
// on a context the committer's drain spent.
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

// stoppingWriter is a [closingWriter]: Stop closes it and drains (a stuck one,
// like a committer an election holds, until its context ends; another until
// its publish in flight finishes), recording its context's error at the end.
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
