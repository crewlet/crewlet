package engine

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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
var perSeatGroups = []string{"AgentInboxGroup", "AgentControlGroup", "AgentReflectGroup"}

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
	subscribed, unreadable := fleetSubscriptions(t, sourcetree.Root(t))
	for _, where := range unreadable {
		t.Errorf("%s subscribes a group this gate cannot read; name it with a "+
			"constant so it can be held to internal/engine/groups.go", where)
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

// subscribeMethods are the queue verbs that attach a consumer group, which
// is what the gate above reads the source for — and, being identifiers, what
// a file must spell to make such a call at all.
var subscribeMethods = []string{"Subscribe", "SubscribeBatch"}

// fleetSubscriptions is every fleet-wide group the Go source under root's
// internal/ subscribes, with where, and where a call's group cannot be read.
//
// IN TWO PASSES, because parsing the whole of internal/ to find a handful of
// calls was nine seconds under the race detector. The first parses only the
// files that spell a subscribe verb (sourcetree.Identifiers) — every caller
// is among them. The second parses every file of the only two places a
// caller's group constant is looked up: the caller's own package, and each of
// this module's packages it imports. So every constant the gate could resolve
// before, it resolves now.
func fleetSubscriptions(t *testing.T, root string) (map[string][]string, []string) {
	t.Helper()
	fset := token.NewFileSet()
	parsed := map[string]*ast.File{}
	parse := func(f moduleFile) *ast.File {
		if file, ok := parsed[f.path]; ok {
			return file
		}
		file, err := parser.ParseFile(fset, f.path, f.body, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f.rel, err)
		}
		parsed[f.path] = file
		return file
	}

	type caller struct {
		moduleFile
		ast     *ast.File
		imports map[string]string // local name -> dir
	}
	verbs := sourcetree.MustIdentifiers(subscribeMethods...)
	files := moduleFiles(t, root)
	var callers []caller
	lookIn := map[string]bool{} // directories whose constants a group may name
	for _, f := range files {
		if !f.under("internal") || f.under("internal/queue") || !verbs.In(f.body) {
			continue
		}
		c := caller{moduleFile: f, ast: parse(f), imports: map[string]string{}}
		for _, imp := range c.ast.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if !strings.HasPrefix(path, modulePath+"/") {
				continue
			}
			name := filepath.Base(path)
			if imp.Name != nil {
				name = imp.Name.Name
			}
			c.imports[name] = filepath.Join(root, strings.TrimPrefix(path, modulePath+"/"))
			lookIn[c.imports[name]] = true
		}
		lookIn[c.dir()] = true
		callers = append(callers, c)
	}

	consts := map[string]string{} // <dir>.<Name> -> value
	for _, f := range files {
		if !f.under("internal") || !lookIn[f.dir()] {
			continue
		}
		for _, decl := range parse(f).Decls {
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
							consts[f.dir()+"."+name.Name] = s
						}
					}
				}
			}
		}
	}

	subscribed := map[string][]string{} // group -> where
	var unreadable []string
	for _, c := range callers {
		for _, decl := range c.ast.Decls {
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
				if !ok || !slices.Contains(subscribeMethods, sel.Sel.Name) || len(call.Args) < 4 {
					return true
				}
				where := fset.Position(call.Pos()).String()
				group, perSeat, ok := resolveGroup(call.Args[2], c.dir(), c.imports, consts, fn)
				switch {
				case !ok:
					unreadable = append(unreadable, where)
				case !perSeat:
					subscribed[group] = append(subscribed[group], where)
				}
				return true
			})
		}
	}
	return subscribed, unreadable
}

// THE GATE ABOVE, ON A TREE WHOSE VERDICT IS KNOWN — through the same read,
// prefilter and two passes.
//
// The second pass is the one a prefilter could quietly break: a group named
// by a constant in a file of its own, which spells no subscribe verb and so
// is never a caller, must still be read. So the planted constants live in
// exactly such files — one in the caller's own package, one in a package it
// imports — beside a literal, a seat's own group, a group no constant names,
// the queue package's own subscriptions, and a file that does not even parse
// and names no verb, which the gate must never open.
func TestTheGroupGateReadsAConstantFromAFileThatSubscribesNothing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"internal/work/work.go": "package work\n\n" +
			"import \"" + modulePath + "/internal/names\"\n\n" +
			"func run(q Q, unknown string) {\n" +
			"\tq.Subscribe(ctx, \"t\", names.Imported, h)\n" +
			"\tq.Subscribe(ctx, \"t\", local, h)\n" +
			"\tq.SubscribeBatch(ctx, \"t\", \"literal-group\", h, 8)\n" +
			"\tseat := topics.AgentInboxGroup(\"alice\")\n" +
			"\tq.Subscribe(ctx, \"t\", seat, h)\n" +
			"\tq.Subscribe(ctx, \"t\", unknown, h)\n" +
			"}\n",
		"internal/work/consts.go":      "package work\n\nconst local = \"local-group\"\n",
		"internal/names/names.go":      "package names\n\nconst Imported = \"imported-group\"\n",
		"internal/queue/memory/sub.go": "package memory\n\nfunc f(q Q) { q.Subscribe(ctx, \"t\", \"queue-own\", h) }\n",
		"internal/broken/broken.go":    "package broken\n\nthis is not Go\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	subscribed, unreadable := fleetSubscriptions(t, root)
	var groups []string
	for group := range subscribed {
		groups = append(groups, group)
	}
	slices.Sort(groups)
	if want := []string{"imported-group", "literal-group", "local-group"}; !slices.Equal(groups, want) {
		t.Errorf("subscribed groups = %q, want %q", groups, want)
	}
	if len(unreadable) != 1 || !strings.Contains(unreadable[0], "work.go:11") {
		t.Errorf("unreadable = %q, want the one call naming a variable, at work.go:11", unreadable)
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
