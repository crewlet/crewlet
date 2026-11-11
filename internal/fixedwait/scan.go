package fixedwait

import (
	"bytes"
	"cmp"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// waits is the six functions of package time whose reference this walk
// counts: each one makes the code around it wait for the wall clock.
var waits = map[string]bool{
	"Sleep":     true,
	"After":     true,
	"AfterFunc": true,
	"NewTimer":  true,
	"NewTicker": true,
	"Tick":      true,
}

// elapsed is the two functions of package time that read how much wall-clock
// time lies between now and an instant: a claim held to their answer is a
// positive deadline whose clock is read rather than waited on.
var elapsed = map[string]bool{
	"Since": true,
	"Until": true,
}

// instantMethods is the methods of time.Time this walk counts by their
// receiver's type, each a comparison of two instants: Sub reads the wall
// clock between them as Since reads it between one and now, and Before,
// After and Compare decide on that same span — time.Now().Before(deadline)
// is time.Until(deadline) > 0, and t.Compare(d) < 0 and !t.After(d) are the
// claim t.Before(d) is. Their names alone are every other type's too (a
// big.Int's Sub, a set's, a version's Compare, a page's After), and their
// shape is any call, so each is told apart by type and nothing else
// ([timeMethod]). Equal is not one: equality decides nothing about elapsed
// time.
var instantMethods = map[string]bool{
	"Sub":     true,
	"Before":  true,
	"After":   true,
	"Compare": true,
}

// timeMethod is how [types.Func.FullName] names the time.Time method of the
// given name, (time.Time).Sub for Sub: what tells it from every other type's
// method of that name.
func timeMethod(name string) string { return "(time.Time)." + name }

// methodPrefix opens how [Ref.Name] names a reference to one of
// [instantMethods]: Time.Sub, which the listing prints as time.Time.Sub.
const methodPrefix = "Time."

// markerPrefix opens every marker, written as a directive is: no space after
// the slashes, so prose that mentions a marker is never one.
const markerPrefix = "//fixedwait:"

// Kind is what a marker says about the wait under it.
type Kind string

const (
	// Liveness says the wait only bounds how long the test waits for something
	// that must happen.
	Liveness Kind = "liveness"
	// Simulated says the wait plays the world's latency inside a fake, and the
	// marker says what it stands in for.
	Simulated Kind = "simulated"
	// Bench says the wait is a benchmark's own work.
	Bench Kind = "bench"
	// Measure says a reading of elapsed time decides nothing — it is logged or
	// reported, or it is a poll's own deadline — and the marker says why.
	// It covers any elapsed reading, and no fixed wait.
	Measure Kind = "measure"
	// Stated says a comparison of two instants — a time.Time's Sub, Before,
	// After or Compare — reads no wall clock between them: the test stated
	// both, a clock the test controls stamped them, or the code under test
	// derived one from the other by its own arithmetic, computed once. So
	// whatever it decides, it decides exactly, and the marker says which
	// instants, which clock or which derivation. It covers those four alone:
	// time.Since and time.Until read the wall clock at the call, so nothing
	// about them is stated.
	Stated Kind = "stated"
	// Bubble declares a bubble-only helper, on a function's doc only.
	Bubble Kind = "bubble"
)

// Valid reports whether k is a kind a marker may carry.
func (k Kind) Valid() bool {
	switch k {
	case Liveness, Simulated, Bench, Measure, Stated, Bubble:
		return true
	}
	return false
}

// shape is which of the class's references a reference is, as a set of
// bits so a marker can cover more than one: a fixed wait, a reading of the
// wall clock against now (time.Since, time.Until), or a comparison of two
// instants (a time.Time's Sub, Before, After or Compare), whichever clock
// stamped them.
type shape uint8

const (
	waitShape shape = 1 << iota
	clockShape
	instantsShape
)

// shapeOf is the shape of a reference by what of package time it names, as
// [Ref.Name] spells it.
func shapeOf(name string) shape {
	if method, ok := strings.CutPrefix(name, methodPrefix); ok && instantMethods[method] {
		return instantsShape
	}
	if elapsed[name] {
		return clockShape
	}
	return waitShape
}

// covers is the shapes a marker of kind k takes out of the class: measure
// any elapsed reading, stated a comparison of two instants alone, and every
// other kind a fixed wait. A wait is not a measurement nor a measurement a
// wait, and a reading of now is never stated.
func (k Kind) covers() shape {
	switch k {
	case Measure:
		return clockShape | instantsShape
	case Stated:
		return instantsShape
	}
	return waitShape
}

// Ref is one counted reference in test code that no bubble, bubble-only
// helper or marker takes out of the class: a wait on the wall clock, or a
// reading of elapsed time.
type Ref struct {
	// File is the path from the walk's root, slash-separated.
	File string
	Line int
	// Func is the enclosing top-level declaration: a function's name, a
	// method's as Type.Method, a package-level variable's own name.
	Func string
	// Name is what of package time it names: Sleep for time.Sleep, Since for
	// time.Since, and Time.Sub, Time.Before, Time.After or Time.Compare for a
	// time.Time's method.
	Name string
}

// Elapsed reports whether r reads elapsed time rather than waiting for it:
// time.Since, time.Until, or a time.Time's Sub, Before, After or Compare.
func (r Ref) Elapsed() bool { return shapeOf(r.Name) != waitShape }

// String is the reference as the gate lists it, one per line:
// path:line: function: time.X.
func (r Ref) String() string {
	return fmt.Sprintf("%s:%d: %s: time.%s", r.File, r.Line, r.Func, r.Name)
}

// Tree is what [Scan] read.
type Tree struct {
	// Refs is every counted reference, in the walk's order.
	Refs []Ref
	// Findings are the walk's own refusals: a malformed or stale marker, a
	// bubble declaration out of place, a bubble-only helper used outside a
	// bubble.
	Findings []Finding
	// Decls is, for every test-code file read, the top-level declarations
	// it holds, by the names [Ref.Func] gives them: what an inventory entry
	// naming a function that is gone is told apart by. It is nil for a file
	// importing neither time nor synctest and spelling none of
	// [instantMethods], which was not parsed because it can hold no
	// reference.
	Decls map[string]map[string]bool
	// TestFiles counts the test-code files read, so a walk that read
	// nothing cannot pass.
	TestFiles int
}

// source is one Go file the walk keeps.
type source struct {
	rel, dir string
	test     bool
	src      []byte
	file     *ast.File
}

// pkgDecls is what one directory's files declare that the walk resolves by
// name: its bubble-only helpers and its bubble runners.
type pkgDecls struct {
	// helpers maps a bubble-only helper's name to whether it is a method.
	helpers map[string]bool
	// helperDecls are the helpers' declarations, for their positions.
	helperDecls []helperDecl
	// runners maps a bubble runner to the indices of the parameters it
	// hands to synctest whole.
	runners map[runnerKey][]int
}

// runnerKey names a bubble runner by its package clause as well as its own
// name: a directory holds a package and its external test package, and each
// may declare a function of the same name that does something else.
type runnerKey struct{ pkg, name string }

type helperDecl struct {
	src  *source
	decl *ast.FuncDecl
}

// Scan walks the Go module rooted at root, which must hold its go.mod, and
// reports every counted reference and every finding of the walk's own rules.
// The inventory is judged separately, by [Check].
func Scan(root string) (*Tree, error) {
	module, err := modulePath(root)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	var files []*source
	err = sourcetree.Walk(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == "testdata":
			// NOT THE MODULE'S CODE: nothing compiles it, and this
			// package's own fixtures live there.
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(p, ".go"):
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		dir := path.Dir(rel)
		files = append(files, &source{
			rel:  rel,
			dir:  dir,
			test: strings.HasSuffix(rel, "_test.go") || strings.HasSuffix(path.Base(dir), "test"),
			src:  src,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}

	tree := &Tree{Decls: map[string]map[string]bool{}}
	for _, s := range files {
		if s.test {
			tree.TestFiles++
			// Read, and until it is parsed known to declare nothing the
			// walk keys a reference by: it imports neither time nor
			// synctest, so it holds no reference.
			tree.Decls[s.rel] = nil
		}
	}
	parse := func(s *source) error {
		if s.file != nil {
			return nil
		}
		f, err := parser.ParseFile(fset, s.rel, s.src, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return fmt.Errorf("fixedwait: parse %s: %w", s.rel, err)
		}
		s.file = f
		return nil
	}

	// PASS ONE: every file that can hold a counted reference, a marker or a
	// bubble. A file is parsed in full when it imports time or
	// testing/synctest — read from its imports, never its bytes, because an
	// import path is a string a literal may spell with escapes — or when
	// its bytes spell a marker, which a comment cannot escape.
	for _, s := range files {
		marked := bytes.Contains(s.src, []byte(markerPrefix))
		if !s.test && !marked {
			continue
		}
		if !marked {
			imports, err := parser.ParseFile(fset, s.rel, s.src, parser.ImportsOnly)
			if err != nil {
				return nil, fmt.Errorf("fixedwait: parse %s: %w", s.rel, err)
			}
			if importName(imports, "time") == "" && importName(imports, "testing/synctest") == "" {
				continue
			}
		}
		if err := parse(s); err != nil {
			return nil, err
		}
	}

	// What each package declares: bubble-only helpers (only in test code,
	// where a marker means something) and bubble runners.
	pkgs := map[string]*pkgDecls{}
	pkg := func(dir string) *pkgDecls {
		p := pkgs[dir]
		if p == nil {
			p = &pkgDecls{helpers: map[string]bool{}, runners: map[runnerKey][]int{}}
			pkgs[dir] = p
		}
		return p
	}
	for _, s := range files {
		if s.file == nil || !s.test {
			continue
		}
		syncName := importName(s.file, "testing/synctest")
		for _, d := range s.file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if isHelper(fn) {
				p := pkg(s.dir)
				p.helpers[fn.Name.Name] = fn.Recv != nil
				p.helperDecls = append(p.helperDecls, helperDecl{s, fn})
			}
			if params := runnerParams(fn, syncName); len(params) > 0 {
				pkg(s.dir).runners[runnerKey{s.file.Name.Name, fn.Name.Name}] = params
			}
		}
	}

	// PASS TWO: a file the first pass left that can name a helper — one in
	// a helper's own directory spelling its name, or one importing a
	// test-support package that declares an exported one. A name in Go
	// source is its bytes, so the prefilter is exact. Production code is
	// read too: it can import a test-support package, and a helper it names
	// runs on the machine's clock as surely as one a test names.
	for _, s := range files {
		if s.file != nil {
			continue
		}
		if mayReferHelper(s, pkgs, module, fset) {
			if err := parse(s); err != nil {
				return nil, err
			}
		}
	}

	// THE TYPED PASS: a time.Time's Sub, Before, After and Compare are told
	// from every other method of their names by the receiver's type, so the
	// packages whose test code may hold one are type-checked, and what that
	// parses is parsed here.
	typed, untyped, typeErr := typedReadings(root, fset, files, parse)
	if typeErr != nil {
		return nil, typeErr
	}
	tree.Findings = append(tree.Findings, untyped...)

	for _, s := range files {
		if s.file == nil {
			continue
		}
		scanFile(fset, s, pkgs, typed[s], tree)
	}
	for dir, p := range pkgs {
		for _, h := range p.helperDecls {
			tree.Findings = append(tree.Findings,
				verifyHelper(fset, dir, h, files, module, pkgs)...)
			if !holdsWait(h, pkgs, module, typed[h.src]) {
				tree.Findings = append(tree.Findings, Finding{
					Rule: StaleMarker, File: h.src.rel, Line: fset.Position(bubbleMarker(h.decl).Pos()).Line,
					Func: declName(h.decl),
					Detail: "this bubble-only helper holds no wait, reads no elapsed time and calls " +
						"no other bubble-only helper, so the declaration takes nothing out of the " +
						"class and only binds its callers: remove it",
				})
			}
		}
	}
	sortFindings(tree.Findings)
	return tree, nil
}

// modulePath reads the module's path from root's go.mod, which names the
// import path a test-support package's helpers are reached by.
func modulePath(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("fixedwait: the walk starts at a module's root, "+
			"and %s holds no readable go.mod: %w", root, err)
	}
	for line := range strings.Lines(string(data)) {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			name := strings.TrimSpace(rest)
			if unq, err := strconv.Unquote(name); err == nil {
				name = unq
			}
			return name, nil
		}
	}
	return "", fmt.Errorf("fixedwait: %s/go.mod declares no module", root)
}

