package decltype

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// World is what a walk has read: every package it has been asked for, each
// read once, and every file it has parsed. It is not safe for concurrent use;
// a gate builds one per walk.
type World struct {
	root   string
	module string
	// dirs is where each package outside the module that this world reads
	// lives, by import path.
	dirs map[string]string
	// tests is whether this world reads the module's packages as their tests
	// compile them ([NewWithTests]).
	tests bool
	fset  *token.FileSet
	// pkgs is every package asked for, by import path: nil for one this
	// world does not read, or could not.
	pkgs map[string]*Package
	// xtests is every external test package an external test file has been
	// asked for of, by the package it tests ([World.FileAt]).
	xtests map[*Package]*Package
	// files is every file parsed, by the path it was read from.
	files map[string]*File
	// univ is the universe's own declarations ([universeSource]).
	univ *Package
	errs []error
	// depth bounds [World.TypeOf]'s recursion, which a declaration that
	// reaches back to itself (var a = b; var b = a) would otherwise make
	// endless.
	depth int
}

// New is a world that reads the packages of the module whose import path is
// module from the tree under root, and each package dirs names from the
// directory it names — every other package it leaves unread.
func New(root, module string, dirs map[string]string) *World {
	return &World{root: root, module: module, dirs: dirs, fset: token.NewFileSet(),
		pkgs: map[string]*Package{}, xtests: map[*Package]*Package{}, files: map[string]*File{}}
}

// NewWithTests is a world that reads the module's packages as their tests
// compile them, for a gate whose subject is test code: in a world that reads
// no test file, a value a test helper of another file returns is one the walk
// cannot type, and a gate that judges test files would report most of what
// they build.
//
// A package whose file is asked for ([World.FileAt]), or which is loaded by
// its directory ([World.LoadDir]), is read with its in-package test files —
// the declarations a _test.go file of package p adds to p, a method it
// declares on one of p's types among them — and an external test file
// (package p_test) is read in a package of its own,
// whose import path is p's with _test and which imports p as the go command
// builds p for its tests: with those declarations. A package that is only
// imported is read as [New] reads it, since that is what its importers compile
// against, and so is every package outside the module.
func NewWithTests(root, module string, dirs map[string]string) *World {
	w := New(root, module, dirs)
	w.tests = true
	return w
}

// FileSet is the positions of every file this world parsed.
func (w *World) FileSet() *token.FileSet { return w.fset }

// Errors is every file this world could not parse, in the order it met them.
// A gate reports them: a file it could not parse is a file it judged nothing
// in.
func (w *World) Errors() []error { return slices.Clone(w.errs) }

// Read is how many packages this world has read, an external test package
// among them.
func (w *World) Read() int {
	n := len(w.xtests)
	for _, p := range w.pkgs {
		if p != nil {
			n++
		}
	}
	return n
}

// Package is what one package declares at package level, in the files this
// platform builds.
type Package struct {
	// Path is the package's import path.
	Path string
	// Dir is the directory it was read from.
	Dir string

	// name is the name its files' package clause gives it.
	name    string
	files   []*File
	types   map[string]typeDecl
	values  map[string]valueDecl
	funcs   map[string]funcDecl
	methods map[string]map[string]funcDecl // by receiver type, then method
	// testsRead is whether its test files have been read, in a world that
	// reads them; xfiles is every external test file among them this
	// platform builds, which [World.external] reads.
	testsRead bool
	xfiles    []string
}

func newPackage(importPath, dir string) *Package {
	return &Package{Path: importPath, Dir: dir, types: map[string]typeDecl{},
		values: map[string]valueDecl{}, funcs: map[string]funcDecl{},
		methods: map[string]map[string]funcDecl{}}
}

// Files is every file of the package this platform builds, by name — in a
// world that reads tests, its in-package test files among them once a file of
// it has been asked for.
func (p *Package) Files() []*File { return slices.Clone(p.files) }

