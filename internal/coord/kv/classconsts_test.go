package kv

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unsafe"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// EVERY KEY CLASS THE PACKAGE WRITES WITH IS DECLARED BY THE BUCKET ITS KEY
// REACHES, read off the source: each key a coord function or method builds is
// followed through helpers, variables, lists, maps, views and returns to where
// it ends ([classGate.reach]); every bucket it reaches must declare its class,
// a segment this reading cannot read declared only by a wildcard. A way on it
// cannot follow fails unless [gateUnread] names the place and why.
//
// Mutation: drop ConversationHoldClass from positionsHolding, SaveLedgerClass
// from uploadsHolding or the integration status from integrationsHolding; put
// a session claim's or a node status row's key into the atlas sources through
// a helper, a channel, a record, a list's index, a map's key, a Sprintf or a
// builder value; or give the SSH tests' entryClass another filter or key: red.
func TestEveryKeyClassThePackageWritesWithIsDeclaredForItsBucket(t *testing.T) {
	t.Parallel()
	g := newClassGate(t, ".")
	g.check(t)
}

// gateUnread is every place a key goes on that this reading cannot follow, by
// file and enclosing function, with why it is no key of a bucket there.
var gateUnread = map[string]string{
	"casline.go report": "the test seam's observer, set only by tests, which counts a batch by its key",
}

// classGate is one reading of this package's source against its holdings.
type classGate struct {
	fset     *token.FileSet
	coord    coordClasses
	local    map[string]string // this package's own string constants
	holdings map[string]holding
	funcs    map[string][]*ast.FuncDecl
	vars     map[string]string // the type of each package variable this reading can tell
	decls    []ast.Decl
	excused  map[string]bool // gateUnread's entries this reading used
	calls    map[*ast.FuncDecl][]called
	fields   map[string]string // each struct field's type, by "Type.field"
}

// classRef is one key a declaration builds on a class, and where.
type classRef struct {
	at, what string
	samples  []string // keys of its class, one per class it may build
	node     ast.Node
	fn       *ast.FuncDecl // nil for a package-level declaration
	builder  bool          // a coord builder named as a value, followed to its calls
}

func newClassGate(t *testing.T, dir string) *classGate {
	t.Helper()
	g := &classGate{fset: token.NewFileSet(), coord: coordClassValues(t), local: map[string]string{},
		holdings: fleetHoldingsByField(t), funcs: map[string][]*ast.FuncDecl{}, vars: map[string]string{},
		excused: map[string]bool{}, fields: map[string]string{}}
	for _, file := range parsePackage(t, g.fset, dir) {
		for _, decl := range file.Decls {
			g.decls = append(g.decls, decl)
			if fn, ok := decl.(*ast.FuncDecl); ok {
				g.funcs[fn.Name.Name] = append(g.funcs[fn.Name.Name], fn)
			}
			for name, v := range stringConsts(decl) {
				g.local[name] = v
			}
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.TYPE {
				for _, spec := range gen.Specs {
					ts := spec.(*ast.TypeSpec)
					if st, ok := ts.Type.(*ast.StructType); ok {
						for _, field := range st.Fields.List {
							for _, name := range field.Names {
								g.fields[ts.Name.Name+"."+name.Name] = baseType(field.Type)
							}
						}
					}
				}
			}
			if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.VAR {
				for _, spec := range gen.Specs {
					vs := spec.(*ast.ValueSpec)
					for i, name := range vs.Names {
						switch {
						case vs.Type != nil:
							g.vars[name.Name] = baseType(vs.Type)
						case i < len(vs.Values):
							if lit, ok := vs.Values[i].(*ast.CompositeLit); ok {
								g.vars[name.Name] = baseType(lit.Type)
							}
						}
					}
				}
			}
		}
	}
	return g
}

// excuse reports whether gateUnread names where n is in fn, marking it used.
func (g *classGate) excuse(fn *ast.FuncDecl, n ast.Node) bool {
	file := filepath.Base(g.fset.Position(n.Pos()).Filename)
	for _, key := range []string{file, file + " " + funcLabel(fn)} {
		if _, ok := gateUnread[key]; ok {
			g.excused[key] = true
			return true
		}
	}
	return false
}

// funcLabel is fn's name, "" for none.
func funcLabel(fn *ast.FuncDecl) string {
	if fn == nil {
		return ""
	}
	return fn.Name.Name
}

// check fails t for every key that reaches a bucket not declaring its class,
// every way a key goes on that this reading cannot follow and every class it
// cannot read, but those gateUnread names.
func (g *classGate) check(t *testing.T) {
	t.Helper()
	entryBuckets := map[string]bool{}
	for _, decl := range g.decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && receiverIs(fn, "entryClass") {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				for _, b := range g.bucketsOf(frame{fn: fn}, n) {
					entryBuckets[b] = true
				}
				return true
			})
		}
	}
	if len(entryBuckets) != 1 {
		t.Fatalf("premise: entryClass's verbs touch %v, want the one bucket every class of it writes", entryBuckets)
	}
	reached := map[string]bool{}
	entries, keys := 0, 0
	for _, decl := range g.decls {
		for _, r := range g.refsIn(t, decl) {
			var f flow
			switch {
			case r.fn == nil:
				entries++
				f.buckets = entryBuckets
			case r.builder:
				f = g.reachBuilder(r.fn, r.node, 0, map[string]bool{})
			default:
				f = g.reach(frame{fn: r.fn}, r.node, 0, map[string]bool{})
			}
			for _, where := range f.lost {
				t.Errorf("%s builds %s, which goes on at %s where this reading cannot follow it: write "+
					"it through a store's bucket field or a helper of this package, or name the place in "+
					"gateUnread with why", r.at, r.what, where)
			}
			if len(f.buckets) > 0 && r.fn != nil {
				keys++
				reached[r.what] = true
			}
			for b := range f.buckets {
				for _, sample := range r.samples {
					if !declares(g.holdings[b], sample) {
						t.Errorf("%s hands %s (a key %q) to %s, which declares no class for it", r.at, r.what,
							sample, b)
					}
				}
			}
		}
	}
	for key := range gateUnread {
		if !g.excused[key] {
			t.Errorf("gateUnread names %q, which this reading never met: drop it", key)
		}
	}
	t.Logf("%d entryClass filters and %d keys followed to a bucket", entries, keys)
	if entries < 10 || keys < 60 || !reached["coord.ConversationHoldKey"] || !reached["coord.SaveLedgerKey"] ||
		!reached["coord.NodeStatusRowKey.Key"] {
		t.Fatalf("premise: %d entryClass filters and %d keys followed to a bucket (conversation holds %v, "+
			"save ledgers %v, node status rows %v), want every one in the package", entries, keys,
			reached["coord.ConversationHoldKey"], reached["coord.SaveLedgerKey"], reached["coord.NodeStatusRowKey.Key"])
	}
}

// declares reports whether h declares a class for key: its one class where
// it declares no other, else a class whose keys match it.
func declares(h holding, key string) bool {
	return len(h.classes) == 0 || slices.ContainsFunc(h.classes, func(k keyClass) bool {
		if key == anyKey {
			return k.keys == ">"
		}
		return server.SubjectMatchesFilter(key, k.keys)
	})
}

// unreadSegment stands for a segment this reading cannot read: only a class
// matching any value there (a wildcard) declares a key holding it.
const unreadSegment = "u0nread"

// anyKey stands for a key of segments this reading cannot count: only a
// bucket of one class, or one declaring every key, declares it.
const anyKey = ">"

// samplesOf is a key of each class call (coord.DocumentKey, or a filter's
// with a segment more) may build, every segment read as classesOf reads it.
func (g *classGate) samplesOf(fn *ast.FuncDecl, call *ast.CallExpr, filter bool) []string {
	alts := g.segmentsOf(fn, call, 0)
	var out []string
	for _, segs := range alts {
		if segs == nil {
			out = append(out, anyKey)
			continue
		}
		if filter {
			segs = append(segs, []string{unreadSegment})
		}
		for _, key := range product(segs) {
			if !slices.Contains(out, key) {
				out = append(out, key)
			}
		}
	}
	return out
}