// importName is the name f refers to the package at importPath by: its own
// name's last element, an alias, "." for a dot import, or "" when f does not
// import it (or imports it as _, which names nothing).
func importName(f *ast.File, importPath string) string {
	for _, spec := range f.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != importPath {
			continue
		}
		if spec.Name != nil {
			if spec.Name.Name == "_" {
				return ""
			}
			return spec.Name.Name
		}
		return path.Base(p)
	}
	return ""
}

// bubbleMarker is the comment of fn's own doc that declares it bubble-only,
// or nil.
func bubbleMarker(fn *ast.FuncDecl) *ast.Comment {
	if fn.Doc == nil {
		return nil
	}
	for _, c := range fn.Doc.List {
		if kind, _, ok := parseMarker(c.Text); ok && kind == Bubble {
			return c
		}
	}
	return nil
}

// isHelper reports whether fn is a bubble-only helper the walk can hold to
// its declaration: declared so on its doc, and not a method with an
// exported name. Such a method is reached where nothing names its package —
// through an interface, an embedding, or a value another package hands
// back — so a use found by name would miss it, and its declaration is
// refused (see [unverifiable]) and its waits are counted.
func isHelper(fn *ast.FuncDecl) bool {
	return bubbleMarker(fn) != nil && !unverifiable(fn)
}

