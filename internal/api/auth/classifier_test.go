package auth_test

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// TestGitAccessIsTheOneClassifier fails the build when anything but
// auth.GitAccess, in this module's non-test Go, decides which git service a
// request asks for.
//
// # Why one classifier
//
// Whether a request is a FETCH or a PUSH decides two things a mistake turns
// into a hole: a draining node finishes a fetch and refuses a push, and the
// code host serves a fetch to a token bound to nobody while a push needs a
// person.
// Read twice — once by the drain gate, once by the host — the two readings
// agree the day they are written and drift one commit at a time, each passing
// its own tests; and the day one of them reads a request the other calls a
// push as a fetch, a drain admits a push or an unbound token reaches
// receive-pack. So the reading is GitAccess's, and its readers ask it.
//
// # What it flags
//
// A request's service is named by git's own words for it, git-upload-pack and
// git-receive-pack, and it travels in the query's `service` key. So, outside
// the allowances below, every place that:
//
//   - spells a git service in a string literal, escapes included;
//   - builds one from pieces: a concatenation that folds to one, or a constant
//     naming one — this file's, another file's or another package's, one a
//     const group repeats included — compared with == or !=, switched on or
//     listed in a case, or handed to a function of strings, bytes, path,
//     path/filepath, regexp or slices;
//   - reads the key `service` — spelled as a literal or a constant — from a
//     request's query: Get or Has on a url.Values, an index of one (r.Form
//     and r.PostForm are one), or FormValue or PostFormValue on an
//     *http.Request. The receiver is TYPED, by internal/sourcetree/decltype
//     reading net/url's and net/http's own declarations, so a query held in
//     a variable is the url.Values it is; and a receiver the walk cannot type
//     is reported rather than passed, because an absence gate that skipped
//     what it could not read would certify nothing about exactly those.
//
// A constant naming a service handed anywhere ELSE is not a reading: the API
// mounts the code host's routes by the host's own pattern constants, and
// mounting a route is not deciding what a request is (so the route table
// needs no allowance of its own).
//
// # The allowances, and why they are live
//
// internal/api/auth/git.go, where GitAccess is, and internal/code/host, which
// answers each service with its own process and so has to read which one was
// asked for — after asking GitAccess what the request is. Each allowance is a
// CONTROL as well: git.go must hold a hit, and internal/code/host must hold
// one once it holds a file, so a matcher that went inert fails here rather
// than certifying a clean tree, and an allowance the code moved away from is
// deleted rather than kept as a place a second classifier could hide.
//
// # What it parses, and why that is exact
//
// Not every file: parsing the whole module costs seconds under the race
// detector, and almost none of it can hold a reading. Every construct above
// is built from PIECES — the literals and the constants it is made of — and
// the walk reads a construct whose pieces include a literal SPELLING
// receive-pack or upload-pack (or, for the key, a literal that IS service),
// or a constant carrying one. So a file is parsed when its bytes hold such a
// literal — receive-pack, upload-pack, "service" or `service`, or an escape
// that can spell a letter (\x, \u, \U, an octal digit), since a literal's
// value need not be its bytes — or spell the NAME of a constant carrying one,
// which is exact because an identifier has no escapes. Every such constant is
// declared in a file parsed by that same rule, so the names are gathered to a
// fixed point: a constant declared from one (const mine = host.Pattern)
// carries it too, and the files spelling it are parsed in turn. The two
// allowances are always parsed, since they are the controls.
//
// # Coverage boundary
//
// Not seen: a service name or the key split INSIDE receive-pack, upload-pack
// or service across pieces ("git-receive-pa" + "ck"); a name assembled at run
// time (fmt.Sprintf, a byte slice, a value read from a file); a query read
// through RawQuery by hand with no literal naming a service; a url.Values held
// in an interface value, converted to a map type literal or produced by a
// package the walk does not read; and a constant naming a service handed to a
// function outside the packages listed above. A classifier written in any of
// those shapes is one a reviewer reads; the shapes a second classifier is
// actually written in are the ones above.
//
// # Where it looks
//
// Every non-test Go file under the module root — not only internal/, so a
// directory added beside it is read unasked — skipping testdata, which is not
// compiled, and every nested checkout ([sourcetree.Walk]).
func TestGitAccessIsTheOneClassifier(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	found := walkForClassifiers(t, root, modulePath)
	if found.read == 0 {
		t.Fatal("read no source file under the module root — this guard was certifying nothing")
	}
	for _, msg := range found.verdict() {
		t.Error(msg)
	}
	t.Logf("read %d files and parsed %d, %d package(s) loaded: %d place(s) in the allowances, "+
		"%d receiver(s) typed", found.read, found.parsed, found.packages,
		found.allowed[classifierFile]+found.allowed[hostDir], found.typed)
}

