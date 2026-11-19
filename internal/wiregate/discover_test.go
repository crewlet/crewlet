package wiregate

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

// module is this module's path, trimmed from every name a finding prints.
const module = "github.com/crewlet/crewlet/"

// surfaces are the packages whose values reach a client: the API (REST, the
// socket, the assistant's stream) and the CLI, which prints JSON for scripts.
var surfaces = []string{"./internal/api/...", "./cmd/crewlet"}

// declaring are packages read from source, never as surfaces, because an
// opaque field's row names an unexported type of theirs, which export data
// does not hold.
var declaring = []string{"./internal/events", "./internal/events/types", "./internal/insights",
	"./internal/agent/builtin"}

// surface reports whether the package at path is one of [surfaces].
func surface(path string) bool {
	for _, pattern := range surfaces {
		dir := module + strings.TrimSuffix(strings.TrimPrefix(pattern, "./"), "/...")
		if path == dir || strings.HasSuffix(pattern, "/...") && strings.HasPrefix(path, dir+"/") {
			return true
		}
	}
	return false
}

// printers are the CLI's one way to a reader, by package: everything else it
// hands an interface is a request to a node, which decodes it in Go.
var printers = map[string]string{module + "cmd/crewlet": module + "cmd/crewlet.printJSON"}

// sinkless are the packages a value handed to an interface reaches no client
// through: formatting, logging, errors, contexts, sync, and the digest a
// write's operation id is keyed on.
var sinkless = []string{"fmt", "log", "log/slog", "errors", "context", "sync", "sync/atomic", "reflect",
	module + "internal/api/opkey"}

// decoders are the functions that write into the value they are handed, by
// the index of that argument: what goes there is a destination, not a value
// sent. A surface's own function joins them when it hands its parameter on to
// one ([discovery.findDecoders]).
var decoders = map[string][]int{
	"encoding/json.Unmarshal":            {1},
	"(*encoding/json.Decoder).Decode":    {0},
	"encoding/json/v2.Unmarshal":         {1},
	"encoding/json/v2.UnmarshalRead":     {1},
	"encoding/json/v2.UnmarshalDecode":   {1},
	"gopkg.in/yaml.v3.Unmarshal":         {1},
	"(*gopkg.in/yaml.v3.Decoder).Decode": {0},
	"(*gopkg.in/yaml.v3.Node).Decode":    {0},
}

// wireField is one field of a struct a client receives.
type wireField struct {
	// owner names the struct: its package and name, or the named type an
	// anonymous struct is reached through.
	owner string
	field *types.Var
	// name is the key it is sent under, and options the tag's options.
	name, options string
	// via is where its struct was first found on the wire.
	via string
}

// omitted reports whether the field is left out of the object when it holds
// its zero value.
func (f wireField) omitted() bool {
	for opt := range strings.SplitSeq(f.options, ",") {
		if opt == "omitempty" || opt == "omitzero" {
			return true
		}
	}
	return false
}

// kind is what the field's zero means: "flag" for a bool, "number" for an
// integer or a float, and "" for anything a zero cannot be mistaken for,
// a type that writes its own JSON included (its zero is its own to say).
func (f wireField) kind() string {
	if marshals(f.field.Type()) {
		return ""
	}
	b, ok := f.field.Type().Underlying().(*types.Basic)
	switch {
	case !ok:
		return ""
	case b.Info()&types.IsBoolean != 0:
		return "flag"
	case b.Info()&(types.IsInteger|types.IsFloat) != 0:
		return "number"
	}
	return ""
}

// marshals reports whether typ writes its own JSON or text.
func marshals(typ types.Type) bool {
	set := types.NewMethodSet(types.NewPointer(typ))
	for _, name := range []string{"MarshalJSON", "MarshalText"} {
		if set.Lookup(nil, name) != nil {
			return true
		}
	}
	return false
}