// TypeNames is every type the package declares at package level, sorted.
func (p *Package) TypeNames() []string {
	names := make([]string, 0, len(p.types))
	for n := range p.types {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

// TypeSpec is the declaration of the package-level type name, or nil.
func (p *Package) TypeSpec(name string) *ast.TypeSpec {
	if d, ok := p.types[name]; ok {
		return d.spec
	}
	return nil
}

// File is one parsed file, read whole with the parser's file-scoped object
// resolution — which is what types a local by the declaration it names.
type File struct {
	// Package is the package whose scope the file's names resolve in: its
	// own, even for a file this platform does not build ([World.FileAt]).
	Package *Package
	// Path is the path it was read from.
	Path string
	// Rel is Path relative to the world's root, slash-separated, or Path
	// itself for a file outside it.
	Rel string
	// AST is the file, as parsed.
	AST *ast.File

	// imports maps the name each import is used by in this file to its
	// import path. A blank import names nothing, and a dot import is not
	// followed (the coverage boundary).
	imports map[string]string
	// slots is where each composite literal whose type is elided sits, read
	// from the whole file the first time one is typed ([File.slot]).
	slots map[*ast.CompositeLit]slot
}

// slot is where a composite literal whose type is elided sits: in the literal
// of its collection, as an element or a value there, or as a map's key.
type slot struct {
	in  *ast.CompositeLit
	key bool
}

// slot is where the composite literal lit, whose type is elided, sits in the
// file; false for one that sits in no literal, which the parser does not
// produce.
func (f *File) slot(lit *ast.CompositeLit) (slot, bool) {
	if f.slots == nil {
		f.slots = map[*ast.CompositeLit]slot{}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			outer, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, e := range outer.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if c, ok := kv.Key.(*ast.CompositeLit); ok && c.Type == nil {
						f.slots[c] = slot{in: outer, key: true}
					}
					e = kv.Value
				}
				if c, ok := e.(*ast.CompositeLit); ok && c.Type == nil {
					f.slots[c] = slot{in: outer}
				}
			}
			return true
		})
	}
	s, ok := f.slots[lit]
	return s, ok
}

// Import is the import path a file-scope name names, when it names one.
func (f *File) Import(name string) (string, bool) {
	ip, ok := f.imports[name]
	return ip, ok
}

type typeDecl struct {
	spec *ast.TypeSpec
	file *File
}

// name is the declared type's identity.
func (d typeDecl) name() TypeName {
	return TypeName{Package: d.file.Package.Path, Name: d.spec.Name.Name}
}

type funcDecl struct {
	decl *ast.FuncDecl
	file *File
}

// valueDecl is one name of a var or const declaration.
type valueDecl struct {
	spec  *ast.ValueSpec
	index int
	file  *File
}

// Load reads the package at an import path, once, and answers nil for a
// package this world does not read — one outside the module that no
// directory was named for — or could not.
func (w *World) Load(importPath string) *Package {
	if p, seen := w.pkgs[importPath]; seen {
		return p
	}
	w.pkgs[importPath] = nil
	var dir string
	switch rest, inModule := strings.CutPrefix(importPath, w.module+"/"); {
	case w.dirs[importPath] != "":
		dir = w.dirs[importPath]
	case importPath == w.module:
		dir = w.root
	case inModule:
		dir = filepath.Join(w.root, filepath.FromSlash(rest))
	default:
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	p := newPackage(importPath, dir)
	w.pkgs[importPath] = p
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// THE FILES THIS PLATFORM BUILDS make the package's scope, so two
		// platforms' declarations of one name never meet. A file built only
		// elsewhere is still read when it is asked for ([World.FileAt]),
		// against this scope.
		if match, err := build.Default.MatchFile(dir, name); err != nil || !match {
			continue
		}
		if f := w.parse(filepath.Join(dir, name), p); f != nil {
			p.files = append(p.files, f)
			p.declare(f)
		}
	}
	if len(p.files) > 0 {
		p.name = p.files[0].AST.Name.Name
	}
	return p
}

