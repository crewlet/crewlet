package coordtest_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// stopsTheGoroutine are the testing methods that end execution by calling
// runtime.Goexit rather than by returning.
var stopsTheGoroutine = map[string]bool{
	"Fatal": true, "Fatalf": true, "FailNow": true,
	"Skip": true, "Skipf": true, "SkipNow": true,
}

// TestNoFatalOffTheTestGoroutine fails the build if any case reports a failure
// from a goroutine it spawned.
//
// t.Fatal is only a failure on the test's OWN goroutine. Anywhere else it
// marks the test failed and then calls runtime.Goexit on a goroutine nobody
// is waiting on, so the spawned worker vanishes silently: a WaitGroup it had
// not yet marked done never completes and the case hangs until a timeout
// blames a package. The concurrency cases here are written to collect into
// slices under a mutex and report from the test goroutine after joining, and
// this is what keeps that a rule rather than a habit.
//
// It walks the syntax tree because the line-based version of this check
// cannot see scope, and is not merely imprecise but wrong in both directions:
// a closure passed as an argument does not close its brace at the end of its
// body — the line continues `}, arg)` — so a line scan runs past the closure
// into the enclosing function and flags its perfectly legal Fatals, while a
// Fatal further into a long body than the scan's window falls out entirely. A
// teammate's line-based version of this same check reported six violations,
// all false; "fixing" them would have converted correct Fatals into Errorfs
// and let tests carry on into assertions whose preconditions never held.
//
// BOUNDARY: it catches a stop-call written lexically inside a `go` statement.
// A `go` statement calling a NAMED function that fatals is not caught, and
// nothing here pretends otherwise.
func TestNoFatalOffTheTestGoroutine(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parsing the suite: %v", err)
	}

	files, goStmts := 0, 0
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			files++
			ast.Inspect(file, func(n ast.Node) bool {
				gs, ok := n.(*ast.GoStmt)
				if !ok {
					return true
				}
				goStmts++
				ast.Inspect(gs, func(inner ast.Node) bool {
					call, ok := inner.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if !ok || !stopsTheGoroutine[sel.Sel.Name] {
						return true
					}
					t.Errorf("%s: %s() inside a spawned goroutine — it will Goexit a worker "+
						"nobody is waiting on, and the case hangs instead of failing. Collect "+
						"the failure and report it from the test goroutine after joining",
						fset.Position(call.Pos()), sel.Sel.Name)
					return true
				})
				return true
			})
		}
	}

	// A guard that scanned nothing passes for the wrong reason, which is the
	// same trap as a case whose result cannot vary. Both counts are asserted
	// so a wrong working directory or a broken walk fails loudly instead of
	// certifying an empty set.
	if files == 0 {
		t.Fatal("parsed no files — this guard was certifying nothing")
	}
	if goStmts == 0 {
		t.Fatal("found no `go` statements to inspect. If the suite genuinely no longer " +
			"spawns goroutines this guard is dead code: delete it rather than weaken it")
	}
	t.Logf("scanned %d files, %d go statements", files, goStmts)
}
