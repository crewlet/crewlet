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
// t.Fatal, t.Skip or t.FailNow from anywhere but the test goroutine.
//
// Those three end the calling goroutine with runtime.Goexit. On the test's own
// goroutine that is how a test stops; on a delivery handler or a spawned
// goroutine it kills that goroutine instead, so the failure is never recorded,
// whatever the test was waiting for never arrives, and the case fails later as
// an unexplained timeout naming nothing. The suite's answer to a broken backend
// would be to lose the diagnosis — the same family as a panic taking the binary
// down with it, and quieter.
//
// t.Errorf and t.Logf are safe anywhere and are what the handlers here use.
//
// This is a source scan for the same reason the memory backend's lock guard is:
// reproducing it needs a backend that is already misbehaving, and a test that
// has to break a backend to prove a point becomes the thing that breaks CI. It
// also replaced a line-based scan of mine that reported six violations, all
// false — a closure passed as an argument does not close its brace at the end
// of its body, so the scan ran on into the enclosing test function and found
// its perfectly legal error checks. Hence the AST.
//
// WHAT IT CATCHES AND WHAT IT DOES NOT. It catches a stop-call written
// LEXICALLY inside a `go` statement or a handler literal — measured on both
// halves separately, since they are different matchers: a t.Fatalf added inside
// `go func()` and one added inside a handler literal each came back named by
// file and line. It does not see through a CALL: a goroutine that invokes a
// NAMED function which fatals is invisible to it — better to say so than to let
// the next reader infer coverage the walk does not have.
//
// This paragraph used to open "WHAT IT DOES NOT CATCH: a stop-call written
// lexically inside a `go` statement or a handler literal", i.e. it disclaimed
// precisely what the guard is for. The same inverted sentence sat in the memory
// backend's lock guard, which is the tell: one construction copied to a second
// site, not two independent slips. See that guard for why the shape survives —
// a stated limit reads as modesty and gets read past.
//
// This walker is HALF rename-proof, which is why it carries the strong vacuity
// check anyway. The `go` half keys on *ast.GoStmt and on testing.T's own method
// names: language and stdlib, nothing this repo can rename. The handler half
// keys on queue.Result and *events.Event, which are repo-local — rename either
// and every handler literal reads as on-goroutine while the guard keeps
// passing. A matcher is only as rename-proof as its weakest clause.
//
// Both counts below are asserted rather than logged. A guard that checks for
// the ABSENCE of something passes identically when the thing is absent and when
// the guard has stopped working, and those are indistinguishable from outside —
// which matters most here, where the appearance of certification is the entire
// deliverable. If this suite ever legitimately stops using goroutines and
// handlers, DELETE this guard rather than relaxing the count: a guard kept
// alive past its subject is how the number gets quietly lowered to zero.
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
