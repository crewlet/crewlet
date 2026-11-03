package decltype

import (
	"go/ast"
	"go/token"
	"maps"
	"slices"
)

// TypeOf is the static type of the value e, spelled in f: [Type.Known] where
// the walk established it, [Type.Unread] where only a package it does not
// read could, and the zero Type where it cannot tell.
func (w *World) TypeOf(e ast.Expr, f *File) Type {
	w.depth++
	defer func() { w.depth-- }()
	if w.depth > maxRecursion {
		return Type{}
	}
	switch x := e.(type) {
	case *ast.ParenExpr:
		return w.TypeOf(x.X, f)
	case *ast.Ident:
		return w.identType(x, f)
	case *ast.BasicLit:
		// An untyped constant's default type.
		switch x.Kind {
		case token.INT:
			return w.basic("int")
		case token.FLOAT:
			return w.basic("float64")
		case token.IMAG:
			return w.basic("complex128")
		case token.CHAR:
			return w.basic("rune")
		case token.STRING:
			return w.basic("string")
		}
	case *ast.SelectorExpr:
		if ip, isPkg := w.PackageNamed(x.X, f); isPkg {
			p := w.Load(ip)
			if p == nil {
				return unreadFrom(ip)
			}
			if t := w.packageValue(p, x.Sel.Name); t.Known() || t.fromUnread {
				return t
			}
			if p.types[x.Sel.Name].spec != nil {
				// A TYPE of the package named where a value goes: a method
				// expression's receiver (lib.T.M), as identType's own.
				return Spelled(x, f)
			}
			return Type{}
		}
		return w.member(w.TypeOf(x.X, f), x.Sel.Name)
	case *ast.CallExpr:
		rs, from := w.results(x, f)
		if from.fromUnread {
			return from
		}
		if len(rs) > 0 {
			return rs[0]
		}
	case *ast.StarExpr:
		// Pointers are transparent, as a selector makes them.
		return w.TypeOf(x.X, f)
	case *ast.UnaryExpr:
		switch x.Op {
		case token.AND, token.ADD, token.SUB, token.XOR:
			return w.TypeOf(x.X, f)
		case token.NOT:
			return w.basic("bool")
		case token.ARROW:
			return w.elem(w.TypeOf(x.X, f))
		}
	case *ast.BinaryExpr:
		switch x.Op {
		case token.EQL, token.NEQ, token.LSS, token.LEQ, token.GTR, token.GEQ, token.LAND, token.LOR:
			return w.basic("bool")
		case token.SHL, token.SHR:
			return w.TypeOf(x.X, f)
		}
		// Either operand's: an untyped constant takes the other's type,
		// and the walk reads the left one's where it can.
		if t := w.TypeOf(x.X, f); t.Known() {
			return t
		}
		return w.TypeOf(x.Y, f)
	case *ast.IndexExpr:
		return w.elem(w.TypeOf(x.X, f))
	case *ast.SliceExpr:
		return w.TypeOf(x.X, f)
	case *ast.TypeAssertExpr:
		return Spelled(x.Type, f)
	case *ast.CompositeLit:
		return Spelled(x.Type, f)
	case *ast.FuncLit:
		return Spelled(x.Type, f)
	}
	return Type{}
}

// maxRecursion bounds TypeOf's recursion: a declaration that reaches back to
// itself through other declarations (var a = b; var b = a) ends here,
// unknown. The deepest either gate's walk of the tree goes is seven (five in
// this package's fixture), so sixty-four never cuts a real expression short.
const maxRecursion = 64

// member is the type of name selected on a value of type t: a field's type, a
// method's signature (a method value), or unread where nothing the walk read
// holds it and a type it does not read may.
func (w *World) member(t Type, name string) Type {
	sel := w.Select(t, name)
	switch sel.Kind {
	case Field, Method:
		return sel.Type
	case FromUnread:
		return t
	case Beyond:
		return unreadFrom(sel.Outside[0].Package)
	}
	return Type{}
}

// PackageNamed is the import path an expression names, when it is the name of
// an import in f rather than a value: an identifier the parser resolved to no
// local declaration, which no package-level value of f's package shadows.
func (w *World) PackageNamed(e ast.Expr, f *File) (string, bool) {
	id, ok := e.(*ast.Ident)
	if !ok || id.Obj != nil {
		return "", false
	}
	ip, ok := f.imports[id.Name]
	if !ok || w.declaresValue(f, id.Name) {
		return "", false
	}
	return ip, true
}

