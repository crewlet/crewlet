package paced_test

import (
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// TestOnePacingImplementation fails the build when a deadline is set on a
// client's connection anywhere a client's connection is served except in this
// package.
//
// The pace a client is held to was written once, in the file routes, and the
// code host needs the same two rules — a request read a mebibyte at a time
// under what is left of its budget, a response written so. A second copy is
// exactly the shape ADR-0008 was written against: the two agree the day they
// are written, and diverge one commit at a time, each package's tests passing
// against its own copy. The tell would be that one of them charged the
// server's own time to the client, or re-armed a whole deadline at every
// read, and nothing would notice.
//
// # What it flags: the CONNECTION, never the name
//
// A deadline is a pace only on a client's connection. The host also bounds
// connections that are no client's — the hook relay's handshake on a unix
// socket it accepted, a pipe to a git process — and a guard matching the
// method's name would refuse those, leaving a third exemption or a bound
// moved onto a timer to dodge it. So the walk judges what each deadline is SET
// ON, and a handler reaches its client's connection through exactly three
// doors, each judged in every non-test Go file under internal/api and
// internal/code/host:
//
//   - an *http.ResponseController — SetReadDeadline or SetWriteDeadline
//     selected, called or taken as a method value, resolving to the method
//     net/http's ResponseController declares: on the result of
//     http.NewResponseController, a parameter, a variable, a field, a
//     function's result, through pointers and embedded fields;
//   - the http.ResponseWriter itself, whose own deadline methods are what a
//     ResponseController calls — a type assertion (or a type switch case) of
//     a value typed http.ResponseWriter to an interface carrying any of the
//     three deadline methods is flagged where it is made;
//   - a HIJACKED connection — the first result of a call of a method named
//     Hijack, which is net/http's name for taking the client's connection
//     whoever offers it — on which a deadline (SetDeadline among them) is set.
//     It is followed through the variables it is assigned to and the
//     assertions made on it, inside the function that took it; one that
//     LEAVES that function — passed, stored, sent or returned — is reported,
//     because past that point a hijacked net.Conn and one a listener accepted
//     are the same type and the walk cannot say which a deadline is on. The
//     one return it allows is a method named Hijack handing the connection
//     straight back, which is a wrapper passing the door on, and its caller's
//     own Hijack call is judged in turn.
//
// Any other receiver — a connection a listener accepted or the engine
// dialled, an *os.File, a type of this module that declares the method itself
// (whose body is judged on its own) — is passed. A receiver or an asserted
// operand the walk CANNOT type, or whose method it cannot place, is reported
// rather than passed, as the statelog identity guard reports one: a gate
// asserting an absence that skipped what it could not read would certify
// nothing about exactly the values it skipped. Spell the value's type where
// it is declared, or teach decltype the shape.
//
// The exemptions are FUNCTIONS, never files: this package's three — Body's
// Read and the Writer's Write and End, which are the pace — and
// internal/api/httpjson's ReadBody, which sets ONE deadline over a small
// bounded body, a bound and not a pace. A deadline on a client's connection
// anywhere else in either file is a finding like any other, since a second
// pacing loop written beside ReadBody is the very copy this guard refuses,
// and a file-wide exemption would be the one place it was not looked for.
// Each exempt function must still set one, or its exemption is stale.
//
// # How a value is typed
//
// By internal/sourcetree/decltype, the one walk every gate judging a value by
// its type is built on: from declarations alone, on demand, with no type
// checker — into any package of this module, and into net/http and net,
// which it reads from the toolchain's tree, so a controller, a response
// writer, a hijacker and a connection are each the type its own declaration
// says, never a name this gate keeps a list of. A value only a package the
// walk does not read could type — a function's result, a member of one of
// its types — is passed: net/http, which hands a handler its client's
// connection, is read, so nothing else in reach does, and that is the
// coverage boundary. A ResponseWriter erased into an interface of another
// kind and asserted back from there, a controller held in an interface value
// whose deadline method is called through the interface, a hijack made
// inside a library (coder/websocket's Accept makes one) and a value reached
// through reflection are not seen.
//
// # Where it looks
//
// internal/api and internal/code/host, the two places a client's connection
// is served. internal/code/host is named before it exists, and a walk of a
// root that is not there reads nothing rather than failing; internal/api
// always exists, and a walk that read nothing there fails, since a guard
// asserting an absence passes alike on a clean tree and on a walk that saw
// none of it.
//
// # Cost
//
// It parses every non-test file under both roots and the 16 packages their
// values reach, net/http and net included, in 0.21–0.22 s with the `go list`
// that locates those two, 0.9 s under the race detector (four vCPUs).
func TestOnePacingImplementation(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	found := walkForDeadlines(t, root)

	if found.read["internal/api"] == 0 {
		t.Fatal("read no source file under internal/api — this guard was certifying nothing")
	}
	for _, f := range found.findings {
		t.Error(f.message())
	}
	// THE EXEMPTIONS ARE LIVE CONTROLS TOO: each exempt function does set a
	// deadline on a client's connection, so a matcher that stopped
	// recognising one fails here rather than certifying a clean tree — and
	// an exemption whose function stopped setting one is deleted rather than
	// kept as somewhere a second copy could hide.
	for _, e := range exempt {
		for _, fn := range e.funcs {
			if found.exempt[e.file+" "+fn] == 0 {
				t.Errorf("%s in %s is exempt from this guard and the walk found no deadline "+
					"set on a client's connection there: the matcher has gone inert, or the "+
					"exemption is stale and goes", fn, e.file)
			}
		}
	}
	t.Logf("read %d files under internal/api and %d under internal/code/host, and "+
		"%d package(s) in all: %d deadline(s) judged", found.read["internal/api"],
		found.read["internal/code/host"], found.packages, found.judged)
}

// roots are where a client's connection is served, relative to the module
// root.
var roots = []string{"internal/api", "internal/code/host"}

// exemption is a file, relative to the module root, and the functions in it
// that may set a deadline on a client's connection: each a function's name,
// or a method's as Type.Method — a closure counts as the function it is
// written in.
type exemption struct {
	file  string
	funcs []string
}

// exempt is every place a deadline may be set on a client's connection.
var exempt = []exemption{
	{file: "internal/api/paced/paced.go", funcs: []string{"Body.Read", "Writer.Write", "Writer.End"}},
	{file: "internal/api/httpjson/httpjson.go", funcs: []string{"ReadBody"}},
}

// deadlineNames are the methods that set a connection's deadline.
var deadlineNames = []string{"SetReadDeadline", "SetWriteDeadline", "SetDeadline"}

// modulePath is this module's import path: an import under it is read from
// the tree.
const modulePath = "github.com/crewlet/crewlet"

// The types the doors are judged by, read from net/http's own declarations.
var (
	controller     = decltype.TypeName{Package: "net/http", Name: "ResponseController"}
	responseWriter = decltype.TypeName{Package: "net/http", Name: "ResponseWriter"}
)

// readFromTheToolchain are the standard packages the walk reads: net/http,
// which declares the controller, the response writer and the hijacker, and
// net, which declares the connection a hijack returns and a listener
// accepts.
var readFromTheToolchain = []string{"net/http", "net"}

// findingKind is what the walk concluded about one place.
type findingKind int

const (
	// onConnection is a deadline set on a client's connection.
	onConnection findingKind = iota
	// untyped is a receiver or an operand whose type the walk cannot
	// establish.
	untyped
	// escaped is a hijacked connection that leaves the function that took
	// it.
	escaped
)

type finding struct {
	kind findingKind
	pos  string // file:line:column, the file relative to the module root
	what string // the expression the finding is about
}

func (f finding) message() string {
	switch f.kind {
	case untyped:
		return fmt.Sprintf("%s: %s may be a client's connection, and this walk cannot establish "+
			"its type to say whether it is. Spell the value's type where it is declared, or "+
			"teach decltype the shape", f.pos, f.what)
	case escaped:
		return fmt.Sprintf("%s: the connection %s hijacked leaves the function that took it, where "+
			"this walk cannot follow it to say whether a deadline is set on it. Keep a hijacked "+
			"connection inside the function that takes it, or pace it through internal/api/paced",
			f.pos, f.what)
	default:
		return fmt.Sprintf("%s: %s sets a deadline on a client's connection outside "+
			"internal/api/paced — pace a transfer through paced.Body/Writer/Send, the one "+
			"implementation of the floor every leg is held to (ADR-0008)", f.pos, f.what)
	}
}

type deadlineWalk struct {
	// read is how many non-test Go files each root held.
	read map[string]int
	// findings is every deadline on a client's connection outside the
	// exempt functions, and everything the walk could not judge.
	findings []finding
	// exempt is how many deadlines on a client's connection each exempt
	// function set, by its file and its name ("file Type.Method").
	exempt map[string]int
	// packages is how many packages were read, judged or reached; judged
	// how many deadline selectors were typed.
	packages, judged int
}

// walkForDeadlines judges every non-test Go file under the roots below root.
func walkForDeadlines(t *testing.T, root string) deadlineWalk {
	t.Helper()
	found := deadlineWalk{read: map[string]int{}, exempt: map[string]int{}}
	// The toolchain's packages are those the module builds against, so they
	// are located from the module's own root, whatever tree is judged.
	dirs, err := decltype.Locate(t.Context(), sourcetree.Root(t), readFromTheToolchain...)
	if err != nil {
		t.Fatal(err)
	}
	w := decltype.New(root, modulePath, dirs)
	for _, r := range roots {
		start := filepath.Join(root, filepath.FromSlash(r))
		var files []string
		err := sourcetree.Walk(start, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if p == start && errors.Is(err, fs.ErrNotExist) {
					// A root not written yet holds nothing to judge.
					return fs.SkipAll
				}
				return err
			}
			if d.IsDir() {
				// testdata is not compiled.
				if d.Name() == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, ".go") && !strings.HasSuffix(p, "_test.go") {
				found.read[r]++
				files = append(files, p)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", r, err)
		}
		for _, p := range files {
			// Every file, wherever it is built: one this platform does not
			// build is read against the scope of the files it does.
			if f := w.FileAt(p); f != nil {
				judge(w, f, &found)
			}
		}
	}
	for _, err := range w.Errors() {
		t.Error(err)
	}
	found.packages = w.Read()
	return found
}

// judge collects what one file does to a client's connection.
func judge(w *decltype.World, f *decltype.File, found *deadlineWalk) {
	i := slices.IndexFunc(exempt, func(e exemption) bool { return e.file == f.Rel })
	parents := parentsOf(f.AST)
	report := func(kind findingKind, at ast.Node, e ast.Expr) {
		if i >= 0 && kind == onConnection {
			if fn := funcName(parents, at); slices.Contains(exempt[i].funcs, fn) {
				found.exempt[f.Rel+" "+fn]++
				return
			}
		}
		pos := w.FileSet().Position(at.Pos())
		found.findings = append(found.findings, finding{kind: kind,
			pos: fmt.Sprintf("%s:%d:%d", f.Rel, pos.Line, pos.Column), what: types.ExprString(e)})
	}

	// THE THIRD DOOR FIRST: a hijacked connection, followed inside the
	// function that took it.
	judgedHere := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f.AST, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if sel, ok := ast.Unparen(call.Fun).(*ast.SelectorExpr); ok && sel.Sel.Name == "Hijack" {
			followHijack(parents, call, judgedHere, report)
		}
		return true
	})

	ast.Inspect(f.AST, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if !slices.Contains(deadlineNames, n.Sel.Name) || judgedHere[n] {
				return true
			}
			if _, isPkg := w.PackageNamed(n.X, f); isPkg {
				// A function of another package that happens to share
				// the name, not a method.
				return true
			}
			found.judged++
			switch deadlineOwner(w, w.TypeOf(n.X, f), n.Sel.Name) {
			case hit:
				report(onConnection, n, n)
			case cannot:
				report(untyped, n, n.X)
			}
		case *ast.TypeAssertExpr:
			if n.Type != nil {
				judgeAssertion(w, f, n.X, []ast.Expr{n.Type}, n, report)
			}
		case *ast.TypeSwitchStmt:
			var operand ast.Expr
			switch a := n.Assign.(type) {
			case *ast.AssignStmt:
				operand = a.Rhs[0].(*ast.TypeAssertExpr).X
			case *ast.ExprStmt:
				operand = a.X.(*ast.TypeAssertExpr).X
			}
			for _, c := range n.Body.List {
				judgeAssertion(w, f, operand, c.(*ast.CaseClause).List, c, report)
			}
		}
		return true
	})
}