// unverifiable reports whether fn is a method with an exported name.
func unverifiable(fn *ast.FuncDecl) bool {
	return fn.Recv != nil && token.IsExported(fn.Name.Name)
}

// holdsWait reports whether a bubble-only helper's body holds a counted
// reference of its own — a wait, or a reading of elapsed time, of which typed
// is its file's typed comparisons of instants — or refers to another
// bubble-only helper: what its declaration takes out of the class. One
// holding neither binds every caller for nothing.
func holdsWait(h helperDecl, pkgs map[string]*pkgDecls, module string, typed []candidate) bool {
	f, fn := h.src.file, h.decl
	if timeName := importName(f, "time"); timeName != "" && len(timeRefs(fn, timeName)) > 0 {
		return true
	}
	if slices.ContainsFunc(typed, func(c candidate) bool { return fn.Pos() <= c.pos && c.pos < fn.End() }) {
		return true
	}
	// The other helpers it can name: its own package's by name, another
	// package's exported ones through this file's name for that package.
	quals := map[string]map[string]bool{}
	for dir, p := range pkgs {
		if dir == h.src.dir {
			continue
		}
		if q := importName(f, module+"/"+dir); q != "" {
			quals[q] = p.helpers
		}
	}
	own := pkgs[h.src.dir].helpers
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if method, ok := own[n.Sel.Name]; ok && method {
				found = true
			}
			if x, ok := n.X.(*ast.Ident); ok {
				if method, ok := quals[x.Name][n.Sel.Name]; ok && !method {
					found = true
				}
			}
		case *ast.Ident:
			if method, ok := own[n.Name]; ok && !method {
				found = true
			}
			if method, ok := quals["."][n.Name]; ok && !method {
				found = true
			}
		}
		return !found
	})
	return found
}