// identType is the type of a name, by the declaration it names.
func (w *World) identType(id *ast.Ident, f *File) Type {
	if id.Obj == nil {
		if f.Package == nil {
			return Type{}
		}
		switch id.Name {
		case "true", "false":
			if !w.declaresValue(f, id.Name) {
				return w.basic("bool")
			}
		}
		if t := w.packageValue(f.Package, id.Name); t.Known() || t.fromUnread {
			return t
		}
		if f.Package.types[id.Name].spec != nil {
			// A TYPE named where a value goes — a method expression's
			// receiver, as the TypeSpec case below — declared in another
			// file of the package, which the parser's file-scoped
			// resolution does not see: a package never declares a value and
			// a type by one name, so the name is the type.
			return Spelled(id, f)
		}
		return Type{}
	}
	switch d := id.Obj.Decl.(type) {
	case *ast.Field:
		// A parameter, a result or a receiver — never a type parameter,
		// which the parser declares by a field too.
		if id.Obj.Kind == ast.Var {
			if ell, ok := d.Type.(*ast.Ellipsis); ok {
				return Type{expr: &ast.ArrayType{Elt: ell.Elt}, file: f}
			}
			return Spelled(d.Type, f)
		}
	case *ast.ValueSpec:
		return w.specType(d, slices.IndexFunc(d.Names, func(n *ast.Ident) bool { return n.Name == id.Name }), f)
	case *ast.AssignStmt:
		return w.assignType(d, slices.IndexFunc(d.Lhs, func(l ast.Expr) bool {
			n, ok := l.(*ast.Ident)
			return ok && n.Name == id.Name
		}), f)
	case *ast.FuncDecl:
		return Spelled(d.Type, f)
	case *ast.TypeSpec:
		// A type named where a value goes: a method expression's receiver.
		return Spelled(id, f)
	}
	return Type{}
}

// packageValue is the type of a package-level var, const or function.
func (w *World) packageValue(p *Package, name string) Type {
	if v, ok := p.values[name]; ok {
		return w.specType(v.spec, v.index, v.file)
	}
	if fn, ok := p.funcs[name]; ok {
		return Spelled(fn.decl.Type, fn.file)
	}
	return Type{}
}

// specType is the type of the i-th name a var or const declaration declares.
func (w *World) specType(s *ast.ValueSpec, i int, f *File) Type {
	switch {
	case i < 0:
		return Type{}
	case s.Type != nil:
		return Spelled(s.Type, f)
	case len(s.Values) == len(s.Names):
		return w.TypeOf(s.Values[i], f)
	case len(s.Values) == 1:
		return w.multi(s.Values[0], i, f)
	}
	return Type{}
}

// assignType is the type of the i-th variable a := declares.
func (w *World) assignType(a *ast.AssignStmt, i int, f *File) Type {
	if i < 0 {
		return Type{}
	}
	if len(a.Rhs) == 1 {
		// The parser declares a range clause's variables as a := of a
		// unary RANGE over what is ranged.
		if u, ok := a.Rhs[0].(*ast.UnaryExpr); ok && u.Op == token.RANGE {
			return w.ranged(w.TypeOf(u.X, f), i)
		}
	}
	if len(a.Rhs) == len(a.Lhs) {
		return w.TypeOf(a.Rhs[i], f)
	}
	if len(a.Rhs) == 1 {
		return w.multi(a.Rhs[0], i, f)
	}
	return Type{}
}

// multi is the type of the i-th value of a multi-valued expression: a call's
// i-th result, or the value or the bool of a comma-ok form.
func (w *World) multi(e ast.Expr, i int, f *File) Type {
	switch x := ast.Unparen(e).(type) {
	case *ast.CallExpr:
		rs, from := w.results(x, f)
		if from.fromUnread {
			return from
		}
		if i < len(rs) {
			return rs[i]
		}
	case *ast.TypeAssertExpr, *ast.IndexExpr, *ast.UnaryExpr:
		if i == 0 {
			return w.TypeOf(x, f)
		}
		return w.basic("bool")
	}
	return Type{}
}

// elem is the type of an element of t: a slice's, an array's, a map's value,
// a channel's, a string's byte — unread for a type the walk does not read.
func (w *World) elem(t Type) Type {
	if _, unread := t.Unread(); unread {
		return t
	}
	lit, _, how, out := w.under(t)
	switch how {
	case Outside:
		if out.Package == "" {
			if out.Name == "string" {
				return w.basic("byte")
			}
			return Type{}
		}
		return unreadFrom(out.Package)
	case Unresolved:
		return Type{}
	}
	switch x := lit.expr.(type) {
	case *ast.ArrayType:
		return lit.In(x.Elt)
	case *ast.MapType:
		return lit.In(x.Value)
	case *ast.ChanType:
		return lit.In(x.Value)
	}
	return Type{}
}