// LoadDir reads the module's package in dir, a directory under the root — in
// a world that reads tests, with its in-package test files ([NewWithTests]).
func (w *World) LoadDir(dir string) *Package {
	rel, err := filepath.Rel(w.root, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil
	}
	importPath := w.module
	if rel != "." {
		importPath += "/" + filepath.ToSlash(rel)
	}
	p := w.Load(importPath)
	if p != nil && w.tests {
		w.readTests(p)
	}
	return p
}

// readTests reads the test files of p this platform builds, once: an
// in-package one into p's own scope, after the files it holds, and an
// external one aside, for the package [World.external] reads it in.
func (w *World) readTests(p *Package) {
	if p.testsRead {
		return
	}
	p.testsRead = true
	entries, err := os.ReadDir(p.Dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		if match, err := build.Default.MatchFile(p.Dir, name); err != nil || !match {
			continue
		}
		path := filepath.Join(p.Dir, name)
		if w.isExternal(p, path) {
			p.xfiles = append(p.xfiles, path)
			continue
		}
		if f := w.parse(path, p); f != nil {
			p.files = append(p.files, f)
			p.declare(f)
		}
	}
}

// isExternal reports whether the test file at path belongs to p's external
// test package: its package clause names a package with _test that is not p
// itself — p's name with _test, as the go command requires of one.
func (w *World) isExternal(p *Package, path string) bool {
	clause, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.PackageClauseOnly)
	if err != nil {
		// Read whole against p, which names why it does not parse.
		return false
	}
	return clause.Name.Name != p.name && strings.HasSuffix(clause.Name.Name, "_test")
}

// external is p's external test package, read once from the external test
// files [World.readTests] set aside.
func (w *World) external(p *Package) *Package {
	if x, ok := w.xtests[p]; ok {
		return x
	}
	x := newPackage(p.Path+"_test", p.Dir)
	w.xtests[p] = x
	for _, path := range p.xfiles {
		if f := w.parse(path, x); f != nil {
			x.files = append(x.files, f)
			x.declare(f)
		}
	}
	if len(x.files) > 0 {
		x.name = x.files[0].AST.Name.Name
	}
	return x
}

// FileAt is the file at path, a file of one of the module's packages under
// the root, read in its package's scope — the file the package read, or, for
// a file this platform does not build, the file parsed against the scope of
// the files it does, so that every file is judged, wherever it is built. In a
// world that reads tests, an external test file's package is its external
// test package ([NewWithTests]). Nil for a file that could not be read; the
// reason is among [World.Errors].
func (w *World) FileAt(path string) *File {
	if f, ok := w.files[path]; ok {
		return f
	}
	p := w.LoadDir(filepath.Dir(path))
	if p == nil {
		w.errs = append(w.errs, fmt.Errorf("read the package of %s: not a package of %s under %s",
			path, w.module, w.root))
		return nil
	}
	if f, ok := w.files[path]; ok {
		return f
	}
	if w.tests && strings.HasSuffix(path, "_test.go") &&
		(slices.Contains(p.xfiles, path) || w.isExternal(p, path)) {
		p = w.external(p)
		if f, ok := w.files[path]; ok {
			return f
		}
	}
	return w.parse(path, p)
}

// parse parses one file whole, once.
func (w *World) parse(path string, p *Package) *File {
	if f, ok := w.files[path]; ok {
		return f
	}
	// WITH the parser's object resolution, never parser.SkipObjectResolution:
	// it is what types a local by the declaration it names.
	parsed, err := parser.ParseFile(w.fset, path, nil, 0)
	if err != nil {
		w.errs = append(w.errs, fmt.Errorf("parse %s: %w", path, err))
		return nil
	}
	rel, err := filepath.Rel(w.root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		rel = path
	}
	f := &File{Package: p, Path: path, Rel: filepath.ToSlash(rel), AST: parsed,
		imports: map[string]string{}}
	for _, imp := range parsed.Imports {
		ip, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := ImportName(ip)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name != "_" && name != "." {
			f.imports[name] = ip
		}
	}
	w.files[path] = f
	return f
}