// modulePath is this module's import path: an import under it is read from the
// tree.
const modulePath = "github.com/crewlet/crewlet"

// The allowances, relative to the module root.
const (
	classifierFile = "internal/api/auth/git.go"
	hostDir        = "internal/code/host/"
)

// gitServices are git's names for the two services, which a classifier cannot
// avoid spelling; servicePieces are what a literal spells to be a piece of one.
var (
	gitServices   = []string{"git-upload-pack", "git-receive-pack"}
	servicePieces = []string{"upload-pack", "receive-pack"}
)

// serviceKey is the query key git's advertisement request names its service by.
const serviceKey = "service"

// seeds are the bytes a file holds when one of its literals spells a piece or
// the key without an escape: the pieces themselves, and the key quoted either
// way (a literal can equal it only by being it).
var seeds = [][]byte{
	[]byte("upload-pack"), []byte("receive-pack"), []byte(`"service"`), []byte("`service`"),
}

// searchPackages are the standard packages whose functions compare or search
// strings: a constant naming a service handed to one is a reading.
var searchPackages = []string{"strings", "bytes", "path", "path/filepath", "regexp", "slices"}

// The types a reading of the service key is judged by, read from the standard
// library's own declarations.
var (
	urlValues   = decltype.TypeName{Package: "net/url", Name: "Values"}
	httpRequest = decltype.TypeName{Package: "net/http", Name: "Request"}
)

// classifierWalk is what one walk of a tree found.
type classifierWalk struct {
	// read is how many non-test Go files the walk reached; parsed how many it
	// parsed and judged; hostFiles how many of those it reached are under the
	// host's allowance.
	read, parsed, hostFiles int
	// findings is every reading outside the allowances, as a message.
	findings []string
	// allowed is how many readings each allowance holds, by its key.
	allowed map[string]int
	// typed is how many receivers were typed; packages how many packages the
	// typing and the constants of other files loaded.
	typed, packages int
}

// verdict is every failure the walk amounts to: its findings, and each
// allowance that holds no reading.
func (c classifierWalk) verdict() []string {
	msgs := slices.Clone(c.findings)
	if c.allowed[classifierFile] == 0 {
		msgs = append(msgs, classifierFile+" spells no git service and reads no service key: "+
			"the matcher has gone inert, or GitAccess moved and this allowance is stale and goes")
	}
	if c.hostFiles > 0 && c.allowed[hostDir] == 0 {
		msgs = append(msgs, hostDir+" holds files and none of them spells a git service or reads "+
			"the service key: the matcher has gone inert, or the host stopped reading the service "+
			"and this allowance is stale and goes")
	}
	return msgs
}

// located is where net/http and net/url live, asked of `go list` once for every
// walk this binary makes: the standard packages are those the module builds
// against, so they are located from the module's own root, whatever tree is
// judged. Read-only once set, so walks running in parallel share it.
var located struct {
	sync.Mutex
	dirs map[string]string
}

// standardDirs is [located], locating it on first use.
func standardDirs(t *testing.T) map[string]string {
	t.Helper()
	located.Lock()
	defer located.Unlock()
	if located.dirs == nil {
		dirs, err := decltype.Locate(t.Context(), sourcetree.Root(t), "net/http", "net/url")
		if err != nil {
			t.Fatal(err)
		}
		located.dirs = dirs
	}
	return located.dirs
}

// sourceFile is one parsed file and what resolves its names.
type sourceFile struct {
	// path is where it was read; rel is path relative to the root,
	// slash-separated.
	path, rel string
	ast       *ast.File
	// importOf is the import path a file-scope name names.
	importOf func(name string) (string, bool)
	// pkg is the import path of the file's package.
	pkg string
}

// constDecl is a constant: the spec that declares it and the file it is in.
type constDecl struct {
	spec *ast.ValueSpec
	file *sourceFile
}

// classifierJudge is one walk's state.
type classifierJudge struct {
	module string
	fset   *token.FileSet
	// w types the receivers and reads the constants of a package a fold
	// reaches; it loads a package only when asked.
	w     *decltype.World
	found *classifierWalk
	// values is each const spec's value list, per file: its own, or the
	// one it repeats in its group.
	values map[*ast.File]map[*ast.ValueSpec][]ast.Expr
	// pkgConsts is each package's package-level constants by name, by
	// import path, as the world read them.
	pkgConsts map[string]map[string]constDecl
	// carriers and keys are the names of the constants a parsed file
	// declares that carry a piece of a service name, or the key.
	carriers, keys map[string]bool
}

