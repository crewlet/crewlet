package tools_test

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

// forwarders are the wrappers that forward a call to the tool they wrap,
// each reached only through [tools.Invoke] itself: builtin's authority gate
// decides the call, then hands it on.
var forwarders = map[string][]string{
	"internal/agent/builtin/authority.go": {"gated", "gatedSeat", "gatedDetached"},
}

// EVERY TOOL IS CALLED THROUGH tools.Invoke (ADR-0059): a tool's Call,
// CallForTurn or CallDetached invoked anywhere else is a caller that skips the
// check of a call against its tool's schema, as the operator surface did
// before it was routed here. Allowed besides the one call site in invoke.go:
// a tool's own method delegating to its other variant (`t.CallForTurn` inside
// `func (t *x) Call`), and a wrapper forwarding to the tool it wraps.
//
// Mutation: call `tool.Call` from operator.Server.run and this names it.
func TestEveryToolIsCalledThroughInvoke(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	fset := token.NewFileSet()
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
		file, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		walked++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			receiver, receiverType := receiverOf(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !toolCall(call) {
					return true
				}
				sel := call.Fun.(*ast.SelectorExpr)
				switch {
				case rel == "internal/tools/invoke.go" && fn.Name.Name == "call":
				case isIdent(sel.X, receiver) && strings.HasPrefix(fn.Name.Name, "Call"):
					// A tool delegating to its own other variant.
				case slices.Contains(forwarders[rel], receiverType):
					// A wrapper forwarding to the tool it wraps.
				default:
					t.Errorf("%s: a tool's %s is called outside tools.Invoke, which skips the check of "+
						"the call against its schema", fset.Position(call.Pos()), sel.Sel.Name)
				}
				return true
			})
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

// toolCall reports a call shaped as a tool's own call: Call(ctx, args),
// CallForTurn(ctx, turn, args) or CallDetached(ctx, turn, args).
func toolCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch sel.Sel.Name {
	case "Call":
		return len(call.Args) == 2
	case "CallForTurn", "CallDetached":
		return len(call.Args) == 3
	}
	return false
}

// receiverOf is a method's receiver name and its type's name.
func receiverOf(fn *ast.FuncDecl) (string, string) {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return "", ""
	}
	field := fn.Recv.List[0]
	name := ""
	if len(field.Names) > 0 {
		name = field.Names[0].Name
	}
	typ := field.Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if index, ok := typ.(*ast.IndexExpr); ok {
		typ = index.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return name, id.Name
	}
	return name, ""
}

// isIdent reports whether x is the identifier name (and name is not empty).
func isIdent(x ast.Expr, name string) bool {
	id, ok := x.(*ast.Ident)
	return ok && name != "" && id.Name == name
}
