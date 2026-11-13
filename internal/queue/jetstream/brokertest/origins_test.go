package brokertest_test

import (
	"go/ast"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// brokertestPath is this package, whose TempDir is the one directory a test
// broker keeps its store in.
const brokertestPath = serverModule + "/internal/queue/jetstream/brokertest"

// storeOrigins follows what a store directory's value is made from, within
// one file: through every local the value reads to each value that local is
// given, through a parameter of one of the file's functions to what each of
// the file's calls of that function passes there, and through a field of a
// type this module declares to each value the file gives that field
// (TestEveryNATSServerATestBuildsKeepsItsStoreInATempDir has the rule and its
// boundary).
type storeOrigins struct {
	w *decltype.World
	f *decltype.File
	// test is whether f is a test file, where a directory os.MkdirTemp or
	// os.TempDir makes is a case's own, removed by the case.
	test bool
	// given is every value each local of the file is given — by :=, =, a
	// var declaration or a range clause.
	given map[local][]ast.Expr
	// params is the function and position of every parameter of the file's
	// function declarations.
	params map[local]paramAt
	// calls is every call the file makes, for what a parameter is passed.
	calls []*ast.CallExpr
	// sets is every assignment to a selector the file makes, and literals
	// every composite literal it spells: what a field is given, read into
	// fields the first time a value is followed through a field, since
	// typing each one is the costly part.
	sets     []fieldSet
	literals []*ast.CompositeLit
	fields   map[fieldOf][]ast.Expr
}

// local is a variable or a parameter of the file: the declaration the
// parser's resolution names for an identifier, and the name. It is what an
// *ast.Object stands for, spelled as decltype spells it rather than by the
// type go/ast deprecates, since what that deprecation warns of — a composite
// literal's key, which only types can tell a field from a variable — is never
// followed here ([storeOrigins.follow]). One declaration declares a name once,
// so a local shadowing another is its own.
type local struct {
	decl any
	name string
}

// localOf is the local id names, where the parser resolved it to one.
func localOf(id *ast.Ident) (local, bool) {
	if id.Obj == nil || id.Obj.Decl == nil {
		return local{}, false
	}
	return local{decl: id.Obj.Decl, name: id.Name}, true
}

// paramAt is a parameter: its function, and its position among the
// function's parameters.
type paramAt struct {
	fn    *ast.FuncDecl
	index int
}

// fieldOf is a field of a type this module declares.
type fieldOf struct {
	owner decltype.TypeName
	name  string
}

// fieldSet is one assignment to a selector: the selector, and the value it is
// given.
type fieldSet struct {
	sel   *ast.SelectorExpr
	value ast.Expr
}

// storeMade is what a store's value was followed to.
type storeMade struct {
	// kept is whether it reaches brokertest.TempDir.
	kept bool
	// makers is every other maker of a temporary directory it reaches.
	makers []tempMaker
}

// tempMaker is a call making a temporary directory other than
// brokertest.TempDir: what it is and where, or, for a TempDir call whose
// receiver the walk could not type, that receiver.
type tempMaker struct {
	what     string
	at       ast.Node
	untyped  bool
	receiver ast.Expr
}

// originsIn reads the file f for [storeOrigins.made].
func originsIn(w *decltype.World, f *decltype.File) *storeOrigins {
	o := &storeOrigins{w: w, f: f, test: strings.HasSuffix(f.Path, "_test.go"),
		given: map[local][]ast.Expr{}, params: map[local]paramAt{}}
	for _, decl := range f.AST.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		index := 0
		for _, field := range fn.Type.Params.List {
			if len(field.Names) == 0 {
				index++
				continue
			}
			for _, name := range field.Names {
				if l, ok := localOf(name); ok {
					o.params[l] = paramAt{fn: fn, index: index}
				}
				index++
			}
		}
	}
	ast.Inspect(f.AST, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range x.Lhs {
				// The value paired with it, or the one call whose results
				// the whole left side takes.
				value := x.Rhs[0]
				if len(x.Rhs) == len(x.Lhs) {
					value = x.Rhs[i]
				}
				switch l := ast.Unparen(lhs).(type) {
				case *ast.Ident:
					o.give(l, value)
				case *ast.SelectorExpr:
					o.sets = append(o.sets, fieldSet{sel: l, value: value})
				}
			}
		case *ast.ValueSpec:
			for i, name := range x.Names {
				switch {
				case len(x.Values) == len(x.Names):
					o.give(name, x.Values[i])
				case len(x.Values) == 1:
					o.give(name, x.Values[0])
				}
			}
		case *ast.RangeStmt:
			for _, e := range []ast.Expr{x.Key, x.Value} {
				if id, ok := e.(*ast.Ident); ok {
					o.give(id, x.X)
				}
			}
		case *ast.CompositeLit:
			o.literals = append(o.literals, x)
		case *ast.CallExpr:
			o.calls = append(o.calls, x)
		}
		return true
	})
	return o
}

