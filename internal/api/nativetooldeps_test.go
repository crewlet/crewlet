package api_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/sourcetree"
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

// A PERSON'S PAGE WRITES ARE HANDED EVERY SEAM A SEAT'S ARE.
//
// The knowledge half has no per-actor seam to read off the struct, so the
// list is the ENGINE'S: whatever its own [builtin.PageDeps] for a seat's turn
// names, the person's surfaces name too. The default container is the seam that
// went missing: a seat's page that named no space landed in its team's, while
// the same write from the dashboard's "Save as a page" was refused for every
// person who made it, because the operator surface was handed none. The two
// the operator surface sets itself (who a write is attributed to, and the
// authority decision) are not seams of this constructor.
//
// Asserted over the SOURCE, for [TestAPersonsSurfacesAreHandedEveryWriterSeam]'s
// reason, and against the engine's own literal so a seam added there is a
// failure here the day it lands.
func TestAPersonsPageWritesAreHandedEverySeamASeatsAre(t *testing.T) {
	t.Parallel()
	keys := func(path, fn string) []string {
		t.Helper()
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		var out []string
		found := false
		ast.Inspect(file, func(n ast.Node) bool {
			decl, ok := n.(*ast.FuncDecl)
			if !ok || decl.Name.Name != fn {
				return true
			}
			ast.Inspect(decl.Body, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok || !isSelector(lit.Type, "builtin", "PageDeps") || len(lit.Elts) == 0 {
					return true
				}
				found = true
				for _, elt := range lit.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if key, ok := kv.Key.(*ast.Ident); ok {
							out = append(out, key.Name)
						}
					}
				}
				return false
			})
			return false
		})
		if !found {
			t.Fatalf("no builtin.PageDeps literal in %s's %s, so this gate certifies "+
				"nothing", path, fn)
		}
		return out
	}
	seat := keys(filepath.Join(sourcetree.Root(t), "internal", "engine", "native.go"), "pageDeps")
	person := keys("humans.go", "NativeToolDeps")
	if !slices.Contains(seat, "DefaultContainer") {
		t.Fatal("the engine's own page deps name no DefaultContainer, so this gate is " +
			"not reading the literal it is about")
	}
	for _, seam := range seat {
		if !slices.Contains(person, seam) {
			t.Errorf("NativeToolDeps leaves builtin.PageDeps.%s unwired, which every "+
				"seat's own page tools are handed: a person's write through the "+
				"operator surface or the HTTP write surface goes without it", seam)
		}
	}
}

// A PERSON'S SURFACES REACH THE ESTATE THROUGH THE ROUTER, never through this
// node's own copy.
//
// The operator's assistant, the dashboard's act route, the human write surface
// and the read surface's board, pages, search and file listing are all built
// in humans.go and native.go, and the engine hands them the tracker and the
// knowledge base two ways: its own copy ([engine.Engine.Tracker] and the rest),
// and its estate router ([engine.OperatorWork] and the rest), which answers
// from this node's copy while it serves and from another data node's while it
// does not. Built over the first, a data node whose copy was taken out of
// service — wrong rather than behind, or its file shut — sent its own seats to
// a peer's copy and went on answering a PERSON from the one it had stopped
// serving: a board, an inbox or a purge read off rows the node itself would
// not vouch for. And a read after a person's own write waited on this node's
// applier ([engine.Engine.WaitReached]) rather than the router's session
// floor ([engine.Engine.AwaitEstate]), which is the floor whichever holder
// answers the next read honours.
//
// Asserted over the SOURCE, for [TestAPersonsSurfacesAreHandedEveryWriterSeam]'s
// reason: these constructors need a running engine. An engine value is
// spelled `e`, or `.engine` on a per-request source, in both files — the
// controls below fail if the walk finds no routed call, which is what a
// rename of that convention would look like.
func TestAPersonsSurfacesReachTheEstateThroughTheRouter(t *testing.T) {
	t.Parallel()
	// Each accessor of this node's own copy, and the routed one to call.
	local := map[string]string{
		"Tracker":       "engine.OperatorWork",
		"TrackerWriter": "engine.OperatorWorkWriter",
		"Pages":         "engine.OperatorPages",
		"PagesStore":    "engine.OperatorPageWriter",
		"WorkSearch":    "engine.WorkSearcher",
		"WaitReached":   "Engine.AwaitEstate",
	}
	routed := map[string]bool{}
	for _, name := range []string{"humans.go", "native.go"} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "engine" {
				routed[sel.Sel.Name] = true
				return true
			}
			use, isLocal := local[sel.Sel.Name]
			if isLocal && isEngineValue(sel.X) {
				t.Errorf("%s reaches this node's own copy through %s — use %s, "+
					"which a copy taken out of service is answered around",
					fset.Position(sel.Pos()), sel.Sel.Name, use)
			}
			return true
		})
	}
	for _, want := range []string{"OperatorWork", "OperatorWorkWriter",
		"OperatorPages", "OperatorPageWriter", "WorkSearcher"} {
		if !routed[want] {
			t.Errorf("no call to engine.%s in humans.go or native.go, so this gate "+
				"is not reading the surfaces it is about", want)
		}
	}
}

// isEngineValue reports whether e is an engine as these files spell one: `e`,
// or a source's `.engine`.
func isEngineValue(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name == "e"
	case *ast.SelectorExpr:
		return x.Sel.Name == "engine"
	}
	return false
}
