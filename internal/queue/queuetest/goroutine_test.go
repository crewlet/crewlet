package queuetest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoFatalOffTheTestGoroutine fails the build if this suite can call
// t.Fatal, t.Skip or t.FailNow from anywhere but the test goroutine: on a
// handler or spawned goroutine they end only that goroutine, the failure is
// never recorded and the case times out unexplained. t.Errorf and t.Logf
// are safe anywhere.
//
// An AST scan, since reproducing it needs a backend already misbehaving. It
// catches a stop-call LEXICALLY inside a `go` statement or a handler
// literal, not through a CALL to a NAMED function. The handler half keys on
// repo-local queue.Result and *events.Event, so a rename would blind it:
// both counts are asserted, as a guard for ABSENCE passes identically when
// it stops working. If the suite stops using them, DELETE the guard.
func TestNoFatalOffTheTestGoroutine(t *testing.T) {
	t.Parallel()
	dir := packageDir(t)
	sources, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var scanned, inspected int
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		scanned++
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		lits := offGoroutineLiterals(file)
		inspected += len(lits)
		for _, lit := range lits {
			for _, call := range fatalCalls(lit.Body) {
				t.Errorf("%s: %s runs off the test goroutine and calls it\n"+
					"\tuse t.Errorf — Fatal/Skip/FailNow only end the goroutine they run on",
					fset.Position(call.pos).String(), call.name)
			}
		}
	}
	if scanned == 0 {
		t.Fatalf("parsed no source files; this guard was certifying nothing")
	}
	if inspected == 0 {
		// The walker keys on `go` statements and on handler signatures. If
		// either stops matching — a renamed result type, a changed handler
		// shape — every literal reads as on-goroutine and the guard passes
		// for ever having examined none of them.
		t.Fatalf("found no off-goroutine function literals to inspect; the walker " +
			"no longer recognises this suite's handlers, so the check is vacuous")
	}
}

func packageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("cannot locate this test's own source file")
	}
	return filepath.Dir(file)
}

// offGoroutineLiterals collects every function literal that does NOT run on the
// test goroutine: one launched with `go`, and one that is a delivery, stream or
// publish-listener handler, which a backend invokes on a goroutine of its own.
func offGoroutineLiterals(file *ast.File) []*ast.FuncLit {
	var out []*ast.FuncLit
	ast.Inspect(file, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.GoStmt:
			if lit, ok := node.Call.Fun.(*ast.FuncLit); ok {
				out = append(out, lit)
			}
		case *ast.FuncLit:
			if isHandlerSignature(node.Type) {
				out = append(out, node)
			}
		}
		return true
	})
	return out
}

// isHandlerSignature reports whether a literal has the shape of something a
// backend calls back into: it returns queue.Result (a delivery or batch
// handler), or it takes an *events.Event and returns nothing (a stream handler
// or a publish listener).
func isHandlerSignature(t *ast.FuncType) bool {
	if t.Results != nil && len(t.Results.List) == 1 {
		if sel, ok := t.Results.List[0].Type.(*ast.SelectorExpr); ok && sel.Sel.Name == "Result" {
			return true
		}
	}
	if t.Results != nil && len(t.Results.List) > 0 {
		return false
	}
	for _, param := range t.Params.List {
		star, ok := param.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Event" {
			return true
		}
	}
	return false
}

type fatalCall struct {
	name string
	pos  token.Pos
}

func fatalCalls(body *ast.BlockStmt) []fatalCall {
	var out []fatalCall
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "t" {
			return true
		}
		switch sel.Sel.Name {
		case "Fatal", "Fatalf", "Skip", "Skipf", "SkipNow", "FailNow":
			out = append(out, fatalCall{name: "t." + sel.Sel.Name, pos: call.Pos()})
		}
		return true
	})
	return out
}
