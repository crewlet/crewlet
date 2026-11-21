package coordtest_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// stopsTheGoroutine are the testing methods that end execution by calling
// runtime.Goexit rather than by returning.
var stopsTheGoroutine = map[string]bool{
	"Fatal": true, "Fatalf": true, "FailNow": true,
	"Skip": true, "Skipf": true, "SkipNow": true,
}

// TestNoFatalOffTheTestGoroutine fails the build if any case reports a failure
// from a goroutine it spawned. t.Fatal only fails the test on its OWN goroutine; elsewhere it Goexits one
// nobody waits on, so a WaitGroup never completes and the case hangs until a
// timeout blames a package. Cases collect under a mutex and report after
// joining. It walks the syntax tree: a line scan cannot see scope.
//
// BOUNDARY: it CATCHES a stop-call lexically inside a `go` statement, directly
// or in a closure; it does NOT catch `go namedWorkerThatFatals(h)`
// (landing-checked). The vacuity check is only a count because the subject is
// `*ast.GoStmt` and the names are testing.T's, so no rename blinds it. An
// aliased or unrelated receiver (log.Fatalf) is caught on purpose.
func TestNoFatalOffTheTestGoroutine(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	parsed := parseEveryGoFile(t, fset, ".")

	files, goStmts := 0, 0
	for _, file := range parsed {
		{
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

// parseEveryGoFile parses every .go file in dir.
//
// Not parser.ParseDir, which Go 1.25 deprecated because it associates files
// with packages without consulting build tags. A guard wants EVERY file
// regardless of tags — a rule that stopped applying to the //go:build unix
// half of a package would be a rule that quietly stopped applying — so the
// walk is explicit rather than inherited from a helper whose behaviour here
// was a happy accident.
func parseEveryGoFile(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("listing %s: %v", dir, err)
	}
	files := make([]*ast.File, 0, len(paths))
	for _, path := range paths {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		t.Fatalf("no Go files under %s — the guard would pass by scanning nothing", dir)
	}
	return files
}
