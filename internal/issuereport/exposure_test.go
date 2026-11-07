package issuereport

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

// isToolCall reports whether a declaration is a tool's Call: a method taking a
// context and the model's arguments, the shape every tool the engine offers has.
func isToolCall(f *ast.FuncDecl) bool {
	if f.Recv == nil || f.Name.Name != "Call" || f.Type.Params == nil || f.Type.Results == nil {
		return false
	}
	var params []ast.Expr
	for _, field := range f.Type.Params.List {
		for range max(len(field.Names), 1) {
			params = append(params, field.Type)
		}
	}
	if len(params) != 2 || len(f.Type.Results.List) != 2 {
		return false
	}
	ctx, ok := params[0].(*ast.SelectorExpr)
	if !ok || ctx.Sel.Name != "Context" {
		return false
	}
	args, ok := params[1].(*ast.MapType)
	if !ok {
		return false
	}
	key, keyOK := args.Key.(*ast.Ident)
	value, valueOK := args.Value.(*ast.Ident)
	_, empty := args.Value.(*ast.InterfaceType)
	return keyOK && key.Name == "string" && (valueOK && value.Name == "any" || empty)
}

// EVERY PACKAGE THAT DECLARES A TOOL IS ONE A CRASH IN IS HELD: a tool runs on
// arguments a model chose, so a fatal crash under one may be steered by whoever
// can put text in front of a seat. Walked over the source, so a tool declared in
// a new package fails here until that package is named.
//
// Mutation: drop internal/agent/subagent from the set, and this goes red.
func TestEveryPackageThatDeclaresAToolExecutesToolCalls(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	declaring := map[Component]bool{}
	err := sourcetree.Walk(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		if slices.ContainsFunc(file.Decls, func(decl ast.Decl) bool {
			f, ok := decl.(*ast.FuncDecl)
			return ok && isToolCall(f)
		}) {
			rel, err := filepath.Rel(root, filepath.Dir(path))
			if err != nil {
				return err
			}
			declaring[Component(filepath.ToSlash(rel))] = true
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !declaring["internal/agent/builtin"] || len(declaring) < 3 {
		t.Fatalf("the walk found tools in %v, so it certifies nothing", declaring)
	}
	for pkg := range declaring {
		if !ExecutesToolCalls(pkg) && !BeforeAuthentication(pkg) {
			t.Errorf("%s declares a tool and is not in executesToolCalls: a crash there would be filed publicly", pkg)
		}
	}
}