// discovery collects the values the surfaces hand to an encoder and every
// struct reachable from them.
type discovery struct {
	fset *token.FileSet
	// destinations are the surfaces' own decoders, by the function's full
	// name (a package checked from source is read from export data by its
	// importers, so its objects are two), with the arguments each writes into.
	destinations map[string][]int
	// roots are the types of the values handed to an empty interface, each
	// with the place it was first seen.
	roots map[string]root
	// packages is every package the checked ones import, by path, so a
	// declared root resolves to its go/types type.
	packages map[string]*types.Package
	// surface says which checked packages' values reach a client.
	surface func(path string) bool
}

type root struct {
	typ types.Type
	at  string
}

func newDiscovery(fset *token.FileSet) *discovery {
	return &discovery{fset: fset, destinations: map[string][]int{},
		roots: map[string]root{}, packages: map[string]*types.Package{}, surface: surface}
}

// add reads one type-checked package: its decoders first, then every value it
// hands to an empty interface.
func (d *discovery) add(pkg *types.Package, files []*ast.File, info *types.Info, rootDir string) {
	d.remember(pkg)
	d.packages[pkg.Path()] = pkg
	if !d.surface(pkg.Path()) {
		return
	}
	d.findDecoders(files, info)
	for _, f := range files {
		d.findRoots(f, info, rootDir, printers[pkg.Path()])
	}
}

// remember records pkg and everything it imports.
func (d *discovery) remember(pkg *types.Package) {
	if _, ok := d.packages[pkg.Path()]; ok {
		return
	}
	d.packages[pkg.Path()] = pkg
	for _, imp := range pkg.Imports() {
		d.remember(imp)
	}
}

// findDecoders adds every function of files that hands one of its empty
// interface parameters to an argument that is never sent ([discovery.withheld]),
// until no more join: such a parameter is withheld too.
func (d *discovery) findDecoders(files []*ast.File, info *types.Info) {
	for changed := true; changed; {
		changed = false
		for _, f := range files {
			for _, decl := range f.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				obj, _ := info.Defs[fn.Name].(*types.Func)
				if obj == nil {
					continue
				}
				sig := obj.Type().(*types.Signature)
				params := map[*types.Var]int{}
				for i := range sig.Params().Len() {
					if p := sig.Params().At(i); isEmptyInterface(p.Type()) {
						params[p] = i
					}
				}
				if len(params) == 0 {
					continue
				}
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					for _, at := range d.withheld(info, call) {
						if at >= len(call.Args) {
							continue
						}
						id, ok := ast.Unparen(call.Args[at]).(*ast.Ident)
						if !ok {
							continue
						}
						v, _ := info.Uses[id].(*types.Var)
						i, isParam := params[v]
						if !isParam || slices.Contains(d.destinations[obj.FullName()], i) {
							continue
						}
						d.destinations[obj.FullName()] = append(d.destinations[obj.FullName()], i)
						changed = true
					}
					return true
				})
			}
		}
	}
}

// withheld is the arguments of call its callee never sends: those a decoder
// writes into, and every one a sinkless package's function receives.
func (d *discovery) withheld(info *types.Info, call *ast.CallExpr) []int {
	fn := callee(info, call.Fun)
	if fn == nil {
		return nil
	}
	if fn.Pkg() != nil && slices.Contains(sinkless, fn.Pkg().Path()) {
		all := make([]int, len(call.Args))
		for i := range all {
			all[i] = i
		}
		return all
	}
	if at, ok := decoders[fn.FullName()]; ok {
		return at
	}
	return d.destinations[fn.Origin().FullName()]
}

