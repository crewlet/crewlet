package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
)

// A PERSON'S SURFACES ARE HANDED EVERY WRITER SEAM A SEAT'S ARE.
//
// Each per-actor seam on [builtin.WorkDeps] is a family of tools, and the
// catalogue registers a tool only where its seam is wired — so a seam this
// constructor leaves out is not a refusal anybody sees but a verb that is
// simply absent from the operator's assistant and the HTTP write surface.
// That is how the cross-project move arrived: the engine wired it for every
// seat, and a person could only move their own item by asking one.
//
// Asserted over the SOURCE because the constructor needs a running engine and
// a concrete tracker writer, which no case here can build without a broker:
// the question is which seams the literal names, and that is visible only in
// the literal. The seams are read off the struct by reflection, so a new one is
// covered the day it is declared.
func TestAPersonsSurfacesAreHandedEveryWriterSeam(t *testing.T) {
	t.Parallel()
	actor := reflect.TypeFor[builtin.Actor]()
	var seams []string
	deps := reflect.TypeFor[builtin.WorkDeps]()
	for i := range deps.NumField() {
		field := deps.Field(i)
		if field.Type.Kind() == reflect.Func && field.Type.NumIn() == 1 &&
			field.Type.In(0) == actor {
			seams = append(seams, field.Name)
		}
	}
	if len(seams) == 0 {
		t.Fatal("builtin.WorkDeps declares no per-actor seam, so this gate " +
			"certifies nothing")
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "humans.go", nil, 0)
	if err != nil {
		t.Fatalf("parse humans.go: %v", err)
	}
	var wired []string
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "NativeToolDeps" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isSelector(lit.Type, "builtin", "WorkDeps") {
				return true
			}
			found = true
			for _, elt := range lit.Elts {
				if kv, ok := elt.(*ast.KeyValueExpr); ok {
					if key, ok := kv.Key.(*ast.Ident); ok {
						wired = append(wired, key.Name)
					}
				}
			}
			return false
		})
		return false
	})
	if !found {
		t.Fatal("no builtin.WorkDeps literal in NativeToolDeps, so this gate " +
			"certifies nothing")
	}
	for _, seam := range seams {
		if !slices.Contains(wired, seam) {
			t.Errorf("NativeToolDeps leaves builtin.WorkDeps.%s unwired, so every "+
				"tool it serves is missing from a person's surfaces", seam)
		}
	}
}