// segmentsOf is call's arguments as segments, each the values it may hold
// (unreadSegment where this reading cannot tell), one list per way a spread
// argument is filled; a nil list where it cannot count them.
func (g *classGate) segmentsOf(fn *ast.FuncDecl, call *ast.CallExpr, depth int) [][][]string {
	var segs [][]string
	for i, arg := range call.Args {
		if call.Ellipsis.IsValid() && i == len(call.Args)-1 {
			param := paramOf(fn, arg)
			if param < 0 || depth > 6 {
				return [][][]string{nil}
			}
			var alts [][][]string
			for _, caller := range g.callers(fn) {
				spread := &ast.CallExpr{Args: caller.call.Args[min(param, len(caller.call.Args)):],
					Ellipsis: caller.call.Ellipsis}
				switch {
				case variadic(fn, param):
				case param >= len(caller.call.Args):
					return [][][]string{nil}
				default:
					// A slice handed whole: its literal's elements, or another spread.
					spread = &ast.CallExpr{Args: []ast.Expr{caller.call.Args[param]}, Ellipsis: 1}
					if lit, ok := caller.call.Args[param].(*ast.CompositeLit); ok {
						spread = &ast.CallExpr{Args: lit.Elts}
					}
				}
				for _, tail := range g.segmentsOf(caller.fn, spread, depth+1) {
					if tail == nil {
						alts = append(alts, nil)
						continue
					}
					alts = append(alts, append(slices.Clone(segs), tail...))
				}
			}
			if len(alts) == 0 {
				return [][][]string{nil}
			}
			return alts
		}
		vals, ok := g.classesOf(fn, arg, depth)
		if !ok {
			vals = []string{unreadSegment}
		}
		segs = append(segs, vals)
	}
	return [][][]string{segs}
}

// product is coord.DocumentKey of every choice of one value per segment.
func product(segs [][]string) []string {
	keys := [][]string{{}}
	for _, vals := range segs {
		var next [][]string
		for _, k := range keys {
			for _, v := range vals {
				next = append(next, append(slices.Clone(k), v))
			}
		}
		keys = next
	}
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = coord.DocumentKey(k...)
	}
	return out
}

// classesOf is every value the string e may hold in fn, where this reading
// can tell: a constant, coord's or this package's; every constant of a
// coord enum it is typed as; a parameter's, from every call of fn; a range's,
// over a literal list; a variable's, from its one definition.
func (g *classGate) classesOf(fn *ast.FuncDecl, e ast.Expr, depth int) ([]string, bool) {
	if v, ok := g.coord.value(e, "coord"); ok {
		return []string{v}, true
	}
	if depth > 6 || fn == nil {
		return nil, false
	}
	if enum := g.coord.enum(g.coordTypeOf(fn, e, 0)); len(enum) > 0 {
		return enum, true
	}
	switch e := e.(type) {
	case *ast.ParenExpr:
		return g.classesOf(fn, e.X, depth)
	case *ast.CallExpr:
		if len(e.Args) == 1 && (isIdent(e.Fun, "string") || g.coord.enumType(e.Fun) != "") {
			return g.classesOf(fn, e.Args[0], depth)
		}
	case *ast.Ident:
		if v, ok := g.local[e.Name]; ok {
			return []string{v}, true
		}
		if param := paramOf(fn, e); param >= 0 {
			var out []string
			callers := g.callers(fn)
			for _, caller := range callers {
				if param >= len(caller.call.Args) {
					return nil, false
				}
				vals, ok := g.classesOf(caller.fn, caller.call.Args[param], depth+1)
				if !ok {
					return nil, false
				}
				out = append(out, vals...)
			}
			return out, len(callers) > 0
		}
		if def := definition(fn, e); def != nil {
			if lit, ok := def.(*ast.CompositeLit); ok {
				var out []string
				for _, el := range lit.Elts {
					vals, ok := g.classesOf(fn, el, depth+1)
					if !ok {
						return nil, false
					}
					out = append(out, vals...)
				}
				return out, true
			}
			return g.classesOf(fn, def, depth+1)
		}
	}
	return nil, false
}

// variadic reports whether fn's parameter at argument i is its variadic tail.
func variadic(fn *ast.FuncDecl, i int) bool {
	params := fn.Type.Params.List
	if len(params) == 0 {
		return false
	}
	_, ok := params[len(params)-1].Type.(*ast.Ellipsis)
	return ok && i >= fn.Type.Params.NumFields()-1
}

// paramOf is the argument e names as fn's parameter, -1 for none.
func paramOf(fn *ast.FuncDecl, e ast.Expr) int {
	id, ok := e.(*ast.Ident)
	if !ok || fn == nil {
		return -1
	}
	i := 0
	for _, f := range fn.Type.Params.List {
		for _, name := range f.Names {
			if name.Name == id.Name && (id.Obj == nil || id.Obj == name.Obj) {
				return i
			}
			i++
		}
		if len(f.Names) == 0 {
			i++
		}
	}
	return -1
}

// definition is what the local variable id is defined as in fn: the value
// its one := or var gives it (the first of a comma-ok or a call's results),
// or the list a range over a literal takes it from; nil for any other, or
// where it is given another value.
func definition(fn *ast.FuncDecl, id *ast.Ident) ast.Expr {
	defs := values(fn, id)
	if len(defs) != 1 {
		return nil
	}
	return defs[0]
}

// values is every value fn gives the local variable id, nil for one this
// reading cannot tell.
func values(fn *ast.FuncDecl, id *ast.Ident) []ast.Expr {
	if id.Obj == nil || fn == nil || fn.Body == nil {
		return nil
	}
	same := func(n *ast.Ident) bool { return n.Obj == id.Obj && n.Name == id.Name }
	value := func(i int, lhs int, rhs []ast.Expr) ast.Expr {
		switch {
		case lhs == len(rhs):
			return rhs[i]
		case len(rhs) == 1 && i == 0:
			return rhs[0]
		}
		return nil
	}
	var out []ast.Expr
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, l := range n.Lhs {
				if lid, ok := l.(*ast.Ident); ok && same(lid) {
					out = append(out, value(i, len(n.Lhs), n.Rhs))
				}
			}
		case *ast.RangeStmt:
			if v, ok := n.Value.(*ast.Ident); ok && same(v) {
				lit, _ := n.X.(*ast.CompositeLit)
				out = append(out, ast.Expr(lit))
				if lit == nil {
					out[len(out)-1] = nil
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if same(name) && len(n.Values) > 0 {
					out = append(out, value(i, len(n.Names), n.Values))
				}
			}
		}
		return true
	})
	return out
}

// coordTypeOf is the coord type e has in fn, "T" or "[]T", where this reading
// can tell: a parameter's or a receiver's, a variable's from its definition
// or the list it ranges over, a coord function's or method's result, a
// conversion's, a literal's.
func (g *classGate) coordTypeOf(fn *ast.FuncDecl, e ast.Expr, depth int) string {
	if depth > 6 {
		return ""
	}
	switch e := e.(type) {
	case *ast.Ident:
		var fields []*ast.Field
		if fn.Recv != nil {
			fields = append(fields, fn.Recv.List...)
		}
		fields = append(fields, fn.Type.Params.List...)
		for _, f := range fields {
			if slices.ContainsFunc(f.Names, func(n *ast.Ident) bool { return n.Name == e.Name && n.Obj == e.Obj }) {
				return coordTypeExpr(f.Type)
			}
		}
		if e.Obj == nil || fn.Body == nil {
			return ""
		}
		typ := ""
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.AssignStmt:
				for i, l := range n.Lhs {
					if lid, ok := l.(*ast.Ident); ok && lid.Obj == e.Obj && n.Tok == token.DEFINE &&
						len(n.Lhs) == len(n.Rhs) && typ == "" {
						typ = g.coordTypeOf(fn, n.Rhs[i], depth+1)
					}
				}
			case *ast.RangeStmt:
				if v, ok := n.Value.(*ast.Ident); ok && v.Obj == e.Obj && typ == "" {
					typ = strings.TrimPrefix(g.coordTypeOf(fn, n.X, depth+1), "[]")
				}
			}
			return typ == ""
		})
		return typ
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		switch {
		case !ok:
			if isIdent(e.Fun, "append") && len(e.Args) > 0 {
				return g.coordTypeOf(fn, e.Args[0], depth+1)
			}
		case isIdent(sel.X, "coord"):
			if t := g.coord.enumType(sel); t != "" {
				return t
			}
			return g.coord.results[sel.Sel.Name]
		default:
			if t := g.coordTypeOf(fn, sel.X, depth+1); t != "" {
				return g.coord.results[t+"."+sel.Sel.Name]
			}
		}
	case *ast.CompositeLit:
		return coordTypeExpr(e.Type)
	case *ast.SelectorExpr:
		// A FIELD of a coord value: its type, where it is coord's own.
		if t := g.coordTypeOf(fn, e.X, depth+1); t != "" && !strings.HasPrefix(t, "[]") {
			if field := g.coord.fields[t+"."+e.Sel.Name]; g.coord.declared(strings.TrimPrefix(field, "[]")) {
				return field
			}
		}
	case *ast.ParenExpr:
		return g.coordTypeOf(fn, e.X, depth+1)
	}
	return ""
}