// parseMarker splits a comment into a marker's kind and the rest of its
// line; ok is false for a comment that is not a marker.
func parseMarker(text string) (kind Kind, rest string, ok bool) {
	body, ok := strings.CutPrefix(text, markerPrefix)
	if !ok {
		return "", "", false
	}
	word, rest, _ := strings.Cut(body, " ")
	return Kind(word), strings.TrimSpace(rest), true
}

// runnerParams is, for a top-level function that hands one or more of its
// own function parameters to synctest whole, the indices of those
// parameters: a function literal handed to it in that position runs inside
// a bubble as surely as one handed to synctest itself. A parameter the body
// uses in any other way (calls it directly, stores it, passes it elsewhere)
// is not one, since the literal might then run outside the bubble.
func runnerParams(fn *ast.FuncDecl, syncName string) []int {
	if syncName == "" || fn.Recv != nil || fn.Body == nil {
		return nil
	}
	var names []*ast.Ident
	for _, field := range fn.Type.Params.List {
		if len(field.Names) == 0 {
			names = append(names, nil)
		}
		names = append(names, field.Names...)
	}
	handed := map[*ast.Ident]bool{}
	uses := map[string]int{}
	local := localNames(fn)
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if isSynctestCall(n, syncName, local) && len(n.Args) == 2 {
				if id, ok := n.Args[1].(*ast.Ident); ok {
					handed[id] = true
				}
			}
		case *ast.Ident:
			uses[n.Name]++
		}
		return true
	})
	handedBy := map[string]int{}
	for id := range handed {
		handedBy[id.Name]++
	}
	var out []int
	for i, n := range names {
		if n == nil || n.Name == "_" {
			continue
		}
		if handedBy[n.Name] > 0 && handedBy[n.Name] == uses[n.Name] {
			out = append(out, i)
		}
	}
	return out
}

// isSynctestCall reports whether call is synctest.Test or synctest.Run
// through the file's own name for testing/synctest, in a declaration whose
// own names are local: a local of that name is something else.
func isSynctestCall(call *ast.CallExpr, syncName string, local map[string]bool) bool {
	switch fun := call.Fun.(type) {
	case *ast.SelectorExpr:
		x, ok := fun.X.(*ast.Ident)
		return ok && syncName != "" && syncName != "." && x.Name == syncName && !local[x.Name] &&
			(fun.Sel.Name == "Test" || fun.Sel.Name == "Run")
	case *ast.Ident:
		return syncName == "." && !local[fun.Name] && (fun.Name == "Test" || fun.Name == "Run")
	}
	return false
}

// localNames is every name declared inside the top-level declaration d —
// a parameter, a result, a receiver, a type parameter, a local variable,
// constant, type or function literal's parameter — leaving out the names d
// itself declares at package scope. A call through one of these names is
// not to the package-level function or package of that name, whatever the
// call's position in its scope: one set per declaration rather than per
// scope, because the conservative answer only counts a wait that a finer
// one might have bubbled, never the reverse.
func localNames(d ast.Decl) map[string]bool {
	out := map[string]bool{}
	top := map[*ast.Ident]bool{}
	switch d := d.(type) {
	case *ast.FuncDecl:
		top[d.Name] = true
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch spec := spec.(type) {
			case *ast.ValueSpec:
				for _, id := range spec.Names {
					top[id] = true
				}
			case *ast.TypeSpec:
				top[spec.Name] = true
			}
		}
	}
	add := func(ids ...*ast.Ident) {
		for _, id := range ids {
			if id != nil && !top[id] {
				out[id.Name] = true
			}
		}
	}
	ast.Inspect(d, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.Field:
			add(n.Names...)
		case *ast.ValueSpec:
			add(n.Names...)
		case *ast.TypeSpec:
			add(n.Name)
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, e := range n.Lhs {
					if id, ok := e.(*ast.Ident); ok {
						add(id)
					}
				}
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if id, ok := e.(*ast.Ident); ok {
						add(id)
					}
				}
			}
		}
		return true
	})
	return out
}