// findRoots records the type of every value f hands to an empty interface:
// an argument, a result, an assignment, a composite literal's element or a
// conversion. Each is a value some encoder may write, which is why it is the
// whole of the definition: the encoders take `any`, and so does everything
// that carries a value to one (a query's answer, a frame's data, a refusal's
// detail).
//
// A package with a printer sends only what it hands the printer.
func (d *discovery) findRoots(f *ast.File, info *types.Info, rootDir, printer string) {
	typeOf := func(e ast.Expr) types.Type {
		if tv, ok := info.Types[e]; ok {
			return tv.Type
		}
		return nil
	}
	flow := func(target types.Type, e ast.Expr) {
		if target == nil || !isEmptyInterface(target) {
			return
		}
		typ := typeOf(e)
		if typ == nil || types.IsInterface(typ) {
			return
		}
		if _, tuple := typ.(*types.Tuple); tuple {
			return
		}
		key := types.TypeString(typ, nil)
		if _, seen := d.roots[key]; !seen {
			d.roots[key] = root{typ: typ, at: relative(d.fset.Position(e.Pos()), rootDir)}
		}
	}
	var results []*types.Tuple
	var visit func(ast.Node) bool
	enter := func(sig *types.Signature, body *ast.BlockStmt) {
		if sig == nil || body == nil {
			return
		}
		results = append(results, sig.Results())
		ast.Inspect(body, visit)
		results = results[:len(results)-1]
	}
	visit = func(n ast.Node) bool {
		if printer != "" {
			if call, ok := n.(*ast.CallExpr); ok {
				if fn := callee(info, call.Fun); fn != nil && fn.FullName() == printer {
					d.flowCall(info, call, typeOf, flow)
				}
			}
			return true
		}
		switch n := n.(type) {
		case *ast.FuncDecl:
			if obj, _ := info.Defs[n.Name].(*types.Func); obj != nil {
				enter(obj.Type().(*types.Signature), n.Body)
			}
			return false
		case *ast.FuncLit:
			sig, _ := typeOf(n).(*types.Signature)
			enter(sig, n.Body)
			return false
		case *ast.ReturnStmt:
			if len(results) > 0 && len(n.Results) == results[len(results)-1].Len() {
				for i, r := range n.Results {
					flow(results[len(results)-1].At(i).Type(), r)
				}
			}
		case *ast.CallExpr:
			d.flowCall(info, n, typeOf, flow)
		case *ast.AssignStmt:
			if n.Tok == token.ASSIGN && len(n.Lhs) == len(n.Rhs) {
				for i := range n.Lhs {
					flow(typeOf(n.Lhs[i]), n.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			if n.Type != nil {
				for _, v := range n.Values {
					flow(typeOf(n.Type), v)
				}
			}
		case *ast.CompositeLit:
			flowLiteral(n, typeOf, flow)
		case *ast.SendStmt:
			if ch, ok := typeOf(n.Chan).Underlying().(*types.Chan); ok {
				flow(ch.Elem(), n.Value)
			}
		}
		return true
	}
	ast.Inspect(f, visit)
}

// flowCall hands flow each argument of call with the parameter it is passed
// to, or a conversion's operand with its target: never an argument a decoder
// writes into, nor one a sinkless package receives.
func (d *discovery) flowCall(info *types.Info, call *ast.CallExpr, typeOf func(ast.Expr) types.Type,
	flow func(types.Type, ast.Expr)) {
	if tv := info.Types[call.Fun]; tv.IsType() {
		if len(call.Args) == 1 {
			flow(tv.Type, call.Args[0])
		}
		return
	}
	sig, _ := typeOf(call.Fun).Underlying().(*types.Signature)
	if sig == nil {
		return
	}
	written := d.withheld(info, call)
	params := sig.Params()
	for i, arg := range call.Args {
		if slices.Contains(written, i) {
			continue
		}
		var param types.Type
		switch {
		case sig.Variadic() && i >= params.Len()-1:
			param = params.At(params.Len() - 1).Type()
			if !call.Ellipsis.IsValid() {
				param = param.(*types.Slice).Elem()
			}
		case i < params.Len():
			param = params.At(i).Type()
		}
		flow(param, arg)
	}
}

// flowLiteral hands flow each element of a composite literal with the type
// it is stored as.
func flowLiteral(lit *ast.CompositeLit, typeOf func(ast.Expr) types.Type, flow func(types.Type, ast.Expr)) {
	typ := typeOf(lit)
	if typ == nil {
		return
	}
	value := func(el ast.Expr) ast.Expr {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			return kv.Value
		}
		return el
	}
	switch u := typ.Underlying().(type) {
	case *types.Struct:
		for i, el := range lit.Elts {
			kv, keyed := el.(*ast.KeyValueExpr)
			if !keyed {
				if i < u.NumFields() {
					flow(u.Field(i).Type(), el)
				}
				continue
			}
			id, _ := kv.Key.(*ast.Ident)
			for j := range u.NumFields() {
				if id != nil && u.Field(j).Name() == id.Name {
					flow(u.Field(j).Type(), kv.Value)
				}
			}
		}
	case *types.Slice:
		for _, el := range lit.Elts {
			flow(u.Elem(), value(el))
		}
	case *types.Array:
		for _, el := range lit.Elts {
			flow(u.Elem(), value(el))
		}
	case *types.Map:
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				flow(u.Elem(), kv.Value)
			}
		}
	}
}

