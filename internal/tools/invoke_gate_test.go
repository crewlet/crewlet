package tools_test

import (
	"fmt"
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
	"internal/agent/builtin/authority.go": {"gated", "gatedSeat"},
}

// EVERY TOOL IS CALLED THROUGH tools.Invoke (ADR-0059): a tool's Call or
// CallForTurn invoked anywhere else is a caller that skips the check of a
// call against its tool's schema, as the operator surface did before it was
// routed here. Allowed besides the one call site in invoke.go:
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
		for _, found := range outsideInvoke(fset, file, rel) {
			t.Error(found)
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

// outsideInvoke is every place file reaches a tool's own call other than
// through [tools.Invoke], in each shape Go has for it: a call
// (`t.Call(ctx, a)`), a method value (`f := t.Call; f(ctx, a)`) and a method
// expression (`tools.Callable.Call(t, ctx, a)`).
func outsideInvoke(fset *token.FileSet, file *ast.File, rel string) []string {
	var found []string
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		receiver, receiverType := receiverOf(fn)
		// called is each selector a call is made through; bound is each
		// name a tool's method value is bound to, and boundCalled the calls
		// later made through that name, by the tool's arity.
		called := map[*ast.SelectorExpr]*ast.CallExpr{}
		bound := map[string]*ast.SelectorExpr{}
		boundCalled := map[*ast.SelectorExpr]bool{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
					called[sel] = n
				}
			case *ast.AssignStmt:
				for i, rhs := range n.Rhs {
					sel, ok := rhs.(*ast.SelectorExpr)
					if !ok || i >= len(n.Lhs) {
						continue
					}
					if _, named := toolMethods[sel.Sel.Name]; !named {
						continue
					}
					if id, ok := n.Lhs[i].(*ast.Ident); ok {
						bound[id.Name] = sel
					}
				}
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok {
				if sel, held := bound[id.Name]; held && len(call.Args) == toolMethods[sel.Sel.Name] {
					boundCalled[sel] = true
				}
			}
			return true
		})
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			arity, named := toolMethods[sel.Sel.Name]
			if !named {
				return true
			}
			shape := "method value"
			if call, isCall := called[sel]; isCall {
				switch {
				case len(call.Args) == arity:
					shape = "call"
				case len(call.Args) == arity+1 && typeExpr(sel.X):
					shape = "method expression"
				default:
					return true // another API's method of the same name
				}
			} else if sel.Sel.Name == "Call" && !boundCalled[sel] {
				// A field or another API's method named Call, read as a
				// value: a tool's Call is a method value only where the
				// value is called as one.
				return true
			}
			switch {
			case rel == "internal/tools/invoke.go" && fn.Name.Name == "call" && shape == "call":
			case shape == "call" && isIdent(sel.X, receiver) && strings.HasPrefix(fn.Name.Name, "Call"):
				// A tool delegating to its own other variant.
			case shape == "call" && slices.Contains(forwarders[rel], receiverType):
				// A wrapper forwarding to the tool it wraps.
			default:
				found = append(found, fmt.Sprintf("%s: a tool's %s is reached as a %s outside "+
					"tools.Invoke, which skips the check of the call against its schema",
					fset.Position(sel.Pos()), sel.Sel.Name, shape))
			}
			return true
		})
	}
	return found
}

// toolMethods are a tool's own calls by the arguments each takes:
// Call(ctx, args) and CallForTurn(ctx, turn, args).
var toolMethods = map[string]int{"Call": 2, "CallForTurn": 3}

// typeExpr reports x names a type, as a method expression's receiver does:
// an exported name (`Callable`, `tools.Callable`), a pointer type or a
// parenthesised one.
func typeExpr(x ast.Expr) bool {
	switch x := x.(type) {
	case *ast.Ident:
		return ast.IsExported(x.Name)
	case *ast.SelectorExpr:
		return ast.IsExported(x.Sel.Name)
	case *ast.ParenExpr:
		return typeExpr(x.X)
	case *ast.StarExpr:
		return true
	case *ast.InterfaceType:
		return true
	}
	return false
}

// THE GATE SEES EVERY SHAPE (#423 review): a method value and a method
// expression reach a tool's call as surely as a call does, and the first
// version of the gate matched only the call.
//
// Mutation: drop the method value or the method expression from
// outsideInvoke and its fixture is not named.
func TestTheInvokeGateSeesEveryShape(t *testing.T) {
	t.Parallel()
	for name, src := range map[string]string{
		"call":              `t.Call(ctx, args)`,
		"method value":      `f := t.Call; f(ctx, args)`,
		"method expression": `tools.Callable.Call(t, ctx, args)`,
		"seat method value": `g := t.CallForTurn; g(ctx, turn, args)`,
		"pointer method":    `(*tool).CallForTurn(t, ctx, turn, args)`,
	} {
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go",
			"package fixture\nfunc run() {\n"+src+"\n}\n", 0)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if found := outsideInvoke(fset, file, "internal/fixture/fixture.go"); len(found) != 1 {
			t.Errorf("%s (%s) was named %d times: %v", name, src, len(found), found)
		}
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go",
		"package fixture\nfunc run() { client.Call(a, b, c); rpc.Call(a) }\n", 0)
	if err != nil {
		t.Fatal(err)
	}
	if found := outsideInvoke(fset, file, "internal/fixture/fixture.go"); len(found) != 0 {
		t.Errorf("another API's Call was named: %v", found)
	}
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
