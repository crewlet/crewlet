package tabular_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// THE DATASTORE EXPORT WRITES THROUGH TABULAR, and so does every other table
// the engine hands a person (ADR-0008).
//
// The formula escape is only one rule while there is only one writer: a second
// `csv.NewWriter` anywhere in the tree is a download that can forget it. So
// this walks every non-test Go file, fails on a CSV writer built outside this
// package, and holds the datastore's export to building its writer here.
// Mutation: put `csv.NewWriter` back in internal/artifactdata's export and this
// names the file.
func TestTheDatastoreExportWritesThroughTabular(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	read, exportUsesTabular := 0, false
	err := sourcetree.Walk(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "node_modules", "static":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			return err
		}
		read++
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case pkg.Name == "csv" && sel.Sel.Name == "NewWriter" &&
				!strings.HasPrefix(rel, "internal/tabular/"):
				t.Errorf("%s builds a CSV writer of its own: write the table through "+
					"internal/tabular, which neutralises a formula cell", rel)
			case pkg.Name == "tabular" && sel.Sel.Name == "NewCSV" &&
				rel == "internal/artifactdata/export.go":
				exportUsesTabular = true
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if read < 500 {
		t.Fatalf("the walk read %d Go files, so it is not reading the tree it guards", read)
	}
	if !exportUsesTabular {
		t.Error("internal/artifactdata/export.go does not write through tabular.NewCSV")
	}
}
