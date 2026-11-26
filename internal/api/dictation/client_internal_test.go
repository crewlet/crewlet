package dictation

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/clientsource"
)

// THE DASHBOARD READS THIS SOCKET IN THE ENGINE'S WORDS: why the viewer says
// dictation is unavailable, and how a dictation ended. Read from
// dashboard/src/contract/dictation.ts and held both ways against the declared
// constants, so a reason added here is one the composer can say.
func TestTheDashboardReadsTheDictationSocket(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for _, c := range []struct {
		union  string
		engine []string
	}{
		{"DictationReason", constants(t, filepath.Join("..", "..", "speech"), func(s *ast.ValueSpec, _ string) bool {
			id, ok := s.Type.(*ast.Ident)
			return ok && id.Name == "Reason"
		})},
		{"DictationDoneReason", constants(t, ".", func(_ *ast.ValueSpec, name string) bool {
			return strings.HasPrefix(name, "done")
		})},
	} {
		client, err := clientsource.Union(tree, c.union)
		if err != nil {
			t.Fatalf("%s: %v", c.union, err)
		}
		slices.Sort(client)
		if !slices.Equal(client, c.engine) {
			t.Errorf("the dashboard's %s is %v and the engine declares %v", c.union, client, c.engine)
		}
	}
}

// constants answers the sorted string values of the package's declared
// constants that keep says to.
func constants(t *testing.T, dir string, keep func(*ast.ValueSpec, string) bool) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if i >= len(vs.Values) || !keep(vs, id.Name) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, _ := strconv.Unquote(lit.Value)
					out = append(out, value)
				}
			}
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s declares no constant to hold the dashboard to", dir)
	}
	slices.Sort(out)
	return out
}