// mayReferHelper reports whether a file the first pass left could refer to
// a bubble-only helper, so that the helper's verification reads it.
func mayReferHelper(s *source, pkgs map[string]*pkgDecls, module string, fset *token.FileSet) bool {
	if p := pkgs[s.dir]; p != nil && len(p.helpers) > 0 && spellsAny(s.src, p.helpers) {
		return true
	}
	var imports *ast.File
	for dir, p := range pkgs {
		if dir == s.dir || !spellsAny(s.src, p.helpers) {
			continue
		}
		if imports == nil {
			f, err := parser.ParseFile(fset, s.rel, s.src, parser.ImportsOnly)
			if err != nil {
				// Parsed in full, the same error is reported there.
				return true
			}
			imports = f
		}
		if importName(imports, module+"/"+dir) != "" {
			return true
		}
	}
	return false
}

func spellsAny(src []byte, names map[string]bool) bool {
	for name := range names {
		if bytes.Contains(src, []byte(name)) {
			return true
		}
	}
	return false
}

// span is a half-open range of positions inside which a wait is bubbled.
type span struct{ from, to token.Pos }

func inSpans(spans []span, pos token.Pos) bool {
	return slices.ContainsFunc(spans, func(s span) bool { return s.from <= pos && pos < s.to })
}

// bubbles reads what of f runs in a synctest bubble. spans is every range
// whose code does: the function literal handed to synctest.Test or
// synctest.Run, or to one of its own package's bubble runners in the
// runner's position; and the body of a bubble-only helper declared in f.
// handed is every expression synctest or a runner is handed in that
// position, which is what the bubble runs.
//
// A runner is its own package clause's — a directory's external test
// package may declare a function of the same name that runs its parameter
// on the machine's clock — and a call through a name the enclosing
// declaration declares locally is to that local, never to the runner or to
// package synctest ([localNames]).
func bubbles(f *ast.File, syncName string, p *pkgDecls) (spans []span, handed map[ast.Node]bool) {
	handed = map[ast.Node]bool{}
	for _, d := range f.Decls {
		local := localNames(d)
		ast.Inspect(d, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			var at []int
			switch {
			case isSynctestCall(call, syncName, local):
				at = []int{len(call.Args) - 1}
			case p != nil:
				if id, ok := call.Fun.(*ast.Ident); ok && !local[id.Name] {
					at = p.runners[runnerKey{f.Name.Name, id.Name}]
				}
			}
			for _, i := range at {
				if i < 0 || i >= len(call.Args) {
					continue
				}
				handed[call.Args[i]] = true
				if lit, ok := call.Args[i].(*ast.FuncLit); ok {
					spans = append(spans, span{lit.Pos(), lit.End()})
				}
			}
			return true
		})
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil && isHelper(fn) {
			spans = append(spans, span{fn.Pos(), fn.End()})
		}
	}
	return spans, handed
}

// declName is the name a top-level declaration's references are keyed by.
func declName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	for {
		switch x := t.(type) {
		case *ast.StarExpr:
			t = x.X
			continue
		case *ast.IndexExpr:
			t = x.X
			continue
		case *ast.IndexListExpr:
			t = x.X
			continue
		case *ast.ParenExpr:
			t = x.X
			continue
		case *ast.Ident:
			return x.Name + "." + fn.Name.Name
		}
		return fn.Name.Name
	}
}

// candidate is a reference before the markers are read.
type candidate struct {
	pos  token.Pos
	line int
	fn   string
	name string
}

