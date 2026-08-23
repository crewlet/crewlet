package auth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The token comparison has two properties a behavioural test cannot see,
// because both are about TIME rather than about the answer: a plain string
// comparison returns the same true or false as a constant-time one, and a loop
// that stops at the first match returns the same operator id as one that does
// not. What differs is how long each takes, and the difference leaks the token.
//
// So they are asserted against the source, which is what this repository does
// elsewhere for a rule no runtime observation can carry — the same shape as the
// guards that fail the build on a hand-built subject or a second copy of a
// shared pattern.

// operatorBody returns the AST of Guard.Operator.
func operatorBody(t *testing.T) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "auth.go", nil, 0)
	if err != nil {
		t.Fatalf("parse auth.go: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Operator" || fn.Recv == nil {
			continue
		}
		return fn
	}
	t.Fatal("Guard.Operator not found — this guard is asserting about nothing")
	return nil
}

func TestTheTokenComparisonIsConstantTime(t *testing.T) {
	t.Parallel()
	// A plain == returns the same answer and leaks the token through how
	// long it takes to say it: string comparison stops at the first byte
	// that differs, so an attacker can find a token one byte at a time.
	found := false
	ast.Inspect(operatorBody(t), func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if ok && pkg.Name == "subtle" && sel.Sel.Name == "ConstantTimeCompare" {
			found = true
		}
		return true
	})
	if !found {
		t.Error("Guard.Operator does not use subtle.ConstantTimeCompare: " +
			"the token is compared in a way that leaks it a byte at a time")
	}
}

func TestTheTokenLoopDoesNotStopAtTheFirstMatch(t *testing.T) {
	t.Parallel()
	// An early exit makes the time taken depend on WHICH id matched, and
	// on how many did not — the same leak the constant-time compare above
	// exists to close, reintroduced one level up.
	var early bool
	ast.Inspect(operatorBody(t), func(n ast.Node) bool {
		loop, ok := n.(*ast.RangeStmt)
		if !ok {
			return true
		}
		ast.Inspect(loop.Body, func(inner ast.Node) bool {
			switch inner.(type) {
			case *ast.ReturnStmt, *ast.BranchStmt:
				// A break is the same leak as a return here.
				early = true
			}
			return true
		})
		return true
	})
	if early {
		t.Error("Guard.Operator leaves its comparison loop early: the time it " +
			"takes then depends on which token matched")
	}
}
