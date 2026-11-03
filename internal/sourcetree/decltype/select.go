package decltype

import "go/ast"

// Selection is what a selector x.name resolves to, on a value of x's type.
type Selection struct {
	// Kind is what the walk found.
	Kind SelectionKind
	// Type is a field's type, or a method's signature — its *ast.FuncType —
	// read where it is declared, a generic receiver's type parameters bound
	// to the instantiation it is selected on.
	Type Type
	// Owner is the declared type holding the member: the one whose struct
	// literal declares the field, whose method set declares the method, or
	// whose interface literal lists it. Zero for a member of a literal
	// spelled in place.
	Owner TypeName
	// Interface reports a method an interface lists, rather than one a type
	// declares with a body.
	Interface bool
	// Outside names, for Beyond, every type of a package this world does
	// not read that the search met, any of which may hold the member.
	Outside []TypeName
}

// SelectionKind is what a selector resolves to, as far as the walk can say.
type SelectionKind int

const (
	// Undetermined is a selector the walk cannot place: the receiver is a
	// value it could not type, a part of it could not be read, two members
	// at one depth answer (which the compiler refuses, so something was
	// misread), or the member found is deeper than a part the walk does not
	// read, which may hold the same name and shadow it.
	Undetermined SelectionKind = iota
	// Field is a struct field, the receiver's own or promoted.
	Field
	// Method is a method, declared on a type or listed by an interface.
	Method
	// Beyond is a name nothing the walk read holds, where part of the
	// receiver is a type it does not read, which may ([Selection.Outside]).
	Beyond
	// Absent is a name a receiver read whole does not hold. The compiler
	// refuses such a selector, so the walk misread something.
	Absent
	// FromUnread is a receiver that is itself a value only a declaration of
	// a package the world does not read can type ([Type.Unread]).
	FromUnread
)

// maxDepth bounds how deep through embedded fields a selector is looked up,
// and how deep an interface's embeddings are followed — a guard against a
// type that embeds a pointer to itself, which the compiler accepts, never a
// limit a real walk meets: every member both gates found in the tree sat at
// depth zero, the deepest this package's fixture reaches is one (two for an
// interface's embeddings), and the NATS server's API responses embed their
// stream information one level down. Eight is what both gates' walks used
// before they were one.
const maxDepth = 8

// Select resolves the selector name on a value of type t the way the
// language does: a field or a method at the SHALLOWEST depth, the receiver's
// own at depth zero and each embedded field's a level deeper, a method
// declared on a defined type found where that type is, and an interface's
// whole method set — what it lists and what it embeds — at the depth of the
// interface.
func (w *World) Select(t Type, name string) Selection {
	if _, unread := t.Unread(); unread {
		return Selection{Kind: FromUnread}
	}
	if !t.Known() {
		return Selection{}
	}
	var outside []TypeName
	unresolved := false
	// unsure is the shallowest depth at which a part of t could not be read,
	// or was a type of a package the world does not read: a member found
	// deeper than that may be shadowed by one it would have held.
	unsure := -1
	met := func(depth int, how Resolution, out TypeName) {
		switch how {
		case Outside:
			if !containsName(outside, out) {
				outside = append(outside, out)
			}
		case Unresolved:
			unresolved = true
		}
		if unsure < 0 {
			unsure = depth
		}
	}
	seen := map[*ast.StructType]bool{}
	level := []Type{t}
	for depth := 0; depth < maxDepth && len(level) > 0; depth++ {
		var hits []Selection
		var next []Type
		for _, lt := range level {
			lit, owner, how, out := w.under(lt)
			if how != Resolved {
				met(depth, how, out)
				continue
			}
			// A method its defined type declares. A type declaring a
			// field and a method of one name is a program the compiler
			// refuses, so one found ends this part's search.
			if d, inst, how, _ := w.defined(lt); how == Resolved {
				if m, ok := d.file.Package.methods[d.spec.Name.Name][name]; ok {
					hits = append(hits, Selection{Kind: Method, Owner: d.name(),
						Type: Type{expr: m.decl.Type, file: m.file, subst: receiverSubst(m.decl, inst)}})
					continue
				}
			}
			switch x := lit.expr.(type) {
			case *ast.StructType:
				if seen[x] {
					continue
				}
				seen[x] = true
				for _, fl := range x.Fields.List {
					ft := lit.In(fl.Type)
					if len(fl.Names) == 0 {
						if BaseName(fl.Type) == name {
							hits = append(hits, Selection{Kind: Field, Type: ft, Owner: nameOf(owner)})
						}
						next = append(next, ft)
						continue
					}
					for _, n := range fl.Names {
						if n.Name == name {
							hits = append(hits, Selection{Kind: Field, Type: ft, Owner: nameOf(owner)})
						}
					}
				}
			case *ast.InterfaceType:
				hits = w.listed(lit, owner, name, depth, 0, hits, met)
			}
		}
		hits = distinct(hits)
		switch {
		case len(hits) > 1:
			return Selection{}
		case len(hits) == 1 && unsure >= 0 && unsure < depth:
			return Selection{}
		case len(hits) == 1:
			return hits[0]
		}
		level = next
	}
	switch {
	case unresolved:
		return Selection{}
	case len(outside) > 0:
		return Selection{Kind: Beyond, Outside: outside}
	}
	return Selection{Kind: Absent}
}

