package workapi_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A GESTURE NO TOOL MAKES NAMES ITS ITEM AS EVERY TOOL'S RECEIPT DOES.
//
// The rank move and the comment edit write through the tracker themselves, and
// both answered `item: <the key>`. For an item holding a key another task
// claimed first, that key opens the claimant — so a client that put `item`
// back in the path, as this surface's own documentation tells it to, moved or
// edited the wrong card. They answer in the receipt's three fields now, by the
// rule every row and every tool follows.
//
// Mutation: answer either route's `item` from the key and this fails.
func TestAGestureNoToolMakesNamesADuplicateByItsID(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	dup := r.reader.tasks["t-1"]
	dup.Task.ID, dup.KeyCollision = "t-3", true
	r.reader.tasks["t-3"] = dup
	r.reader.addComment("t-3", tracker.Comment{ID: "c-1", Task: "t-3",
		Author: "ana", AuthorKind: tracker.AuthorHuman, Body: "first"})
	for name, call := range map[string]struct {
		method, path string
		body         map[string]any
	}{
		"a rank move": {http.MethodPost, "/work/items/t-3/rank",
			map[string]any{"after": "ENG-2"}},
		"a comment edit": {http.MethodPatch, "/work/items/t-3/comments/c-1",
			map[string]any{"body": "second"}},
	} {
		got := r.do(as(colleague("ana")), call.method, call.path, call.body)
		if got.status != http.StatusOK {
			t.Fatalf("%s answered %d: %v", name, got.status, got.body)
		}
		if got.body["item"] != "t-3" || got.body["key"] != "ENG-1" ||
			got.body["key_collision"] != true {
			t.Errorf("%s answered item %v, key %v, key_collision %v — want "+
				"t-3 beside ENG-1, flagged: ENG-1 opens the claimant",
				name, got.body["item"], got.body["key"], got.body["key_collision"])
		}
	}
	if got := r.do(as(colleague("ana")), http.MethodPost, "/work/items/ENG-1/rank",
		map[string]any{"after": "ENG-2"}); got.body["item"] != "ENG-1" ||
		got.body["key_collision"] != nil {
		t.Errorf("the claimant's rank move answered item %v, key_collision %v — "+
			"an item that answers to its key is named by it", got.body["item"],
			got.body["key_collision"])
	}
}

// NOTHING ON THIS SURFACE NAMES AN ITEM BY A KEY READ OFF A DETAIL, outside
// the one route where the key is what is meant.
//
// The tools hold the same rule over their own package; this surface writes
// through the tracker for the verbs no tool makes, and its receipts and its
// refusals are read by the same clients. Both directions, as the tools' walk.
func TestNoRouteNamesAnItemByAKeyOutsideTheAddressRule(t *testing.T) {
	t.Parallel()
	allowed := map[string]string{
		"served.postPurge": "a purge is CONFIRMED by repeating the item's key, " +
			"so it compares, refuses and reports the key — the receipt names " +
			"the item by its id in `task`, and a purged item opens nothing",
	}
	dir := filepath.Join(sourcetree.Root(t), "internal", "api", "workapi")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	found := map[string]bool{}
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		read++
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name),
			nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			owner := fn.Name.Name
			if fn.Recv != nil && len(fn.Recv.List) > 0 {
				recv := fn.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if ident, ok := recv.(*ast.Ident); ok {
					owner = ident.Name + "." + owner
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Key" {
					return true
				}
				if inner, ok := sel.X.(*ast.SelectorExpr); !ok || inner.Sel.Name != "Task" {
					return true
				}
				found[owner] = true
				if _, ok := allowed[owner]; !ok {
					t.Errorf("%s: %s reads an item's key off a detail — name "+
						"it through its Address or Named, or builtin.ReceiptOf, "+
						"or declare here why the key is what this text means",
						name, owner)
				}
				return true
			})
		}
	}
	if read < 3 {
		t.Fatalf("read %d source files in %s, so this walk certifies nothing", read, dir)
	}
	for owner, why := range allowed {
		if !found[owner] {
			t.Errorf("%s is allowed to read an item's key (%s) and no longer "+
				"does — drop the allowance", owner, why)
		}
	}
}
