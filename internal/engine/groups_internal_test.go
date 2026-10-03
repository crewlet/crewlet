package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// modulePath is this module's import path, which a selector's package is
// resolved under.
const modulePath = "github.com/crewlet/crewlet"

// perSeatGroups are the `topics` functions that mint a seat's OWN group —
// attached only by the seat's holder, so never fleet-wide.
var perSeatGroups = []string{"AgentInboxGroup", "AgentControlGroup"}

// EVERY FLEET-WIDE GROUP THE TREE SUBSCRIBES IS DECLARED, AND EVERY DECLARED
// GROUP IS SUBSCRIBED — the two directions of groups.go's table.
//
// The first is the one that keeps a node without `data` honest: a group
// somebody subscribed without declaring is attached on every node, and the
// question of whether its handler needs data was never asked. The second
// keeps the table true: a declaration nothing subscribes is one a reader
// trusts about a group that no longer exists.
//
// Read off the SOURCE, every Subscribe and SubscribeBatch call outside the
// queue package itself (its backends and its suite subscribe whatever a case
// names). A group argument must resolve — to a declared constant, a literal,
// or a seat's own group from `topics` — and one that does not is a failure
// naming where it is, because a group this test cannot read is one it cannot
// hold to the table.
func TestEveryFleetGroupSaysWhetherItNeedsData(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	type file struct {
		dir  string
		path string
		ast  *ast.File
	}
	fset := token.NewFileSet()
	var files []file
	consts := map[string]string{} // <dir>.<Name> -> value
	err := sourcetree.Walk(filepath.Join(root, "internal"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		parsed, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		dir := filepath.Dir(path)
		files = append(files, file{dir: dir, path: path, ast: parsed})
		for _, decl := range parsed.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				value, ok := spec.(*ast.ValueSpec)
				if !ok || len(value.Names) != len(value.Values) {
					continue
				}
				for i, name := range value.Names {
					if lit, ok := value.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						if s, err := strconv.Unquote(lit.Value); err == nil {
							consts[dir+"."+name.Name] = s
						}
					}
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}

	queueDir := filepath.Join(root, "internal", "queue") + string(filepath.Separator)
	subscribed := map[string][]string{} // group -> where
	for _, f := range files {
		if strings.HasPrefix(f.path+string(filepath.Separator), queueDir) || strings.HasPrefix(f.path, queueDir) {
			continue
		}
		imports := map[string]string{} // local name -> dir
		for _, imp := range f.ast.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(path, modulePath+"/") {
				continue
			}
			name := filepath.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			imports[name] = filepath.Join(root, strings.TrimPrefix(path, modulePath+"/"))
		}
		for _, decl := range f.ast.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || (sel.Sel.Name != "Subscribe" && sel.Sel.Name != "SubscribeBatch") || len(call.Args) < 4 {
					return true
				}
				where := fset.Position(call.Pos()).String()
				group, perSeat, ok := resolveGroup(call.Args[2], f.dir, imports, consts, fn)
				switch {
				case !ok:
					t.Errorf("%s subscribes a group this gate cannot read; name it with a "+
						"constant so it can be held to internal/engine/groups.go", where)
				case !perSeat:
					subscribed[group] = append(subscribed[group], where)
				}
				return true
			})
		}
	}
	if len(subscribed) == 0 {
		t.Fatal("no fleet-wide subscription was found, so this gate is reading a tree it cannot see")
	}

	declared := map[string]bool{}
	for _, g := range fleetGroups {
		declared[g.Name] = true
		if strings.TrimSpace(g.Why) == "" {
			t.Errorf("group %q is declared with no reason", g.Name)
		}
		if _, ok := subscribed[g.Name]; !ok {
			t.Errorf("group %q is declared and nothing in the tree subscribes it", g.Name)
		}
	}
	for group, where := range subscribed {
		if !declared[group] {
			t.Errorf("group %q is subscribed at %v and does not say whether it needs "+
				"data: declare it in internal/engine/groups.go", group, where)
		}
	}
}

// resolveGroup reads a Subscribe call's group argument: its value, or that it
// is a seat's own group.
func resolveGroup(expr ast.Expr, dir string, imports map[string]string, consts map[string]string,
	fn *ast.FuncDecl) (group string, perSeat, ok bool) {

	switch x := expr.(type) {
	case *ast.BasicLit:
		value, err := strconv.Unquote(x.Value)
		return value, false, err == nil
	case *ast.Ident:
		if value, found := consts[dir+"."+x.Name]; found {
			return value, false, true
		}
		// A LOCAL bound to a seat's own group in the same function.
		var seat bool
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			assign, isAssign := n.(*ast.AssignStmt)
			if !isAssign || len(assign.Lhs) != len(assign.Rhs) {
				return true
			}
			for i, lhs := range assign.Lhs {
				if id, isIdent := lhs.(*ast.Ident); isIdent && id.Name == x.Name && mintsSeatGroup(assign.Rhs[i]) {
					seat = true
				}
			}
			return true
		})
		return "", seat, seat
	case *ast.SelectorExpr:
		pkg, isIdent := x.X.(*ast.Ident)
		if !isIdent {
			return "", false, false
		}
		value, found := consts[imports[pkg.Name]+"."+x.Sel.Name]
		return value, false, found
	case *ast.CallExpr:
		return "", true, mintsSeatGroup(x)
	}
	return "", false, false
}

// mintsSeatGroup reports whether expr is a call to one of the `topics`
// functions that mint a seat's own group.
func mintsSeatGroup(expr ast.Expr) bool {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "topics" && slices.Contains(perSeatGroups, sel.Sel.Name)
}

// A NODE WITHOUT `data` JOINS NO GROUP THAT NEEDS IT, a data node joins every
// group, and NOBODY joins a group the table does not name — the door every
// fleet-wide attachment passes.
func TestANodeWithoutDataJoinsNoGroupThatNeedsData(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	stateless, data := &Engine{}, &Engine{local: &localEstate{}}
	var needs, needsNot int
	for _, g := range declaredFleetGroups() {
		if g.Data {
			needs++
		} else {
			needsNot++
		}
		if got := stateless.joins(ctx, g.Name); got == g.Data {
			t.Errorf("a node without data joins %q = %v, and the group needs data = %v",
				g.Name, got, g.Data)
		}
		if !data.joins(ctx, g.Name) {
			t.Errorf("a data node does not join %q", g.Name)
		}
	}
	if needs == 0 || needsNot == 0 {
		t.Fatalf("%d declared group(s) need data and %d do not; with either at zero "+
			"this case cannot tell the door from a constant", needs, needsNot)
	}
	for _, e := range []*Engine{stateless, data} {
		if e.joins(ctx, "a-group-nobody-declared") {
			t.Error("an undeclared group was joined")
		}
	}
}
