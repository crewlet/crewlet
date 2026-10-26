package code

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// THE READ GATE'S WALK: every statement text this package's source spells that
// reads a code table, and every function that assembles one at run time —
// what TestEveryCodeReadSeeksANamedIndex holds to the plans plan_test.go
// registers (readindex_test.go says why the rule is held at all).
//
// # What a text is
//
// A text is a MAXIMAL CONSTANT STRING EXPRESSION: a literal, a constant, or +
// over them, wherever it is written — a constant's declaration, a statement's
// argument, a slice of statements — evaluated whole, so a statement built of
// constants (`SELECT ` + pushColumns + ` FROM code_pushes …`, `INDEXED BY ` +
// refsKey) is judged as the text the driver is handed rather than as its
// pieces. A bare reference to a constant spells nothing new: the constant's
// declaration is where its text is counted, and the reference is a USE of it,
// which says which function the text serves.
//
// # What reading is
//
// A text READS a table its FROM clause, a JOIN or an UPDATE names — what a
// SELECT answers from, and what an UPDATE or a DELETE finds the rows it writes
// by, which a scan finds as slowly as it finds a SELECT's. A table an INSERT or
// a REPLACE only writes is not read: VALUES locates no row, and an upsert's
// conflict is found by the very key it conflicts on. Read lexically
// ([codeReads]) — a comment, a string literal, an index INDEXED BY names and a
// column a table qualifies are none of them — and only a code_* table counts:
// this package's statements are the code tables', and json_each, a statement's
// own list, is a function rather than a table.
//
// # What a builder is
//
// A STATEMENT SITE is a call of a method named as one of database/sql's
// statement methods ([statementMethods]) whose declaration takes a text where
// those take theirs ([takesText]) — typed through
// internal/sourcetree/decltype, by declaration, database/sql's own read from
// the toolchain beside this module's. So a *sql.Tx's, a *sql.DB's and a
// *sql.Conn's are sites, however the walk came by the handle; a *sql.Stmt's
// Exec and Query, which take the arguments of the statement it was prepared
// from and no text, are none, since its Prepare is the site that names the
// text; statelog.Freshness.Query, which builds a read rather than issuing
// one, is none; and a statement method on a value the walk cannot type is
// reported rather than passed. A site's text is FOLLOWED: a constant; a local
// variable every write of which is followed (an assignment, a declaration, a
// range over a literal of them); a parameter of an unexported function every
// call of which hands it a text followed in turn. A text the walk cannot
// follow out of a function — a concatenation with something only the run
// knows, a call's result, a field, a package-level variable, an exported
// function's parameter, a closure's — makes THAT function a builder: it
// issues a statement no constant spells whole, which only the shapes it is
// registered with ([planShapes]) can be planned by.
//
// # Coverage boundary
//
// Read: every non-test file of the package, wherever it is built, and the
// declarations its values are typed by — this module's and database/sql's
// ([sqlPackage]). Not seen: a statement another package issues from text it
// assembles over this package's declarations — objstore's builders over
// [PackReferences] and [RetiredPackReferences] are the one kind there is, and
// [planShapes] answers for them (holdReferences); a statement method reached
// through reflection; one called on a value only a package the walk does not
// read could type — a driver's own interfaces (database/sql/driver) among
// them; and a constant of another package, which is not evaluated, so a text
// built with one is read as the fragments around it.
type readWalk struct {
	// files is how many non-test files the walk read, and sites how many
	// statement sites it judged.
	files, sites int
	// spelled is every text the source spells that reads a code table, by
	// the text.
	spelled map[string]*spelling
	// builders is every function a site's text cannot be followed out of, by
	// the name the walk gives a function ("" for a package-level
	// declaration), with each place and why.
	builders map[string][]string
	// declared is every function the package declares, by that name.
	declared map[string]bool
	// references is every objstore.ReferenceTable the package declares at
	// package level, by its variable.
	references map[string]bool
	findings   []string
}

// spelling is one text, wherever the source spells it.
type spelling struct {
	// reads is every code table the text reads, in the order it names them.
	reads []string
	// at is every place it is spelled, file:line, and names every constant
	// it is declared as.
	at, names []string
	// users is every function that uses the text: the one an inline text is
	// written in, and each that names a constant it is declared as — ""
	// for a package-level declaration. A text only builders use may be a
	// FRAGMENT of what they assemble, which no plan can be read of alone.
	users map[string]bool
	// folded reports a constant it is declared as that another text names
	// inside itself: its text is then judged in that one's too.
	folded bool
	// consts is every constant it is declared as, whose uses are its users.
	consts []constKey
}

// String names a text for a finding: the constants it is declared as, or
// where it is written.
func (s *spelling) String() string {
	if len(s.names) > 0 {
		return fmt.Sprintf("%s (%s)", strings.Join(s.names, ", "), strings.Join(s.at, ", "))
	}
	return "the text at " + strings.Join(s.at, ", ")
}