// scanFile reads one parsed file: its references, its markers and its
// bubble declarations. typed is the file's references to a time.Time's
// method of [instantMethods], read from its types ([typedReadings]).
func scanFile(fset *token.FileSet, s *source, pkgs map[string]*pkgDecls, typed []candidate, tree *Tree) {
	f := s.file
	line := func(pos token.Pos) int { return fset.Position(pos).Line }

	var cands []candidate
	if s.test {
		decls := map[string]bool{}
		tree.Decls[s.rel] = decls
		timeName := importName(f, "time")
		spans, _ := bubbles(f, importName(f, "testing/synctest"), pkgs[s.dir])
		for _, d := range f.Decls {
			for _, name := range topNames(d) {
				decls[name] = true
			}
			var refs []candidate
			if timeName != "" {
				refs = timeRefs(d, timeName)
			}
			for _, c := range typed {
				if d.Pos() <= c.pos && c.pos < d.End() {
					refs = append(refs, c)
				}
			}
			for _, r := range refs {
				if inSpans(spans, r.pos) {
					continue
				}
				r.line = line(r.pos)
				r.fn = enclosing(d, r.pos)
				cands = append(cands, r)
			}
		}
		slices.SortFunc(cands, func(a, b candidate) int { return cmp.Compare(a.pos, b.pos) })
	}

	// The markers. Each covers the counted references of ONE line, which its
	// place decides: a marker that trails code covers that code's line, and
	// a marker alone on its line covers the line under it. Never both, so a
	// marker written after a liveness timer does not also take the wait on
	// the next line out of the class, unseen by whoever reads the diff. One
	// that covers no counted reference is stale.
	//
	// AND EACH COVERS ITS OWN SHAPE ([Kind.covers]): measure a reading of
	// elapsed time, stated a comparison of two instants (a time.Time's Sub,
	// Before, After or Compare), every other kind a fixed wait.
	// A wait is not a measurement nor a measurement a wait, and a reading of
	// now is never stated, so a marker over only what it does not cover is
	// refused rather than taken as stale or, worse, as covering it. A line
	// holding a wait and an elapsed reading takes one marker for each, one
	// alone above it and one trailing it: two markers alone on consecutive
	// lines would put the second under the first, which covers no reference.
	type marker struct {
		line, target int
		kind         Kind
	}
	var markers []marker
	docs := map[*ast.CommentGroup]*ast.FuncDecl{}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Doc != nil {
			docs[fn.Doc] = fn
		}
	}
	for _, g := range f.Comments {
		for _, c := range g.List {
			kind, rest, ok := parseMarker(c.Text)
			if !ok {
				continue
			}
			at := line(c.Pos())
			switch {
			case kind == Bubble:
				switch {
				case docs[g] == nil:
					tree.Findings = append(tree.Findings, Finding{
						Rule: MisplacedBubble, File: s.rel, Line: at,
						Detail: "a bubble-only declaration belongs in a function's doc " +
							"comment, where it names the function every caller must bubble",
					})
				case unverifiable(docs[g]):
					tree.Findings = append(tree.Findings, Finding{
						Rule: MisplacedBubble, File: s.rel, Line: at,
						Detail: "a method with an exported name is reached where nothing names " +
							"its package — through an interface, an embedding or a value another " +
							"package hands back — so no reading of the source finds every call " +
							"to hold to a bubble: unexport the method, or hand the bubble its " +
							"waits as a function literal",
					})
				case !s.test:
					tree.Findings = append(tree.Findings, Finding{
						Rule: MisplacedBubble, File: s.rel, Line: at,
						Detail: "this file is not test code, so no wait in it is counted " +
							"and a bubble-only declaration takes nothing out of the class",
					})
				}
			case !kind.Valid():
				tree.Findings = append(tree.Findings, Finding{
					Rule: BadMarker, File: s.rel, Line: at,
					Detail: fmt.Sprintf("%q is not a marker's kind: a fixed wait's is liveness, "+
						"simulated <what it stands in for> or bench, a measured elapsed "+
						"time's is measure <why it decides nothing>, and a time.Time's Sub, "+
						"Before, After or Compare between instants no wall clock separates "+
						"is stated <which instants or clock>", string(kind)),
				})
			case kind == Simulated && rest == "":
				tree.Findings = append(tree.Findings, Finding{
					Rule: BadMarker, File: s.rel, Line: at,
					Detail: "a simulated wait says what it stands in for " +
						"(//fixedwait:simulated a slow peer's answer): it is where " +
						"a deciding wait would hide",
				})
			case kind == Measure && rest == "":
				tree.Findings = append(tree.Findings, Finding{
					Rule: BadMarker, File: s.rel, Line: at,
					Detail: "a measure marker says why the elapsed time it covers decides " +
						"nothing (//fixedwait:measure logged for the run's report): it is " +
						"where a deadline held to a loaded runner's clock would hide",
				})
			case kind == Stated && rest == "":
				tree.Findings = append(tree.Findings, Finding{
					Rule: BadMarker, File: s.rel, Line: at,
					Detail: "a stated marker says which instants or which clock its comparison " +
						"reads — the case's own constants, a clock the case controls, or one " +
						"instant the code derives from the other, computed once " +
						"(//fixedwait:stated both on the case's stepped clock): it is where a " +
						"comparison of instants the code stamped from the wall clock would hide",
				})
			case kind == Bench && !inBenchmark(f, c.Pos()):
				tree.Findings = append(tree.Findings, Finding{
					Rule: BadMarker, File: s.rel, Line: at,
					Detail: "a bench marker says the wait is a benchmark's own work, so it " +
						"sits in a function that takes a *testing.B; in a test it would take " +
						"a deciding wait out of the class for free",
				})
			default:
				target := at + 1
				if trailsCode(s.src, fset.Position(c.Pos())) {
					target = at
				}
				markers = append(markers, marker{at, target, kind})
			}
		}
	}
	// By line, the shapes a marker on it may cover, the shapes its counted
	// references are, and the shapes a marker did cover.
	marked, held, covered := map[int]shape{}, map[int]shape{}, map[int]shape{}
	for _, m := range markers {
		marked[m.target] |= m.kind.covers()
	}
	for _, c := range cands {
		sh := shapeOf(c.name)
		held[c.line] |= sh
		if marked[c.line]&sh != 0 {
			covered[c.line] |= sh
			continue
		}
		tree.Refs = append(tree.Refs, Ref{File: s.rel, Line: c.line, Func: c.fn, Name: c.name})
	}
	for _, m := range markers {
		if covered[m.target]&m.kind.covers() != 0 {
			continue
		}
		rule, detail := StaleMarker, "no counted reference of its kind sits on the line this "+
			"marker covers — its own line when it trails code, the line under it when it "+
			"stands alone — so it marks nothing: remove it, or move it onto the "+
			"reference it means"
		other := held[m.target]
		switch {
		case !s.test:
			detail = "this file is not test code, so nothing in it is counted and " +
				"the marker marks nothing: remove it"
		case other == 0:
		case m.kind == Measure:
			rule, detail = BadMarker, "a measure marker is a measured elapsed time's, and the "+
				"line it covers holds a fixed wait and no elapsed reading: a fixed wait that "+
				"decides nothing is liveness, simulated <what it stands in for> or bench"
		case m.kind == Stated && other&clockShape != 0:
			rule, detail = BadMarker, "a stated marker covers a comparison of two instants — a "+
				"time.Time's Sub, Before, After or Compare — and the line it covers reads the "+
				"wall clock through time.Since or time.Until and holds no such comparison: each "+
				"reads now at the call, so nothing about it is stated — decide its claim by a "+
				"signal, or mark it //fixedwait:measure <why it decides nothing>"
		case m.kind == Stated:
			rule, detail = BadMarker, "a stated marker covers a comparison of two instants, and "+
				"the line it covers holds a fixed wait and no Sub, Before, After or Compare: a "+
				"fixed wait that decides nothing is liveness, simulated <what it stands in for> "+
				"or bench"
		default:
			rule, detail = BadMarker, fmt.Sprintf("a %s marker is a fixed wait's, and the line "+
				"it covers reads elapsed time and holds no fixed wait: a measurement that "+
				"decides nothing is //fixedwait:measure <why it decides nothing>, and a Sub, "+
				"Before, After or Compare between instants no wall clock separates is "+
				"//fixedwait:stated <which instants or clock>", m.kind)
		}
		tree.Findings = append(tree.Findings, Finding{
			Rule: rule, File: s.rel, Line: m.line, Detail: detail,
		})
	}
}