// listed adds to hits every method named name that the interface literal lit
// lists or embeds — an interface's method set is whole at its own depth, so
// an embedded interface's methods are found beside its own — and reports to
// met what it could not read.
func (w *World) listed(lit Type, owner *typeDecl, name string, depth, nesting int,
	hits []Selection, met func(int, Resolution, TypeName)) []Selection {

	if nesting >= maxDepth {
		met(depth, Unresolved, TypeName{})
		return hits
	}
	for _, fl := range lit.expr.(*ast.InterfaceType).Methods.List {
		if len(fl.Names) > 0 {
			for _, n := range fl.Names {
				if n.Name == name {
					hits = append(hits, Selection{Kind: Method, Type: lit.In(fl.Type),
						Owner: nameOf(owner), Interface: true})
				}
			}
			continue
		}
		switch fl.Type.(type) {
		case *ast.BinaryExpr, *ast.UnaryExpr:
			// A constraint's type terms (~int | ~string) carry no methods.
			continue
		}
		elit, eown, how, out := w.under(lit.In(fl.Type))
		if how != Resolved {
			met(depth, how, out)
			continue
		}
		if _, isInterface := elit.expr.(*ast.InterfaceType); isInterface {
			hits = w.listed(elit, eown, name, depth, nesting+1, hits, met)
		}
	}
	return hits
}

// receiverSubst binds a method's receiver type parameters — the names its
// receiver spells, `func (o op[A, R]) …` — to the instantiation it is
// selected on.
func receiverSubst(m *ast.FuncDecl, inst Type) subst {
	recv := m.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	params, args := typeArgs(recv), typeArgs(inst.expr)
	if len(params) == 0 {
		return nil
	}
	s := subst{}
	for i, p := range params {
		if id, ok := p.(*ast.Ident); ok && i < len(args) {
			s[paramKey{receiverParams{}, id.Name}] = inst.In(args[i])
		}
	}
	return s
}

// nameOf is a declaration's identity, or the zero name for none.
func nameOf(d *typeDecl) TypeName {
	if d == nil {
		return TypeName{}
	}
	return d.name()
}

func containsName(ns []TypeName, n TypeName) bool {
	for _, x := range ns {
		if x == n {
			return true
		}
	}
	return false
}

// distinct drops a member reached twice — an interface embedded through two
// others lists its methods once.
func distinct(hits []Selection) []Selection {
	var out []Selection
	for _, h := range hits {
		dup := false
		for _, o := range out {
			if o.Kind == h.Kind && o.Owner == h.Owner && o.Type.expr == h.Type.expr {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, h)
		}
	}
	return out
}
