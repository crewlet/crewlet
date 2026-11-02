package provenance

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// textShapes is what a field declared somebody's words may hold, by kind: a
// fence is one string, and a name is a line, a list of them, or a map whose
// entries hold text — the shapes a reader can neutralise in place.
var textShapes = map[string][]string{
	FieldFence: {"string"},
	FieldName: {"string", "[]string", "map[string]string", "map[string]any",
		"map[string]json.RawMessage", "json.RawMessage"},
}

// EVERY FIELD DECLARED SOMEBODY'S WORDS IS DECLARED AS ONE OF THE TWO KINDS,
// ON A FIELD THAT HOLDS TEXT. The tag is read at run time by whatever answers
// the record, and a misspelt kind or a fence on a list is found there only
// when a test happens to answer that field — so the walk reads every struct
// in the tree for the tag and holds each to [FieldKinds] and to [textShapes].
func TestEveryDeclaredFieldHoldsText(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	declared := 0
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); name == "testdata" || name == "node_modules" ||
				name == "dashboard" || name == "static" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if field.Tag == nil {
					continue
				}
				raw, err := strconv.Unquote(field.Tag.Value)
				if err != nil {
					continue
				}
				kind, ok := reflect.StructTag(raw).Lookup(FieldTag)
				if !ok {
					continue
				}
				declared++
				name := "an embedded field"
				if len(field.Names) > 0 {
					name = field.Names[0].Name
				}
				shapes, known := textShapes[kind]
				if !known || !slices.Contains(FieldKinds, kind) {
					t.Errorf("%s: %s is declared %s:%q, which is neither of %v", rel, name,
						FieldTag, kind, FieldKinds)
					continue
				}
				if shape := types.ExprString(field.Type); !slices.Contains(shapes, shape) {
					t.Errorf("%s: %s is declared %s:%q and holds %s, which a reader cannot %s",
						rel, name, FieldTag, kind, shape, map[string]string{
							FieldFence: "fence: a fence is one string",
							FieldName:  "neutralise in place",
						}[kind])
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking the tree: %v", err)
	}
	if declared < 30 {
		t.Fatalf("found %d declared fields — the walk certified nothing", declared)
	}
}