// ranged is the type of a range clause's i-th variable over t.
func (w *World) ranged(t Type, i int) Type {
	if _, unread := t.Unread(); unread {
		return t
	}
	lit, _, how, out := w.under(t)
	switch how {
	case Outside:
		switch {
		case out.Package != "":
			return unreadFrom(out.Package)
		case out.Name == "string" && i == 0:
			return w.basic("int")
		case out.Name == "string":
			return w.basic("rune")
		case i == 0:
			// Over an integer: its own type.
			return t
		}
		return Type{}
	case Unresolved:
		return Type{}
	}
	switch x := lit.expr.(type) {
	case *ast.ArrayType:
		if i == 0 {
			return w.basic("int")
		}
		return lit.In(x.Elt)
	case *ast.MapType:
		if i == 0 {
			return lit.In(x.Key)
		}
		return lit.In(x.Value)
	case *ast.ChanType:
		if i == 0 {
			return lit.In(x.Value)
		}
	case *ast.FuncType:
		// An iterator: the yield function's parameters.
		if x.Params != nil && len(x.Params.List) == 1 {
			if yield, ok := x.Params.List[0].Type.(*ast.FuncType); ok {
				if ps := expand(yield.Params, lit); i < len(ps) {
					return ps[i]
				}
			}
		}
	}
	return Type{}
}

// expand lists a field list's types, one per name, spelled where at is.
func expand(fl *ast.FieldList, at Type) []Type {
	if fl == nil {
		return nil
	}
	var out []Type
	for _, field := range fl.List {
		for range max(1, len(field.Names)) {
			out = append(out, at.In(field.Type))
		}
	}
	return out
}

// results is what a call evaluates to: a conversion's type, a builtin's, or
// the results the called function declares — a generic one's with its type
// parameters bound, explicitly or as the compiler infers them from the
// arguments. from is unread, and the results nil, where only a package the
// walk does not read declares the function.
func (w *World) results(c *ast.CallExpr, f *File) (rs []Type, from Type) {
	fun := ast.Unparen(c.Fun)
	if w.isType(fun, f) {
		return []Type{Spelled(fun, f)}, Type{}
	}
	if id, ok := fun.(*ast.Ident); ok && id.Obj == nil && !w.declaresValue(f, id.Name) {
		switch id.Name {
		case "new", "make":
			if len(c.Args) > 0 {
				return []Type{Spelled(c.Args[0], f)}, Type{}
			}
		case "append", "min", "max":
			if len(c.Args) > 0 {
				return []Type{w.TypeOf(c.Args[0], f)}, Type{}
			}
		case "len", "cap", "copy":
			return []Type{w.basic("int")}, Type{}
		case "recover":
			return []Type{w.basic("any")}, Type{}
		}
		return nil, Type{}
	}
	callee, explicit := fun, []ast.Expr(nil)
	if args := typeArgs(fun); args != nil {
		// AN INSTANTIATION, or a call of a function held in a collection.
		var x ast.Expr
		switch ix := fun.(type) {
		case *ast.IndexExpr:
			x = ix.X
		case *ast.IndexListExpr:
			x = ix.X
		}
		if lit, _, how, _ := w.under(w.TypeOf(x, f)); how == Resolved {
			if ft, ok := lit.expr.(*ast.FuncType); ok && ft.TypeParams != nil {
				callee, explicit = x, args
			}
		}
	}
	ct := w.TypeOf(callee, f)
	if _, unread := ct.Unread(); unread {
		return nil, ct
	}
	lit, _, how, out := w.under(ct)
	if how == Outside && out.Package != "" {
		// A value of a function type a package the walk does not read
		// declares.
		return nil, unreadFrom(out.Package)
	}
	ft, ok := lit.expr.(*ast.FuncType)
	if how != Resolved || !ok {
		return nil, Type{}
	}
	s := subst{}
	maps.Copy(s, lit.subst)
	if params := typeParams(ft.TypeParams); len(params) > 0 {
		for i, p := range params {
			if i < len(explicit) {
				s[p] = Spelled(explicit[i], f)
			}
		}
		w.infer(ft, lit.file, c, f, params, s)
	}
	return expand(ft.Results, Type{expr: ft, file: lit.file, subst: s}), Type{}
}