// coordMethodOf is "T.M" where sel names method M of a value of coord type T
// in fn, "" for anything else.
func (g *classGate) coordMethodOf(fn *ast.FuncDecl, sel *ast.SelectorExpr) string {
	if fn == nil || isIdent(sel.X, "coord") {
		return ""
	}
	t := g.coordTypeOf(fn, sel.X, 0)
	if t == "" || strings.HasPrefix(t, "[]") {
		return ""
	}
	return t + "." + sel.Sel.Name
}

// coordTypeExpr is the coord type e spells, "T" or "[]T"; "" for another.
func coordTypeExpr(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.SelectorExpr:
		if isIdent(e.X, "coord") {
			return e.Sel.Name
		}
	case *ast.ArrayType:
		if t := coordTypeExpr(e.Elt); t != "" && !strings.HasPrefix(t, "[]") {
			return "[]" + t
		}
	case *ast.Ellipsis:
		if t := coordTypeExpr(e.Elt); t != "" && !strings.HasPrefix(t, "[]") {
			return "[]" + t
		}
	}
	return ""
}

// exprList is args as the source spells them.
func exprList(args []ast.Expr) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = exprString(a)
	}
	return strings.Join(parts, ", ")
}

// refsIn is every key decl builds on a class: a call of a coord builder, one
// named as a value, coord.DocumentKey or DocumentFilter (its segments read by
// samplesOf), a key or class constant named bare, coord's or this package's,
// and an entryClass literal's filter and key, which must name the same classes.
func (g *classGate) refsIn(t *testing.T, decl ast.Decl) []classRef {
	t.Helper()
	fn, _ := decl.(*ast.FuncDecl)
	var out []classRef
	at := func(n ast.Node) string { return g.fset.Position(n.Pos()).String() }
	calledAt := map[ast.Expr]bool{}
	ast.Inspect(decl, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			calledAt[call.Fun] = true
		}
		return true
	})
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			if declaresClasses(n) {
				return false // a class's declaration, what a key is held to
			}
			if !isEntryClass(n.Type) {
				return true
			}
			out = append(out, g.entryRef(t, n, at(n)))
			return false
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if isIdent(sel.X, "coord") {
				if classes, ok := g.coord.funcs[sel.Sel.Name]; ok {
					out = append(out, classRef{at: at(n), what: "coord." + sel.Sel.Name, samples: keysOf(classes),
						node: n, fn: fn})
					return false
				}
				if g.coord.unread[sel.Sel.Name] {
					t.Errorf("%s calls coord.%s, whose class this gate cannot read: build it on a coord "+
						"class constant", at(n), sel.Sel.Name)
				}
			}
			if m := g.coordMethodOf(fn, sel); m != "" {
				if classes, ok := g.coord.funcs[m]; ok {
					out = append(out, classRef{at: at(n), what: "coord." + m, samples: keysOf(classes), node: n,
						fn: fn})
					return false
				}
				if g.coord.unread[m] {
					t.Errorf("%s calls coord.%s, whose class this gate cannot read: build it on a coord "+
						"class constant", at(n), m)
				}
			}
			if isIdent(sel.X, "coord") && (sel.Sel.Name == "DocumentKey" || sel.Sel.Name == "DocumentFilter") {
				samples := g.samplesOf(fn, n, sel.Sel.Name == "DocumentFilter")
				out = append(out, classRef{at: at(n), what: "coord." + sel.Sel.Name + " of " + exprList(n.Args),
					samples: samples, node: n, fn: fn})
				return false
			}
		case *ast.SelectorExpr:
			if m := g.coordMethodOf(fn, n); m != "" && !calledAt[n] && g.coord.funcs[m] != nil {
				out = append(out, classRef{at: at(n), what: "coord." + m + " as a value",
					samples: keysOf(g.coord.funcs[m]), node: n, fn: fn, builder: true})
			}
			if !isIdent(n.X, "coord") {
				return true
			}
			if classes, ok := g.coord.funcs[n.Sel.Name]; ok && !calledAt[n] {
				out = append(out, classRef{at: at(n), what: "coord." + n.Sel.Name + " as a value",
					samples: keysOf(classes), node: n, fn: fn, builder: true})
			}
			if v, ok := g.coord.consts[n.Sel.Name]; ok {
				switch {
				case strings.HasSuffix(n.Sel.Name, "Key"):
					out = append(out, classRef{at: at(n), what: "coord." + n.Sel.Name, samples: []string{v},
						node: n, fn: fn})
				case strings.HasSuffix(n.Sel.Name, "Class"):
					out = append(out, classRef{at: at(n), what: "coord." + n.Sel.Name,
						samples: keysOf([]string{v}), node: n, fn: fn})
				}
			}
		case *ast.Ident:
			v, ok := g.local[n.Name]
			switch {
			case !ok || fn == nil:
			case strings.HasSuffix(n.Name, "Key"):
				out = append(out, classRef{at: at(n), what: n.Name, samples: []string{v}, node: n, fn: fn})
			case strings.HasSuffix(n.Name, "Class"):
				out = append(out, classRef{at: at(n), what: n.Name, samples: keysOf([]string{v}), node: n, fn: fn})
			}
		}
		return true
	})
	if fn == nil {
		// Outside a function only an entryClass literal is read; any other
		// key there would reach its bucket where this reading does not look.
		for _, r := range out {
			if r.what != "an entryClass" {
				t.Errorf("%s builds %s outside a function, where this gate cannot follow it: build it "+
					"where it is written", r.at, r.what)
			}
		}
		out = slices.DeleteFunc(out, func(r classRef) bool { return r.what != "an entryClass" })
	}
	return out
}

// entryRef is an entryClass literal's classes: its filter's, which its key
// function must build too, read as filterOf reads a filter.
func (g *classGate) entryRef(t *testing.T, lit *ast.CompositeLit, at string) classRef {
	t.Helper()
	var filter, key []string
	var built bool
	for _, el := range lit.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		switch {
		case isIdent(kv.Key, "filter"):
			filter = g.coord.filterOf(t, kv.Value)
		case isIdent(kv.Key, "key"):
			key, built = g.coord.keyOf(t, kv.Value)
		}
	}
	slices.Sort(filter)
	slices.Sort(key)
	if len(filter) == 0 || built && !slices.Equal(filter, key) {
		t.Errorf("%s: an entryClass's key builds %v and its filter selects %v: a class lists what it "+
			"writes", at, key, filter)
	}
	return classRef{at: at, what: "an entryClass", samples: keysOf(filter), node: lit}
}

// keysOf is a key of each class.
func keysOf(classes []string) []string {
	out := make([]string, len(classes))
	for i, c := range classes {
		out[i] = coord.DocumentKey(c, "x")
	}
	return out
}

// flow is where one key goes: the buckets it is a key of, and each place it
// goes on that this reading cannot follow.
type flow struct {
	buckets map[string]bool
	lost    []string
}

func (f *flow) merge(o flow) {
	for b := range o.buckets {
		if f.buckets == nil {
			f.buckets = map[string]bool{}
		}
		f.buckets[b] = true
	}
	f.lost = append(f.lost, o.lost...)
}

// bucketKeyArg is the argument a bucket's method takes its key at.
var bucketKeyArg = map[string]int{"Put": 1, "PutString": 1, "Update": 1, "Create": 1, "Get": 1,
	"Delete": 1, "Purge": 1, "update": 1, "carriedBack": 0}

