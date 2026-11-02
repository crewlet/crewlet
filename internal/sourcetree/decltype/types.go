package decltype

import (
	"go/ast"
	"go/parser"
	"go/token"
)

// Type is what the walk knows of a value's type: an expression it resolved —
// a type expression, read where it is spelled, which resolves its names, with
// what its type parameters are bound to there — or the package whose
// declaration alone says what it is, or nothing.
//
// Three answers, never two ([Type.Known], [Type.Unread], neither). A gate
// decides what an unread or an unknown value means for its own subject: an
// absence gate that took "cannot tell" for "not the thing" would certify
// nothing about exactly the values it skipped.
type Type struct {
	expr  ast.Expr
	file  *File
	subst subst

	// fromUnread marks a value only a declaration of a package this world
	// does not read can type — unread names that package.
	fromUnread bool
	unread     string
}

// Known reports whether the walk established the type: a type expression it
// resolved, which may still name a type of a package it does not read — that
// type's identity is known ([World.Named]) and its structure is not.
func (t Type) Known() bool { return t.expr != nil }

// Unread reports whether the value is one only a declaration of a package
// this world does not read can type — a function's result, a variable, a
// member or an element of such a package's type — and names that package
// ("" for the universe's own).
func (t Type) Unread() (string, bool) { return t.unread, t.fromUnread }

// Expr is the type expression, or nil where the type is not known.
func (t Type) Expr() ast.Expr { return t.expr }

// File is the file the type expression is spelled in, which resolves its
// names.
func (t Type) File() *File { return t.file }

// In is the type expression e, spelled where t is — with t's bindings — such
// as an element of the literal t resolved to.
func (t Type) In(e ast.Expr) Type { return Type{expr: e, file: t.file, subst: t.subst} }

// Spelled is the type a type expression names where f spells it: the zero
// Type for a nil expression, which is what a type switch's guard asserts to.
func Spelled(e ast.Expr, f *File) Type {
	if e == nil {
		return Type{}
	}
	return Type{expr: e, file: f}
}

func unreadFrom(importPath string) Type { return Type{fromUnread: true, unread: importPath} }

// TypeName names a declared type by its package's import path: "" for the
// universe's (error, int).
type TypeName struct{ Package, Name string }

// String is the name as a file importing the package with no name of its
// own spells it.
func (n TypeName) String() string {
	if n.Package == "" {
		return n.Name
	}
	return ImportName(n.Package) + "." + n.Name
}

// Resolution is how far a type resolved.
type Resolution int

const (
	// Resolved is a type resolved to a type literal this world read.
	Resolved Resolution = iota
	// Outside is a type resolved to one whose structure this world does not
	// read — a type of a package it does not read, or a basic type of the
	// universe. Its identity is still known.
	Outside
	// Unresolved is a type resolved to something the walk cannot read — a
	// type parameter nothing bound, a name it cannot find — which may be
	// anything.
	Unresolved
)

// subst binds a generic declaration's type parameters to the types an
// instantiation or an inference gave them: op[A, R] read as
// op[ensureTagsArgs, ensuredTags], or a call of
// call[A, R any](…, o op[A, R], …) (R, error) whose R its argument decides.
type subst map[paramKey]Type

// paramKey names one type parameter: the node that declares it — the field of
// the parameter list, which the parser's object for each of its names records
// — and its name.
//
// A GENERIC METHOD'S RECEIVER is the exception: the parser gives
// `func (b box[T]) get() T`'s T no object, neither the receiver's nor any use
// of it (go.dev/issue/50956), so its parameters are bound by name alone, under
// [receiverParams]. That binding is safe for exactly the expressions it
// reaches: those spelled in the method's own signature, where the receiver's
// names shadow every other, because resolving a named type starts its
// declaration's expressions with that declaration's own bindings.
type paramKey struct {
	decl any
	name string
}

// receiverParams is the declaration a generic method's receiver parameters
// are bound under.
type receiverParams struct{}

// keyOf is the key of the type parameter a use of one names.
func keyOf(id *ast.Ident) paramKey { return paramKey{id.Obj.Decl, id.Name} }

