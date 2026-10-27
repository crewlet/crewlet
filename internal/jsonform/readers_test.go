package jsonform_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// strictReads are the format reads that hold a version to its format as
// written today: what a publish writes, never what a read of a stored version
// opens ([jsonform.DecodeStored] is that read's).
var strictReads = map[string][]string{
	"slideformat":  {"Check", "ParseIndex"},
	"docindex":     {"Check", "ParseIndex"},
	"canvasformat": {"Check", "Parse"},
	"dashspec":     {"Check", "Parse"},
	"jsonform":     {"Decode"},
}

// mayReadStrictly are the packages a strict read belongs in: the publish
// pipeline, which judges what a version will be, and the formats themselves.
var mayReadStrictly = []string{
	"internal/artifactupload",
	"internal/artifacts/brandformat", // brand.json was exact before ADR-0059, on every read
	"internal/artifacts/slideformat", "internal/artifacts/docindex",
	"internal/artifacts/canvasformat", "internal/artifacts/dashspec",
	"internal/jsonform",
}

// A STORED VERSION IS NEVER HELD TO THE PUBLISH READ (ADR-0059). The deck, doc,
// canvas and dashboard indexes match their keys exactly and refuse a key
// written twice only when a publish writes them; every read of a version
// already stored (a render, get_artifact, the dashboard's data, a compare)
// goes through the stored read, which a fuzz holds to encoding/json's answer.
// This walks the tree for a strict read anywhere else.
//
// Mutation: call slideformat.Check from get_artifact and this names the call.
func TestOnlyThePublishReadsAVersionStrictly(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	fset := token.NewFileSet()
	walked := 0
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		dir := filepath.ToSlash(filepath.Dir(rel))
		if slices.Contains(mayReadStrictly, dir) {
			return nil
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		walked++
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if ok && slices.Contains(strictReads[pkg.Name], sel.Sel.Name) {
				t.Errorf("%s: %s.%s reads a version strictly outside a publish; a stored "+
					"version is read with the format's Read", fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked < 100 {
		t.Fatalf("walked %d files, which is not the tree", walked)
	}
}