// endsIn is every function of another package a key handed to ends in: one
// answering no text (a test, a position, an error); [keepsIn] those whose
// text holds the key's, which go on as it. Any other is a call this reading
// cannot resolve, and lost.
var endsIn = map[string]bool{"fmt.Errorf": true, "errors.New": true, "errors.Is": true, "errors.As": true,
	"strings.HasPrefix": true, "strings.HasSuffix": true, "strings.Contains": true, "strings.Count": true,
	"strings.Compare": true, "strings.EqualFold": true, "strings.Index": true, "strings.LastIndex": true,
	"slices.Contains": true, "slices.Index": true, "slices.Equal": true}

// keepsIn is every function of another package whose text holds the key it
// is handed, which goes on as that key.
var keepsIn = map[string]bool{"fmt.Sprintf": true, "fmt.Sprint": true, "strings.Join": true,
	"strings.Clone": true, "strings.Repeat": true, "strings.Replace": true, "strings.ReplaceAll": true,
	"strings.ToLower": true, "strings.ToUpper": true, "strings.Trim": true, "strings.TrimSpace": true,
	"strings.TrimPrefix": true, "strings.TrimSuffix": true, "strconv.Quote": true, "slices.Clone": true}

// frame is one function a key is followed through, and the buckets each of
// its parameters is handed by the call it was followed from.
type frame struct {
	fn    *ast.FuncDecl
	binds map[string][]string
}

// reach is where the key node builds in f's function goes: up its enclosing
// expressions to a bucket it is the key of (a call on a bucket, or one
// passing a bucket and the key right after it to what this reading cannot
// open), into a helper of this package by its parameter, with the buckets its
// other arguments are, through a variable, a slice and a range, out of a
// function returning it to its callers, or to its end (a comparison, a
// message, a value written beside a key). Anything else is lost.
func (g *classGate) reach(f frame, node ast.Node, depth int, seen map[string]bool) flow {
	var out flow
	fn := f.fn
	lose := func(n ast.Node, what string) flow {
		if g.excuse(fn, n) {
			return flow{}
		}
		return flow{lost: []string{g.fset.Position(n.Pos()).String() + " (" + what + ")"}}
	}
	if depth > 16 || fn.Body == nil {
		return lose(node, "too deep to follow")
	}
	path := pathTo(fn.Body, node)
	for i := len(path) - 2; i >= 0; i-- {
		child := path[i+1]
		switch p := path[i].(type) {
		case *ast.ParenExpr, *ast.StarExpr, *ast.UnaryExpr:
			continue
		case *ast.BinaryExpr:
			if p.Op == token.ADD {
				continue
			}
			return out // a comparison
		case *ast.CompositeLit:
			if declaresClasses(p) || i > 0 && slices.ContainsFunc(path[:i], func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				return ok && declaresClasses(lit)
			}) {
				return out // a class's declaration, what a key is held to
			}
			if _, keyed := child.(*ast.KeyValueExpr); keyed {
				if _, isMap := p.Type.(*ast.MapType); !isMap {
					return lose(p, "a field of a record")
				}
			}
			continue // an element of a slice, or a key or value of a map
		case *ast.KeyValueExpr:
			continue // a map's key or value; a record's field is lost above
		case *ast.IndexExpr:
			if child == p.X {
				continue // the element is a key the container held
			}
			if isAssignedTo(path[i-1], p) {
				// A MAP'S KEY: what ranges the map takes it out again.
				out.merge(g.reachContainer(f, p.X, true, depth, seen))
				return out
			}
			return out // a lookup
		case *ast.SliceExpr:
			if child == p.X {
				continue
			}
			return out // a bound
		case *ast.IfStmt, *ast.SwitchStmt, *ast.CaseClause, *ast.ExprStmt:
			return out // a condition, or a value no one keeps
		case *ast.RangeStmt:
			if child != p.X {
				return out
			}
			// THE VALUE OF A LIST OR A MAP, AND THE KEY OF A MAP, are what
			// it held; a list's index is an int, whose uses end it.
			for _, v := range []ast.Expr{p.Key, p.Value} {
				if id, ok := v.(*ast.Ident); ok && id.Name != "_" {
					out.merge(g.reachVar(f, id, depth, seen))
				}
			}
			return out
		case *ast.CallExpr:
			if child == p.Fun {
				return lose(p, "a call of it")
			}
			arg := slices.IndexFunc(p.Args, func(a ast.Expr) bool { return a == child })
			if sel, ok := p.Fun.(*ast.SelectorExpr); ok {
				if b := g.bucketsOf(f, sel.X); len(b) > 0 {
					if at, known := bucketKeyArg[sel.Sel.Name]; known && at != arg {
						return out // a value beside a key
					}
					return bucketFlow(b) // a key, or a filter it lists or watches
				}
			}
			if g.transform(fn, p.Fun) {
				continue
			}
			if g.ends(fn, p.Fun) {
				return out
			}
			if id, ok := p.Fun.(*ast.Ident); ok && id.Obj != nil && id.Obj.Kind == ast.Var && paramOf(fn, id) < 0 {
				if vals := values(fn, id); len(vals) > 0 {
					out.merge(g.reachFuncVar(f, p, vals, arg, depth, seen))
					return out
				}
			}
			callees := g.callees(fn, p.Fun)
			if len(callees) == 0 {
				if arg > 0 {
					if b := g.bucketsOf(f, p.Args[arg-1]); len(b) > 0 {
						return bucketFlow(b)
					}
				}
				return lose(p, "a call this reading cannot resolve")
			}
			for _, callee := range callees {
				out.merge(g.reachParam(f, p, callee, arg, depth, seen))
			}
			return out
		case *ast.AssignStmt:
			if slices.Contains(p.Lhs, child.(ast.Expr)) {
				return out // the variable given another value, followed from where it is built
			}
			j := slices.IndexFunc(p.Rhs, func(a ast.Expr) bool { return a == child })
			if j < 0 || len(p.Lhs) != len(p.Rhs) {
				return lose(p, "an assignment")
			}
			if ix, ok := p.Lhs[j].(*ast.IndexExpr); ok {
				out.merge(g.reachContainer(f, ix.X, false, depth, seen)) // an element of a list or a map
				return out
			}
			id, ok := p.Lhs[j].(*ast.Ident)
			switch {
			case !ok:
				return lose(p, "a field assigned")
			case id.Name != "_":
				out.merge(g.reachVar(f, id, depth, seen))
			}
			return out
		case *ast.ValueSpec:
			j := slices.IndexFunc(p.Values, func(a ast.Expr) bool { return a == child })
			if j >= 0 && j < len(p.Names) {
				out.merge(g.reachVar(f, p.Names[j], depth, seen))
				return out
			}
			return lose(p, "a declaration")
		case *ast.ReturnStmt:
			if slices.ContainsFunc(path[:i], func(n ast.Node) bool { _, ok := n.(*ast.FuncLit); return ok }) {
				return lose(p, "returned out of a function literal")
			}
			if !returnsString(fn) {
				return lose(p, "returned beside other values")
			}
			for _, caller := range g.callers(fn) {
				out.merge(g.reach(frame{fn: caller.fn}, caller.call, depth+1, seen))
			}
			return out
		default:
			return lose(p, fmt.Sprintf("a %T", p))
		}
	}
	return out
}

// reachFuncVar is where the key call hands a function variable at argument
// arg goes, for each value the variable is given: a bucket's method value,
// or a function literal of fn, followed by its parameter.
func (g *classGate) reachFuncVar(f frame, call *ast.CallExpr, vals []ast.Expr, arg, depth int,
	seen map[string]bool) flow {
	var out flow
	for _, v := range vals {
		switch v := v.(type) {
		case *ast.SelectorExpr:
			if b := g.bucketsOf(f, v.X); len(b) > 0 {
				out.merge(bucketFlow(b))
				continue
			}
		case *ast.FuncLit:
			lit := &ast.FuncDecl{Type: v.Type}
			if param := paramIdent(lit, arg); param != nil {
				for _, use := range uses(f.fn, param) {
					if use != param {
						out.merge(g.reach(f, use, depth+1, seen))
					}
				}
				continue
			}
		}
		out.lost = append(out.lost, g.fset.Position(call.Pos()).String()+" (a function variable)")
	}
	return out
}

// isAssignedTo reports whether e is a left-hand side of the assignment n.
func isAssignedTo(n ast.Node, e ast.Expr) bool {
	as, ok := n.(*ast.AssignStmt)
	return ok && slices.Contains(as.Lhs, e)
}