// paramOf reports whether id, spelled where t is, names a type parameter
// where t's bindings bind them, and what it is bound to.
//
// A generic method's receiver parameter nothing binds is a parameter too,
// found by the method that declares it ([receiverParam]): taken for a name of
// the package's scope, a value of it was a type the walk could not resolve
// — a gate refusing what it cannot type refused it (code/cache's writers
// gate, over `func (c ListChange[T]) …`) — or, where the package declares a
// type of that name too, that type, which the receiver's name shadows.
func paramOf(id *ast.Ident, t Type) (bound Type, isBound, isParam bool) {
	switch {
	case typeParam(id):
		bound, isBound = t.subst[keyOf(id)]
		return bound, isBound, true
	case id.Obj == nil:
		if bound, isBound = t.subst[paramKey{receiverParams{}, id.Name}]; isBound {
			return bound, true, true
		}
		if receiverParam(t.file, id) {
			return Type{}, false, true
		}
	}
	return Type{}, false, false
}

// receiverParam reports whether id, a name the parser gave no object, is
// spelled inside a generic method of f whose receiver declares a type
// parameter of that name: the parser resolves no use of one
// (go.dev/issue/50956), and inside its method the receiver's name shadows
// the package's, while a local declaration shadowing it in turn would carry
// an object of its own.
func receiverParam(f *File, id *ast.Ident) bool {
	if f == nil || f.AST == nil {
		return false
	}
	for _, d := range f.AST.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || id.Pos() < fn.Pos() || id.Pos() >= fn.End() {
			continue
		}
		recv := fn.Recv.List[0].Type
		if star, ok := recv.(*ast.StarExpr); ok {
			recv = star.X
		}
		for _, p := range typeArgs(recv) {
			if name, ok := p.(*ast.Ident); ok && name.Name == id.Name {
				return true
			}
		}
		return false
	}
	return false
}

// Param reports whether t is a type parameter nothing binds where it is
// spelled — a function's, a type's or a generic method's receiver's — through
// the parameters that are bound to another: a value of one is a value of
// whatever an instantiation will make it, which a gate judges where a
// conversion or an assertion types it, never as a type it cannot resolve.
func (w *World) Param(t Type) bool {
	for range maxChain {
		id, ok := ast.Unparen(t.expr).(*ast.Ident)
		if !ok {
			return false
		}
		b, isBound, isParam := paramOf(id, t)
		switch {
		case !isParam:
			return false
		case !isBound:
			return true
		}
		t = b
	}
	return false
}

// typeParam reports whether id names a type parameter of a type parameter
// list: a type name the parser declared other than by a type declaration. A
// generic method's receiver parameters carry no object at all ([paramKey]).
func typeParam(id *ast.Ident) bool {
	if id.Obj == nil || id.Obj.Kind != ast.Typ {
		return false
	}
	_, isSpec := id.Obj.Decl.(*ast.TypeSpec)
	return !isSpec
}

// typeParams lists the parameters a type parameter list declares, in order.
func typeParams(fl *ast.FieldList) []paramKey {
	if fl == nil {
		return nil
	}
	var out []paramKey
	for _, f := range fl.List {
		for _, n := range f.Names {
			out = append(out, paramKey{f, n.Name})
		}
	}
	return out
}

// typeArgs lists an instantiation's type arguments, or nil for an expression
// that instantiates nothing.
func typeArgs(e ast.Expr) []ast.Expr {
	switch x := ast.Unparen(e).(type) {
	case *ast.IndexExpr:
		return []ast.Expr{x.Index}
	case *ast.IndexListExpr:
		return x.Indices
	}
	return nil
}

// predeclared are the universe's type names that are not read from
// [universeSource]: the basic types, whose structure there is none of, and
// comparable, a constraint no value has.
var predeclared = map[string]bool{
	"bool": true, "byte": true, "complex64": true, "complex128": true,
	"float32": true, "float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true, "uint": true,
	"uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"comparable": true,
}

// universeSource declares the universe's two types that have a structure to
// read: error's one method, and any, which is an empty interface.
const universeSource = `package universe

type error interface{ Error() string }

type any = interface{}
`

