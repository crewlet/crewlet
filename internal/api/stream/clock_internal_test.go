package stream

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// EVERY DECISION THIS PACKAGE TAKES IS TAKEN ON THE SERVICE'S CLOCK.
//
// The service's clock ([Options.Now]) is what the guard, the credential timer
// (lifetime.go) and every envelope's stamp read. A watch decision reading the
// wall clock would, under a pinned clock, judge a recency window by a
// different clock from the one that proved it: latent while no row asks
// recency. The only wall-clock reading left is the service's default.
//
// Syntactic: EVERY MENTION of the wall clock's readers counts, `time.Now`
// handed over as a value (`now: time.Now`) included. Mutation: put time.Now()
// back at either watch decision, or build the watch with `now: time.Now`.
func TestEveryDecisionHereIsTakenOnTheServicesClock(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var read, outside []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read = append(read, name)
		// THE DEFAULT, which is the clock rather than a second one: an
		// assignment to the service's own `now`.
		var allowed []ast.Node
		ast.Inspect(file, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 {
				return true
			}
			if sel, ok := assign.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "now" {
				allowed = append(allowed, assign)
			}
			return true
		})
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "time" ||
				(sel.Sel.Name != "Now" && sel.Sel.Name != "Since" && sel.Sel.Name != "Until") {
				return true
			}
			for _, a := range allowed {
				if sel.Pos() >= a.Pos() && sel.End() <= a.End() {
					return true
				}
			}
			outside = append(outside, fset.Position(sel.Pos()).String())
			return true
		})
	}
	// A GATE THAT READ NOTHING PASSES — so it says what it read.
	if len(read) < 5 {
		t.Fatalf("the gate read only %v; it is not reading the package", read)
	}
	for _, at := range outside {
		t.Errorf("%s reads the wall clock: take the service's clock (Service.now, "+
			"or a field it is handed), which the guard and the credential's timer read", at)
	}
}