// reachContainer is where the keys stored in the list or map c names go, as
// its keys (a map's, where keys) or its elements: out of every range over
// it, and, for its elements, out of every index into it. c is a variable of
// f's function, or a field, followed through every function of the package
// that uses that field of that type. A use that hands c on whole is lost.
func (g *classGate) reachContainer(f frame, c ast.Expr, keys bool, depth int, seen map[string]bool) flow {
	type use struct {
		fn   *ast.FuncDecl
		node ast.Node
	}
	var all []use
	name := ""
	switch c := c.(type) {
	case *ast.Ident:
		name = fmt.Sprintf("%p/%d", f.fn, c.Pos())
		if c.Obj != nil {
			if decl, ok := c.Obj.Decl.(ast.Node); ok {
				name = fmt.Sprintf("%p/%d", f.fn, decl.Pos())
			}
		}
		for _, u := range uses(f.fn, c) {
			all = append(all, use{f.fn, u})
		}
	case *ast.SelectorExpr:
		typ := g.typeOf(f.fn, c.X)
		if typ == "" {
			break
		}
		name = typ + "." + c.Sel.Name
		for _, decl := range g.decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == c.Sel.Name && g.typeOf(fn, sel.X) == typ {
					all = append(all, use{fn, sel})
				}
				return true
			})
		}
	}
	if name == "" {
		return flow{lost: []string{g.fset.Position(c.Pos()).String() + " (a list or map this reading cannot follow)"}}
	}
	key := fmt.Sprintf("container/%s/%v", name, keys)
	if seen[key] {
		return flow{}
	}
	seen[key] = true
	var out flow
	for _, u := range all {
		path := pathTo(u.fn.Body, u.node)
		if len(path) < 2 {
			continue
		}
		switch p := path[len(path)-2].(type) {
		case *ast.RangeStmt:
			v := p.Value
			if keys {
				v = p.Key
			}
			if id, ok := v.(*ast.Ident); ok && id.Name != "_" && p.X == u.node {
				out.merge(g.reachVar(frame{fn: u.fn}, id, depth+1, seen))
			}
			continue
		case *ast.IndexExpr:
			if p.X == u.node && !keys {
				out.merge(g.reach(frame{fn: u.fn}, p, depth+1, seen))
			}
			continue // a key is looked up, or a value set or read
		case *ast.CallExpr:
			if id, ok := p.Fun.(*ast.Ident); ok && (id.Name == "len" || id.Name == "delete" || id.Name == "clear") {
				continue
			}
		case *ast.AssignStmt:
			if slices.Contains(p.Lhs, u.node.(ast.Expr)) {
				continue // made anew
			}
		case *ast.BinaryExpr:
			continue // compared with nil
		}
		out.lost = append(out.lost, g.fset.Position(u.node.Pos()).String()+" (a list or map handed on whole)")
	}
	return out
}

func bucketFlow(buckets []string) flow {
	out := flow{buckets: map[string]bool{}}
	for _, b := range buckets {
		out.buckets[b] = true
	}
	return out
}

// reachParam is where the key call hands to callee's parameter at argument
// arg goes in callee, each other parameter bound to the buckets its argument
// is in f.
func (g *classGate) reachParam(f frame, call *ast.CallExpr, callee *ast.FuncDecl, arg, depth int,
	seen map[string]bool) flow {
	param := paramIdent(callee, arg)
	if param == nil {
		return flow{}
	}
	into := frame{fn: callee, binds: map[string][]string{}}
	for j, a := range call.Args {
		if b := g.bucketsOf(f, a); len(b) > 0 {
			if id := paramIdent(callee, j); id != nil {
				into.binds[id.Name] = b
			}
		}
	}
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok && receiverName(callee) != "" {
		if b := g.bucketsOf(f, sel.X); len(b) > 0 {
			into.binds[receiverName(callee)] = b
		}
	}
	key := fmt.Sprintf("%p/%d/%v", callee, arg, into.binds)
	if seen[key] {
		return flow{}
	}
	seen[key] = true
	var out flow
	for _, use := range uses(callee, param) {
		if use != param {
			out.merge(g.reach(into, use, depth+1, seen))
		}
	}
	return out
}

// reachBuilder is where the keys a coord builder named as a value in fn
// builds go: into the helper it is handed to, where each call of the
// parameter holding it builds one.
func (g *classGate) reachBuilder(fn *ast.FuncDecl, node ast.Node, depth int, seen map[string]bool) flow {
	path := pathTo(fn.Body, node)
	lost := flow{lost: []string{g.fset.Position(node.Pos()).String() + " (a builder handed where this " +
		"reading cannot follow it)"}}
	if len(path) < 2 {
		return lost
	}
	call, ok := path[len(path)-2].(*ast.CallExpr)
	if !ok {
		return lost
	}
	arg := slices.IndexFunc(call.Args, func(a ast.Expr) bool { return a == node })
	callees := g.callees(fn, call.Fun)
	if arg < 0 || len(callees) == 0 {
		return lost
	}
	var out flow
	for _, callee := range callees {
		param := paramIdent(callee, arg)
		if param == nil {
			continue
		}
		into := frame{fn: callee, binds: map[string][]string{}}
		for j, a := range call.Args {
			if b := g.bucketsOf(frame{fn: fn}, a); len(b) > 0 {
				if id := paramIdent(callee, j); id != nil {
					into.binds[id.Name] = b
				}
			}
		}
		for _, use := range uses(callee, param) {
			if use == param {
				continue
			}
			if up := pathTo(callee.Body, use); len(up) >= 2 {
				if c, isCall := up[len(up)-2].(*ast.CallExpr); isCall && c.Fun == use {
					out.merge(g.reach(into, c, depth+1, seen))
					continue
				}
			}
			out.lost = append(out.lost, g.fset.Position(use.Pos()).String()+" (a builder passed on)")
		}
	}
	return out
}

// reachVar is where the key the variable id holds goes, through each of its
// uses in f's function but its definition.
func (g *classGate) reachVar(f frame, id *ast.Ident, depth int, seen map[string]bool) flow {
	key := fmt.Sprintf("%p/var/%d/%v", f.fn, id.Pos(), f.binds)
	if seen[key] {
		return flow{}
	}
	seen[key] = true
	var out flow
	for _, use := range uses(f.fn, id) {
		if use != id {
			out.merge(g.reach(f, use, depth+1, seen))
		}
	}
	return out
}

// ends reports whether fun is a function a key handed to ends in: one
// [endsIn] names, the package's logger, a coord function answering anything
// but one text, a builtin that measures it or deletes it from a map, or an
// entryClass's reading of a key (its entry, its record's fields).
func (g *classGate) ends(fn *ast.FuncDecl, fun ast.Expr) bool {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name == "len" || f.Name == "cap" || f.Name == "delete"
	case *ast.SelectorExpr:
		x, ok := f.X.(*ast.Ident)
		if !ok {
			return false
		}
		switch {
		case endsIn[x.Name+"."+f.Sel.Name], x.Name == "log" && g.vars["log"] == "":
			return true
		case x.Name == "coord":
			return f.Sel.Name != "DocumentKey" && f.Sel.Name != "DocumentFilter" &&
				g.coord.funcs[f.Sel.Name] == nil && g.coord.results[f.Sel.Name] != "string"
		}
		return (f.Sel.Name == "entry" || f.Sel.Name == "stored") && g.entryClassValue(fn, f.X)
	}
	return false
}

// entryClassValue reports whether x is an entryClass in fn.
func (g *classGate) entryClassValue(fn *ast.FuncDecl, x ast.Expr) bool {
	return g.typeOf(fn, x) == "entryClass"
}

// transform reports whether fun builds a key out of another unchanged in its
// class: a coord key builder or a coord text of a key, a conversion, a text
// function [keepsIn] names, or an entryClass's key function, which builds its
// own class's (entryRef) or hands the key back.
func (g *classGate) transform(fn *ast.FuncDecl, fun ast.Expr) bool {
	if sel, ok := fun.(*ast.SelectorExpr); ok {
		if x, ok := sel.X.(*ast.Ident); ok && keepsIn[x.Name+"."+sel.Sel.Name] {
			return true
		}
		if isIdent(sel.X, "coord") {
			// A COORD FUNCTION ANSWERING ONE TEXT of a key may answer the key.
			return sel.Sel.Name == "DocumentKey" || sel.Sel.Name == "DocumentFilter" ||
				g.coord.funcs[sel.Sel.Name] == nil && g.coord.results[sel.Sel.Name] == "string"
		}
		return sel.Sel.Name == "key" && g.entryClassValue(fn, sel.X)
	}
	return isIdent(fun, "string") || isIdent(fun, "encodeKey") || isIdent(fun, "append")
}