// give records that the local id names is given value.
func (o *storeOrigins) give(id *ast.Ident, value ast.Expr) {
	if l, ok := localOf(id); ok {
		o.given[l] = append(o.given[l], value)
	}
}

// made is what the value e is made from, followed through what it reads.
func (o *storeOrigins) made(e ast.Expr) storeMade {
	var out storeMade
	if e != nil {
		o.follow(e, map[any]bool{}, &out)
	}
	return out
}

// follow adds to out what e is made from, following each local, parameter and
// field it reads that seen does not hold yet.
func (o *storeOrigins) follow(e ast.Expr, seen map[any]bool, out *storeMade) {
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CallExpr:
			o.maker(x, out)
		case *ast.SelectorExpr:
			if key, ok := o.field(x); ok && !seen[key] {
				seen[key] = true
				for _, v := range o.fieldValues()[key] {
					o.follow(v, seen, out)
				}
			}
		case *ast.KeyValueExpr:
			// A COMPOSITE LITERAL'S KEY IS NEVER FOLLOWED, only its value:
			// in a struct's literal the key is a field's name, which the
			// parser's resolution may have bound to a local of that name.
			o.follow(x.Value, seen, out)
			return false
		case *ast.Ident:
			l, ok := localOf(x)
			if !ok || seen[l] {
				break
			}
			seen[l] = true
			for _, v := range o.given[l] {
				o.follow(v, seen, out)
			}
			if p, ok := o.params[l]; ok {
				for _, arg := range o.argsTo(p) {
					o.follow(arg, seen, out)
				}
			}
		}
		return true
	})
}

// maker adds the call c to out where it makes a temporary directory.
func (o *storeOrigins) maker(c *ast.CallExpr, out *storeMade) {
	sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
	if !ok {
		return
	}
	if pkg, ok := o.w.PackageNamed(sel.X, o.f); ok {
		switch name := sel.Sel.Name; {
		case pkg == brokertestPath && name == "TempDir":
			out.kept = true
		case pkg == "os" && o.test && (name == "MkdirTemp" || name == "TempDir"):
			out.makers = append(out.makers, tempMaker{what: "os." + name, at: c})
		}
		return
	}
	if sel.Sel.Name != "TempDir" || len(c.Args) != 0 {
		return
	}
	switch o.testsTempDir(sel.X) {
	case tempDirOfATest:
		out.makers = append(out.makers, tempMaker{what: "a testing.TB's TempDir", at: c})
	case tempDirUnknown:
		out.makers = append(out.makers, tempMaker{at: c, untyped: true, receiver: sel.X})
	}
}

// tempDirOf is whose TempDir a method call is.
type tempDirOf int