// declare records what one file declares at package level. The first
// declaration of a name wins: within one platform's files a name is declared
// once.
func (p *Package) declare(f *File) {
	for _, decl := range f.AST.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if _, dup := p.types[s.Name.Name]; !dup {
						p.types[s.Name.Name] = typeDecl{s, f}
					}
				case *ast.ValueSpec:
					for i, n := range s.Names {
						if _, dup := p.values[n.Name]; !dup {
							p.values[n.Name] = valueDecl{s, i, f}
						}
					}
				}
			}
		case *ast.FuncDecl:
			if d.Recv == nil || len(d.Recv.List) == 0 {
				if _, dup := p.funcs[d.Name.Name]; !dup {
					p.funcs[d.Name.Name] = funcDecl{d, f}
				}
				continue
			}
			recv := BaseName(d.Recv.List[0].Type)
			if p.methods[recv] == nil {
				p.methods[recv] = map[string]funcDecl{}
			}
			if _, dup := p.methods[recv][d.Name.Name]; !dup {
				p.methods[recv][d.Name.Name] = funcDecl{d, f}
			}
		}
	}
}

// ImportName is the name an import with no name of its own is used by, by
// the convention every package this tree imports follows: the last element of
// its path; the element before it where the last is a major version
// (github.com/golang-jwt/jwt/v5 is jwt); less a gopkg.in version
// (gopkg.in/yaml.v3 is yaml); less a ".go" suffix and a "go-" prefix
// (github.com/nats-io/nats.go is nats, github.com/mattn/go-sqlite3 is
// sqlite3). Every package of this module is named for its directory.
//
// A package that breaks the convention is one whose name a file uses is not
// recognised as a package at all: a value reached through it is one the walk
// cannot type, which a gate reports rather than passes.
func ImportName(importPath string) string {
	elems := strings.Split(importPath, "/")
	last := elems[len(elems)-1]
	if major, ok := strings.CutPrefix(last, "v"); ok && len(elems) > 1 && digits(major) {
		last = elems[len(elems)-2]
	}
	if elems[0] == "gopkg.in" {
		if name, version, ok := strings.Cut(last, ".v"); ok && digits(version) {
			last = name
		}
	}
	return strings.TrimPrefix(strings.TrimSuffix(last, ".go"), "go-")
}

// digits reports whether s is one or more decimal digits.
func digits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// BaseName is the name of the type a receiver or an embedded field is spelled
// with — T of T, *T, T[K], pkg.T — or "" for any other expression.
func BaseName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return BaseName(x.X)
	case *ast.ParenExpr:
		return BaseName(x.X)
	case *ast.IndexExpr:
		return BaseName(x.X)
	case *ast.IndexListExpr:
		return BaseName(x.X)
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.Ident:
		return x.Name
	}
	return ""
}

// Locate is the directory of each package importPaths names, as the module in
// dir builds against it — a dependency in the module cache, a standard
// package in the toolchain's tree — answered by `go list`, which builds
// nothing.
func Locate(ctx context.Context, dir string, importPaths ...string) (map[string]string, error) {
	args := append([]string{"list", "-f", "{{.ImportPath}}\t{{.Dir}}"}, importPaths...)
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("locate %s with go list: %w: %s", strings.Join(importPaths, ", "),
			err, strings.TrimSpace(stderr.String()))
	}
	dirs := map[string]string{}
	for line := range strings.Lines(string(out)) {
		p, d, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || d == "" {
			return nil, fmt.Errorf("go list answered %q for %s", line, strings.Join(importPaths, ", "))
		}
		dirs[p] = d
	}
	for _, p := range importPaths {
		if dirs[p] == "" {
			return nil, fmt.Errorf("go list named no directory for %s", p)
		}
	}
	return dirs, nil
}
