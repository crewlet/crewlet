package memory_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoLogCallHoldsTheBrokerLock fails the build if a log call is made while
// the broker mutex is held: a blocked log write would stall every peer.
// A source scan, not a runtime assertion: a test blocking stderr becomes the
// thing that hangs CI. It catches a log call LEXICALLY inside a locked region,
// not one reached through a helper or interface, so moving a log line into a
// helper defeats it.
//
// Both counts are asserted: a guard for an ABSENCE passes identically when the
// guard stopped working. Every name it keys on is renameable (a field called
// `mu`), so it counts files scanned AND acquisitions matched. If the package
// stops using a mutex, DELETE this guard rather than lowering the count.
func TestNoLogCallHoldsTheBrokerLock(t *testing.T) {
	t.Parallel()
	sources, err := filepath.Glob(filepath.Join(packageDir(t), "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	var scanned, acquisitions int
	for _, path := range sources {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		scanned++
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		acquisitions += countMutexAcquisitions(file)
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			for _, call := range lockedLogCalls(fn.Body.List, false, false) {
				t.Errorf("%s logs while holding the broker lock (in %s)\n"+
					"\tgather what the line needs, unlock, then log",
					fset.Position(call).String(), fn.Name.Name)
			}
		}
	}
	if scanned == 0 {
		// A glob that silently matches nothing would make this a
		// permanent, invisible pass.
		t.Fatalf("parsed no source files; this guard was certifying nothing")
	}
	if acquisitions == 0 {
		// The matcher keys on a field named `mu`. Rename it and every
		// function below reads as lock-free, so the guard reports zero
		// violations for ever while inspecting nothing.
		t.Fatalf("found no mutex acquisitions to inspect; the matcher no longer " +
			"recognises this package's lock, so the check is vacuous")
	}
}

// countMutexAcquisitions reports how many lock acquisitions the matcher can
// still see, which is the only evidence that the check above examined anything.
func countMutexAcquisitions(file *ast.File) int {
	var n int
	ast.Inspect(file, func(node ast.Node) bool {
		if expr, ok := node.(*ast.ExprStmt); ok && isMutexCall(expr.X, "Lock") {
			n++
		}
		if def, ok := node.(*ast.DeferStmt); ok && isMutexCall(def.Call, "Unlock") {
			n++
		}
		return true
	})
	return n
}

// packageDir is the directory this source file lives in.
//
// Derived from the compiled-in path of this file rather than from the working
// directory: `go test` happens to run a test binary with its CWD set to the
// package directory, but anything that runs the binary directly — a stress
// harness, a CI step that reuses a built binary — does not, and the check would
// then quietly scan nothing.
func packageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatalf("cannot locate this test's own source file")
	}
	return filepath.Dir(file)
}

// lockedLogCalls walks a statement list in order, tracking whether the lock is
// held, and returns the position of every log call reached while it is.
//
// It works on the syntax tree rather than on lines because control flow is the
// whole difficulty: nearly every method here unlocks inside an early-return
// branch, and a line-based scan treats that as releasing the lock for the rest
// of the function — so it silently stops checking exactly the code that
// follows. A branch's lock state therefore stays inside that branch, and only a
// statement at this level can change the state the following statements see.
func lockedLogCalls(stmts []ast.Stmt, held, deferredUnlock bool) []token.Pos {
	var found []token.Pos
	for _, stmt := range stmts {
		switch s := stmt.(type) {
		case *ast.ExprStmt:
			switch {
			case isMutexCall(s.X, "Lock"):
				held = true
			case isMutexCall(s.X, "Unlock"):
				held = false
			case (held || deferredUnlock) && isLogCall(s.X):
				found = append(found, s.Pos())
			}
		case *ast.DeferStmt:
			// A deferred unlock holds the lock for the whole of the
			// rest of the function, which is the form that hid most of
			// the original violations.
			if isMutexCall(s.Call, "Unlock") {
				deferredUnlock = true
			}
		case *ast.IfStmt:
			found = append(found, lockedLogCalls(s.Body.List, held, deferredUnlock)...)
			if block, ok := s.Else.(*ast.BlockStmt); ok {
				found = append(found, lockedLogCalls(block.List, held, deferredUnlock)...)
			}
			if elseIf, ok := s.Else.(*ast.IfStmt); ok {
				found = append(found, lockedLogCalls([]ast.Stmt{elseIf}, held, deferredUnlock)...)
			}
		case *ast.ForStmt:
			found = append(found, lockedLogCalls(s.Body.List, held, deferredUnlock)...)
		case *ast.RangeStmt:
			found = append(found, lockedLogCalls(s.Body.List, held, deferredUnlock)...)
		case *ast.BlockStmt:
			found = append(found, lockedLogCalls(s.List, held, deferredUnlock)...)
		case *ast.SwitchStmt:
			found = append(found, lockedLogCalls(s.Body.List, held, deferredUnlock)...)
		case *ast.TypeSwitchStmt:
			found = append(found, lockedLogCalls(s.Body.List, held, deferredUnlock)...)
		case *ast.CaseClause:
			found = append(found, lockedLogCalls(s.Body, held, deferredUnlock)...)
		}
	}
	return found
}

// isMutexCall reports whether expr is a `<something>mu.Lock()` / `.Unlock()`
// call. Matched on the method name rather than on the receiver expression so it
// keeps working for both `b.mu` and `q.broker.mu`.
func isMutexCall(expr ast.Expr, method string) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return false
	}
	receiver, ok := sel.X.(*ast.SelectorExpr)
	return ok && receiver.Sel.Name == "mu"
}

func isLogCall(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok || ident.Name != "log" {
		return false
	}
	switch sel.Sel.Name {
	case "Debug", "Info", "Warn", "Error":
		return true
	}
	return false
}