const (
	// tempDirUnknown is one on a value the walk cannot type.
	tempDirUnknown tempDirOf = iota
	// tempDirOfATest is the testing package's: a testing.TB's, a
	// *testing.T's, a *testing.B's or a *testing.F's, promoted through an
	// embedding or not, or one an interface lists.
	tempDirOfATest
	// tempDirOfTheModule is a method a type of the module declares.
	tempDirOfTheModule
)

// testsTempDir is whose TempDir a call on the value x is.
func (o *storeOrigins) testsTempDir(x ast.Expr) tempDirOf {
	t := o.w.TypeOf(x, o.f)
	if name, ok := o.w.Named(t); ok && name.Package == "testing" {
		return tempDirOfATest
	}
	switch s := o.w.Select(t, "TempDir"); s.Kind {
	case decltype.Method:
		// AN INTERFACE'S IS A TEST'S: what the module would list a
		// TempDir for is a testing.TB, or a stand-in for one.
		if s.Owner.Package == "testing" || s.Interface {
			return tempDirOfATest
		}
		return tempDirOfTheModule
	case decltype.Beyond:
		if slices.ContainsFunc(s.Outside, func(n decltype.TypeName) bool { return n.Package == "testing" }) {
			return tempDirOfATest
		}
	}
	return tempDirUnknown
}

// field is the field of a type this module declares that sel selects.
func (o *storeOrigins) field(sel *ast.SelectorExpr) (fieldOf, bool) {
	s := o.w.Select(o.w.TypeOf(sel.X, o.f), sel.Sel.Name)
	if s.Kind != decltype.Field || !ownType(s.Owner) {
		return fieldOf{}, false
	}
	return fieldOf{owner: s.Owner, name: sel.Sel.Name}, true
}

// fieldValues is every value the file gives a field of a type this module
// declares: by assignment, or as a keyed element of a literal of the type.
func (o *storeOrigins) fieldValues() map[fieldOf][]ast.Expr {
	if o.fields != nil {
		return o.fields
	}
	o.fields = map[fieldOf][]ast.Expr{}
	for _, set := range o.sets {
		if key, ok := o.field(set.sel); ok {
			o.fields[key] = append(o.fields[key], set.value)
		}
	}
	for _, lit := range o.literals {
		owner, ok := o.w.Named(o.w.TypeOf(lit, o.f))
		if !ok || !ownType(owner) {
			continue
		}
		for _, e := range lit.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if id, ok := kv.Key.(*ast.Ident); ok {
				key := fieldOf{owner: owner, name: id.Name}
				o.fields[key] = append(o.fields[key], kv.Value)
			}
		}
	}
	return o.fields
}

// argsTo is what each of the file's calls of p's function passes as p.
func (o *storeOrigins) argsTo(p paramAt) []ast.Expr {
	var args []ast.Expr
	for _, c := range o.calls {
		if p.index < len(c.Args) && o.invokes(c, p.fn) {
			args = append(args, c.Args[p.index])
		}
	}
	return args
}

// invokes reports whether the call c calls fn, a function of the file: by its
// name, which the parser resolves within the file, or as a method of the type
// that declares it.
func (o *storeOrigins) invokes(c *ast.CallExpr, fn *ast.FuncDecl) bool {
	switch fun := ast.Unparen(c.Fun).(type) {
	case *ast.Ident:
		return fn.Recv == nil && fun.Obj != nil && fun.Obj.Decl == fn
	case *ast.SelectorExpr:
		if fn.Recv == nil || len(fn.Recv.List) == 0 || fun.Sel.Name != fn.Name.Name {
			return false
		}
		m := o.w.Select(o.w.TypeOf(fun.X, o.f), fun.Sel.Name)
		return m.Kind == decltype.Method &&
			m.Owner == decltype.TypeName{Package: o.f.Package.Path, Name: decltype.BaseName(fn.Recv.List[0].Type)}
	}
	return false
}

// ownType reports whether name is a type this module declares.
func ownType(name decltype.TypeName) bool {
	return name.Package == serverModule || strings.HasPrefix(name.Package, serverModule+"/")
}