// statementMethods is every statement method of database/sql's Tx, DB and
// Conn, by name, with the place of the argument that is its text — the place
// at which a *sql.Stmt's method of the same name takes the first of its
// statement's arguments instead.
var statementMethods = map[string]int{
	"Exec": 0, "ExecContext": 1,
	"Query": 0, "QueryContext": 1,
	"QueryRow": 0, "QueryRowContext": 1,
	"Prepare": 0, "PrepareContext": 1,
}

// sqlPackage is the one package outside the module the walk reads, from the
// toolchain the module builds with: what declares a statement handle's
// methods, and the results that hand one on — a connection DB.Conn opens, a
// statement Tx.PrepareContext prepares — so each is judged by its own
// declaration rather than by the package it came from.
const sqlPackage = "database/sql"

// codeDir is the package the gate judges, relative to the module root.
const codeDir = "internal/code"

// modulePath is this module's import path.
const modulePath = "github.com/crewlet/crewlet"

// walkForReads judges every non-test Go file of module's internal/code under
// root.
func walkForReads(t *testing.T, root, module string) readWalk {
	t.Helper()
	// The toolchain's database/sql is the one the module builds against, so
	// it is located from the module's own root, whatever tree is judged.
	dirs, err := decltype.Locate(t.Context(), sourcetree.Root(t), sqlPackage)
	if err != nil {
		t.Fatal(err)
	}
	rw := &readWalker{
		w:      decltype.New(root, module, dirs),
		consts: map[string]constKey{},
		gens:   map[*ast.ValueSpec]*ast.GenDecl{},
		pkgVar: map[*ast.ValueSpec]bool{},
		vars:   map[string]bool{},
		funcs:  map[*ast.FuncDecl]*funcDecl{},
		evals:  map[constKey]evalResult{},
		uses:   map[constKey]map[string]bool{},
		folds:  map[constKey]bool{},
		locals: map[*ast.Object]*origin{},
		params: map[paramKey]*origin{},
		refs:   map[*funcDecl][]funcRef{},
		out: readWalk{spelled: map[string]*spelling{}, builders: map[string][]string{},
			declared: map[string]bool{}, references: map[string]bool{}},
	}
	dir := filepath.Join(root, filepath.FromSlash(codeDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		rw.report("read %s: %v", codeDir, err)
		return rw.out
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Every file, wherever it is built: one this platform does not
		// build is read against the scope of the files it does.
		if f := rw.w.FileAt(filepath.Join(dir, name)); f != nil {
			rw.files = append(rw.files, f)
		}
	}
	rw.out.files = len(rw.files)
	if len(rw.files) > 0 {
		rw.pkgPath = rw.files[0].Package.Path
		rw.declare()
		for _, f := range rw.files {
			rw.judgeFile(f)
		}
		// A declared constant's text serves every function that names it.
		for _, s := range rw.out.spelled {
			for _, k := range s.consts {
				for fn := range rw.uses[k] {
					s.users[fn] = true
				}
				s.folded = s.folded || rw.folds[k]
			}
		}
	}
	// EVERY FILE THE WALK COULD NOT PARSE, asked once the judging is done:
	// a package a value is typed by — database/sql's, the module's others —
	// is read when the walk first meets such a value, and a file of one it
	// could not parse is a file whose declarations typed nothing.
	for _, err := range rw.w.Errors() {
		rw.report("%v", err)
	}
	return rw.out
}

// readWalker is the walk's state.
type readWalker struct {
	w       *decltype.World
	pkgPath string
	files   []*decltype.File
	// consts is every package-level constant by name, and gens every
	// constant specification's declaration, which an implicit repetition
	// is read from.
	consts map[string]constKey
	gens   map[*ast.ValueSpec]*ast.GenDecl
	// pkgVar is every package-level variable's specification, and vars
	// every such variable by name.
	pkgVar map[*ast.ValueSpec]bool
	vars   map[string]bool
	funcs  map[*ast.FuncDecl]*funcDecl
	evals  map[constKey]evalResult
	// uses is every function that names a constant bare, by the constant,
	// and folds every constant another text names inside itself.
	uses  map[constKey]map[string]bool
	folds map[constKey]bool
	// locals and params are what a variable and a parameter were followed
	// to — nil while the walk is following one, since a cycle hands on no
	// text the rest of the cycle does not — and refs every reference to a
	// function.
	locals map[*ast.Object]*origin
	params map[paramKey]*origin
	refs   map[*funcDecl][]funcRef
	out    readWalk
}

// constKey is one constant: one name of a specification.
type constKey struct {
	spec  *ast.ValueSpec
	index int
}

// evalResult is a constant's text, where it is a string constant.
type evalResult struct {
	text string
	ok   bool
}

// funcDecl is one function the package declares.
type funcDecl struct {
	decl *ast.FuncDecl
	// name is what the walk calls it: its own name, or its receiver type's
	// and its own ("Reader.page"); recv is its receiver's type name, "" for
	// a function.
	name, recv string
}

// scope is where an expression is written: its file, and the function
// declaration that encloses it, nil at package level.
type scope struct {
	file *decltype.File
	fn   *funcDecl
}

// in is the name of the function a scope is in, "" at package level.
func (sc scope) in() string {
	if sc.fn == nil {
		return ""
	}
	return sc.fn.name
}

// paramKey is one parameter of one function.
type paramKey struct {
	fn    *funcDecl
	index int
}

// funcRef is one reference to a function: the call it is the callee of, nil
// where it is taken as a value or the walk cannot place it, and where it is
// written.
type funcRef struct {
	call *ast.CallExpr
	at   scope
	pos  string
}

// origin is where a text comes from, as far as the walk follows it: every
// text it can hold a constant, or the function the walk cannot follow it out
// of, and why.
type origin struct {
	ok  bool
	in  string
	why string
}

func (rw *readWalker) report(format string, args ...any) {
	rw.out.findings = append(rw.out.findings, fmt.Sprintf(format, args...))
}

// pos is a node's place, file:line.
func (rw *readWalker) pos(f *decltype.File, n ast.Node) string {
	return fmt.Sprintf("%s:%d", f.Rel, rw.w.FileSet().Position(n.Pos()).Line)
}

// declare reads what the package declares: its functions, its package-level
// constants and variables, every constant specification's declaration, and
// the reference tables it declares.
func (rw *readWalker) declare() {
	for _, f := range rw.files {
		ast.Inspect(f.AST, func(n ast.Node) bool {
			if gen, ok := n.(*ast.GenDecl); ok && gen.Tok == token.CONST {
				for _, s := range gen.Specs {
					rw.gens[s.(*ast.ValueSpec)] = gen
				}
			}
			return true
		})
		for _, d := range f.AST.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				fd := &funcDecl{decl: d, name: d.Name.Name}
				if d.Recv != nil && len(d.Recv.List) > 0 {
					fd.recv = decltype.BaseName(d.Recv.List[0].Type)
					fd.name = fd.recv + "." + d.Name.Name
				}
				rw.funcs[d] = fd
				rw.out.declared[fd.name] = true
			case *ast.GenDecl:
				for _, s := range d.Specs {
					vs, ok := s.(*ast.ValueSpec)
					if !ok {
						continue
					}
					for i, name := range vs.Names {
						switch d.Tok {
						case token.CONST:
							rw.consts[name.Name] = constKey{vs, i}
						case token.VAR:
							rw.pkgVar[vs] = true
							rw.vars[name.Name] = true
							if i < len(vs.Values) && rw.isReferenceTable(f, vs.Values[i]) {
								rw.out.references[name.Name] = true
							}
						}
					}
				}
			}
		}
	}
}

