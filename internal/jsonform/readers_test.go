package jsonform_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

const module = "github.com/crewlet/crewlet/"

// strictReads are the format reads that hold a version to its format as
// written today, by import path: what a publish writes, never what a read of a
// stored version opens ([jsonform.Stored] is that read's).
var strictReads = map[string][]string{
	module + "internal/artifacts/slideformat":  {"Check", "ParseIndex"},
	module + "internal/artifacts/docindex":     {"Check", "ParseIndex"},
	module + "internal/artifacts/canvasformat": {"Check", "Parse"},
	module + "internal/artifacts/dashspec":     {"Check", "Parse"},
	module + "internal/jsonform":               {"Decode"},
}

// mayReadStrictly are where a strict read belongs: the one file of the publish
// pipeline that judges what a version will be, and the formats themselves
// (brand.json was exact before ADR-0059, on every read).
var mayReadStrictly = []string{
	"internal/artifactupload/format.go",
	"internal/artifacts/brandformat/", "internal/artifacts/slideformat/",
	"internal/artifacts/docindex/", "internal/artifacts/canvasformat/",
	"internal/artifacts/dashspec/", "internal/jsonform/",
}

// A STORED VERSION IS NEVER HELD TO THE PUBLISH READ (ADR-0059). The deck, doc,
// canvas and dashboard indexes match their keys exactly and refuse a key
// written twice only when a publish writes them; every read of a version
// already stored (a render, get_artifact, the dashboard's data, a compare)
// goes through the format's Read. This walks the tree, resolving each file's
// imports by path so an alias or a dot import is no way round it.
//
// Mutation: call slideformat.Check from get_artifact, under any import name,
// and this names the call.
func TestOnlyThePublishReadsAVersionStrictly(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	walked := 0
	err := sourcetree.Walk(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "node_modules" || name == "testdata" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(p) != ".go" || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if slices.ContainsFunc(mayReadStrictly, func(allowed string) bool {
			return rel == allowed || (strings.HasSuffix(allowed, "/") && strings.HasPrefix(rel, allowed) &&
				!strings.Contains(strings.TrimPrefix(rel, allowed), "/"))
		}) {
			return nil
		}
		walked++
		for _, finding := range strictCalls(t, p) {
			t.Errorf("%s: %s", rel, finding)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked < 100 {
		t.Fatalf("walked %d files, which is not the tree", walked)
	}
}

// strictCalls are the strict reads one file names, each as "line: pkg.Func".
func strictCalls(t *testing.T, p string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, p, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	local := map[string]string{} // import name in this file to import path
	var out []string
	for _, spec := range file.Imports {
		ip, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		if _, strict := strictReads[ip]; !strict {
			continue
		}
		name := path.Base(ip)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "." {
			out = append(out, fmt.Sprintf("%d: imports %s with a dot, which hides its strict reads",
				fset.Position(spec.Pos()).Line, ip))
			continue
		}
		local[name] = ip
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if ip, imported := local[id.Name]; imported && slices.Contains(strictReads[ip], sel.Sel.Name) {
			out = append(out, fmt.Sprintf("%d: %s.%s reads a version strictly outside a publish; "+
				"a stored version is read with the format's Read", fset.Position(sel.Pos()).Line,
				path.Base(ip), sel.Sel.Name))
		}
		return true
	})
	return out
}