// verdict is what the walk concludes about one receiver or operand.
type verdict int

const (
	pass verdict = iota
	hit
	cannot
)

// deadlineOwner judges the receiver of a deadline method: hit when the
// method it selects is the one net/http's ResponseController declares.
func deadlineOwner(w *decltype.World, t decltype.Type, method string) verdict {
	sel := w.Select(t, method)
	switch sel.Kind {
	case decltype.Method:
		if sel.Owner == controller && !sel.Interface {
			return hit
		}
		// A type of this module declaring the method itself, whose body is
		// judged where it is written; a connection's; an interface's,
		// where what put a client's connection in it is judged where it
		// happened — the assertion of a ResponseWriter, or the hijack.
		return pass
	case decltype.Field, decltype.Beyond, decltype.FromUnread:
		// A field holding a function; or a value only a package the walk
		// does not read could type, which hands nobody a client's
		// connection (the coverage boundary).
		return pass
	}
	// Undetermined, or Absent: a selector the compiler accepts and the walk
	// cannot place.
	return cannot
}

// judgeAssertion reports an assertion of an http.ResponseWriter to a type
// carrying a deadline method — the second door — at node at.
func judgeAssertion(w *decltype.World, f *decltype.File, operand ast.Expr, targets []ast.Expr,
	at ast.Node, report func(findingKind, ast.Node, ast.Expr)) {
	named := pass
	for _, target := range targets {
		if id, ok := target.(*ast.Ident); ok && id.Name == "nil" && id.Obj == nil {
			continue
		}
		switch namesDeadline(w, decltype.Spelled(target, f)) {
		case hit:
			named = hit
		case cannot:
			if named == pass {
				named = cannot
			}
		}
	}
	if named == pass {
		return
	}
	switch isWriter(w, w.TypeOf(operand, f), 0) {
	case hit:
		if named == hit {
			report(onConnection, at, operand)
			return
		}
		report(untyped, at, targets[0])
	case cannot:
		report(untyped, at, operand)
	}
}