// universe is the universe's declarations, read once per world.
func (w *World) universe() *Package {
	if w.univ != nil {
		return w.univ
	}
	parsed, err := parser.ParseFile(w.fset, "universe.go", universeSource, 0)
	if err != nil {
		panic("decltype: the universe's own declarations do not parse: " + err.Error())
	}
	// Path "" — the universe's types are named by no package.
	p := &Package{types: map[string]typeDecl{}, values: map[string]valueDecl{},
		funcs: map[string]funcDecl{}, methods: map[string]map[string]funcDecl{}}
	f := &File{Package: p, Path: "universe.go", Rel: "universe.go", AST: parsed,
		imports: map[string]string{}}
	p.files = []*File{f}
	p.declare(f)
	w.univ = p
	return p
}

// named resolves a name in a type position — a generic one's instantiation
// included — to the declaration it names, and binds that declaration's type
// parameters to the instantiation's arguments, each read where the
// instantiation is spelled. A type it does not read is Outside, and named by
// outside.
func (w *World) named(t Type) (d typeDecl, s subst, how Resolution, outside TypeName) {
	base := ast.Unparen(t.expr)
	switch x := base.(type) {
	case *ast.IndexExpr:
		base = ast.Unparen(x.X)
	case *ast.IndexListExpr:
		base = ast.Unparen(x.X)
	}
	switch x := base.(type) {
	case *ast.Ident:
		switch {
		case x.Obj != nil:
			spec, isSpec := x.Obj.Decl.(*ast.TypeSpec)
			if !isSpec || x.Obj.Kind != ast.Typ {
				return d, nil, Unresolved, outside // a type parameter
			}
			d = typeDecl{spec, t.file}
		case t.file.Package != nil && t.file.Package.types[x.Name].spec != nil:
			d = t.file.Package.types[x.Name]
		case w.universe().types[x.Name].spec != nil:
			d = w.universe().types[x.Name]
		case predeclared[x.Name]:
			return d, nil, Outside, TypeName{Name: x.Name}
		default:
			return d, nil, Unresolved, outside
		}
	case *ast.SelectorExpr:
		pkgID, ok := x.X.(*ast.Ident)
		if !ok {
			return d, nil, Unresolved, outside
		}
		ip, ok := t.file.imports[pkgID.Name]
		if !ok {
			return d, nil, Unresolved, outside
		}
		p := w.Load(ip)
		if p == nil {
			return d, nil, Outside, TypeName{Package: ip, Name: x.Sel.Name}
		}
		if d, ok = p.types[x.Sel.Name]; !ok {
			return d, nil, Unresolved, outside
		}
	default:
		return d, nil, Unresolved, outside
	}
	params := typeParams(d.spec.TypeParams)
	if len(params) == 0 {
		return d, nil, Resolved, outside
	}
	args := typeArgs(t.expr)
	s = subst{}
	for i, p := range params {
		if i < len(args) {
			s[p] = t.In(args[i])
		}
	}
	return d, s, Resolved, outside
}

// bound follows t while it names a bound type parameter, to the type it is
// bound to; ok is false for a type parameter nothing bound.
func (w *World) bound(t Type) (Type, bool) {
	for range maxChain {
		id, isIdent := ast.Unparen(t.expr).(*ast.Ident)
		if !isIdent {
			return t, true
		}
		b, isBound, isParam := paramOf(id, t)
		switch {
		case !isParam:
			return t, true
		case !isBound:
			return Type{}, false
		}
		t = b
	}
	return Type{}, false
}

// maxChain bounds every chain of names the walk follows — aliases, bound
// parameters, defined types over defined types — so a declaration that
// reaches back to itself ends the walk rather than the test. The longest
// chain either gate's walk of the tree follows is four links (three in this
// package's fixture), so thirty-two never cuts a real one short, and a
// cycle costs thirty-two cheap steps.
const maxChain = 32

