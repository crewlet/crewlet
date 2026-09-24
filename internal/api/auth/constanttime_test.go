package auth

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
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

// comparisonBody returns the AST of Guard.entry, which is where the token
// comparison lives.
//
// IT FOLLOWED THE COMPARISON. This read Guard.Operator while that function
// held the loop; the loop moved into `entry`, and Operator — which nothing but
// tests called — is gone. A walk left pointing at a function that no longer
// compares would have found no compare and no loop and reported the absence as
// a failure — or, had it been written to tolerate that, would have certified a
// comparison it was no longer reading. Which function is walked is the one
// thing this suite cannot get wrong quietly, so it fails loudly when the name
// is gone.
func comparisonBody(t *testing.T) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "auth.go", nil, 0)
	if err != nil {
		t.Fatalf("parse auth.go: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "entry" || fn.Recv == nil {
			continue
		}
		return fn
	}
	t.Fatal("Guard.entry not found — this guard is asserting about nothing")
	return nil
}

func TestTheTokenComparisonIsConstantTime(t *testing.T) {
	t.Parallel()
	// A plain == returns the same answer and leaks the token through how
	// long it takes to say it: string comparison stops at the first byte
	// that differs, so an attacker can find a token one byte at a time.
	found := false
	ast.Inspect(comparisonBody(t), func(n ast.Node) bool {
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
		t.Error("Guard.entry does not use subtle.ConstantTimeCompare: " +
			"the token is compared in a way that leaks it a byte at a time")
	}
}

func TestTheTokenLoopDoesNotStopAtTheFirstMatch(t *testing.T) {
	t.Parallel()
	// An early exit makes the time taken depend on WHICH id matched, and
	// on how many did not — the same leak the constant-time compare above
	// exists to close, reintroduced one level up.
	var early bool
	ast.Inspect(comparisonBody(t), func(n ast.Node) bool {
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
		t.Error("Guard.entry leaves its comparison loop early: the time it " +
			"takes then depends on which token matched")
	}
}

// AN EMPTY CANDIDATE MATCHES NOTHING, not even a token configured as "".
//
// Config refuses an empty token value, but Bootstrap is an exported struct an
// embedder can build directly, and a token whose environment variable was
// unset resolves to "" — so without this check a request presenting no
// credential at all would match it. Every path into the comparison today
// passes a non-empty candidate, which is exactly why the check is held here
// rather than trusted to the callers. Mutation: drop the check and the empty
// candidate matches the empty token.
func TestAnEmptyCandidateMatchesNoTokenEvenAnEmptyOne(t *testing.T) {
	t.Parallel()
	g := &Guard{tokens: map[string]config.APIToken{"founder": {ID: "founder", Token: ""}}}
	if entry, ok := g.entry(""); ok {
		t.Errorf("an empty candidate matched %q", entry.ID)
	}
}