// reach walks every struct the roots reach through the fields encoding/json
// writes, recording each field, each opaque one (an interface or raw JSON,
// whose struct the types cannot say), and each JSON object's keys.
type reach struct {
	seen   map[types.Type]bool
	fields []wireField
	opaque []wireField
	// objects is every object a client receives, by the struct it is
	// written from, keyed as it is sent, a promoted field included.
	objects map[string]map[string]wireField
}

func newReach() *reach {
	return &reach{seen: map[types.Type]bool{}, objects: map[string]map[string]wireField{}}
}

// walk reads typ, a struct reached through a field being owned by its path
// from the named struct that holds it.
func (r *reach) walk(typ types.Type, owner, via string) {
	if typ == nil || r.seen[typ] {
		return
	}
	r.seen[typ] = true
	switch t := typ.(type) {
	case *types.Alias:
		r.walk(types.Unalias(t), owner, via)
		return
	case *types.Pointer:
		r.walk(t.Elem(), owner, via)
		return
	case *types.Slice:
		r.walk(t.Elem(), owner, via)
		return
	case *types.Array:
		r.walk(t.Elem(), owner, via)
		return
	case *types.Map:
		r.walk(t.Elem(), owner, via)
		return
	case *types.Named:
		owner = name(t)
		if _, isStruct := t.Underlying().(*types.Struct); !isStruct {
			r.walk(t.Underlying(), owner, via)
			return
		}
	}
	st, ok := typ.Underlying().(*types.Struct)
	if !ok {
		return
	}
	keys := r.objects[owner]
	if keys == nil {
		keys = map[string]wireField{}
		r.objects[owner] = keys
	}
	for _, f := range object(st, owner, via, nil) {
		keys[f.name] = f
		r.fields = append(r.fields, f)
		if opaqueType(f.field.Type()) {
			r.opaque = append(r.opaque, f)
		}
		r.walk(f.field.Type(), f.owner+"."+f.name, via)
	}
}

// object is the object encoding/json writes st as: an untagged embedded
// struct's fields promoted into it, every other exported field under its
// tag's name, none tagged "-". Each field keeps the struct that declares it.
func object(st *types.Struct, owner, via string, within []*types.Struct) []wireField {
	var out []wireField
	for i := range st.NumFields() {
		f := st.Field(i)
		jsonTag, tagged := reflect.StructTag(st.Tag(i)).Lookup("json")
		if jsonTag == "-" {
			continue
		}
		key, options, _ := strings.Cut(jsonTag, ",")
		if f.Embedded() && key == "" {
			inner := f.Type()
			if p, ok := inner.Underlying().(*types.Pointer); ok {
				inner = p.Elem()
			}
			if in, ok := inner.Underlying().(*types.Struct); ok {
				if !slices.Contains(within, in) {
					promoted := owner
					if n, ok := types.Unalias(inner).(*types.Named); ok {
						promoted = name(n)
					}
					out = append(out, object(in, promoted, via, append(within, st))...)
				}
				continue
			}
		}
		if !f.Exported() {
			continue
		}
		if key == "" || !tagged {
			key = f.Name()
		}
		out = append(out, wireField{owner: owner, field: f, name: key, options: options, via: via})
	}
	return out
}

