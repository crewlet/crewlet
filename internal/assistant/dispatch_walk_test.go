package assistant_test

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

// THE ASSISTANT SENDS THROUGH THE DISPATCHER AND NOTHING ELSE (ADR-0050).
//
// The import walk keeps every other way into the company out of reach; this
// keeps the assistant from growing a transport of its own beside the one it is
// handed: no package under internal/assistant (its test support aside) builds
// an HTTP client, dials a socket or calls a request's Do on anything but the
// dispatcher, so every call it makes meets the guard, the authority table and
// the person.
//
// Mutation: add `http.DefaultClient.Do(req)` to the surface and this names
// it.
func TestTheAssistantSendsThroughTheDispatcherAlone(t *testing.T) {
	t.Parallel()
	root := filepath.Join(sourcetree.Root(t), "internal", "assistant")
	banned := map[string]bool{"http.Client": true, "http.DefaultClient": true,
		"http.Get": true, "http.Post": true, "http.NewRequest": true,
		"http.NewRequestWithContext": true, "net.Dial": true, "net.DialTimeout": true}
	walked, doCalls := 0, 0
	err := sourcetree.Walk(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") ||
			strings.HasSuffix(path, "_test.go") ||
			strings.Contains(path, string(filepath.Separator)+"assistanttest"+string(filepath.Separator)) {
			return err
		}
		fset := token.NewFileSet()
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
			if pkg, ok := sel.X.(*ast.Ident); ok && banned[pkg.Name+"."+sel.Sel.Name] {
				t.Errorf("%s: %s.%s is a transport beside the dispatcher",
					fset.Position(sel.Pos()), pkg.Name, sel.Sel.Name)
			}
			if sel.Sel.Name == "Do" {
				doCalls++
				if inner, ok := sel.X.(*ast.SelectorExpr); !ok ||
					!strings.EqualFold(inner.Sel.Name, "dispatcher") {
					t.Errorf("%s: a Do call on something other than the dispatcher",
						fset.Position(sel.Pos()))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if walked < 10 || doCalls == 0 {
		t.Fatalf("walked %d files and found %d dispatcher calls, so the walk reads "+
			"nothing it guards", walked, doCalls)
	}
}