// maxFold bounds a constant's resolution, which a constant defined by itself
// through another package would otherwise make endless (the compiler refuses
// one, and the walk reads files the compiler may not have).
const maxFold = 32

// walkForClassifiers judges every non-test Go file under root, a tree of the
// module named module.
func walkForClassifiers(t *testing.T, root, module string) classifierWalk {
	t.Helper()
	found := classifierWalk{allowed: map[string]int{}}
	var paths []string
	err := sourcetree.Walk(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
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
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	j := &classifierJudge{module: module, fset: token.NewFileSet(),
		w: decltype.New(root, module, standardDirs(t)), found: &found,
		values:    map[*ast.File]map[*ast.ValueSpec][]ast.Expr{},
		pkgConsts: map[string]map[string]constDecl{},
		carriers:  map[string]bool{}, keys: map[string]bool{}}

	srcs := make(map[string][]byte, len(paths))
	admitted := map[string]bool{}
	for _, p := range paths {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		srcs[p] = src
		rel := relTo(root, p)
		if strings.HasPrefix(rel, hostDir) {
			found.hostFiles++
		}
		if rel == classifierFile || strings.HasPrefix(rel, hostDir) || seeded(src) {
			admitted[p] = true
		}
	}
	found.read = len(paths)

	// THE FIXED POINT: parse what is admitted, gather the constants its
	// files declare that carry a piece or the key, and admit every file
	// spelling one of their names — until a round admits nothing.
	var parsed []*sourceFile
	var decls []constDecl
	done := map[string]bool{}
	for {
		for _, p := range paths {
			if !admitted[p] || done[p] {
				continue
			}
			done[p] = true
			f, err := parser.ParseFile(j.fset, p, srcs[p], 0)
			if err != nil {
				t.Errorf("parse %s: %v", p, err)
				continue
			}
			sf := j.sourceOf(root, p, f)
			parsed = append(parsed, sf)
			decls = append(decls, j.constsIn(sf)...)
		}
		names := j.gather(decls)
		if len(names) == 0 {
			break
		}
		prefilter := sourcetree.MustIdentifiers(names...)
		grew := false
		for _, p := range paths {
			if !admitted[p] && prefilter.In(srcs[p]) {
				admitted[p], grew = true, true
			}
		}
		if !grew {
			break
		}
	}
	for _, sf := range parsed {
		j.judge(sf)
	}
	found.parsed = len(parsed)
	for _, err := range j.w.Errors() {
		t.Error(err)
	}
	found.packages = j.w.Read()
	return found
}

// relTo is p relative to root, slash-separated.
func relTo(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// seeded reports whether src may hold a literal spelling a piece or the key:
// it holds one's bytes, or an escape that can spell a letter.
func seeded(src []byte) bool {
	for _, s := range seeds {
		if bytes.Contains(src, s) {
			return true
		}
	}
	for i := 0; i+1 < len(src); i++ {
		if src[i] != '\\' {
			continue
		}
		switch c := src[i+1]; {
		case c == 'x', c == 'u', c == 'U', c >= '0' && c <= '7':
			return true
		}
	}
	return false
}

// sourceOf is a file this walk parsed, with what resolves its names.
func (j *classifierJudge) sourceOf(root, path string, f *ast.File) *sourceFile {
	imports := map[string]string{}
	for _, imp := range f.Imports {
		ip, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := decltype.ImportName(ip)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name != "_" && name != "." {
			imports[name] = ip
		}
	}
	rel := relTo(root, path)
	pkg := j.module
	if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." {
		pkg += "/" + dir
	}
	return &sourceFile{path: path, rel: rel, ast: f, pkg: pkg,
		importOf: func(name string) (string, bool) { ip, ok := imports[name]; return ip, ok }}
}

// worldFile is a file the world read, as a [sourceFile].
func worldFile(f *decltype.File) *sourceFile {
	return &sourceFile{path: f.Path, rel: f.Rel, ast: f.AST, importOf: f.Import, pkg: f.Package.Path}
}

// constsIn is every constant sf declares, at any depth.
func (j *classifierJudge) constsIn(sf *sourceFile) []constDecl {
	var out []constDecl
	for spec := range j.valuesOf(sf.ast) {
		out = append(out, constDecl{spec: spec, file: sf})
	}
	return out
}

// gather extends the names of the constants that carry a piece of a service
// name or the key, over decls, to a fixed point among them, and answers every
// name known so far.
func (j *classifierJudge) gather(decls []constDecl) []string {
	for grew := true; grew; {
		grew = false
		for _, d := range decls {
			values := j.valuesOf(d.file.ast)[d.spec]
			for i, n := range d.spec.Names {
				if n.Name == "_" || i >= len(values) {
					continue
				}
				if !j.carriers[n.Name] && j.carries(values[i]) {
					j.carriers[n.Name], grew = true, true
				}
				if !j.keys[n.Name] && j.carriesKey(values[i]) {
					j.keys[n.Name], grew = true, true
				}
			}
		}
	}
	var names []string
	for n := range j.carriers {
		names = append(names, n)
	}
	for n := range j.keys {
		if !j.carriers[n] {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

// carries reports whether e holds a piece of a service name: a literal spelling
// one, or a name a constant carrying one has.
func (j *classifierJudge) carries(e ast.Expr) bool {
	return holds(e, func(s string) bool {
		return slices.ContainsFunc(servicePieces, func(p string) bool { return strings.Contains(s, p) })
	}, j.carriers)
}

// carriesKey reports whether e holds the key: a literal that is it, or a name a
// constant carrying it has.
func (j *classifierJudge) carriesKey(e ast.Expr) bool {
	return holds(e, func(s string) bool { return s == serviceKey }, j.keys)
}

// holds reports whether e holds a string literal lit accepts, or an identifier
// names holds.
func holds(e ast.Expr, lit func(string) bool, names map[string]bool) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.BasicLit:
			if x.Kind == token.STRING {
				if s, err := strconv.Unquote(x.Value); err == nil && lit(s) {
					found = true
				}
			}
		case *ast.Ident:
			if names[x.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

// judge collects every reading in one file.
func (j *classifierJudge) judge(sf *sourceFile) {
	allowance := ""
	switch {
	case sf.rel == classifierFile:
		allowance = classifierFile
	case strings.HasPrefix(sf.rel, hostDir):
		allowance = hostDir
	}
	report := func(at ast.Node, what string) {
		if allowance != "" {
			j.found.allowed[allowance]++
			return
		}
		pos := j.fset.Position(at.Pos())
		j.found.findings = append(j.found.findings, fmt.Sprintf("%s:%d:%d: %s classifies a git "+
			"request outside auth.GitAccess — ask GitAccess; a second reading of which requests "+
			"are pushes is how a drain admits one", sf.rel, pos.Line, pos.Column, what))
	}
	untyped := func(at ast.Node, e ast.Expr) {
		if allowance != "" {
			return
		}
		pos := j.fset.Position(at.Pos())
		j.found.findings = append(j.found.findings, fmt.Sprintf("%s:%d:%d: %s may be a request's "+
			"query, and this walk cannot establish its type to say whether reading %q from it "+
			"classifies a git request. Spell the value's type where it is declared, or ask "+
			"auth.GitAccess", sf.rel, pos.Line, pos.Column, types.ExprString(e), serviceKey))
	}

	var stack []ast.Node
	ast.Inspect(sf.ast, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		var parent ast.Node
		if len(stack) > 0 {
			parent = stack[len(stack)-1]
		}
		stack = append(stack, n)

		switch x := n.(type) {
		case *ast.BasicLit:
			if v, ok := j.fold(x, sf, 0); ok && namesService(v) {
				report(x, "the literal "+x.Value)
			}
		case *ast.BinaryExpr:
			switch x.Op {
			case token.ADD:
				// The outermost concatenation only, and only where no
				// literal inside it names a service alone (that literal
				// is its own finding).
				if p, ok := parent.(*ast.BinaryExpr); ok && p.Op == token.ADD {
					break
				}
				if !j.carries(x) || literalNamesService(x) {
					break
				}
				if v, ok := j.fold(x, sf, 0); ok && namesService(v) {
					report(x, "the concatenation "+types.ExprString(x))
				}
			case token.EQL, token.NEQ:
				j.judgeOperand(x.X, sf, report)
				j.judgeOperand(x.Y, sf, report)
			}
		case *ast.SwitchStmt:
			if x.Tag != nil {
				j.judgeOperand(x.Tag, sf, report)
			}
			for _, s := range x.Body.List {
				if cc, ok := s.(*ast.CaseClause); ok {
					for _, e := range cc.List {
						j.judgeOperand(e, sf, report)
					}
				}
			}
		case *ast.CallExpr:
			j.judgeCall(x, sf, report, untyped)
		case *ast.IndexExpr:
			if j.isKey(x.Index, sf) {
				j.judgeReceiver(x, x.X, sf, urlValues, report, untyped)
			}
		}
		return true
	})
}

// judgeOperand reports a constant naming a service where it is compared,
// switched on or searched for. A literal or a concatenation is judged where it
// stands, so only a name is judged here.
func (j *classifierJudge) judgeOperand(e ast.Expr, sf *sourceFile, report func(ast.Node, string)) {
	switch ast.Unparen(e).(type) {
	case *ast.Ident, *ast.SelectorExpr, *ast.CallExpr:
		if !j.carries(e) {
			return
		}
		if v, ok := j.fold(e, sf, 0); ok && namesService(v) {
			report(e, "the constant "+types.ExprString(e)+" ("+strconv.Quote(v)+")")
		}
	}
}

// judgeCall reports a constant naming a service handed to a search, and a
// read of the service key.
func (j *classifierJudge) judgeCall(c *ast.CallExpr, sf *sourceFile,
	report func(ast.Node, string), untyped func(ast.Node, ast.Expr)) {
	sel, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr)
	if !ok {
		return
	}
	if ip, isPkg := packageNamed(sel.X, sf); isPkg {
		if slices.Contains(searchPackages, ip) {
			for _, a := range c.Args {
				j.judgeOperand(a, sf, report)
			}
		}
		// A function of a package that happens to share a method's name
		// is not a read of a query.
		return
	}
	var want decltype.TypeName
	switch sel.Sel.Name {
	case "Get", "Has":
		want = urlValues
	case "FormValue", "PostFormValue":
		want = httpRequest
	default:
		return
	}
	if len(c.Args) > 0 && j.isKey(c.Args[0], sf) {
		j.judgeReceiver(c, sel.X, sf, want, report, untyped)
	}
}

// isKey reports whether e is the service key.
func (j *classifierJudge) isKey(e ast.Expr, sf *sourceFile) bool {
	if !j.carriesKey(e) {
		return false
	}
	v, ok := j.fold(e, sf, 0)
	return ok && v == serviceKey
}

// packageNamed is the import path e names when it is the name of an import in
// sf rather than a value: an identifier the parser resolved to no local
// declaration (a package-level one beside an import of that name is a
// compile error).
func packageNamed(e ast.Expr, sf *sourceFile) (string, bool) {
	id, ok := e.(*ast.Ident)
	if !ok || id.Obj != nil {
		return "", false
	}
	return sf.importOf(id.Name)
}

// judgeReceiver reports a read of the service key from x when x is of the type
// that holds a request's query.
func (j *classifierJudge) judgeReceiver(at, x ast.Expr, sf *sourceFile, want decltype.TypeName,
	report func(ast.Node, string), untyped func(ast.Node, ast.Expr)) {
	j.found.typed++
	// The world's own reading of the file, in its package's scope; the
	// receiver is the expression at the same offsets.
	wf := j.w.FileAt(sf.path)
	var wx ast.Expr
	if wf != nil {
		wx = exprAt(wf.AST, j.w.FileSet(), j.fset.Position(x.Pos()).Offset, j.fset.Position(x.End()).Offset)
	}
	if wx == nil {
		untyped(at, x)
		return
	}
	t := j.w.TypeOf(wx, wf)
	if name, ok := j.w.Named(t); ok {
		if name == want {
			report(at, "reading "+strconv.Quote(serviceKey)+" with "+types.ExprString(at))
		}
		return
	}
	if _, unread := t.Unread(); unread || t.Known() {
		// A value only a package the walk does not read can type, or a
		// type literal (an interface, a map): the coverage boundary.
		return
	}
	untyped(at, x)
}

// exprAt is the outermost expression of f spanning exactly [off, end).
func exprAt(f *ast.File, fset *token.FileSet, off, end int) ast.Expr {
	var found ast.Expr
	ast.Inspect(f, func(n ast.Node) bool {
		if found != nil || n == nil {
			return false
		}
		e, ok := n.(ast.Expr)
		if ok && fset.Position(e.Pos()).Offset == off && fset.Position(e.End()).Offset == end {
			found = e
			return false
		}
		return true
	})
	return found
}

// fold is the string constant e is, when the walk can evaluate it: a literal, a
// concatenation, a parenthesis, a string conversion, or a constant this file,
// this package or an imported package of the module (or a package the walk
// reads) declares.
func (j *classifierJudge) fold(e ast.Expr, sf *sourceFile, depth int) (string, bool) {
	if depth > maxFold {
		return "", false
	}
	switch x := ast.Unparen(e).(type) {
	case *ast.BasicLit:
		if x.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(x.Value)
		return s, err == nil
	case *ast.BinaryExpr:
		if x.Op != token.ADD {
			return "", false
		}
		a, ok := j.fold(x.X, sf, depth+1)
		if !ok {
			return "", false
		}
		b, ok := j.fold(x.Y, sf, depth+1)
		return a + b, ok
	case *ast.CallExpr:
		// string(c) keeps c's value.
		if id, ok := ast.Unparen(x.Fun).(*ast.Ident); ok && id.Name == "string" && id.Obj == nil &&
			len(x.Args) == 1 {
			return j.fold(x.Args[0], sf, depth+1)
		}
	case *ast.Ident:
		if x.Obj != nil {
			spec, ok := x.Obj.Decl.(*ast.ValueSpec)
			if !ok || x.Obj.Kind != ast.Con {
				return "", false
			}
			return j.specValue(spec, x.Name, sf, depth+1)
		}
		// Resolved to nothing in this file: a package-level declaration of
		// another file of this package, or the universe's.
		if c, ok := j.constsOf(sf.pkg)[x.Name]; ok {
			return j.specValue(c.spec, x.Name, c.file, depth+1)
		}
	case *ast.SelectorExpr:
		ip, ok := packageNamed(x.X, sf)
		if !ok {
			return "", false
		}
		if c, ok := j.constsOf(ip)[x.Sel.Name]; ok {
			return j.specValue(c.spec, x.Sel.Name, c.file, depth+1)
		}
	}
	return "", false
}

// specValue is the value spec gives the constant name, in the file that holds
// it — its own expression, or the one it repeats from its group.
func (j *classifierJudge) specValue(spec *ast.ValueSpec, name string, sf *sourceFile, depth int) (string, bool) {
	i := slices.IndexFunc(spec.Names, func(n *ast.Ident) bool { return n.Name == name })
	values := j.valuesOf(sf.ast)[spec]
	if i < 0 || i >= len(values) {
		return "", false
	}
	return j.fold(values[i], sf, depth)
}

// valuesOf is every const spec in f mapped to the value list it declares or
// repeats: a spec with no values in a const group repeats the last one that has
// them.
func (j *classifierJudge) valuesOf(f *ast.File) map[*ast.ValueSpec][]ast.Expr {
	if m, ok := j.values[f]; ok {
		return m
	}
	m := map[*ast.ValueSpec][]ast.Expr{}
	ast.Inspect(f, func(n ast.Node) bool {
		g, ok := n.(*ast.GenDecl)
		if !ok || g.Tok != token.CONST {
			return true
		}
		var last []ast.Expr
		for _, s := range g.Specs {
			spec := s.(*ast.ValueSpec)
			if len(spec.Values) > 0 {
				last = spec.Values
			}
			m[spec] = last
		}
		return false
	})
	j.values[f] = m
	return m
}

// constsOf is every package-level constant the package at importPath declares,
// by name, as the world reads it — loading it the first time a fold asks.
func (j *classifierJudge) constsOf(importPath string) map[string]constDecl {
	if m, ok := j.pkgConsts[importPath]; ok {
		return m
	}
	m := map[string]constDecl{}
	j.pkgConsts[importPath] = m
	p := j.w.Load(importPath)
	if p == nil {
		return m
	}
	for _, f := range p.Files() {
		sf := worldFile(f)
		for _, d := range f.AST.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				spec := s.(*ast.ValueSpec)
				for _, n := range spec.Names {
					if _, dup := m[n.Name]; !dup {
						m[n.Name] = constDecl{spec: spec, file: sf}
					}
				}
			}
		}
	}
	return m
}

// namesService reports whether s spells one of git's two services.
func namesService(s string) bool {
	return slices.ContainsFunc(gitServices, func(name string) bool { return strings.Contains(s, name) })
}

// literalNamesService reports whether a string literal inside e names a service
// on its own.
func literalNamesService(e ast.Expr) bool {
	return holds(e, namesService, nil)
}

// --- the controls -------------------------------------------------------- //

// fixtureModule is the module a synthetic tree is read as.
const fixtureModule = "example.com/fixture"

// fixtureHost is a code host that spells a service, so a reading of its
// constants elsewhere has something to name.
const fixtureHost = `package host

const (
	PatternReceivePack = "POST /git/{repo}/git-receive-pack"
	ReceivePackSuffix  = "/git-receive-pack"
	Part               = "receive-pack"
)
`

// fixtureClassifier is the allowance's own file, holding a reading.
const fixtureClassifier = `package auth

import "net/http"

func GitAccess(r *http.Request) string { return r.URL.Query().Get("service") }
`

// walkFixture writes files under a fresh root and walks it.
func walkFixture(t *testing.T, files map[string]string) classifierWalk {
	t.Helper()
	root := t.TempDir()
	for name, src := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return walkForClassifiers(t, root, fixtureModule)
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN: every shape the guard flags, each
// beside the shape it must pass, each in a package of its own with no
// allowance — beside an allowed classifier and a host that spells a service —
// and the whole tree walked ONCE, as the module is, so the prefilter's fixed
// point is exercised across packages rather than one case at a time.
func TestTheClassifierWalkFindsAGitLiteral(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, src string
		want      bool
	}{
		{"a literal", `package x; var _ = "git-receive-pack"`, true},
		{"a route", `package x; var _ = "/git/{repo}/git-upload-pack"`, true},
		{"a coding box's route", `package x; var _ = "/box/git/{repo}/git-receive-pack"`, true},
		{"a raw literal", "package x; var _ = `git-receive-pack`", true},
		{"an escaped literal", `package x; var _ = "git-receive-p\x61ck"`, true},
		{"an octal escape", `package x; var _ = "git-upload-p\141ck"`, true},
		{"a concatenation", `package x; var _ = "git-" + "receive-pack"`, true},
		{"a concatenation of a constant", `package x; const svc = "receive-pack"; var _ = "git-" + svc`, true},
		{"a local constant", `package x; func f() string { const svc = "upload-pack"; return "git-" + svc }`, true},
		{"a repeated constant", `package x; const ( first = "receive-pack"; second ); var _ = "git-" + second`, true},
		{"a constant of another file", `package x; var _ = "git-" + otherFilePiece`, true},
		{"a constant of another package, compared", `package x
			import "example.com/fixture/internal/code/host"
			func f(p string) bool { return p == host.ReceivePackSuffix }`, true},
		{"a constant of another package, searched for", `package x
			import ("strings"; "example.com/fixture/internal/code/host")
			func f(p string) bool { return strings.HasSuffix(p, host.ReceivePackSuffix) }`, true},
		{"a constant of another package, in a case", `package x
			import "example.com/fixture/internal/code/host"
			func f(p string) bool { switch p { case host.PatternReceivePack: return true }; return false }`, true},
		{"a constant of another package, concatenated", `package x
			import "example.com/fixture/internal/code/host"
			var _ = "git-" + host.Part`, true},
		{"a constant declared from another package's, compared", `package x
			import "example.com/fixture/internal/code/host"
			const mine = host.ReceivePackSuffix
			func f(p string) bool { return p == mine }`, true},
		{"a constant two hops from the host, compared", `package x
			import "example.com/fixture/internal/relay"
			func f(p string) bool { return p == relay.Suffix }`, true},
		{"the query's service", `package x
			import "net/http"
			func f(r *http.Request) string { return r.URL.Query().Get("service") }`, true},
		{"the query held in a variable", `package x
			import "net/http"
			func f(r *http.Request) bool { q := r.URL.Query(); return q.Has("service") }`, true},
		{"the key as a constant", `package x
			import "net/http"
			const key = "service"
			func f(r *http.Request) string { return r.URL.Query().Get(key) }`, true},
		{"the key spelled with an escape", `package x
			import "net/http"
			func f(r *http.Request) string { return r.URL.Query().Get("servic\x65") }`, true},
		{"the query indexed", `package x
			import "net/http"
			func f(r *http.Request) []string { return r.URL.Query()["service"] }`, true},
		{"a parsed query", `package x
			import "net/url"
			func f(raw string) string { q, _ := url.ParseQuery(raw); return q.Get("service") }`, true},
		{"a url.Values parameter", `package x
			import "net/url"
			func f(q url.Values) string { return q.Get("service") }`, true},
		{"the request's form", `package x
			import "net/http"
			func f(r *http.Request) string { return r.FormValue("service") }`, true},
		{"the request's parsed form", `package x
			import "net/http"
			func f(r *http.Request) string { return r.Form.Get("service") }`, true},
		{"a receiver the walk cannot type", `package x
			func f() string { return query().Get("service") }`, true},

		// What it passes.
		{"receive-pack alone", `package x; var _ = "receive-pack"`, false},
		{"a mounted pattern", `package x
			import "example.com/fixture/internal/code/host"
			type mux interface{ Handle(string, any) }
			func f(m mux) { m.Handle(host.PatternReceivePack, nil) }`, false},
		{"a header named service", `package x
			import "net/http"
			func f(r *http.Request) string { return r.Header.Get("service") }`, false},
		{"a map keyed service", `package x
			func f(m map[string]string) string { return m["service"] }`, false},
		{"another key of the query", `package x
			import "net/http"
			func f(r *http.Request) string { return r.URL.Query().Get("services") }`, false},
		{"a comparison of something else", `package x
			import "example.com/fixture/internal/code/host"
			func f(p string) bool { return p == host.Part }`, false},
		{"a log key", `package x
			import "log/slog"
			func f() { slog.Info("started", "service", "crewlet") }`, false},
		// The coverage boundary, held here so that moving it is a decision:
		// a name split inside a piece has no piece to be found by.
		{"a name split inside its piece", `package x; var _ = "git-receive-pa" + "ck"`, false},
	}
	files := map[string]string{
		"internal/code/host/host.go": fixtureHost,
		classifierFile:               fixtureClassifier,
		// A package declaring a constant from the host's, which another
		// package compares against: the fixed point's second hop.
		"internal/relay/relay.go": `package relay
			import "example.com/fixture/internal/code/host"
			const Suffix = host.ReceivePackSuffix`,
		// A package spelling nothing the walk looks for, which it must
		// read past rather than parse.
		"internal/inert/inert.go": "package inert\n\nfunc F() int { return 1 }\n",
	}
	for i, tc := range cases {
		dir := fmt.Sprintf("internal/case%02d/", i)
		files[dir+"x.go"] = tc.src
		// Every case's package has a second file, which declares the
		// constant "a constant of another file" reads and spells nothing
		// else: a file the prefilter admits only by that name.
		files[dir+"other.go"] = "package x\n\nconst otherFilePiece = \"receive-pack\"\n"
	}
	found := walkFixture(t, files)
	for i, tc := range cases {
		prefix := fmt.Sprintf("internal/case%02d/", i)
		hits := slices.ContainsFunc(found.findings, func(f string) bool { return strings.HasPrefix(f, prefix) })
		if hits != tc.want {
			t.Errorf("%s: found a reading %v, want %v: %q", tc.name, hits, tc.want, tc.src)
		}
	}
	for _, f := range found.findings {
		if !strings.HasPrefix(f, "internal/case") {
			t.Errorf("a finding outside the cases: %s", f)
		}
	}
	if found.allowed[classifierFile] == 0 || found.allowed[hostDir] == 0 {
		t.Errorf("an allowance's reading went uncounted: %v", found.allowed)
	}
	if found.parsed == found.read {
		t.Errorf("parsed all %d files: the prefilter admitted everything, so this tree "+
			"does not exercise it", found.read)
	}
}

// THE TWO-SIDED ALLOWANCE, and what the walk never reads.
func TestTheClassifierAllowancesAreLive(t *testing.T) {
	t.Parallel()
	// Neither allowance holds a reading: the classifier's is stale (or the
	// matcher inert), and so is the host's once it holds a file.
	found := walkFixture(t, map[string]string{
		classifierFile:               "package auth\n",
		"internal/code/host/host.go": "package host\n",
	})
	if v := found.verdict(); len(v) != 2 {
		t.Errorf("two allowances holding nothing gave %d failure(s), want 2: %v", len(v), v)
	}
	// The host need not exist yet; when it does not, its allowance asks
	// nothing.
	found = walkFixture(t, map[string]string{classifierFile: fixtureClassifier})
	if v := found.verdict(); len(v) != 0 {
		t.Errorf("a tree with no host failed: %v", v)
	}
	// A test file and testdata are not compiled into the engine, and are not
	// read; a file anywhere else in the tree is, internal/ or not.
	found = walkFixture(t, map[string]string{
		classifierFile:             fixtureClassifier,
		"internal/x/x_test.go":     `package x; var _ = "git-receive-pack"`,
		"internal/x/testdata/t.go": `package t; var _ = "git-receive-pack"`,
		"internal/x/x.go":          "package x\n",
		"tools/y/y.go":             `package y; var _ = "git-upload-pack"`,
	})
	if len(found.findings) != 1 || !strings.HasPrefix(found.findings[0], "tools/y/y.go:") {
		t.Errorf("findings = %v, want tools/y/y.go's alone", found.findings)
	}
}