// isReferenceTable reports a value that is an objstore.ReferenceTable literal.
func (rw *readWalker) isReferenceTable(f *decltype.File, e ast.Expr) bool {
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = u.X
	}
	lit, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	sel, ok := lit.Type.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "ReferenceTable" {
		return false
	}
	path, isPkg := rw.w.PackageNamed(sel.X, f)
	return isPkg && strings.HasSuffix(path, "/internal/objstore")
}

// constText is the string constant e is, where it is one: a string literal,
// a constant of the package or of the function, or + over them.
func (rw *readWalker) constText(e ast.Expr) (string, bool) {
	switch e := e.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return rw.constText(e.X)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		x, ok := rw.constText(e.X)
		if !ok {
			return "", false
		}
		y, ok := rw.constText(e.Y)
		return x + y, ok
	case *ast.Ident:
		if k, ok := rw.constOf(e); ok {
			return rw.evalConst(k)
		}
	}
	return "", false
}

// constOf is the constant an identifier names: by the parser's resolution
// where its declaration is in the identifier's own file, and by the
// package's declarations where it is in another.
func (rw *readWalker) constOf(id *ast.Ident) (constKey, bool) {
	if id.Obj == nil {
		k, ok := rw.consts[id.Name]
		return k, ok
	}
	if id.Obj.Kind != ast.Con {
		return constKey{}, false
	}
	spec, ok := id.Obj.Decl.(*ast.ValueSpec)
	if !ok {
		return constKey{}, false
	}
	for i, n := range spec.Names {
		if n.Name == id.Name {
			return constKey{spec, i}, true
		}
	}
	return constKey{}, false
}

// evalConst is a constant's text, where it is a string constant.
func (rw *readWalker) evalConst(k constKey) (string, bool) {
	if r, done := rw.evals[k]; done {
		return r.text, r.ok
	}
	rw.evals[k] = evalResult{} // a declaration that reaches back to itself is none
	var r evalResult
	if v := rw.valueOf(k); v != nil {
		r.text, r.ok = rw.constText(v)
	}
	rw.evals[k] = r
	return r.text, r.ok
}