// isWriter judges whether a value of type t is an http.ResponseWriter: hit
// when it is one, or an interface embedding one.
func isWriter(w *decltype.World, t decltype.Type, depth int) verdict {
	if _, unread := t.Unread(); unread {
		return pass
	}
	if !t.Known() || depth > 8 {
		return cannot
	}
	if n, ok := w.Named(t); ok && n == responseWriter {
		return hit
	}
	lit, how := w.Underlying(t)
	switch how {
	case decltype.Outside:
		return pass
	case decltype.Unresolved:
		return cannot
	}
	it, ok := lit.Expr().(*ast.InterfaceType)
	if !ok {
		return pass
	}
	v := pass
	for _, m := range it.Methods.List {
		if len(m.Names) != 0 {
			continue
		}
		switch m.Type.(type) {
		case *ast.BinaryExpr, *ast.UnaryExpr:
			// A constraint's type terms: no value has such a type.
			continue
		}
		switch isWriter(w, lit.In(m.Type), depth+1) {
		case hit:
			return hit
		case cannot:
			v = cannot
		}
	}
	return v
}

// namesDeadline judges whether an assertion's target type carries a
// deadline method: hit when it is an interface whose method set holds one —
// listed, or embedded, net.Conn's included.
func namesDeadline(w *decltype.World, t decltype.Type) verdict {
	if !t.Known() {
		return cannot
	}
	lit, how := w.Underlying(t)
	switch how {
	case decltype.Outside:
		// A type of a package the walk does not read (the coverage
		// boundary).
		return pass
	case decltype.Unresolved:
		return cannot
	}
	if _, ok := lit.Expr().(*ast.InterfaceType); !ok {
		// A concrete type: its methods are judged where they are written.
		return pass
	}
	v := pass
	for _, name := range deadlineNames {
		switch w.Select(t, name).Kind {
		case decltype.Method:
			return hit
		case decltype.Undetermined:
			v = cannot
		}
	}
	return v
}

