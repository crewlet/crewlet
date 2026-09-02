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

func TestTheListenerBoundsAnIdleConnection(t *testing.T) {
	t.Parallel()
	// Two bounds, and the pair is the point. ReadHeaderTimeout covers a
	// connection opened and left silent BEFORE its first request — the
	// cheapest denial there is against a listener. IdleTimeout covers the
	// silence AFTER a response, which is a different connection state and
	// was unbounded: with both it and ReadTimeout unset, net/http applies
	// no deadline at all between keep-alive requests, so a client that
	// completed one request and went away held its slot indefinitely.
	//
	// ReadTimeout is deliberately absent — it would cap the whole request
	// including the body, putting a ceiling on `crewlet backup` and on a
	// large config import — so its absence must not be read as an
	// oversight the next time somebody audits this block.
	set := serverFieldsIn(t)
	for _, field := range []string{"ReadHeaderTimeout", "IdleTimeout"} {
		if _, ok := set[field]; !ok {
			t.Errorf("the http.Server is built without a %s", field)
		}
	}
}

// serverFieldsIn is the set of field names the http.Server literal in main.go
// sets. The source is what this asserts, rather than behaviour: what these
// bound is a connection left SILENT, so a behavioural test has to wait out the
// timeout it is checking — ten seconds per assertion on every CI run, to learn
// something the source states directly.
func serverFieldsIn(t *testing.T) map[string]struct{} {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}

	found := map[string]struct{}{}
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
			if key, ok := kv.Key.(*ast.Ident); ok {
				found[key.Name] = struct{}{}
			}
		}
		return true
	})
	if len(found) == 0 {
		t.Fatal("no http.Server literal found in main.go")
	}
	return found
}