// valueOf is the expression a constant is declared with — the specification
// before it in its declaration where it repeats one implicitly.
func (rw *readWalker) valueOf(k constKey) ast.Expr {
	if k.index < len(k.spec.Values) {
		return k.spec.Values[k.index]
	}
	if len(k.spec.Values) != 0 || rw.gens[k.spec] == nil {
		return nil
	}
	var last *ast.ValueSpec
	for _, s := range rw.gens[k.spec].Specs {
		if s == k.spec {
			break
		}
		if vs := s.(*ast.ValueSpec); len(vs.Values) > 0 {
			last = vs
		}
	}
	if last == nil || k.index >= len(last.Values) {
		return nil
	}
	return last.Values[k.index]
}

// aliased is the constant whose declaration spells a constant's text: the
// constant itself, or the one it is declared as, followed.
func (rw *readWalker) aliased(k constKey) constKey {
	for range 16 {
		id, ok := ast.Unparen(rw.valueOf(k)).(*ast.Ident)
		if !ok {
			return k
		}
		next, ok := rw.constOf(id)
		if !ok {
			return k
		}
		k = next
	}
	return k
}

// judgeFile reads one file's texts, uses and statement sites: every
// function's body and every package-level constant and variable.
func (rw *readWalker) judgeFile(f *decltype.File) {
	for _, d := range f.AST.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Body != nil {
				rw.judge(scope{file: f, fn: rw.funcs[d]}, d.Body)
			}
		case *ast.GenDecl:
			if d.Tok != token.CONST && d.Tok != token.VAR {
				continue
			}
			for _, s := range d.Specs {
				rw.judge(scope{file: f}, s)
			}
		}
	}
}

// judge walks one function's body, or one package-level specification.
func (rw *readWalker) judge(sc scope, root ast.Node) {
	var stack []ast.Node
	ast.Inspect(root, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		if !rw.node(sc, n, parent) {
			return false
		}
		stack = append(stack, n)
		return true
	})
}

// node judges one node, and reports whether the walk descends into it.
func (rw *readWalker) node(sc scope, n, parent ast.Node) bool {
	switch x := n.(type) {
	case *ast.Ident:
		rw.use(sc, x, parent)
		return false
	case *ast.CallExpr:
		rw.site(sc, x)
		return true
	case *ast.SelectorExpr:
		if _, named := statementMethods[x.Sel.Name]; named {
			if call, ok := parent.(*ast.CallExpr); (!ok || ast.Unparen(call.Fun) != x) &&
				rw.statementCall(sc.file, x) == isSite {
				rw.report("%s: %s.%s is taken as a value — a statement method whose text the "+
					"walk cannot follow: call it where its text is written",
					rw.pos(sc.file, x), types.ExprString(x.X), x.Sel.Name)
			}
		}
		return true
	case ast.Expr:
		text, ok := rw.constText(x)
		if !ok {
			return true
		}
		rw.spell(sc, x, text, parent)
		return false
	}
	return true
}

// use records a bare reference to a constant: the function it is written in
// uses the constant's text. A declaration's own name, a selected name and a
// struct literal's key are no reference, and neither is a constant declared
// as another, whose uses are the other's ([readWalker.aliased]).
func (rw *readWalker) use(sc scope, id *ast.Ident, parent ast.Node) {
	switch p := parent.(type) {
	case *ast.ValueSpec:
		if slices.Contains(p.Names, id) || rw.gens[p] != nil {
			return
		}
	case *ast.SelectorExpr:
		if p.Sel == id {
			return
		}
	case *ast.KeyValueExpr:
		if p.Key == id {
			return
		}
	}
	k, ok := rw.constOf(id)
	if !ok {
		return
	}
	k = rw.aliased(k)
	if rw.uses[k] == nil {
		rw.uses[k] = map[string]bool{}
	}
	rw.uses[k][sc.in()] = true
}

// spell records a maximal constant text that reads a code table, and every
// constant it names inside itself as folded into it.
func (rw *readWalker) spell(sc scope, e ast.Expr, text string, parent ast.Node) {
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if k, isConst := rw.constOf(id); isConst {
				rw.folds[rw.aliased(k)] = true
			}
		}
		return true
	})
	reads := codeReads(text)
	if len(reads) == 0 {
		return
	}
	s := rw.out.spelled[text]
	if s == nil {
		s = &spelling{reads: reads, users: map[string]bool{}}
		rw.out.spelled[text] = s
	}
	s.at = append(s.at, rw.pos(sc.file, e))
	if vs, ok := parent.(*ast.ValueSpec); ok && rw.gens[vs] != nil {
		for i, v := range vs.Values {
			if v == e {
				s.consts = append(s.consts, constKey{vs, i})
				s.names = append(s.names, vs.Names[i].Name)
				return
			}
		}
	}
	s.users[sc.in()] = true
}

