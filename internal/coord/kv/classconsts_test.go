package kv

import (
	"fmt"
	"go/ast"
	"go/parser"
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
// REACHES, read off the source. Each key this package builds on a class (a
// coord key or filter function that builds on one, coord.DocumentKey or
// DocumentFilter of a coord class, a key constant of coord's or its own) is
// followed to the bucket it is handed to: into the package's own helpers by
// argument, through a variable and out of a function that returns it, up to
// a call on a store's bucket field or one passing it. Every bucket it reaches
// must declare its class (a bucket of one class declares every key), and a
// key that reaches none fails, since the gate cannot tell where it goes. A
// class constant named bare is followed the same way where it reaches a
// bucket. An entryClass's filter reaches the bucket its verbs write. The
// recorder ([keyWrites]) holds what the suites write; this holds what no case
// reaches.
//
// Mutation: drop ConversationHoldClass from positionsHolding, SaveLedgerClass
// from uploadsHolding or the integration status from integrationsHolding,
// put a session claim's key into the atlas sources through a helper, or give
// the SSH tests' entryClass the session claims' filter, and this is red.
func TestEveryKeyClassThePackageWritesWithIsDeclaredForItsBucket(t *testing.T) {
	t.Parallel()
	g := newClassGate(t, ".")
	g.check(t)
}

// classGate is one reading of this package's source against its holdings.
type classGate struct {
	fset     *token.FileSet
	coord    coordClasses
	local    map[string]string // this package's own key constants
	holdings map[string]holding
	funcs    map[string][]*ast.FuncDecl
	vars     map[string]string // the type of each package variable this reading can tell
	decls    []ast.Decl
}

// classRef is one key a declaration builds on a class, and where.
type classRef struct {
	at, what string
	samples  []string // keys of its class, one per class it may build
	node     ast.Node
	fn       *ast.FuncDecl // nil for a package-level declaration
	required bool          // a key or a filter built, which must reach a bucket
}

func newClassGate(t *testing.T, dir string) *classGate {
	t.Helper()
	g := &classGate{fset: token.NewFileSet(), coord: coordClassValues(t), local: map[string]string{},
		holdings: fleetHoldingsByField(t), funcs: map[string][]*ast.FuncDecl{}, vars: map[string]string{}}
	for _, file := range parsePackage(t, g.fset, dir) {
		for _, decl := range file.Decls {
			if filepath.Base(g.fset.Position(decl.Pos()).Filename) == "settle.go" {
				continue
			}
			g.decls = append(g.decls, decl)
			if fn, ok := decl.(*ast.FuncDecl); ok {
				g.funcs[fn.Name.Name] = append(g.funcs[fn.Name.Name], fn)
			}
			for name, v := range stringConsts(decl) {
				if strings.HasSuffix(name, "Key") {
					g.local[name] = v
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

// check fails t for every key that reaches a bucket not declaring its class,
// and every key or filter that reaches none.
func (g *classGate) check(t *testing.T) {
	t.Helper()
	entryBuckets := map[string]bool{}
	for _, decl := range g.decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && receiverIs(fn, "entryClass") {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if b := g.bucketOf(fn, n); b != "" {
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
			var buckets map[string]bool
			if r.fn == nil {
				entries++
				buckets = entryBuckets
			} else {
				buckets = g.reach(r.fn, r.node, 0, map[string]bool{})
			}
			switch {
			case len(buckets) == 0 && r.required:
				t.Errorf("%s builds %s and hands it to no bucket this gate can name: write it through a "+
					"store's bucket field, or a helper of this package that does", r.at, r.what)
			case len(buckets) > 0 && r.fn != nil:
				keys++
				reached[r.what] = true
			}
			for b := range buckets {
				for _, sample := range r.samples {
					if !declares(g.holdings[b], sample) {
						t.Errorf("%s hands %s (a key %q) to %s, which declares no class for it", r.at, r.what,
							sample, b)
					}
				}
			}
		}
	}
	t.Logf("%d entryClass filters and %d keys followed to a bucket", entries, keys)
	if entries < 10 || keys < 60 || !reached["coord.ConversationHoldKey"] || !reached["coord.SaveLedgerKey"] {
		t.Fatalf("premise: %d entryClass filters and %d keys followed to a bucket (conversation holds %v, "+
			"save ledgers %v), want every one in the package", entries, keys, reached["coord.ConversationHoldKey"],
			reached["coord.SaveLedgerKey"])
	}
}

// declares reports whether h declares a class for key: its one class where
// it declares no other, else a class whose keys match it.
func declares(h holding, key string) bool {
	return len(h.classes) == 0 || slices.ContainsFunc(h.classes, func(k keyClass) bool {
		return server.SubjectMatchesFilter(key, k.keys)
	})
}

// refsIn is every key decl builds on a class: a call of a coord key or filter
// function building on one, coord.DocumentKey or DocumentFilter of a coord
// class, a key constant (coord's or this package's), a class constant named
// bare, and an entryClass literal's filter.
func (g *classGate) refsIn(t *testing.T, decl ast.Decl) []classRef {
	t.Helper()
	fn, _ := decl.(*ast.FuncDecl)
	var out []classRef
	at := func(n ast.Node) string { return g.fset.Position(n.Pos()).String() }
	ast.Inspect(decl, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CompositeLit:
			if !isEntryClass(n.Type) {
				return true
			}
			for _, el := range n.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok && isIdent(kv.Key, "filter") {
					var samples []string
					for _, class := range g.coord.filterOf(t, kv.Value) {
						samples = append(samples, coord.DocumentKey(class, "x"))
					}
					out = append(out, classRef{at: at(kv), what: "an entryClass filter", samples: samples,
						node: kv, required: true})
				}
			}
			return false
		case *ast.CallExpr:
			sel, ok := n.Fun.(*ast.SelectorExpr)
			if !ok || !isIdent(sel.X, "coord") {
				return true
			}
			if classes, ok := g.coord.funcs[sel.Sel.Name]; ok {
				out = append(out, classRef{at: at(n), what: "coord." + sel.Sel.Name, samples: keysOf(classes),
					node: n, fn: fn, required: true})
				return false
			}
			if (sel.Sel.Name == "DocumentKey" || sel.Sel.Name == "DocumentFilter") && len(n.Args) > 0 {
				if class, ok := g.coord.value(n.Args[0], "coord"); ok {
					out = append(out, classRef{at: at(n), what: "coord." + sel.Sel.Name + " of " + class,
						samples: keysOf([]string{class}), node: n, fn: fn, required: true})
					return false
				}
			}
			if strings.HasSuffix(sel.Sel.Name, "Key") || strings.HasSuffix(sel.Sel.Name, "Filter") {
				if _, known := g.coord.unread[sel.Sel.Name]; known {
					t.Errorf("%s calls coord.%s, whose class this gate cannot read: build it with "+
						"DocumentKey or DocumentFilter of a coord class constant", at(n), sel.Sel.Name)
				}
			}
		case *ast.SelectorExpr:
			if !isIdent(n.X, "coord") {
				return true
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
			if v, ok := g.local[n.Name]; ok && fn != nil {
				out = append(out, classRef{at: at(n), what: n.Name, samples: []string{v}, node: n, fn: fn})
			}
		}
		return true
	})
	if fn == nil {
		// Only an entryClass literal builds a key outside a function.
		out = slices.DeleteFunc(out, func(r classRef) bool { return r.what != "an entryClass filter" })
	}
	return out
}

// keysOf is a key of each class.
func keysOf(classes []string) []string {
	out := make([]string, len(classes))
	for i, c := range classes {
		out[i] = coord.DocumentKey(c, "x")
	}
	return out
}

// reach is every bucket the key node builds in fn reaches: up its enclosing
// expressions to a call on a store's bucket field or one passing one, into a
// helper of this package by its parameter, through a variable, and out of fn
// to its callers where fn returns it.
func (g *classGate) reach(fn *ast.FuncDecl, node ast.Node, depth int, seen map[string]bool) map[string]bool {
	out := map[string]bool{}
	if depth > 12 || fn.Body == nil {
		return out
	}
	path := pathTo(fn.Body, node)
	for i := len(path) - 2; i >= 0; i-- {
		child := path[i+1]
		switch p := path[i].(type) {
		case *ast.ParenExpr, *ast.BinaryExpr, *ast.StarExpr, *ast.UnaryExpr, *ast.CompositeLit,
			*ast.KeyValueExpr:
			// The key, or a slice or a record carrying it.
			continue
		case *ast.RangeStmt:
			if child != p.X {
				return out
			}
			if id, ok := p.Value.(*ast.Ident); ok {
				g.reachVar(fn, id, depth, seen, out)
			}
			return out
		case *ast.CallExpr:
			arg := slices.IndexFunc(p.Args, func(a ast.Expr) bool { return a == child })
			if arg < 0 {
				return out
			}
			if sel, ok := p.Fun.(*ast.SelectorExpr); ok {
				if b := g.bucketOf(fn, sel.X); b != "" {
					out[b] = true
					return out
				}
			}
			for _, a := range p.Args {
				if b := g.bucketOf(fn, a); b != "" {
					out[b] = true
				}
			}
			if len(out) > 0 {
				return out
			}
			callees := g.callees(fn, p.Fun)
			if g.transform(p.Fun) || len(callees) == 0 {
				// A conversion, a builtin, or a function value (an entryClass's
				// key): the key goes on in what it answers.
				continue
			}
			for _, callee := range callees {
				name := paramAt(callee, arg)
				key := fmt.Sprintf("%p/%s", callee, name)
				if name == "" || seen[key] {
					continue
				}
				seen[key] = true
				for _, use := range uses(callee, name) {
					for b := range g.reach(callee, use, depth+1, seen) {
						out[b] = true
					}
				}
			}
			return out
		case *ast.AssignStmt:
			j := slices.IndexFunc(p.Rhs, func(a ast.Expr) bool { return a == child })
			if j < 0 || j >= len(p.Lhs) {
				return out
			}
			if id, ok := p.Lhs[j].(*ast.Ident); ok && id.Name != "_" {
				g.reachVar(fn, id, depth, seen, out)
			}
			return out
		case *ast.ReturnStmt:
			if !returnsString(fn) {
				return out
			}
			for _, caller := range g.callers(fn) {
				for b := range g.reach(caller.fn, caller.call, depth+1, seen) {
					out[b] = true
				}
			}
			return out
		default:
			return out
		}
	}
	return out
}

// reachVar adds to out every bucket the variable id holds the key in reaches,
// through each of its uses in fn but its definition.
func (g *classGate) reachVar(fn *ast.FuncDecl, id *ast.Ident, depth int, seen, out map[string]bool) {
	key := fmt.Sprintf("%p/var/%s", fn, id.Name)
	if seen[key] {
		return
	}
	seen[key] = true
	for _, use := range uses(fn, id.Name) {
		if use != id {
			for b := range g.reach(fn, use, depth+1, seen) {
				out[b] = true
			}
		}
	}
}

// transform reports whether fun builds a key out of another unchanged in its
// class: a coord key builder or a conversion.
func (g *classGate) transform(fun ast.Expr) bool {
	if sel, ok := fun.(*ast.SelectorExpr); ok && isIdent(sel.X, "coord") {
		return sel.Sel.Name == "DocumentKey" || sel.Sel.Name == "DocumentFilter"
	}
	return isIdent(fun, "string") || isIdent(fun, "encodeKey")
}

// callees is every function of this package fun may name, in fn: a free
// function by its name, or a method of the type the value it is called on
// has, where this reading can tell; none where it cannot.
func (g *classGate) callees(fn *ast.FuncDecl, fun ast.Expr) []*ast.FuncDecl {
	var out []*ast.FuncDecl
	switch f := fun.(type) {
	case *ast.Ident:
		for _, c := range g.funcs[f.Name] {
			if c.Recv == nil {
				out = append(out, c)
			}
		}
	case *ast.SelectorExpr:
		typ := g.typeOf(fn, f.X)
		if typ == "" {
			return nil
		}
		for _, c := range g.funcs[f.Sel.Name] {
			if c.Recv != nil && baseType(c.Recv.List[0].Type) == typ {
				out = append(out, c)
			}
		}
	case *ast.IndexExpr:
		return g.callees(fn, f.X)
	}
	return out
}

// typeOf is the name of this package's type x has in fn: a receiver's or a
// parameter's, a package variable's, or what a free function returns; "".
func (g *classGate) typeOf(fn *ast.FuncDecl, x ast.Expr) string {
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
		return g.vars[x.Name]
	case *ast.CallExpr:
		for _, c := range g.callees(fn, x.Fun) {
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

// callers is every call of target in this package.
func (g *classGate) callers(target *ast.FuncDecl) []called {
	var out []called
	for _, decl := range g.decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && slices.Contains(g.callees(fn, call.Fun), target) {
				out = append(out, called{fn: fn, call: call})
			}
			return true
		})
	}
	return out
}

// bucketOf is the bucket n names by a store's field (f.runs, where f is fn's
// *FleetStore or *Store), or by a variable fn asserts one into, "" for none.
func (g *classGate) bucketOf(fn *ast.FuncDecl, n ast.Node) string {
	if id, ok := n.(*ast.Ident); ok && fn.Body != nil {
		found := ""
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			as, ok := m.(*ast.AssignStmt)
			if !ok || found != "" || len(as.Rhs) != 1 || len(as.Lhs) == 0 || !isIdent(as.Lhs[0], id.Name) {
				return found == ""
			}
			if ta, ok := as.Rhs[0].(*ast.TypeAssertExpr); ok {
				found = g.bucketOf(fn, ta.X)
			}
			return found == ""
		})
		return found
	}
	sel, ok := n.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	if _, ok := g.holdings[sel.Sel.Name]; !ok || !storeTyped(fn, id.Name) {
		return ""
	}
	return sel.Sel.Name
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

// paramAt is the name of fn's parameter at argument i, the last for a
// variadic tail; "" where there is none.
func paramAt(fn *ast.FuncDecl, i int) string {
	var names []string
	for _, f := range fn.Type.Params.List {
		if len(f.Names) == 0 {
			names = append(names, "")
		}
		for _, n := range f.Names {
			names = append(names, n.Name)
		}
	}
	switch {
	case i < len(names):
		return names[i]
	case len(names) > 0:
		return names[len(names)-1]
	}
	return ""
}

// uses is every identifier named name in fn's body.
func uses(fn *ast.FuncDecl, name string) []ast.Node {
	var out []ast.Node
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok {
			ast.Inspect(sel.X, func(m ast.Node) bool {
				if id, ok := m.(*ast.Ident); ok && id.Name == name {
					out = append(out, id)
				}
				return true
			})
			return false
		}
		if id, ok := n.(*ast.Ident); ok && id.Name == name {
			out = append(out, id)
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
	consts map[string]string
	funcs  map[string][]string
	unread map[string]bool
}

// coordClassValues reads internal/coord's non-test source.
func coordClassValues(t *testing.T) coordClasses {
	t.Helper()
	c := coordClasses{consts: map[string]string{}, funcs: map[string][]string{}, unread: map[string]bool{}}
	files := parsePackage(t, token.NewFileSet(), "..")
	for _, file := range files {
		for _, decl := range file.Decls {
			for name, v := range stringConsts(decl) {
				c.consts[name] = v
			}
		}
	}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv != nil || fn.Body == nil || fn.Name.Name == "DocumentKey" ||
				fn.Name.Name == "DocumentFilter" {
				continue
			}
			builds := false
			var classes []string
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok || !isIdent(call.Fun, "DocumentKey") && !isIdent(call.Fun, "DocumentFilter") ||
					len(call.Args) == 0 {
					return true
				}
				builds = true
				if v, ok := c.value(call.Args[0], ""); ok && !slices.Contains(classes, v) {
					classes = append(classes, v)
				}
				return true
			})
			switch {
			case len(classes) > 0:
				c.funcs[fn.Name.Name] = classes
			case builds:
				c.unread[fn.Name.Name] = true
			}
		}
	}
	if len(c.funcs) < 30 || !slices.Equal(c.funcs["ConversationHoldKey"], []string{coord.ConversationHoldClass}) ||
		len(c.funcs["AtlasCodeScanKey"]) < 2 {
		t.Fatalf("premise: read %d key functions of internal/coord, ConversationHoldKey's classes %v and "+
			"AtlasCodeScanKey's %v", len(c.funcs), c.funcs["ConversationHoldKey"], c.funcs["AtlasCodeScanKey"])
	}
	return c
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
	ast.Fprint(&b, token.NewFileSet(), e, nil)
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
