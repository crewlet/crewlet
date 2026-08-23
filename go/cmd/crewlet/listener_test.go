package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// The listener's timeouts cannot be observed cheaply: what they defend against
// is a connection opened and left SILENT, so a behavioural test has to wait out
// the timeout it is checking for. That is ten seconds per assertion on every CI
// run, to learn something the source states directly — so the source is what
// this asserts, the same way the token comparison's timing properties are.

func TestTheListenerBoundsHowLongAHeaderMayTake(t *testing.T) {
	t.Parallel()
	// A connection opened and left silent is the cheapest denial there is
	// against a listener, and the listener is the one surface an
	// unauthenticated client can reach. Without this bound it costs one
	// connection slot for ever.
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	var found bool
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Server" {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok || pkg.Name != "http" {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == "ReadHeaderTimeout" {
				found = true
			}
		}
		return true
	})
	if !found {
		t.Error("the http.Server is built without a ReadHeaderTimeout: a " +
			"connection opened and left silent holds its slot for ever")
	}
}