// siteVerdict is what the walk concludes about one selector of a statement
// method's name.
type siteVerdict int

const (
	notSite siteVerdict = iota
	isSite
	untyped
)

// site judges a call: a statement site's text is followed, and the function
// it cannot be followed out of is a builder.
func (rw *readWalker) site(sc scope, call *ast.CallExpr) {
	sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr)
	if !ok {
		return
	}
	idx, named := statementMethods[sel.Sel.Name]
	if !named {
		return
	}
	switch rw.statementCall(sc.file, sel) {
	case untyped:
		rw.report("%s: the walk cannot type %s, so cannot tell whether its %s issues a "+
			"statement: spell its type where it is declared, or teach "+
			"internal/sourcetree/decltype the shape",
			rw.pos(sc.file, call), types.ExprString(sel.X), sel.Sel.Name)
	case isSite:
		rw.out.sites++
		if idx >= len(call.Args) {
			return
		}
		if o := rw.follow(call.Args[idx], sc); !o.ok {
			rw.out.builders[o.in] = append(rw.out.builders[o.in],
				fmt.Sprintf("%s: %s", rw.pos(sc.file, call), o.why))
		}
	}
}

// statementCall judges whether a selector of a statement method's name is a
// statement: a method whose declaration — database/sql's or this module's —
// takes a text where database/sql's Tx, DB and Conn take theirs, or not.
//
// BY THE DECLARATION, NEVER BY THE PACKAGE A VALUE CAME FROM: database/sql
// hands on a *sql.Conn from DB.Conn and a *sql.Stmt from a Prepare alike, and
// only the first is a handle — a statement's runs, Exec(args ...any) and the
// rest, are no site, since what they run is the text its Prepare named, which
// is judged at the Prepare. A walk that took every value database/sql handed
// on for a handle took each run of a statement prepared once for a second
// site and the argument at its text's place for SQL, and so refused the
// prepare-once, run-per-item shape a hot loop wants.
func (rw *readWalker) statementCall(f *decltype.File, sel *ast.SelectorExpr) siteVerdict {
	if _, isPkg := rw.w.PackageNamed(sel.X, f); isPkg {
		// A package's function that shares the name, not a method.
		return notSite
	}
	s := rw.w.Select(rw.w.TypeOf(sel.X, f), sel.Sel.Name)
	switch s.Kind {
	case decltype.Method:
		if takesText(s.Type, statementMethods[sel.Sel.Name]) {
			return isSite
		}
		return notSite
	case decltype.Field, decltype.FromUnread, decltype.Beyond:
		// A field; and a member of a value only a package the walk does
		// not read could type or hold, whose methods issue none of this
		// package's statements (the coverage boundary).
		return notSite
	}
	return untyped
}

// takesText reports a method signature whose parameter at index is a string:
// `query string`, where a *sql.Stmt's same method takes `args ...any`.
func takesText(sig decltype.Type, index int) bool {
	ft, ok := sig.Expr().(*ast.FuncType)
	if !ok || ft.Params == nil {
		return false
	}
	at := 0
	for _, field := range ft.Params.List {
		n := max(len(field.Names), 1)
		if index < at+n {
			id, ok := field.Type.(*ast.Ident)
			return ok && id.Name == "string"
		}
		at += n
	}
	return false
}

// cannot is an origin the walk cannot follow out of the function sc is in.
func cannot(sc scope, format string, args ...any) origin {
	return origin{in: sc.in(), why: fmt.Sprintf(format, args...)}
}

// follow follows a site's text to the constants it can hold.
func (rw *readWalker) follow(e ast.Expr, sc scope) origin {
	if _, ok := rw.constText(e); ok {
		return origin{ok: true}
	}
	id, ok := ast.Unparen(e).(*ast.Ident)
	if !ok {
		return cannot(sc, "its text, %s, is assembled at run time", types.ExprString(e))
	}
	if id.Obj == nil {
		if rw.vars[id.Name] {
			return cannot(sc, "its text is %s, a package-level variable, which holds whatever "+
				"was last stored in it", id.Name)
		}
		return cannot(sc, "its text is %s, which the walk cannot follow", id.Name)
	}
	if id.Obj.Kind != ast.Var {
		return cannot(sc, "its text is %s, which the walk cannot follow", id.Name)
	}
	switch decl := id.Obj.Decl.(type) {
	case *ast.ValueSpec:
		if rw.pkgVar[decl] {
			return cannot(sc, "its text is %s, a package-level variable, which holds whatever "+
				"was last stored in it", id.Name)
		}
	case *ast.Field:
		return rw.param(id, decl, sc)
	}
	return rw.local(id, sc)
}