// followHijack follows the connection a Hijack call returns inside the
// function that made it, reporting every deadline set on it and every way
// it leaves.
func followHijack(parents map[ast.Node]ast.Node, call *ast.CallExpr, judged map[*ast.SelectorExpr]bool,
	report func(findingKind, ast.Node, ast.Expr)) {
	fn, outer := enclosingFuncs(parents, call)
	if outer == nil {
		report(escaped, call, call)
		return
	}
	var follow func(value ast.Expr)
	tracked := map[variable]bool{}
	track := func(id *ast.Ident, value ast.Expr) {
		v, ok := variableOf(id)
		switch {
		case id.Name == "_":
		case !ok || !localTo(v, outer):
			report(escaped, value, value)
		case !tracked[v]:
			tracked[v] = true
			usesOf(parents, outer, v, follow)
		}
	}
	follow = func(value ast.Expr) {
		parent := parents[value]
		for {
			p, ok := parent.(*ast.ParenExpr)
			if !ok {
				break
			}
			value, parent = p, parents[p]
		}
		switch p := parent.(type) {
		case *ast.SelectorExpr:
			if slices.Contains(deadlineNames, p.Sel.Name) {
				judged[p] = true
				report(onConnection, p, p)
			}
			// Any other method — Close, Read, Write, an address — leaves
			// the connection where it was.
		case *ast.TypeAssertExpr:
			follow(p)
		case *ast.BinaryExpr:
			// Compared, with nil or another: nothing leaves.
		case *ast.ExprStmt:
			// Discarded.
		case *ast.AssignStmt:
			if slices.Contains(p.Lhs, value) {
				// Assigned to, not read.
				return
			}
			at := slices.Index(p.Rhs, value)
			if len(p.Rhs) == 1 && len(p.Lhs) > 1 {
				// The connection is the first result of the call or the
				// value of the comma-ok assertion.
				at = 0
			}
			if at < 0 || at >= len(p.Lhs) {
				report(escaped, value, value)
				return
			}
			id, ok := p.Lhs[at].(*ast.Ident)
			if !ok {
				report(escaped, value, value)
				return
			}
			track(id, value)
		case *ast.ValueSpec:
			at := slices.Index(p.Values, value)
			if len(p.Values) == 1 {
				at = 0
			}
			if at < 0 || at >= len(p.Names) {
				report(escaped, value, value)
				return
			}
			track(p.Names[at], value)
		case *ast.ReturnStmt:
			if fd, ok := fn.(*ast.FuncDecl); ok && fd.Name.Name == "Hijack" && fd.Recv != nil {
				// A wrapper passing the door on: its caller's Hijack is
				// judged where it is made.
				return
			}
			report(escaped, value, value)
		default:
			report(escaped, value, value)
		}
	}
	follow(call)
}