// opaqueType reports whether a value of typ holds JSON whose shape no Go type
// declares: an interface, or raw JSON, at any depth of pointers, slices and
// maps.
func opaqueType(typ types.Type) bool {
	for {
		switch t := types.Unalias(typ).(type) {
		case *types.Pointer:
			typ = t.Elem()
		case *types.Slice:
			if b, ok := t.Elem().Underlying().(*types.Basic); ok && b.Kind() == types.Byte {
				return false
			}
			typ = t.Elem()
		case *types.Array:
			typ = t.Elem()
		case *types.Map:
			typ = t.Elem()
		case *types.Named:
			if obj := t.Obj(); obj.Pkg() != nil && (obj.Pkg().Path() == "encoding/json" && obj.Name() == "RawMessage" ||
				obj.Pkg().Path() == "encoding/json/jsontext" && obj.Name() == "Value") {
				return true
			}
			return types.IsInterface(t)
		default:
			return types.IsInterface(typ)
		}
	}
}

// lookup resolves a type named by its package within the module and its
// name, an unexported one included, in the packages the check read.
func (d *discovery) lookup(name string) (types.Type, error) {
	dot := strings.LastIndex(name, ".")
	if dot < 0 {
		return nil, fmt.Errorf("%s names no package", name)
	}
	path, typeName := module+name[:dot], name[dot+1:]
	pkg := d.packages[path]
	if pkg == nil {
		return nil, fmt.Errorf("%s: no surface imports package %s", name, path)
	}
	obj, _ := pkg.Scope().Lookup(typeName).(*types.TypeName)
	if obj == nil {
		return nil, fmt.Errorf("%s: package %s declares no type %s", name, path, typeName)
	}
	return obj.Type(), nil
}

// sortedRoots is the roots in a stable order.
func (d *discovery) sortedRoots() []root {
	keys := slices.Sorted(maps.Keys(d.roots))
	out := make([]root, len(keys))
	for i, k := range keys {
		out[i] = d.roots[k]
	}
	return out
}

// name is a named type as a finding prints it: its package's path within the
// module, and its name.
func name(n *types.Named) string {
	obj := n.Obj()
	if obj.Pkg() == nil {
		return obj.Name()
	}
	return strings.TrimPrefix(obj.Pkg().Path(), module) + "." + obj.Name()
}

// callee is the function or method fun calls, or nil for a function value.
func callee(info *types.Info, fun ast.Expr) *types.Func {
	switch f := ast.Unparen(fun).(type) {
	case *ast.Ident:
		fn, _ := info.Uses[f].(*types.Func)
		return fn
	case *ast.SelectorExpr:
		fn, _ := info.Uses[f.Sel].(*types.Func)
		return fn
	case *ast.IndexExpr:
		return callee(info, f.X)
	case *ast.IndexListExpr:
		return callee(info, f.X)
	}
	return nil
}

func isEmptyInterface(typ types.Type) bool {
	i, ok := typ.Underlying().(*types.Interface)
	return ok && i.Empty() && !isTypeParam(typ)
}

func isTypeParam(typ types.Type) bool {
	_, ok := types.Unalias(typ).(*types.TypeParam)
	return ok
}

// relative is pos with its file named from the module root.
func relative(pos token.Position, rootDir string) string {
	if rel, err := filepath.Rel(rootDir, pos.Filename); err == nil && !strings.HasPrefix(rel, "..") {
		pos.Filename = filepath.ToSlash(rel)
	}
	return fmt.Sprintf("%s:%d", pos.Filename, pos.Line)
}
