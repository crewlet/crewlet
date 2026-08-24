package webhooks

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The ordering this package rests on — verify BEFORE anything is persisted,
// broadcast or republished — is held by the type system: accept takes a
// [verified], and only a guard mints one. That is a compile error rather than
// a convention, which is the whole improvement over the Python this replaces.
//
// What the compiler CANNOT hold is a new handler minting its own verified{}
// out of nothing to get past the signature. It would build, it would pass every
// behavioural test that did not think to send a forgery at that particular
// route, and it would reopen the exact hole. So the mint sites are asserted
// against the source, the way this repository asserts every other rule no
// runtime observation can carry.

// mintSites are the functions allowed to construct a verified.
//
//   - authenticate is the shared HMAC gate for five of the routes.
//   - forgeWebhook checks a JWT rather than an HMAC, so it cannot go through
//     that gate — and it is the one place where a second mint site is
//     justified rather than convenient.
var mintSites = map[string]bool{"authenticate": true, "forgeWebhook": true}

func TestOnlyTheGuardsMintAVerifiedDelivery(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", nil, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	pkg, ok := pkgs["webhooks"]
	if !ok {
		t.Fatal("the webhooks package did not parse — this guard is asserting about nothing")
	}

	mints := map[string]int{}
	for _, file := range pkg.Files {
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			if !isFunc || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				lit, isLit := n.(*ast.CompositeLit)
				if !isLit {
					return true
				}
				if ident, isIdent := lit.Type.(*ast.Ident); isIdent && ident.Name == "verified" {
					mints[fn.Name.Name]++
					if !mintSites[fn.Name.Name] {
						t.Errorf("%s: %s mints a verified{} outside the guards, "+
							"which lets a delivery reach the queue and the event "+
							"store without a credential being checked",
							fset.Position(lit.Pos()), fn.Name.Name)
					}
				}
				return true
			})
		}
	}

	// The control. Without it this test passes for a package that renamed
	// the type, deleted the gate, or never had one — the false-green every
	// source-level assertion is one refactor away from.
	for site := range mintSites {
		if mints[site] == 0 {
			t.Errorf("%s mints no verified{}, so this guard is watching a gate "+
				"that no longer exists", site)
		}
	}
}

func TestAcceptCannotBeReachedWithoutOne(t *testing.T) {
	t.Parallel()
	// The other half: the gate is worth nothing if the thing it guards
	// stops requiring it. accept's signature IS the requirement.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "webhooks.go", nil, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, decl := range file.Decls {
		fn, isFunc := decl.(*ast.FuncDecl)
		if !isFunc || fn.Name.Name != "accept" || fn.Recv == nil {
			continue
		}
		for _, param := range fn.Type.Params.List {
			if ident, isIdent := param.Type.(*ast.Ident); isIdent && ident.Name == "verified" {
				return
			}
		}
		t.Fatal("Receiver.accept no longer takes a verified, so a handler can " +
			"publish and persist a delivery it never authenticated")
	}
	t.Fatal("Receiver.accept not found — this guard is asserting about nothing")
}