// infer binds a generic function's type parameters from the arguments of a
// call of it, as the compiler infers them: each parameter's type unified with
// its argument's.
func (w *World) infer(ft *ast.FuncType, fnFile *File, c *ast.CallExpr, f *File,
	params []paramKey, s subst) {

	want := map[paramKey]bool{}
	for _, p := range params {
		want[p] = true
	}
	var ps []ast.Expr
	for _, field := range ft.Params.List {
		for range max(1, len(field.Names)) {
			ps = append(ps, field.Type)
		}
	}
	for i, a := range c.Args {
		var p ast.Expr
		switch {
		case i < len(ps)-1:
			p = ps[i]
		case len(ps) > 0:
			p = ps[len(ps)-1]
			if ell, ok := p.(*ast.Ellipsis); ok {
				if c.Ellipsis.IsValid() {
					continue
				}
				p = ell.Elt
			} else if i >= len(ps) {
				continue
			}
		default:
			continue
		}
		w.unify(p, fnFile, w.TypeOf(a, f), want, s)
	}
}

// unify binds the type parameters p spells (those in want) to the parts of a
// they stand for.
func (w *World) unify(p ast.Expr, pf *File, a Type, want map[paramKey]bool, s subst) {
	if !a.Known() {
		return
	}
	p = ast.Unparen(p)
	if id, ok := p.(*ast.Ident); ok {
		if typeParam(id) && want[keyOf(id)] {
			if _, done := s[keyOf(id)]; !done {
				s[keyOf(id)] = a
			}
		}
		return
	}
	a, ok := w.bound(a)
	if !ok {
		return
	}
	switch x := p.(type) {
	case *ast.StarExpr:
		if y, ok := ast.Unparen(a.expr).(*ast.StarExpr); ok {
			w.unify(x.X, pf, a.In(y.X), want, s)
		}
	case *ast.IndexExpr, *ast.IndexListExpr:
		// A generic type's instantiation: the argument instantiates the same
		// type, and its own type arguments bind these.
		pd, _, how, _ := w.named(Type{expr: x, file: pf})
		ad, inst, ok := w.instance(a)
		if how != Resolved || !ok || ad.spec != pd.spec {
			return
		}
		pargs, aargs := typeArgs(x), typeArgs(inst.expr)
		for i := range min(len(pargs), len(aargs)) {
			w.unify(pargs[i], pf, inst.In(aargs[i]), want, s)
		}
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType:
		lit, _, how, _ := w.under(a)
		if how != Resolved {
			return
		}
		switch y := lit.expr.(type) {
		case *ast.ArrayType:
			if px, ok := x.(*ast.ArrayType); ok {
				w.unify(px.Elt, pf, lit.In(y.Elt), want, s)
			}
		case *ast.MapType:
			if px, ok := x.(*ast.MapType); ok {
				w.unify(px.Key, pf, lit.In(y.Key), want, s)
				w.unify(px.Value, pf, lit.In(y.Value), want, s)
			}
		case *ast.ChanType:
			if px, ok := x.(*ast.ChanType); ok {
				w.unify(px.Value, pf, lit.In(y.Value), want, s)
			}
		case *ast.FuncType:
			if px, ok := x.(*ast.FuncType); ok {
				for _, pair := range [][2]*ast.FieldList{{px.Params, y.Params}, {px.Results, y.Results}} {
					pp, ap := expand(pair[0], Type{}), expand(pair[1], lit)
					for i := range min(len(pp), len(ap)) {
						w.unify(pp[i].expr, pf, ap[i], want, s)
					}
				}
			}
		}
	}
}

// declaresValue reports whether f's package declares name at package level.
func (w *World) declaresValue(f *File, name string) bool {
	if f.Package == nil {
		return false
	}
	_, v := f.Package.values[name]
	_, fn := f.Package.funcs[name]
	return v || fn
}

// isType reports whether e, in a call's function position, names a type —
// which makes the call a conversion.
func (w *World) isType(e ast.Expr, f *File) bool {
	switch x := e.(type) {
	case *ast.ParenExpr:
		return w.isType(x.X, f)
	case *ast.StarExpr:
		return w.isType(x.X, f)
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType, *ast.InterfaceType, *ast.StructType:
		return true
	case *ast.IndexExpr:
		return w.isType(x.X, f)
	case *ast.IndexListExpr:
		return w.isType(x.X, f)
	case *ast.Ident:
		if x.Obj != nil {
			return x.Obj.Kind == ast.Typ
		}
		if f.Package != nil {
			if _, ok := f.Package.types[x.Name]; ok {
				return true
			}
		}
		return predeclared[x.Name] || w.universe().types[x.Name].spec != nil
	case *ast.SelectorExpr:
		ip, isPkg := w.PackageNamed(x.X, f)
		if !isPkg {
			return false
		}
		p := w.Load(ip)
		if p == nil {
			// A package the walk does not read: whether this is a type or
			// a function, only its declaration says, and the call is
			// unread either way.
			return false
		}
		_, ok := p.types[x.Sel.Name]
		return ok
	}
	return false
}