// callees is every function of this package fun may name, in fn: a free
// function by its name, or a method of the type the value it is called on
// has, where this reading can tell; none where it cannot.
func (g *classGate) callees(fn *ast.FuncDecl, fun ast.Expr) []*ast.FuncDecl {
	return g.calleesIn(fn, fun, map[ast.Expr]bool{})
}

func (g *classGate) calleesIn(fn *ast.FuncDecl, fun ast.Expr, seen map[ast.Expr]bool) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	switch f := fun.(type) {
	case *ast.Ident:
		for _, c := range g.funcs[f.Name] {
			if c.Recv == nil {
				out = append(out, c)
			}
		}
	case *ast.SelectorExpr:
		typ := g.typeIn(fn, f.X, seen)
		if typ == "" {
			return nil
		}
		for _, c := range g.funcs[f.Sel.Name] {
			if c.Recv != nil && baseType(c.Recv.List[0].Type) == typ {
				out = append(out, c)
			}
		}
	case *ast.IndexExpr:
		return g.calleesIn(fn, f.X, seen)
	}
	return out
}

// typeOf is the name of this package's type x has in fn: a receiver's or a
// parameter's, a package variable's, or what a free function returns; "".
func (g *classGate) typeOf(fn *ast.FuncDecl, x ast.Expr) string {
	return g.typeIn(fn, x, map[ast.Expr]bool{})
}

func (g *classGate) typeIn(fn *ast.FuncDecl, x ast.Expr, seen map[ast.Expr]bool) string {
	if seen[x] {
		return ""
	}
	seen[x] = true
	switch x := x.(type) {
	case *ast.Ident:
		var fields []*ast.Field
		if fn.Recv != nil {
			fields = append(fields, fn.Recv.List...)
		}
		fields = append(fields, fn.Type.Params.List...)
		for _, f := range fields {
			if slices.ContainsFunc(f.Names, func(id *ast.Ident) bool { return id.Name == x.Name }) {
				return baseType(f.Type)
			}
		}
		for _, v := range values(fn, x) {
			if v != nil && v != ast.Expr(x) {
				if t := g.typeIn(fn, v, seen); t != "" {
					return t
				}
			}
		}
		return g.vars[x.Name]
	case *ast.SelectorExpr:
		if t := g.typeIn(fn, x.X, seen); t != "" {
			return g.fields[t+"."+x.Sel.Name]
		}
	case *ast.CompositeLit:
		return baseType(x.Type)
	case *ast.UnaryExpr:
		return g.typeIn(fn, x.X, seen)
	case *ast.CallExpr:
		for _, c := range g.calleesIn(fn, x.Fun, seen) {
			if c.Type.Results != nil && len(c.Type.Results.List) > 0 {
				return baseType(c.Type.Results.List[0].Type)
			}
		}
	}
	return ""
}

// baseType is the name of the type e spells, past a pointer and type
// arguments; "" for another package's.
func baseType(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.StarExpr:
		return baseType(e.X)
	case *ast.IndexExpr:
		return baseType(e.X)
	case *ast.IndexListExpr:
		return baseType(e.X)
	case *ast.ArrayType:
		return baseType(e.Elt)
	case *ast.Ident:
		return e.Name
	}
	return ""
}

// returnsString reports whether fn answers one string: a key it built.
func returnsString(fn *ast.FuncDecl) bool {
	r := fn.Type.Results
	return r != nil && len(r.List) == 1 && len(r.List[0].Names) <= 1 && isIdent(r.List[0].Type, "string")
}

// called is one call of a function, in the function that makes it.
type called struct {
	fn   *ast.FuncDecl
	call *ast.CallExpr
}

// callers is every call of target in this package, indexed on first use.
func (g *classGate) callers(target *ast.FuncDecl) []called {
	if g.calls == nil {
		g.calls = map[*ast.FuncDecl][]called{}
		for _, decl := range g.decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					for _, callee := range g.callees(fn, call.Fun) {
						g.calls[callee] = append(g.calls[callee], called{fn: fn, call: call})
					}
				}
				return true
			})
		}
	}
	return g.calls[target]
}

// bucketsOf is the buckets n is in f: a store's field (f.runs, where f is
// a *FleetStore or *Store), a lease lane's (s.leases.kv, or either lane for a
// *lane this reading cannot tell), the epochs', a parameter the call f was
// followed from bound, or a variable asserted from one; none for another.
func (g *classGate) bucketsOf(f frame, n ast.Node) []string {
	return g.bucketsIn(f, n, map[ast.Node]bool{})
}

func (g *classGate) bucketsIn(f frame, n ast.Node, seen map[ast.Node]bool) []string {
	if seen[n] {
		return nil
	}
	seen[n] = true
	fn := f.fn
	switch n := n.(type) {
	case *ast.Ident:
		param, recv := paramOf(fn, n), receiverName(fn) == n.Name
		if param >= 0 || recv {
			if b, ok := f.binds[n.Name]; ok {
				return b
			}
			// Unbound, so whatever every call of fn hands it.
			var out []string
			for _, caller := range g.callers(fn) {
				var arg ast.Expr
				switch sel, ok := caller.call.Fun.(*ast.SelectorExpr); {
				case recv && ok:
					arg = sel.X
				case !recv && param < len(caller.call.Args):
					arg = caller.call.Args[param]
				}
				if arg != nil {
					out = union(out, g.bucketsIn(frame{fn: caller.fn}, arg, seen))
				}
			}
			return out
		}
		var out []string
		for _, v := range values(fn, n) {
			if v != nil {
				out = union(out, g.bucketsIn(f, v, seen))
			}
		}
		return out
	case *ast.TypeAssertExpr:
		return g.bucketsIn(f, n.X, seen)
	case *ast.CallExpr:
		// A value made of a bucket is a view of it (a leader reader, a
		// stream's leader): a key handed to it is a key of the bucket.
		var out []string
		for _, a := range n.Args {
			out = union(out, g.bucketsIn(f, a, seen))
		}
		return out
	case *ast.UnaryExpr:
		return g.bucketsIn(f, n.X, seen)
	case *ast.SelectorExpr:
		if id, ok := n.X.(*ast.Ident); ok && storeTyped(fn, id.Name) {
			if _, ok := g.holdings[n.Sel.Name]; ok {
				return []string{n.Sel.Name}
			}
		}
		if lane, ok := n.X.(*ast.SelectorExpr); ok {
			if id, ok := lane.X.(*ast.Ident); ok && storeTyped(fn, id.Name) {
				if _, ok := g.holdings[lane.Sel.Name]; ok {
					return []string{lane.Sel.Name}
				}
			}
		}
		switch g.typeOf(fn, n.X) {
		case "lane":
			return []string{"leases", "duties"}
		case "Epochs":
			return []string{"counter"}
		}
	}
	return nil
}

// union is a with each of b's not in it.
func union(a, b []string) []string {
	for _, x := range b {
		if !slices.Contains(a, x) {
			a = append(a, x)
		}
	}
	return a
}

// receiverName is fn's receiver's name, "" for none.
func receiverName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List[0].Names) == 0 {
		return ""
	}
	return fn.Recv.List[0].Names[0].Name
}

// storeTyped reports whether name is fn's receiver or parameter of type
// *FleetStore or *Store.
func storeTyped(fn *ast.FuncDecl, name string) bool {
	var fields []*ast.Field
	if fn.Recv != nil {
		fields = append(fields, fn.Recv.List...)
	}
	fields = append(fields, fn.Type.Params.List...)
	for _, f := range fields {
		star, ok := f.Type.(*ast.StarExpr)
		if !ok || !isIdent(star.X, "FleetStore") && !isIdent(star.X, "Store") {
			continue
		}
		if slices.ContainsFunc(f.Names, func(id *ast.Ident) bool { return id.Name == name }) {
			return true
		}
	}
	return false
}