// local follows a variable of the function sc is in through every write of
// it there.
func (rw *readWalker) local(id *ast.Ident, sc scope) origin {
	obj := id.Obj
	if o, seen := rw.locals[obj]; seen {
		if o == nil {
			return origin{ok: true}
		}
		return *o
	}
	if sc.fn == nil {
		return cannot(sc, "its text is %s, which the walk cannot follow", id.Name)
	}
	rw.locals[obj] = nil
	o := origin{ok: true}
	ast.Inspect(sc.fn.decl.Body, func(n ast.Node) bool {
		if !o.ok {
			return false
		}
		switch s := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range s.Lhs {
				if l, isID := lhs.(*ast.Ident); !isID || l.Obj != obj {
					continue
				}
				switch {
				case s.Tok != token.ASSIGN && s.Tok != token.DEFINE:
					o = cannot(sc, "its text, %s, is added to (%s) at %s", id.Name, s.Tok,
						rw.pos(sc.file, s))
				case len(s.Rhs) != len(s.Lhs):
					o = cannot(sc, "its text, %s, is one of a call's results at %s", id.Name,
						rw.pos(sc.file, s))
				default:
					o = rw.follow(s.Rhs[i], sc)
				}
			}
		case *ast.ValueSpec:
			for i, name := range s.Names {
				// Declared with no value it is the empty text, which
				// reads nothing.
				if name.Obj == obj && i < len(s.Values) {
					o = rw.follow(s.Values[i], sc)
				}
			}
		case *ast.RangeStmt:
			o = rw.ranged(id, s, sc)
		case *ast.UnaryExpr:
			if v, isID := ast.Unparen(s.X).(*ast.Ident); s.Op == token.AND && isID && v.Obj == obj {
				o = cannot(sc, "its text, %s, has its address taken at %s", id.Name, rw.pos(sc.file, s))
			}
		}
		return true
	})
	rw.locals[obj] = &o
	return o
}

// ranged follows a variable a range statement writes, where it does: the
// values of a literal it ranges over, each followed.
func (rw *readWalker) ranged(id *ast.Ident, s *ast.RangeStmt, sc scope) origin {
	if key, isID := s.Key.(*ast.Ident); isID && key.Obj == id.Obj {
		return cannot(sc, "its text, %s, is a range's key", id.Name)
	}
	if value, isID := s.Value.(*ast.Ident); !isID || value.Obj != id.Obj {
		return origin{ok: true}
	}
	lit, isLit := ast.Unparen(s.X).(*ast.CompositeLit)
	if !isLit {
		return cannot(sc, "its text, %s, ranges over %s, which the walk cannot read whole",
			id.Name, types.ExprString(s.X))
	}
	for _, elt := range lit.Elts {
		if kv, isKV := elt.(*ast.KeyValueExpr); isKV {
			elt = kv.Value
		}
		if o := rw.follow(elt, sc); !o.ok {
			return o
		}
	}
	return origin{ok: true}
}

// param follows a parameter of the function sc is in through every call of
// that function.
func (rw *readWalker) param(id *ast.Ident, field *ast.Field, sc scope) origin {
	if sc.fn == nil {
		return cannot(sc, "its text is %s, which the walk cannot follow", id.Name)
	}
	index, own := paramIndex(sc.fn.decl.Type, field, id.Name)
	switch {
	case !own:
		return cannot(sc, "its text is %s, a parameter of a function literal or a result, "+
			"which the walk does not follow", id.Name)
	case ast.IsExported(sc.fn.decl.Name.Name):
		return cannot(sc, "its text is %s, a parameter of %s, which a caller outside the "+
			"package may hand anything", id.Name, sc.fn.name)
	}
	if _, variadic := field.Type.(*ast.Ellipsis); variadic {
		return cannot(sc, "its text is %s, a variadic parameter", id.Name)
	}
	key := paramKey{sc.fn, index}
	if o, seen := rw.params[key]; seen {
		if o == nil {
			return origin{ok: true}
		}
		return *o
	}
	rw.params[key] = nil
	o := origin{ok: true}
	for _, ref := range rw.refsOf(sc.fn) {
		switch {
		case ref.call == nil:
			o = cannot(ref.at, "it hands %s to %s, taken as a value at %s, whose calls the walk "+
				"does not follow", id.Name, sc.fn.name, ref.pos)
		case ref.call.Ellipsis.IsValid() && index >= len(ref.call.Args)-1, index >= len(ref.call.Args):
			o = cannot(ref.at, "it calls %s at %s with its %s spread from a slice", sc.fn.name,
				ref.pos, id.Name)
		default:
			o = rw.follow(ref.call.Args[index], ref.at)
		}
		if !o.ok {
			break
		}
	}
	rw.params[key] = &o
	return o
}