// variable is a local variable: the declaration the parser's file-scoped
// resolution names, and its name, since one declaration declares several.
type variable struct {
	decl any
	name string
}

// variableOf is the variable an identifier names, when the parser resolved
// it to a variable's declaration.
func variableOf(id *ast.Ident) (variable, bool) {
	if id.Obj == nil || id.Obj.Kind != ast.Var || id.Obj.Decl == nil {
		return variable{}, false
	}
	return variable{decl: id.Obj.Decl, name: id.Name}, true
}

// usesOf calls use with every reading of v in body.
func usesOf(parents map[ast.Node]ast.Node, body ast.Node, v variable, use func(ast.Expr)) {
	ast.Inspect(body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if u, ok := variableOf(id); ok && u == v && !declares(parents, id, v) {
				use(id)
			}
		}
		return true
	})
}

// declares reports whether id is where v is declared, rather than a use.
func declares(parents map[ast.Node]ast.Node, id *ast.Ident, v variable) bool {
	switch d := v.decl.(type) {
	case *ast.AssignStmt:
		return d.Tok == token.DEFINE && slices.Contains(d.Lhs, ast.Expr(id))
	case *ast.ValueSpec:
		return slices.Contains(d.Names, id)
	}
	return parents[id] == v.decl
}

// localTo reports whether v is declared inside body.
func localTo(v variable, body ast.Node) bool {
	d, ok := v.decl.(ast.Node)
	return ok && d.Pos() >= body.Pos() && d.End() <= body.End()
}