// paramIdent is fn's parameter at argument i, the last for a variadic
// tail; nil where it has none or it is unnamed.
func paramIdent(fn *ast.FuncDecl, i int) *ast.Ident {
	var names []*ast.Ident
	for _, f := range fn.Type.Params.List {
		if len(f.Names) == 0 {
			names = append(names, nil)
		}
		names = append(names, f.Names...)
	}
	switch {
	case i < 0 || len(names) == 0:
		return nil
	case i < len(names):
		return names[i]
	}
	return names[len(names)-1]
}

// uses is every identifier in fn's body naming the variable id declares.
func uses(fn *ast.FuncDecl, id *ast.Ident) []ast.Node {
	var out []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if use, ok := n.(*ast.Ident); ok && use.Name == id.Name && (id.Obj == nil || use.Obj == id.Obj) {
			out = append(out, use)
		}
		return true
	})
	return out
}

// pathTo is the chain of nodes from root down to target, target last; nil
// where root does not hold it.
func pathTo(root, target ast.Node) []ast.Node {
	var stack, found []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if found != nil {
			return false
		}
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		stack = append(stack, n)
		if n == target {
			found = slices.Clone(stack)
			return false
		}
		return true
	})
	return found
}

// stringConsts is every string constant decl declares, by name.
func stringConsts(decl ast.Decl) map[string]string {
	out := map[string]string{}
	gen, ok := decl.(*ast.GenDecl)
	if !ok || gen.Tok != token.CONST {
		return out
	}
	for _, spec := range gen.Specs {
		vs := spec.(*ast.ValueSpec)
		for i, name := range vs.Names {
			if i < len(vs.Values) {
				if v, ok := stringLit(vs.Values[i]); ok {
					out[name.Name] = v
				}
			}
		}
	}
	return out
}

// coordClasses is internal/coord's key classes as its source spells them:
// each string constant's value, each function building a key or a filter on
// one (every class its body builds on, by name), and each that builds one on
// no class this reading resolves.
type coordClasses struct {
	consts  map[string]string
	types   map[string]string // each constant's named type, where it has one
	results map[string]string // each function's ("F") and method's ("T.M") one coord result, "T" or "[]T"
	fields  map[string]string // each struct field's type ("T.f"), "T" or "[]T"
	funcs   map[string][]string
	unread  map[string]bool
}

// declared reports whether t is a type internal/coord declares: one with a
// field, a method or a constant of its own.
func (c coordClasses) declared(t string) bool {
	if t == "" {
		return false
	}
	for name := range c.fields {
		if strings.HasPrefix(name, t+".") {
			return true
		}
	}
	for name := range c.results {
		if strings.HasPrefix(name, t+".") {
			return true
		}
	}
	return len(c.enum(t)) > 0
}

// enum is every constant of the coord type t, none for another type.
func (c coordClasses) enum(t string) []string {
	var out []string
	for name, typ := range c.types {
		if typ == t && t != "" {
			out = append(out, c.consts[name])
		}
	}
	slices.Sort(out)
	return out
}

// enumType is the coord enum fun names as a conversion, coord.T, "" for none.
func (c coordClasses) enumType(fun ast.Expr) string {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok || !isIdent(sel.X, "coord") || len(c.enum(sel.Sel.Name)) == 0 {
		return ""
	}
	return sel.Sel.Name
}

// coordClassValues reads internal/coord's non-test source.
func coordClassValues(t *testing.T) coordClasses {
	t.Helper()
	c := coordClasses{consts: map[string]string{}, types: map[string]string{}, results: map[string]string{},
		fields: map[string]string{}, funcs: map[string][]string{}, unread: map[string]bool{}}
	files := parsePackage(t, token.NewFileSet(), "..")
	for _, file := range files {
		for _, decl := range file.Decls {
			for name, v := range stringConsts(decl) {
				c.consts[name] = v
			}
			switch decl := decl.(type) {
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok && decl.Tok == token.CONST && vs.Type != nil {
						for _, name := range vs.Names {
							c.types[name.Name] = baseType(vs.Type)
						}
					}
					if ts, ok := spec.(*ast.TypeSpec); ok {
						if st, ok := ts.Type.(*ast.StructType); ok {
							for _, field := range st.Fields.List {
								for _, name := range field.Names {
									c.fields[ts.Name.Name+"."+name.Name] = localTypeExpr(field.Type)
								}
							}
						}
					}
				}
			case *ast.FuncDecl:
				if r := decl.Type.Results; r != nil && len(r.List) == 1 && len(r.List[0].Names) <= 1 {
					name := decl.Name.Name
					if decl.Recv != nil {
						name = baseType(decl.Recv.List[0].Type) + "." + name
					}
					c.results[name] = localTypeExpr(r.List[0].Type)
				}
			}
		}
	}
	var builders []builder
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == "DocumentKey" || fn.Name.Name == "DocumentFilter" {
				continue
			}
			b := builder{name: fn.Name.Name, enums: map[string]string{}}
			if fn.Recv != nil {
				// A METHOD IS NAMED BY ITS TYPE, and reads its receiver's
				// fields as its classes ([coordClasses.readBuilder]).
				b.recvType = baseType(fn.Recv.List[0].Type)
				b.name = b.recvType + "." + b.name
				if names := fn.Recv.List[0].Names; len(names) == 1 {
					b.recv = names[0].Name
				}
			}
			for _, field := range fn.Type.Params.List {
				for _, name := range field.Names {
					if t := baseType(field.Type); len(c.enum(t)) > 0 {
						b.enums[name.Name] = t
					}
				}
			}
			// A builder answers its key; one that builds a key to check
			// a value against it, or to name it in an error, is none.
			if r := c.results[b.name]; r == "string" || r == "[]string" {
				c.readBuilder(&b, fn)
				builders = append(builders, b)
			}
		}
	}
	c.settle(builders)
	if len(c.funcs) < 30 || !slices.Equal(c.funcs["ConversationHoldKey"], []string{coord.ConversationHoldClass}) ||
		len(c.funcs["AtlasCodeScanKey"]) < 2 || len(c.funcs["NodeStatusRowKey.Key"]) != len(coord.NodeStatusClasses) ||
		len(c.funcs["GrantFamily.GrantKey"]) != len(coord.GrantFamilies) {
		t.Fatalf("premise: read %d key functions of internal/coord, ConversationHoldKey's classes %v, "+
			"AtlasCodeScanKey's %v, NodeStatusRowKey.Key's %v and GrantFamily.GrantKey's %v", len(c.funcs),
			c.funcs["ConversationHoldKey"], c.funcs["AtlasCodeScanKey"], c.funcs["NodeStatusRowKey.Key"],
			c.funcs["GrantFamily.GrantKey"])
	}
	return c
}

// builder is one function of internal/coord as this reading reads it: the
// classes its own DocumentKey and DocumentFilter calls build on, whether one
// is on a class it cannot read, the functions of coord it calls, and its
// parameters of a coord enum.
type builder struct {
	name           string
	classes        []string
	unread         bool
	calls          []string
	enums          map[string]string
	recv, recvType string // a method's receiver, and its type
}

// coordRegistries is every coord type whose values are a closed list this
// reading takes a method's text class from, by its field: each family's.
var coordRegistries = map[string]any{"GrantFamily": coord.GrantFamilies}

// readBuilder reads fn's body into b.
func (c coordClasses) readBuilder(b *builder, fn *ast.FuncDecl) {
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		id, ok := call.Fun.(*ast.Ident)
		switch {
		case !ok:
			return true
		case id.Name != "DocumentKey" && id.Name != "DocumentFilter":
			b.calls = append(b.calls, id.Name)
			return true
		case len(call.Args) == 0:
			b.unread = true
			return true
		}
		arg := call.Args[0]
		if conv, ok := arg.(*ast.CallExpr); ok && len(conv.Args) == 1 && isIdent(conv.Fun, "string") {
			arg = conv.Args[0]
		}
		if v, ok := c.value(arg, ""); ok {
			b.classes = union(b.classes, []string{v})
		} else if p, ok := arg.(*ast.Ident); ok && b.enums[p.Name] != "" {
			b.classes = union(b.classes, c.enum(b.enums[p.Name]))
		} else if field, ok := c.receiverField(b, arg); ok {
			b.classes = union(b.classes, field)
		} else {
			b.unread = true
		}
		return true
	})
}