// under resolves t through pointers, bound type parameters, names and
// aliases to the type literal that defines it, and the declaration whose
// literal that is (nil for a literal spelled in place). A type it does not
// read is Outside, and named by outside.
func (w *World) under(t Type) (lit Type, owner *typeDecl, how Resolution, outside TypeName) {
	for range maxChain {
		if !t.Known() {
			return Type{}, nil, Unresolved, outside
		}
		if id, ok := t.expr.(*ast.Ident); ok {
			if b, isBound, isParam := paramOf(id, t); isParam {
				if !isBound {
					return Type{}, nil, Unresolved, outside
				}
				t = b
				continue
			}
		}
		switch x := t.expr.(type) {
		case *ast.ParenExpr:
			t = t.In(x.X)
		case *ast.StarExpr:
			t = t.In(x.X)
		case *ast.Ident, *ast.SelectorExpr, *ast.IndexExpr, *ast.IndexListExpr:
			d, s, how, out := w.named(t)
			if how != Resolved {
				return Type{}, nil, how, out
			}
			owner = &d
			t = Type{expr: d.spec.Type, file: d.file, subst: s}
		default:
			if owner != nil && ast.Unparen(owner.spec.Type) != t.expr {
				owner = nil
			}
			return t, owner, Resolved, outside
		}
	}
	return Type{}, nil, Unresolved, outside
}

// defined resolves t through pointers, bound type parameters and aliases to
// the defined type it names — the one its methods are declared on — and the
// instantiation that named it. A literal names none (Unresolved, with no
// outside name), and a type this world does not read is Outside.
func (w *World) defined(t Type) (typeDecl, Type, Resolution, TypeName) {
	for range maxChain {
		var ok bool
		if t, ok = w.bound(t); !ok {
			return typeDecl{}, Type{}, Unresolved, TypeName{}
		}
		switch x := t.expr.(type) {
		case *ast.ParenExpr:
			t = t.In(x.X)
			continue
		case *ast.StarExpr:
			t = t.In(x.X)
			continue
		}
		d, s, how, out := w.named(t)
		if how != Resolved {
			return typeDecl{}, Type{}, how, out
		}
		if !d.spec.Assign.IsValid() {
			return d, t, Resolved, TypeName{}
		}
		t = Type{expr: d.spec.Type, file: d.file, subst: s}
	}
	return typeDecl{}, Type{}, Unresolved, TypeName{}
}

// instance resolves t through bound type parameters and aliases to an
// instantiation of a generic type, and the declaration it instantiates.
func (w *World) instance(t Type) (typeDecl, Type, bool) {
	for range maxChain {
		var ok bool
		if t, ok = w.bound(t); !ok {
			return typeDecl{}, Type{}, false
		}
		t = t.In(ast.Unparen(t.expr))
		d, s, how, _ := w.named(t)
		if how != Resolved {
			return typeDecl{}, Type{}, false
		}
		if typeArgs(t.expr) != nil || !d.spec.Assign.IsValid() {
			return d, t, true
		}
		t = Type{expr: d.spec.Type, file: d.file, subst: s}
	}
	return typeDecl{}, Type{}, false
}

// Named is the defined type t names, through pointers, bound type parameters
// and aliases — read, or of a package this world does not read, whose name is
// all it knows of it. False for a type literal, and for a type the walk
// cannot resolve.
func (w *World) Named(t Type) (TypeName, bool) {
	if !t.Known() {
		return TypeName{}, false
	}
	d, _, how, out := w.defined(t)
	switch how {
	case Resolved:
		return d.name(), true
	case Outside:
		return out, true
	}
	return TypeName{}, false
}

// Underlying is the type literal t is defined by — its struct, interface,
// slice, map, channel, function or array literal, read where it is spelled —
// through pointers, bound type parameters, names and aliases; or how far it
// got where it reached a type it does not read (Outside) or cannot
// (Unresolved).
func (w *World) Underlying(t Type) (Type, Resolution) {
	lit, _, how, _ := w.under(t)
	return lit, how
}

// basic is the universe's basic type name, spelled in the universe's file.
func (w *World) basic(name string) Type {
	return Type{expr: &ast.Ident{Name: name, NamePos: token.NoPos}, file: w.universe().files[0]}
}