// trailsCode reports whether the comment at p follows code on its line,
// read from the file's own bytes between the line's start and the comment.
func trailsCode(src []byte, p token.Position) bool {
	start := p.Offset - (p.Column - 1)
	if start < 0 || p.Offset > len(src) {
		return false
	}
	return len(bytes.TrimSpace(src[start:p.Offset])) > 0
}

// inBenchmark reports whether pos sits in a top-level function of f that
// takes a *testing.B, through f's own name for package testing: a
// benchmark, or a helper only a benchmark can call.
func inBenchmark(f *ast.File, pos token.Pos) bool {
	testingName := importName(f, "testing")
	if testingName == "" {
		return false
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || pos < fn.Pos() || pos >= fn.End() {
			continue
		}
		for _, field := range fn.Type.Params.List {
			star, ok := field.Type.(*ast.StarExpr)
			if !ok {
				continue
			}
			switch x := star.X.(type) {
			case *ast.SelectorExpr:
				if id, ok := x.X.(*ast.Ident); ok && id.Name == testingName && x.Sel.Name == "B" {
					return true
				}
			case *ast.Ident:
				if testingName == "." && x.Name == "B" {
					return true
				}
			}
		}
		return false
	}
	return false
}

// topNames is the names a top-level declaration contributes to [Tree.Decls].
func topNames(d ast.Decl) []string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return []string{declName(d)}
	case *ast.GenDecl:
		var out []string
		for _, spec := range d.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok {
				for _, n := range vs.Names {
					out = append(out, n.Name)
				}
			}
		}
		return out
	}
	return nil
}

// enclosing names the top-level declaration holding pos.
func enclosing(d ast.Decl, pos token.Pos) string {
	switch d := d.(type) {
	case *ast.FuncDecl:
		return declName(d)
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			if vs, ok := spec.(*ast.ValueSpec); ok && vs.Pos() <= pos && pos < vs.End() && len(vs.Names) > 0 {
				return vs.Names[0].Name
			}
		}
	}
	return "package-scope"
}