// receiverField is the classes arg, a field of b's receiver, may hold: its
// enum's constants, or its value in every entry of its type's registry.
func (c coordClasses) receiverField(b *builder, arg ast.Expr) ([]string, bool) {
	sel, ok := arg.(*ast.SelectorExpr)
	if !ok || b.recv == "" || !isIdent(sel.X, b.recv) {
		return nil, false
	}
	if enum := c.enum(c.fields[b.recvType+"."+sel.Sel.Name]); len(enum) > 0 {
		return enum, true
	}
	registry, ok := coordRegistries[b.recvType]
	if !ok {
		return nil, false
	}
	var out []string
	list := reflect.ValueOf(registry)
	for i := range list.Len() {
		if f := list.Index(i).FieldByName(sel.Sel.Name); f.Kind() == reflect.String {
			out = append(out, f.String())
		}
	}
	return out, len(out) == list.Len()
}

// settle records each builder's classes, its own and every key builder's it
// calls, and as unread each that builds on a class this reading cannot read
// or calls one that does.
func (c coordClasses) settle(builders []builder) {
	for changed := true; changed; {
		changed = false
		for i := range builders {
			b := &builders[i]
			for _, callee := range b.calls {
				if c.unread[callee] && !b.unread {
					b.unread, changed = true, true
				}
				for _, class := range c.funcs[callee] {
					if !slices.Contains(b.classes, class) {
						b.classes, changed = append(b.classes, class), true
					}
				}
			}
			switch {
			case b.unread:
				c.unread[b.name] = true
				delete(c.funcs, b.name)
			case len(b.classes) > 0:
				c.funcs[b.name] = b.classes
			}
		}
	}
}

// value is e as a constant string: a literal, or a constant named bare in
// internal/coord (qualifier "") or as coord.Name here (qualifier "coord").
func (c coordClasses) value(e ast.Expr, qualifier string) (string, bool) {
	if v, ok := stringLit(e); ok {
		return v, true
	}
	switch e := e.(type) {
	case *ast.Ident:
		if qualifier == "" {
			v, ok := c.consts[e.Name]
			return v, ok
		}
	case *ast.SelectorExpr:
		if isIdent(e.X, qualifier) {
			v, ok := c.consts[e.Sel.Name]
			return v, ok
		}
	case *ast.CallExpr:
		// string(coord.X), a class of a named string type.
		if len(e.Args) == 1 && isIdent(e.Fun, "string") {
			return c.value(e.Args[0], qualifier)
		}
	}
	return "", false
}

// filterOf is the classes an entryClass's filter selects: coord.DocumentFilter
// of a coord constant, or of a field of a coord.GrantFamily, one class per
// family. It fails t on any other spelling, which it could not read.
func (c coordClasses) filterOf(t *testing.T, e ast.Expr) []string {
	t.Helper()
	call, ok := e.(*ast.CallExpr)
	if ok && len(call.Args) == 1 {
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && isIdent(sel.X, "coord") && sel.Sel.Name == "DocumentFilter" {
			if v, ok := c.value(call.Args[0], "coord"); ok {
				return []string{v}
			}
			if field, ok := call.Args[0].(*ast.SelectorExpr); ok {
				var out []string
				for _, family := range coord.GrantFamilies {
					if f := reflect.ValueOf(family).FieldByName(field.Sel.Name); f.Kind() == reflect.String {
						out = append(out, f.String())
					}
				}
				if len(out) == len(coord.GrantFamilies) {
					return out
				}
			}
		}
	}
	t.Fatalf("an entryClass filter spelled %s, which this gate cannot read: spell it coord.DocumentFilter "+
		"of a coord constant", exprString(e))
	return nil
}

// keyOf is the classes an entryClass's key function builds: a coord
// builder's, or a coord.GrantFamily's, one class per family; nil for the
// identity, whose keys its callers build and this reading follows from there.
// It fails t on any other spelling, which it could not read.
func (c coordClasses) keyOf(t *testing.T, e ast.Expr) ([]string, bool) {
	t.Helper()
	switch e := e.(type) {
	case *ast.SelectorExpr:
		if classes, ok := c.funcs[e.Sel.Name]; ok && isIdent(e.X, "coord") {
			return classes, true
		}
		field := strings.TrimSuffix(e.Sel.Name, "Key") + "Class"
		var out []string
		for _, family := range coord.GrantFamilies {
			if f := reflect.ValueOf(family).FieldByName(field); f.Kind() == reflect.String {
				out = append(out, f.String())
			}
		}
		if len(out) == len(coord.GrantFamilies) && !isIdent(e.X, "coord") {
			return out, true
		}
	case *ast.FuncLit:
		params := e.Type.Params.List
		if len(params) == 1 && len(params[0].Names) == 1 && len(e.Body.List) == 1 {
			if ret, ok := e.Body.List[0].(*ast.ReturnStmt); ok && len(ret.Results) == 1 &&
				isIdent(ret.Results[0], params[0].Names[0].Name) {
				return nil, false
			}
		}
	}
	t.Fatalf("an entryClass key spelled %s, which this gate cannot read: spell it a coord builder, or the "+
		"identity where the entry is the whole key", exprString(e))
	return nil, false
}

// fleetHoldingsByField is the holding of every bucket a fleet store settles,
// by its field's name, and the lease store's three by theirs.
func fleetHoldingsByField(t *testing.T) map[string]holding {
	t.Helper()
	nc := embeddedNATS(t)
	store := openFleetForTest(t, nc, fmt.Sprintf("g%d", bucketSeq.Add(1)))
	out := map[string]holding{}
	fields := reflect.ValueOf(store).Elem()
	for i := range fields.NumField() {
		if f := fields.Field(i); f.Type() == reflect.TypeFor[jetstream.KeyValue]() {
			// The field is the package's own, so it is read as itself.
			handle := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface()
			out[fields.Type().Field(i).Name] = handle.(settledBucket).holding
		}
	}
	leases := openStore(t, nc, time.Minute)
	out["leases"], out["duties"] = leases.leases.kv.(settledBucket).holding, leases.duties.kv.(settledBucket).holding
	out["counter"] = leases.counter.kv.(settledBucket).holding
	return out
}

// localTypeExpr is the type e spells in internal/coord's own source, "T" or
// "[]T"; "" for another package's.
func localTypeExpr(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.ArrayType:
		if id, ok := e.Elt.(*ast.Ident); ok {
			return "[]" + id.Name
		}
	}
	return ""
}

// receiverIs reports whether fn is a method of the generic type named.
func receiverIs(fn *ast.FuncDecl, name string) bool {
	if fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	switch r := fn.Recv.List[0].Type.(type) {
	case *ast.IndexExpr:
		return isIdent(r.X, name)
	case *ast.IndexListExpr:
		return isIdent(r.X, name)
	}
	return false
}

// declaresClasses reports whether lit is a keyClass or a list of them.
func declaresClasses(lit *ast.CompositeLit) bool {
	if arr, ok := lit.Type.(*ast.ArrayType); ok {
		return isIdent(arr.Elt, "keyClass")
	}
	return isIdent(lit.Type, "keyClass")
}

// isEntryClass reports whether e names an entryClass instantiation.
func isEntryClass(e ast.Expr) bool {
	ix, ok := e.(*ast.IndexExpr)
	return ok && isIdent(ix.X, "entryClass")
}

func isIdent(e ast.Expr, name string) bool {
	id, ok := e.(*ast.Ident)
	return ok && id.Name == name
}

func stringLit(e ast.Expr) (string, bool) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	v, err := strconv.Unquote(lit.Value)
	return v, err == nil
}

func exprString(e ast.Expr) string {
	var b strings.Builder
	if err := printer.Fprint(&b, token.NewFileSet(), e); err != nil {
		return err.Error()
	}
	return b.String()
}

// parsePackage parses dir's non-test source, failing t on too few files to be
// a package of this module: a gate that parsed nothing certifies nothing.
func parsePackage(t *testing.T, fset *token.FileSet, dir string) []*ast.File {
	t.Helper()
	names, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var out []*ast.File
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		out = append(out, file)
	}
	if len(out) < 5 {
		t.Fatalf("read %d source files in %s, which is not a package of this module", len(out), dir)
	}
	return out
}
