package credential

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// EVERY COMPARISON IN THIS PACKAGE IS CONSTANT-TIME, AND THAT IS A PROPERTY NO
// BEHAVIOURAL TEST CAN SEE.
//
// A plain `==` returns the same true or false as `subtle.ConstantTimeCompare`,
// and a loop that stops at the first match returns the same index as one that
// does not. What differs is how long each takes, and the difference hands an
// attacker the secret one byte at a time through the endpoint's own latency —
// on endpoints this engine deliberately serves with no other credential in
// front of them.
//
// So it is asserted against the SOURCE, which is what this repository does
// everywhere a rule no runtime observation can carry has to hold.

// comparisons are the functions that check a presented secret against a stored
// one, and the file each lives in.
//
// A LIST RATHER THAN A WALK OF EVERY FUNCTION, because the property is not
// "this package never uses ==" — it uses it on lengths, on prefixes and on
// parameters, all of which are public. It is "these four functions compare a
// SECRET", and naming them is what makes a fifth one added later a visible
// omission rather than a silent one.
var comparisons = map[string]string{
	"Verify":            "password.go",
	"VerifyTOTP":        "totp.go",
	"SpendRecoveryCode": "recovery.go",
	"VerifyToken":       "pat.go",
}

func declOf(t *testing.T, file, name string) *ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range parsed.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("%s is not in %s — this guard is asserting about nothing", name, file)
	return nil
}

// calls reports whether a function body calls pkg.fn anywhere.
func calls(fn *ast.FuncDecl, pkg, name string) bool {
	found := false
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if ok && ident.Name == pkg && sel.Sel.Name == name {
			found = true
		}
		return true
	})
	return found
}

func TestEverySecretComparisonIsConstantTime(t *testing.T) {
	t.Parallel()
	for name, file := range comparisons {
		fn := declOf(t, file, name)
		if !calls(fn, "subtle", "ConstantTimeCompare") {
			t.Errorf("%s does not use subtle.ConstantTimeCompare, so the "+
				"secret it checks is compared in a way that leaks it a byte "+
				"at a time through the endpoint's own latency", name)
		}
	}
}

// THE LOOPS DO NOT STOP AT THE FIRST MATCH.
//
// An early exit makes the time taken depend on WHICH candidate matched and on
// how many did not — the same leak the constant-time compare exists to close,
// reintroduced one level up. For recovery codes that leak is "how far through
// their set this person is", and somebody on their last code is worth
// targeting.
func TestTheCandidateLoopsDoNotStopAtTheFirstMatch(t *testing.T) {
	t.Parallel()
	for name, file := range map[string]string{
		"VerifyTOTP":        "totp.go",
		"SpendRecoveryCode": "recovery.go",
	} {
		fn := declOf(t, file, name)
		var early bool
		ast.Inspect(fn, func(n ast.Node) bool {
			var body *ast.BlockStmt
			switch loop := n.(type) {
			case *ast.RangeStmt:
				body = loop.Body
			case *ast.ForStmt:
				body = loop.Body
			default:
				return true
			}
			ast.Inspect(body, func(inner ast.Node) bool {
				switch inner.(type) {
				case *ast.ReturnStmt, *ast.BranchStmt:
					early = true
				}
				return true
			})
			return true
		})
		if early {
			t.Errorf("%s leaves its candidate loop early, so how long it "+
				"takes says which candidate matched and how many did not", name)
		}
	}
}

// AND THE GUARD ITSELF IS HELD TO ITS OWN NAMES.
//
// A walk keyed on a function that has been renamed passes silently and
// protects nothing, which is how a gate reading a deleted path certified a
// whole rewrite elsewhere in this tree while reporting a pass.
func TestTheComparisonListNamesFunctionsThatExist(t *testing.T) {
	t.Parallel()
	if len(comparisons) < 4 {
		t.Fatalf("the list names %d functions; every one that checks a "+
			"presented secret belongs in it", len(comparisons))
	}
	for name, file := range comparisons {
		declOf(t, file, name)
	}
}