// funcName is the name of the declared function n is written in — a
// method's as Type.Method — or "" outside any.
func funcName(parents map[ast.Node]ast.Node, n ast.Node) string {
	for p := parents[n]; p != nil; p = parents[p] {
		fd, ok := p.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if fd.Recv == nil || len(fd.Recv.List) == 0 {
			return fd.Name.Name
		}
		return decltype.BaseName(fd.Recv.List[0].Type) + "." + fd.Name.Name
	}
	return ""
}

// enclosingFuncs is the innermost function n is written in, and the body of
// the outermost — where a closure's captures are read too.
func enclosingFuncs(parents map[ast.Node]ast.Node, n ast.Node) (inner ast.Node, outer *ast.BlockStmt) {
	for p := parents[n]; p != nil; p = parents[p] {
		switch fn := p.(type) {
		case *ast.FuncLit:
			if inner == nil {
				inner = fn
			}
			outer = fn.Body
		case *ast.FuncDecl:
			if inner == nil {
				inner = fn
			}
			outer = fn.Body
		}
	}
	return inner, outer
}

// parentsOf maps every node of file to the node it is written in.
func parentsOf(file *ast.File) map[ast.Node]ast.Node {
	parents := map[ast.Node]ast.Node{}
	var stack []ast.Node
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return false
		}
		if len(stack) > 0 {
			parents[n] = stack[len(stack)-1]
		}
		stack = append(stack, n)
		return true
	})
	return parents
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN. Every door to a client's
// connection, in each of the shapes a value reaches a deadline by, must be
// reported: a ResponseController as a parameter, a call's result, a :=, a
// field, a promoted method, a method value and a function of another package
// of the module; a ResponseWriter asserted to a deadline interface; a
// hijacked connection and an assertion made on one. A hijacked connection
// handed to another function must be reported as escaping, and a receiver
// the walk cannot type as untyped. And none of these may be: a deadline on a
// connection a listener accepted (the hook relay's handshake), on a pipe's
// file, on a connection of a package imported under a version (named as Go
// names it, never by the path's last element) or through a type's own
// method, an assertion to an interface carrying no deadline, a wrapper
// passing a Hijack on, the exempt functions' own (counted instead — while a
// second deadline in each exempt file, a closure's among them, is reported),
// a test file, testdata and a package outside both roots. Then the same tree
// without internal/code/host, which is not an error.
func TestThePacingWalkFindsADeadline(t *testing.T) {
	t.Parallel()
	head := func(imports ...string) string {
		return "package p\n\nimport (\n\t\"" + strings.Join(imports, "\"\n\t\"") + "\"\n)\n"
	}
	files := map[string]string{
		"internal/api/x.go": head("net/http", "time") + `
func xParam(rc *http.ResponseController) { _ = rc.SetWriteDeadline(time.Time{}) }

func xCall(w http.ResponseWriter) { _ = http.NewResponseController(w).SetReadDeadline(time.Time{}) }

func xDefine(w http.ResponseWriter) {
	ctl := http.NewResponseController(w)
	_ = ctl.SetWriteDeadline(time.Time{})
}

type xHolder struct{ rc *http.ResponseController }

func (v *xHolder) field() { _ = v.rc.SetWriteDeadline(time.Time{}) }

type xEmbeds struct{ *http.ResponseController }

func (v xEmbeds) promoted() { _ = v.SetReadDeadline(time.Time{}) }

func xValue(rc *http.ResponseController) func(time.Time) error { return rc.SetReadDeadline }

func xAssert(w http.ResponseWriter) {
	_ = w.(interface{ SetWriteDeadline(time.Time) error }).SetWriteDeadline(time.Time{})
}
`,
		"internal/api/sub/v.go": head("net/http", "time", modulePath+"/internal/kit") + `
func vKit(w http.ResponseWriter) { _ = kit.Controller(w).SetWriteDeadline(time.Time{}) }
`,
		"internal/kit/kit.go": head("net/http", "time") + `
func Controller(w http.ResponseWriter) *http.ResponseController { return http.NewResponseController(w) }

func outside(rc *http.ResponseController) { _ = rc.SetWriteDeadline(time.Time{}) }
`,
		"internal/api/z.go": head("net/http", "os", "time", "example.com/dial/v2", "gopkg.in/sock.v1") + `
func zFlush(rc *http.ResponseController) { _ = rc.Flush() }

func zVersioned(c *dial.Conn) { _ = c.SetDeadline(time.Time{}) }

func zGopkg(c sock.Conn) { _ = c.SetReadDeadline(time.Time{}) }

func zPipe(f *os.File) { _ = f.SetReadDeadline(time.Time{}) }

type zOwn struct{}

func (zOwn) SetWriteDeadline(time.Time) error { return nil }

func zOwnMethod(o zOwn) { _ = o.SetWriteDeadline(time.Time{}) }

func zFlusher(w http.ResponseWriter) {
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}
}
`,
		"internal/api/u.go": head("net/http", "time") + `
func uSwitch(x any) {
	switch c := x.(type) {
	case *http.ResponseController:
		_ = c.SetWriteDeadline(time.Time{})
	}
}

func uGeneric[T interface{ SetDeadline(time.Time) error }](c T) { _ = c.SetDeadline(time.Time{}) }
`,
		"internal/api/paced/paced.go": head("net/http", "time") + `
type Writer struct{ rc *http.ResponseController }

func (w *Writer) Write(p []byte) (int, error) {
	_ = w.rc.SetWriteDeadline(time.Time{})
	return len(p), nil
}

func pacedSecond(rc *http.ResponseController) { _ = rc.SetReadDeadline(time.Time{}) }
`,
		"internal/api/httpjson/httpjson.go": head("net/http", "time") + `
func ReadBody(w http.ResponseWriter) { _ = http.NewResponseController(w).SetReadDeadline(time.Time{}) }

func readMore(w http.ResponseWriter) {
	read := func() { _ = http.NewResponseController(w).SetReadDeadline(time.Time{}) }
	read()
}
`,
		"internal/api/paced/paced_test.go": head("net/http", "time") + `
func tested(rc *http.ResponseController) { _ = rc.SetWriteDeadline(time.Time{}) }
`,
		"internal/api/testdata/w.go": head("net/http", "time") + `
func fixture(rc *http.ResponseController) { _ = rc.SetWriteDeadline(time.Time{}) }
`,
		"internal/code/host/relay.go": head("net", "time") + `
type relay struct{ ln net.Listener }

func (r *relay) serve() {
	conn, err := r.ln.Accept()
	if err != nil {
		return
	}
	go r.handle(conn)
}

func (r *relay) handle(conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_ = conn.SetReadDeadline(time.Time{})
}

func (r *relay) direct() {
	c, _ := r.ln.Accept()
	_ = c.SetDeadline(time.Time{})
}
`,
		"internal/code/host/h.go": head("bufio", "net", "net/http", "time") + `
func hDeadline(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Time{})
}

func hHandsOn(w http.ResponseWriter) {
	conn, _, _ := w.(http.Hijacker).Hijack()
	go hServe(conn)
}

func hServe(c net.Conn) { _ = c.SetDeadline(time.Time{}) }

type hWrapped struct{ http.ResponseWriter }

func (v hWrapped) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return v.ResponseWriter.(http.Hijacker).Hijack()
}

func hAsserted(w http.ResponseWriter) {
	c, _, _ := hWrapped{w}.Hijack()
	tcp := c.(*net.TCPConn)
	_ = tcp.SetWriteDeadline(time.Time{})
}
`,
		"internal/other/o.go": head("net/http", "time") + `
func elsewhere(rc *http.ResponseController) { _ = rc.SetWriteDeadline(time.Time{}) }
`,
	}
	tree := func(t *testing.T, without string) string {
		root := t.TempDir()
		for p, body := range files {
			if strings.HasPrefix(p, without+"/") {
				continue
			}
			full := filepath.Join(root, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		return root
	}
	verdicts := func(found deadlineWalk) []string {
		var out []string
		for _, f := range found.findings {
			file, _, _ := strings.Cut(f.pos, ":")
			kind := [...]string{"connection", "untyped", "escaped"}[f.kind]
			out = append(out, file+" "+kind+" "+f.what)
		}
		slices.Sort(out)
		return out
	}

	found := walkForDeadlines(t, tree(t, "none"))
	want := []string{
		"internal/api/httpjson/httpjson.go connection http.NewResponseController(w).SetReadDeadline",
		"internal/api/paced/paced.go connection rc.SetReadDeadline",
		"internal/api/sub/v.go connection kit.Controller(w).SetWriteDeadline",
		"internal/api/u.go untyped c",
		"internal/api/u.go untyped c",
		"internal/api/x.go connection ctl.SetWriteDeadline",
		"internal/api/x.go connection http.NewResponseController(w).SetReadDeadline",
		"internal/api/x.go connection rc.SetReadDeadline",
		"internal/api/x.go connection rc.SetWriteDeadline",
		"internal/api/x.go connection v.SetReadDeadline",
		"internal/api/x.go connection v.rc.SetWriteDeadline",
		"internal/api/x.go connection w",
		"internal/code/host/h.go connection conn.SetDeadline",
		"internal/code/host/h.go connection tcp.SetWriteDeadline",
		"internal/code/host/h.go escaped conn",
	}
	if got := verdicts(found); !slices.Equal(got, want) {
		t.Errorf("findings\n\t%s\nwant\n\t%s", strings.Join(got, "\n\t"), strings.Join(want, "\n\t"))
	}
	if found.read["internal/api"] != 6 || found.read["internal/code/host"] != 2 {
		t.Errorf("read %v, want the six non-test files under internal/api and the two "+
			"under internal/code/host", found.read)
	}
	if want := map[string]int{
		"internal/api/paced/paced.go Writer.Write":   1,
		"internal/api/httpjson/httpjson.go ReadBody": 1,
	}; !maps.Equal(found.exempt, want) {
		t.Errorf("exempt %v, want %v: one deadline counted in each exempt function, and "+
			"the second in each exempt file reported above", found.exempt, want)
	}

	hostless := walkForDeadlines(t, tree(t, "internal/code"))
	if got := verdicts(hostless); len(got) != len(want)-3 || hostless.read["internal/code/host"] != 0 {
		t.Errorf("without internal/code/host: findings %q, read %v; want internal/api's and no error",
			got, hostless.read)
	}
}