// paramIndex is the place of the parameter name declares in a function's own
// parameter list — false where field is not in it (a function literal's, or a
// result).
func paramIndex(ft *ast.FuncType, field *ast.Field, name string) (int, bool) {
	at := 0
	for _, f := range ft.Params.List {
		if f != field {
			at += max(len(f.Names), 1)
			continue
		}
		for i, n := range f.Names {
			if n.Name == name {
				return at + i, true
			}
		}
	}
	return 0, false
}

// refsOf is every reference to one of the package's functions: each name of
// a function, and each selector of a method the walk types to its receiver. A
// selector of its name the walk cannot type, or one through an interface that
// may hold it, is a reference it cannot follow, and so is taken as a value.
func (rw *readWalker) refsOf(fn *funcDecl) []funcRef {
	if refs, done := rw.refs[fn]; done {
		return refs
	}
	var refs []funcRef
	name := fn.decl.Name.Name
	for _, f := range rw.files {
		for _, d := range f.AST.Decls {
			sc := scope{file: f}
			if fd, ok := d.(*ast.FuncDecl); ok {
				sc.fn = rw.funcs[fd]
			}
			var stack []ast.Node
			ast.Inspect(d, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				var parent ast.Node
				if len(stack) > 0 {
					parent = stack[len(stack)-1]
				}
				stack = append(stack, n)
				refer := func(placed bool) {
					ref := funcRef{at: sc, pos: rw.pos(f, n)}
					if call, ok := parent.(*ast.CallExpr); ok && placed && ast.Unparen(call.Fun) == n {
						ref.call = call
					}
					refs = append(refs, ref)
				}
				switch x := n.(type) {
				case *ast.SelectorExpr:
					if fn.recv == "" || x.Sel.Name != name {
						break
					}
					s := rw.w.Select(rw.w.TypeOf(x.X, f), name)
					switch {
					case s.Kind == decltype.Method && !s.Interface &&
						s.Owner == (decltype.TypeName{Package: rw.pkgPath, Name: fn.recv}):
						refer(true)
					case s.Kind == decltype.Method && s.Interface,
						s.Kind == decltype.Undetermined, s.Kind == decltype.Absent:
						refer(false)
					}
				case *ast.Ident:
					if fn.recv != "" || x.Name != name || x == fn.decl.Name {
						break
					}
					if sel, isSel := parent.(*ast.SelectorExpr); isSel && sel.Sel == x {
						break
					}
					if kv, isKV := parent.(*ast.KeyValueExpr); isKV && kv.Key == x {
						break
					}
					if _, isPkg := rw.w.PackageNamed(x, f); isPkg {
						break
					}
					if x.Obj == nil || x.Obj.Kind == ast.Fun {
						refer(true)
					}
				}
				return true
			})
		}
	}
	rw.refs[fn] = refs
	return refs
}

// THE LEXER. A statement's text as SQLite reads it — words, quoted
// identifiers, string literals, parameters and marks — with its comments and
// whitespace gone, which is all [codeReads] and [paramCount] need.

// sqlToken is one token of a statement.
type sqlToken struct {
	kind sqlKind
	// text is a word as spelled, a quoted identifier unquoted, a literal or
	// a parameter as written, or a mark.
	text string
}

// sqlKind is what a token is.
type sqlKind int

const (
	sqlWord sqlKind = iota
	sqlParam
	sqlLiteral
	sqlMark
)

// lexSQL splits a statement into its tokens.
func lexSQL(text string) []sqlToken {
	var out []sqlToken
	for i := 0; i < len(text); {
		c := text[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\f' || c == '\v':
			i++
		case strings.HasPrefix(text[i:], "--"):
			if end := strings.IndexByte(text[i:], '\n'); end >= 0 {
				i += end
			} else {
				i = len(text)
			}
		case strings.HasPrefix(text[i:], "/*"):
			if end := strings.Index(text[i+2:], "*/"); end >= 0 {
				i += 2 + end + 2
			} else {
				i = len(text)
			}
		case c == '\'':
			j := closing(text, i, c)
			out = append(out, sqlToken{sqlLiteral, text[i:j]})
			i = j
		case c == '"' || c == '`':
			j := closing(text, i, c)
			body := text[i+1 : max(j-1, i+1)]
			out = append(out, sqlToken{sqlWord, strings.ReplaceAll(body, string([]byte{c, c}), string(c))})
			i = j
		case c == '[':
			j := strings.IndexByte(text[i:], ']')
			if j < 0 {
				j = len(text) - i
			}
			out = append(out, sqlToken{sqlWord, text[i+1 : i+j]})
			i += j + 1
		case wordStart(c):
			j := i + 1
			for j < len(text) && wordPart(text[j]) {
				j++
			}
			out = append(out, sqlToken{sqlWord, text[i:j]})
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < len(text) && (wordPart(text[j]) || text[j] == '.') {
				j++
			}
			out = append(out, sqlToken{sqlLiteral, text[i:j]})
			i = j
		case c == '?':
			j := i + 1
			for j < len(text) && text[j] >= '0' && text[j] <= '9' {
				j++
			}
			out = append(out, sqlToken{sqlParam, text[i:j]})
			i = j
		case (c == ':' || c == '@' || c == '$') && i+1 < len(text) && wordStart(text[i+1]):
			j := i + 1
			for j < len(text) && wordPart(text[j]) {
				j++
			}
			out = append(out, sqlToken{sqlParam, text[i:j]})
			i = j
		default:
			out = append(out, sqlToken{sqlMark, text[i : i+1]})
			i++
		}
	}
	return out
}