// timeRefs is every reference in d to one of [waits] or [elapsed] through
// timeName: a selector time.X, called or not, and with a dot import the bare
// name wherever it is used rather than declared.
func timeRefs(d ast.Decl, timeName string) []candidate {
	var out []candidate
	if timeName != "." {
		ast.Inspect(d, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == timeName &&
				(waits[sel.Sel.Name] || elapsed[sel.Sel.Name]) {
				out = append(out, candidate{pos: sel.Pos(), name: sel.Sel.Name})
			}
			return true
		})
		return out
	}
	// A DOT IMPORT: the names stand bare. Without types an identifier is
	// judged by its position, and every position that declares a name or
	// selects a member is not a reference to package time's.
	notRef := map[*ast.Ident]bool{}
	ast.Inspect(d, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			notRef[n.Sel] = true
		case *ast.KeyValueExpr:
			if id, ok := n.Key.(*ast.Ident); ok {
				notRef[id] = true
			}
		case *ast.FuncDecl:
			notRef[n.Name] = true
		case *ast.Field:
			for _, id := range n.Names {
				notRef[id] = true
			}
		case *ast.ValueSpec:
			for _, id := range n.Names {
				notRef[id] = true
			}
		case *ast.TypeSpec:
			notRef[n.Name] = true
		case *ast.LabeledStmt:
			notRef[n.Label] = true
		case *ast.BranchStmt:
			if n.Label != nil {
				notRef[n.Label] = true
			}
		case *ast.AssignStmt:
			if n.Tok == token.DEFINE {
				for _, e := range n.Lhs {
					if id, ok := e.(*ast.Ident); ok {
						notRef[id] = true
					}
				}
			}
		case *ast.RangeStmt:
			if n.Tok == token.DEFINE {
				for _, e := range []ast.Expr{n.Key, n.Value} {
					if id, ok := e.(*ast.Ident); ok {
						notRef[id] = true
					}
				}
			}
		}
		return true
	})
	ast.Inspect(d, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && (waits[id.Name] || elapsed[id.Name]) && !notRef[id] {
			out = append(out, candidate{pos: id.Pos(), name: id.Name})
		}
		return true
	})
	return out
}

// verifyHelper holds a bubble-only helper to its declaration: every
// reference to it, in test code or production code, sits inside a bubble,
// inside another bubble-only helper, or is the function synctest itself is
// handed. References are found by NAME, which is the conservative direction
// without types — an identifier of the same name in the helper's directory,
// or a selector of the same name where the helper is a method, is held to
// the rule as if it were the helper — so a helper takes a name nothing else
// in its package uses. Outside its directory an exported function is
// reached only through the importing file's own name for its package, and
// a method only by a selector in its own package, since [isHelper] leaves
// out a method with an exported name: that one is reached through an
// interface or an embedding where no name says so.
func verifyHelper(fset *token.FileSet, dir string, h helperDecl, files []*source, module string, pkgs map[string]*pkgDecls) []Finding {
	name := h.decl.Name.Name
	method := h.decl.Recv != nil
	pos := fset.Position(h.decl.Name.Pos())
	var out []Finding
	for _, s := range files {
		if s.file == nil {
			continue
		}
		var qual string
		if s.dir != dir {
			if !token.IsExported(name) {
				continue
			}
			qual = importName(s.file, module+"/"+dir)
			if qual == "" {
				continue
			}
		}
		// HANDED WHOLE to synctest, or to a bubble runner in its runner's
		// position, the helper is what the bubble runs.
		spans, direct := bubbles(s.file, importName(s.file, "testing/synctest"), pkgs[s.dir])
		var uses []ast.Node
		// Not references: a selector's member (unless the helper is a
		// method) and the name a function declaration declares.
		sels := map[*ast.Ident]bool{}
		ast.Inspect(s.file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.FuncDecl:
				sels[n.Name] = true
			case *ast.SelectorExpr:
				sels[n.Sel] = true
				switch {
				case method && n.Sel.Name == name:
					uses = append(uses, n)
				case !method && qual != "" && qual != "." && n.Sel.Name == name:
					if x, ok := n.X.(*ast.Ident); ok && x.Name == qual {
						uses = append(uses, n)
					}
				}
			case *ast.Ident:
				if !method && (qual == "" || qual == ".") && n.Name == name && !sels[n] {
					uses = append(uses, n)
				}
			}
			return true
		})
		for _, u := range uses {
			if direct[u] || inSpans(spans, u.Pos()) {
				continue
			}
			at := fset.Position(u.Pos())
			out = append(out, Finding{
				Rule: BubbleEscapes, File: s.rel, Line: at.Line,
				Func: name,
				Detail: fmt.Sprintf("%s is declared bubble-only at %s:%d, and this "+
					"reference runs it outside a synctest bubble, where its waits are "+
					"the wall clock's: call it inside the bubble, or remove the declaration",
					name, h.src.rel, pos.Line),
			})
		}
	}
	return out
}