// closing is the index past the quote that closes the one at i, a doubled
// quote standing for itself.
func closing(text string, i int, quote byte) int {
	for j := i + 1; j < len(text); j++ {
		if text[j] != quote {
			continue
		}
		if j+1 < len(text) && text[j+1] == quote {
			j++
			continue
		}
		return j + 1
	}
	return len(text)
}

func wordStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func wordPart(c byte) bool { return wordStart(c) || c >= '0' && c <= '9' || c == '$' }

// codeReads is every code_* table a statement reads, in the order it names
// them: each a FROM clause, a JOIN, a comma of a FROM list or an UPDATE names
// as its table — never one an INSERT or a REPLACE only writes INTO, a function
// a FROM clause calls (json_each), an index INDEXED BY names, or a column a
// table qualifies.
func codeReads(text string) []string {
	toks := lexSQL(text)
	word := func(i int) string {
		if i < len(toks) && toks[i].kind == sqlWord {
			return strings.ToUpper(toks[i].text)
		}
		return ""
	}
	mark := func(i int) string {
		if i < len(toks) && toks[i].kind == sqlMark {
			return toks[i].text
		}
		return ""
	}
	var out []string
	// table takes the table a reference names at i — past its schema's name
	// where one qualifies it — reading it where read holds, and answers the
	// index the reference ends at.
	table := func(i int, read bool) int {
		if i >= len(toks) || toks[i].kind != sqlWord {
			return i - 1
		}
		if mark(i+1) == "." && i+2 < len(toks) && toks[i+2].kind == sqlWord {
			i += 2
		}
		if name := strings.ToLower(toks[i].text); read && strings.HasPrefix(name, "code_") &&
			!slices.Contains(out, name) {
			out = append(out, name)
		}
		return i
	}
	// clause is what one parenthesis level is reading: whether it is in a
	// FROM list, where a comma names the next table, and whether the next
	// word names one.
	type clause struct{ from, expect bool }
	levels := []clause{{}}
	prev := ""
	for i := 0; i < len(toks); i++ {
		at := &levels[len(levels)-1]
		this := ""
		switch tk := toks[i]; tk.kind {
		case sqlMark:
			switch tk.text {
			case "(":
				// A subquery in a FROM list, or a function's arguments:
				// the item is no table either way.
				at.expect = false
				levels = append(levels, clause{})
			case ")":
				if len(levels) > 1 {
					levels = levels[:len(levels)-1]
				}
			case ",":
				if at.from {
					at.expect = true
				}
			case ";":
				levels = []clause{{}}
			}
		case sqlWord:
			this = strings.ToUpper(tk.text)
			if at.expect {
				at.expect = false
				// A table-valued function — json_each(?) — is no table.
				if mark(i+1) != "(" {
					i = table(i, true)
				}
				break
			}
			switch this {
			case "FROM", "JOIN":
				at.from, at.expect = true, true
			case "UPDATE":
				// An upsert's DO UPDATE writes the row its conflict
				// found by its key: no table of its own.
				if prev != "DO" {
					j := i + 1
					if word(j) == "OR" {
						j += 2
					}
					i = table(j, true)
				}
			case "INTO":
				i = table(i+1, false)
			case "INDEXED":
				if word(i+1) == "BY" {
					i += 2
				}
			case "WHERE", "GROUP", "HAVING", "ORDER", "LIMIT", "WINDOW", "UNION", "EXCEPT",
				"INTERSECT", "RETURNING", "SET", "VALUES", "SELECT":
				at.from = false
			}
		}
		prev = this
	}
	return out
}

// paramCount is how many arguments a statement binds, by SQLite's numbering:
// a bare ? is one past the highest number so far, ?NNN is NNN, and a name is
// one past the highest the first time it appears and its own number after.
func paramCount(text string) int {
	highest := 0
	named := map[string]bool{}
	for _, tk := range lexSQL(text) {
		if tk.kind != sqlParam {
			continue
		}
		switch {
		case tk.text == "?":
			highest++
		case tk.text[0] == '?':
			if n, err := strconv.Atoi(tk.text[1:]); err == nil {
				highest = max(highest, n)
			}
		case !named[tk.text]:
			named[tk.text] = true
			highest++
		}
	}
	return highest
}
