package statelog_test

import (
	"bytes"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestOnlyStreamCreatedAtReadsAStreamsCreationInstant fails the build when any
// non-test file under internal/ and cmd/ but internal/statelog/identity.go
// reads the Created field of a stream's information — the BROKER'S creation
// instant — rather than asking statelog.StreamCreatedAt for the stream's
// identity (ADR-0035).
//
// # Why a gate, and why the field
//
// A stream's identity is the instant the create that made it stamped into its
// configuration, because a snapshot restore keeps the configuration and dates
// the stream to the restore. Three readers had each read the broker's instant
// on their own — every state log's applier through the log's Stats, the memory
// changelog's hydration and the backup's read past its copy — and each passed
// every test until a restore was tried, when each called a restored log
// recreated. A fourth reader written the same way compiles and passes every
// test except a restore-shaped one, so the rule is held here rather than by
// memory. What every such reader cannot avoid, whatever it is called, is
// selecting that one field on a value of one of a few types, so the field is
// the subject.
//
// # What it flags
//
// A selector X.Created that is not called, in a non-test file under internal/
// or cmd/, whose X the walk types as a value whose Created field is declared
// by one of [streamInstantRoots] — jetstream.StreamInfo, nats.StreamInfo,
// server.StreamInfo, server.StreamDetail, server.FileStreamInfo — directly or
// promoted through embedding, so the server's JSApiStream*Response types and a
// local type embedding a stream's information are flagged too. A consumer's
// information is another type and is never flagged, nor is a type of this
// module with a Created field of its own.
//
// # How X is typed, with no type checker
//
// From declarations alone, on demand: a variable by the declaration the
// parser's file-scoped resolution names (a parameter, a var, a :=, a range
// clause), a field by the struct it is selected on, a call by the results its
// function, method or interface method declares, and an element by its
// collection — across the files of a package and into any package of this
// module and the three NATS packages, which are read from the module cache.
// Pointers are transparent, as a selector makes them. GENERICS ARE FOLLOWED,
// because every seam of the estate router is one (`call[A, R any](…, o op[A,
// R], …) (R, error)`): a generic type's parameters are bound by its
// instantiation, a generic function's by its explicit type arguments or by
// unifying each parameter's type with its argument's, as the compiler infers
// them, and a generic method's by the receiver it is selected on. Nothing is
// compiled: a type checker types every package the judged ones import, which
// for cmd/crewlet is the whole module — measured at 41 s cold with `go list
// -export`, and 10.7 s to type-check one package from source.
//
// # A read the walk cannot type FAILS
//
// A selector whose X it cannot type — a type switch's variable, a value a
// function of a package it does not read returns — or whose field it cannot
// find is reported rather than passed: a gate asserting an absence that
// skipped what it could not read would certify nothing about exactly the reads
// it skipped. Spell the value's type where it is declared, or teach this walk
// the shape.
//
// # Coverage boundary, stated rather than assumed
//
// A value of a package outside this module and the three NATS packages — the
// standard library's, a vendor SDK's — is judged by its package alone and
// never read: none of them declares a stream's information, and one that
// embedded one would be missed. A value reached through reflection, an
// encoding (marshalled and decoded as another type) or an interface whose
// dynamic type is a stream's information is not seen, nor is a selector in a
// file the walk does not judge (a _test.go file, testdata).
//
// # Cost
//
// It reads every non-test .go file under internal/ and cmd/ once and parses
// whole only the packages of those spelling the identifier Created — 25 files
// — and the packages their reads reach, NATS's included: 18 packages, in
// 0.46 s, 1.8 s under the race detector.
func TestOnlyStreamCreatedAtReadsAStreamsCreationInstant(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	found := streamInstantReads(t, root, natsDirs(t, root))

	if found.candidates == 0 || found.selectors == 0 {
		t.Fatalf("judged %d file(s) and %d Created selector(s) — this guard is "+
			"certifying nothing. Check the module root and the internal/ and cmd/ walks",
			found.candidates, found.selectors)
	}
	// A guard asserting an ABSENCE passes identically when the thing is absent
	// and when the matcher has gone inert. The one reader is KNOWN to read the
	// field, so a walk that cannot see it has stopped seeing anything.
	if found.allowed == 0 {
		t.Fatalf("found no read of a stream's creation instant in %s, the one reader "+
			"allowed one. Either the walk has gone inert — and every verdict below is "+
			"worthless — or statelog.StreamCreatedAt stopped reading the broker's instant "+
			"for a stream carrying no stamp, in which case this guard's subject moved",
			allowedReader)
	}
	for _, h := range found.hits {
		t.Errorf("%s:%d in %s reads %s.Created — a stream's creation instant as the "+
			"broker reports it, which a snapshot restore moves.\n\tRead the stream's "+
			"identity with statelog.StreamCreatedAt, which reads the instant the stream's "+
			"creator stamped and falls back to this one only for a stream carrying none "+
			"(ADR-0035)", h.file, h.line, h.fn, h.owner)
	}
	for _, u := range found.untyped {
		t.Errorf("%s:%d in %s selects Created on %s, whose type this walk cannot "+
			"establish, so it cannot say whether that is a stream's creation instant.\n\t"+
			"Spell the value's type where it is declared, or teach this guard the shape",
			u.file, u.line, u.fn, u.owner)
	}
	t.Logf("judged %d file(s) holding Created: %d selector(s), %d in %s; read %d "+
		"package(s) on demand", found.candidates, found.selectors, found.allowed,
		allowedReader, found.packages)
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN: every shape a reader of a
// stream's creation instant takes — the three readers this rule was written
// after among them, each reverted to the field — must be reported; a
// consumer's instant, a field of this module's own and a call must not be; a
// read the walk cannot type must be reported as one; and a test file and
// testdata must never be read.
func TestTheStreamInstantWalkJudgesAKnownTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, body := range streamInstantFixture {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found := streamInstantReads(t, root, natsDirs(t, sourcetree.Root(t)))

	verdicts := func(rs []streamInstantRead) map[string]int {
		out := map[string]int{}
		for _, r := range rs {
			out[path.Dir(r.file)+"."+r.fn+": "+r.owner]++
		}
		return out
	}
	for kind, pair := range map[string][2]map[string]int{
		"read":    {verdicts(found.hits), countOf(streamInstantFixtureHits)},
		"untyped": {verdicts(found.untyped), countOf(streamInstantFixtureUntyped)},
	} {
		got, want := pair[0], pair[1]
		for _, v := range slices.Sorted(maps.Keys(mergeKeys(got, want))) {
			switch d := want[v] - got[v]; {
			case d > 0:
				t.Errorf("missed the %s %s (%d time(s))", kind, v, d)
			case d < 0:
				t.Errorf("reported the %s %s (%d time(s) too many)", kind, v, -d)
			}
		}
	}
	if found.allowed != 1 {
		t.Errorf("recognised %d read(s) in %s, want its 1", found.allowed, allowedReader)
	}
}

// TestEveryCreatedFieldOfTheNATSPackagesIsClassified holds [streamInstantRoots]
// to the NATS packages the module builds against, in both directions: every
// exported type there that declares a Created field is either a stream's
// information or named in [notStreamInstants] with what its instant is, and
// every type either list names still declares one.
//
// A dependency bump is when a new kind of stream information would appear — a
// type the gate above has never heard of, whose Created field it would never
// flag — so the bump fails here until somebody says which kind it is.
func TestEveryCreatedFieldOfTheNATSPackagesIsClassified(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	w := newIdentityWorld(t, root, natsDirs(t, root))
	declared := map[typeName]bool{}
	for _, p := range []string{jetstreamPath, natsPath, serverPath} {
		pkg := w.load(p)
		if pkg == nil {
			t.Fatalf("could not read %s", p)
		}
		for name, d := range pkg.types {
			if !ast.IsExported(name) {
				continue
			}
			st, ok := d.decl.Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, f := range st.Fields.List {
				if slices.ContainsFunc(f.Names, func(n *ast.Ident) bool { return n.Name == "Created" }) {
					declared[typeName{p, name}] = true
				}
			}
		}
	}
	for tn := range declared {
		if !streamInstantRoots[tn] && notStreamInstants[tn] == "" {
			t.Errorf("%s declares a Created field and is in neither streamInstantRoots nor "+
				"notStreamInstants: if it is a stream's information its instant is the "+
				"broker's, and a read of it bypasses the stream's identity — classify it",
				tn)
		}
	}
	for tn := range streamInstantRoots {
		if !declared[tn] {
			t.Errorf("streamInstantRoots names %s, which no longer declares a Created field — "+
				"the gate flags nothing through it", tn)
		}
	}
	for tn := range notStreamInstants {
		if !declared[tn] {
			t.Errorf("notStreamInstants names %s, which no longer declares a Created field", tn)
		}
	}
}

const (
	identityModule = "github.com/crewlet/crewlet"
	// allowedReader is the one file allowed to read the field: the reading
	// every other consumer of a stream's identity goes through.
	allowedReader = "internal/statelog/identity.go"

	jetstreamPath = "github.com/nats-io/nats.go/jetstream"
	natsPath      = "github.com/nats-io/nats.go"
	serverPath    = "github.com/nats-io/nats-server/v2/server"
)

// typeName names a declared type by its package's import path.
type typeName struct{ pkg, name string }

func (n typeName) String() string { return defaultImportName(n.pkg) + "." + n.name }

// streamInstantRoots are the types whose Created field is a stream's creation
// instant as the broker reports it: the client's two views of a stream's
// information, the server's own, the server's monitoring detail of a stream,
// and the server's file store's record of one.
var streamInstantRoots = map[typeName]bool{
	{jetstreamPath, "StreamInfo"}:  true,
	{natsPath, "StreamInfo"}:       true,
	{serverPath, "StreamInfo"}:     true,
	{serverPath, "StreamDetail"}:   true,
	{serverPath, "FileStreamInfo"}: true,
}

// notStreamInstants are the other exported types of those packages that
// declare a Created field, and what their instant is.
var notStreamInstants = map[typeName]string{
	{jetstreamPath, "ConsumerInfo"}:       "a consumer's creation instant",
	{jetstreamPath, "DesiredClusterInfo"}: "when a placement change was recorded",
	{natsPath, "ConsumerInfo"}:            "a consumer's creation instant",
	{natsPath, "DesiredClusterInfo"}:      "when a placement change was recorded",
	{serverPath, "ConsumerInfo"}:          "a consumer's creation instant",
	{serverPath, "DesiredClusterInfo"}:    "when a placement change was recorded",
	{serverPath, "FileConsumerInfo"}:      "the file store's record of a consumer's creation",
}

// natsDirs locates the NATS packages the module builds against, in the module
// cache: `go list` names each one's directory without building anything.
func natsDirs(t *testing.T, root string) map[string]string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "go", "list", "-f", "{{.ImportPath}}\t{{.Dir}}",
		jetstreamPath, natsPath, serverPath)
	cmd.Dir = root
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("locate the NATS packages with go list: %v", err)
	}
	dirs := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		p, dir, ok := strings.Cut(line, "\t")
		if !ok || dir == "" {
			t.Fatalf("go list answered %q", line)
		}
		dirs[p] = dir
	}
	return dirs
}

// streamInstantWalk is what one walk of a tree found.
type streamInstantWalk struct {
	// candidates is every file judged, and selectors every Created selector
	// in them that is not a call.
	candidates, selectors int
	// allowed counts the reads in [allowedReader]: the one reader, recognised.
	allowed int
	// packages is every package whose declarations were read.
	packages int
	hits     []streamInstantRead
	untyped  []streamInstantRead
}

// streamInstantRead is one selector the walk reports.
type streamInstantRead struct {
	file  string // relative to the root
	line  int
	fn    string // the declaration it sits in
	owner string // the type whose field it reads, or the expression it could not type
}

// streamInstantReads walks root's internal/ and cmd/ trees and judges every
// Created selector in their non-test files.
func streamInstantReads(t *testing.T, root string, dirs map[string]string) streamInstantWalk {
	t.Helper()
	w := newIdentityWorld(t, root, dirs)
	var out streamInstantWalk
	for _, sub := range []string{"internal", "cmd"} {
		start := filepath.Join(root, sub)
		if _, err := os.Stat(start); err != nil {
			continue
		}
		err := sourcetree.Walk(start, func(p string, d fs.DirEntry, err error) error {
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
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			// THE PREFILTER IS SOUND: a selector spells its field's name, and
			// an identifier has no escapes.
			if !spellsIdentifier(src, "Created") {
				return nil
			}
			out.candidates++
			w.judge(p, &out)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
	out.packages = len(w.pkgs)
	return out
}

// spellsIdentifier reports whether src holds name as a whole identifier —
// delimited by bytes no identifier continues with — so a file that only says
// CreatedAt or CreatedBy is never parsed.
func spellsIdentifier(src []byte, name string) bool {
	continues := func(b byte) bool {
		return b == '_' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= 0x80
	}
	for i := 0; ; {
		at := bytes.Index(src[i:], []byte(name))
		if at < 0 {
			return false
		}
		start, end := i+at, i+at+len(name)
		if (start == 0 || !continues(src[start-1])) && (end == len(src) || !continues(src[end])) {
			return true
		}
		i = end
	}
}

// identityWorld reads declarations on demand: this module's packages from the
// tree, the NATS packages from the module cache.
type identityWorld struct {
	t    *testing.T
	fset *token.FileSet
	root string
	dirs map[string]string
	// pkgs is every package asked for, by import path; nil for one this walk
	// does not read.
	pkgs map[string]*identityPkg
	// files is every file parsed, by its path.
	files map[string]*identityFile
	depth int
}

func newIdentityWorld(t *testing.T, root string, dirs map[string]string) *identityWorld {
	return &identityWorld{t: t, fset: token.NewFileSet(), root: root, dirs: dirs,
		pkgs: map[string]*identityPkg{}, files: map[string]*identityFile{}}
}

// identityPkg is what one package declares, in the files this platform builds.
type identityPkg struct {
	path    string
	types   map[string]declared[*ast.TypeSpec]
	values  map[string]valueDecl
	funcs   map[string]declared[*ast.FuncDecl]
	methods map[string]map[string]declared[*ast.FuncDecl]
	files   map[string]*identityFile
}

// declared is a declaration and the file it is spelled in.
type declared[D any] struct {
	decl D
	file *identityFile
}

// valueDecl is one name of a var or const declaration.
type valueDecl struct {
	spec  *ast.ValueSpec
	index int
	file  *identityFile
}

// identityFile is one parsed file: its package and the names its imports are
// used by.
type identityFile struct {
	pkg     *identityPkg
	rel     string
	ast     *ast.File
	imports map[string]string
}

// typ is a type expression, the file it is spelled in — which resolves its
// names — and what its type parameters are bound to there. The zero typ is a
// type the walk could not establish.
type typ struct {
	expr  ast.Expr
	file  *identityFile
	subst subst
}

func (t typ) ok() bool { return t.expr != nil }

// in is the type expression e, spelled where t is.
func (t typ) in(e ast.Expr) typ { return typ{e, t.file, t.subst} }

// subst binds a generic declaration's type parameters to the types an
// instantiation or an inference gave them: op[A, R] read as
// op[ensureTagsArgs, ensuredTags], or a call of
// call[A, R any](…, o op[A, R], …) (R, error) whose R its argument decides.
type subst map[paramKey]typ

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

// paramOf reports whether id names a type parameter where s binds them, and
// what it is bound to.
func paramOf(id *ast.Ident, s subst) (bound typ, isBound, isParam bool) {
	switch {
	case typeParam(id):
		bound, isBound = s[keyOf(id)]
		return bound, isBound, true
	case id.Obj == nil:
		if bound, isBound = s[paramKey{receiverParams{}, id.Name}]; isBound {
			return bound, true, true
		}
	}
	return typ{}, false, false
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

// resolution is how far a type resolved.
type resolution int

const (
	// resolved: to a type literal this walk can read.
	resolved resolution = iota
	// foreign: to a type of a package this walk does not read, or a
	// predeclared one — neither declares a stream's information.
	foreign
	// unresolved: to something the walk cannot read (a type parameter, a
	// name it could not find), which may be anything.
	unresolved
)

// load reads one package's declarations, or answers nil for a package this
// walk does not read.
func (w *identityWorld) load(importPath string) *identityPkg {
	if p, seen := w.pkgs[importPath]; seen {
		return p
	}
	var dir string
	switch {
	case w.dirs[importPath] != "":
		dir = w.dirs[importPath]
	case strings.HasPrefix(importPath, identityModule+"/"):
		dir = filepath.Join(w.root, filepath.FromSlash(strings.TrimPrefix(importPath, identityModule+"/")))
	default:
		w.pkgs[importPath] = nil
		return nil
	}
	p := &identityPkg{path: importPath, types: map[string]declared[*ast.TypeSpec]{},
		values: map[string]valueDecl{}, funcs: map[string]declared[*ast.FuncDecl]{},
		methods: map[string]map[string]declared[*ast.FuncDecl]{}, files: map[string]*identityFile{}}
	w.pkgs[importPath] = p
	entries, err := os.ReadDir(dir)
	if err != nil {
		w.pkgs[importPath] = nil
		return nil
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// THE FILES THIS PLATFORM BUILDS make the package's scope, so two
		// platforms' declarations of one name never meet. A file built only
		// elsewhere is still judged, against this scope ([identityWorld.judge]).
		if match, err := build.Default.MatchFile(dir, name); err != nil || !match {
			continue
		}
		f := w.parse(filepath.Join(dir, name), p)
		if f == nil {
			continue
		}
		p.files[name] = f
		w.declare(p, f)
	}
	return p
}

// parse parses one file whole, with the parser's object resolution, which is
// what types a local name by its declaration.
func (w *identityWorld) parse(full string, p *identityPkg) *identityFile {
	if f, ok := w.files[full]; ok {
		return f
	}
	parsed, err := parser.ParseFile(w.fset, full, nil, 0)
	if err != nil {
		w.t.Errorf("parse %s: %v", full, err)
		return nil
	}
	rel, err := filepath.Rel(w.root, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = full
	}
	f := &identityFile{pkg: p, rel: filepath.ToSlash(rel), ast: parsed, imports: map[string]string{}}
	for _, imp := range parsed.Imports {
		ip, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := defaultImportName(ip)
		if imp.Name != nil {
			name = imp.Name.Name
		}
		if name != "_" && name != "." {
			f.imports[name] = ip
		}
	}
	w.files[full] = f
	return f
}

// defaultImportName is the name an import with no name of its own is used by,
// by the convention every package this walk reads follows: the last element
// of its path, skipping a major-version suffix, with a ".go" suffix and a "go-"
// prefix dropped (github.com/nats-io/nats.go is package nats). A package that
// breaks the convention is one whose values the walk cannot type, and a
// Created selected on one is reported, never passed.
func defaultImportName(importPath string) string {
	elems := strings.Split(importPath, "/")
	last := elems[len(elems)-1]
	if len(elems) > 1 && len(last) > 1 && last[0] == 'v' {
		if _, err := strconv.Atoi(last[1:]); err == nil {
			last = elems[len(elems)-2]
		}
	}
	return strings.TrimPrefix(strings.TrimSuffix(last, ".go"), "go-")
}

// declare records what one file declares at package level. The first
// declaration of a name wins: within one platform's files a name is declared
// once.
func (w *identityWorld) declare(p *identityPkg, f *identityFile) {
	for _, decl := range f.ast.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if _, dup := p.types[s.Name.Name]; !dup {
						p.types[s.Name.Name] = declared[*ast.TypeSpec]{s, f}
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
					p.funcs[d.Name.Name] = declared[*ast.FuncDecl]{d, f}
				}
				continue
			}
			recv := baseName(d.Recv.List[0].Type)
			if p.methods[recv] == nil {
				p.methods[recv] = map[string]declared[*ast.FuncDecl]{}
			}
			if _, dup := p.methods[recv][d.Name.Name]; !dup {
				p.methods[recv][d.Name.Name] = declared[*ast.FuncDecl]{d, f}
			}
		}
	}
}

// baseName is the type name a receiver or an embedded field is spelled with.
func baseName(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.StarExpr:
		return baseName(x.X)
	case *ast.ParenExpr:
		return baseName(x.X)
	case *ast.IndexExpr:
		return baseName(x.X)
	case *ast.IndexListExpr:
		return baseName(x.X)
	case *ast.SelectorExpr:
		return x.Sel.Name
	case *ast.Ident:
		return x.Name
	}
	return ""
}

// judge judges every Created selector of the file at full.
func (w *identityWorld) judge(full string, out *streamInstantWalk) {
	rel, err := filepath.Rel(w.root, filepath.Dir(full))
	if err != nil {
		w.t.Errorf("place %s: %v", full, err)
		return
	}
	p := w.load(identityModule + "/" + filepath.ToSlash(rel))
	if p == nil {
		w.t.Errorf("could not read the package of %s", full)
		return
	}
	f := p.files[filepath.Base(full)]
	if f == nil {
		// Built only on another platform: judged against this one's scope.
		if f = w.parse(full, p); f == nil {
			return
		}
	}
	called := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f.ast, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr); ok {
				called[s] = true
			}
		}
		return true
	})
	for _, decl := range f.ast.Decls {
		label := declLabel(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Created" || called[sel] {
				return true
			}
			if _, isPkg := w.packageNamed(sel.X, f); isPkg {
				return true
			}
			out.selectors++
			read := streamInstantRead{file: f.rel, line: w.fset.Position(sel.Pos()).Line, fn: label}
			owner, judged := w.fieldOwner(w.typeOf(sel.X, f), "Created")
			switch {
			case !judged:
				read.owner = exprString(sel.X)
				out.untyped = append(out.untyped, read)
			case owner != nil && streamInstantRoots[*owner]:
				read.owner = owner.String()
				if f.rel == allowedReader {
					out.allowed++
				} else {
					out.hits = append(out.hits, read)
				}
			}
			return true
		})
	}
}

// fieldOwner says which declared type's field name selects on t — nil for a
// field no declared type holds (an anonymous struct's) or for a method — and
// judged false where the walk cannot say: t unknown, or found nowhere while
// part of t could not be read.
func (w *identityWorld) fieldOwner(t typ, name string) (owner *typeName, judged bool) {
	if !t.ok() {
		return nil, false
	}
	_, own, found, read := w.field(t, name)
	switch {
	case found && own == nil:
		return nil, true
	case found:
		tn := typeName{own.file.pkg.path, own.decl.Name.Name}
		return &tn, true
	case read == unresolved:
		return nil, false
	}
	if _, ok := w.method(t, name); ok {
		return nil, true
	}
	// FOUND NOWHERE. Where a part of t is a type of a package this walk does
	// not read, that part holds it, and no such package declares a stream's
	// information; on a type read whole, the walk has missed something.
	return nil, read == foreign
}

// packageNamed answers the package an expression names, when it names one.
func (w *identityWorld) packageNamed(e ast.Expr, f *identityFile) (*identityPkg, bool) {
	id, ok := e.(*ast.Ident)
	if !ok || id.Obj != nil {
		return nil, false
	}
	ip, ok := f.imports[id.Name]
	if !ok || w.declaresValue(f, id.Name) {
		return nil, false
	}
	return w.load(ip), true
}

// bound follows t while it names a bound type parameter, to the type it is
// bound to; ok is false for a type parameter nothing bound.
func (w *identityWorld) bound(t typ) (typ, bool) {
	for range 32 {
		id, isIdent := ast.Unparen(t.expr).(*ast.Ident)
		if !isIdent {
			return t, true
		}
		b, isBound, isParam := paramOf(id, t.subst)
		switch {
		case !isParam:
			return t, true
		case !isBound:
			return typ{}, false
		}
		t = b
	}
	return typ{}, false
}

// named resolves a name in a type position — a generic one's instantiation
// included — to the declaration it names, and binds that declaration's type
// parameters to the instantiation's arguments, each read where the
// instantiation is spelled.
func (w *identityWorld) named(t typ) (declared[*ast.TypeSpec], subst, resolution) {
	var d declared[*ast.TypeSpec]
	base := ast.Unparen(t.expr)
	if args := typeArgs(base); args != nil {
		switch x := base.(type) {
		case *ast.IndexExpr:
			base = ast.Unparen(x.X)
		case *ast.IndexListExpr:
			base = ast.Unparen(x.X)
		}
	}
	switch x := base.(type) {
	case *ast.Ident:
		switch {
		case x.Obj != nil:
			s, isSpec := x.Obj.Decl.(*ast.TypeSpec)
			if !isSpec || x.Obj.Kind != ast.Typ {
				return d, nil, unresolved // a type parameter
			}
			d = declared[*ast.TypeSpec]{s, t.file}
		case t.file.pkg != nil && t.file.pkg.types[x.Name].decl != nil:
			d = t.file.pkg.types[x.Name]
		case predeclaredTypes[x.Name]:
			return d, nil, foreign
		default:
			return d, nil, unresolved
		}
	case *ast.SelectorExpr:
		pkgID, ok := x.X.(*ast.Ident)
		if !ok {
			return d, nil, unresolved
		}
		ip, ok := t.file.imports[pkgID.Name]
		if !ok {
			return d, nil, unresolved
		}
		p := w.load(ip)
		if p == nil {
			return d, nil, foreign
		}
		if d, ok = p.types[x.Sel.Name]; !ok {
			return d, nil, unresolved
		}
	default:
		return d, nil, unresolved
	}
	params := typeParams(d.decl.TypeParams)
	if len(params) == 0 {
		return d, nil, resolved
	}
	args := typeArgs(t.expr)
	s := subst{}
	for i, p := range params {
		if i < len(args) {
			s[p] = t.in(args[i])
		}
	}
	return d, s, resolved
}

// under resolves t through pointers, bound type parameters, names and
// aliases to the type literal that defines it, and the declaration whose
// literal that is (nil for a literal spelled in place).
func (w *identityWorld) under(t typ) (typ, *declared[*ast.TypeSpec], resolution) {
	var owner *declared[*ast.TypeSpec]
	for range 32 {
		if !t.ok() {
			return typ{}, nil, unresolved
		}
		if id, ok := t.expr.(*ast.Ident); ok {
			if b, isBound, isParam := paramOf(id, t.subst); isParam {
				if !isBound {
					return typ{}, nil, unresolved
				}
				t = b
				continue
			}
		}
		switch x := t.expr.(type) {
		case *ast.ParenExpr:
			t = t.in(x.X)
		case *ast.StarExpr:
			t = t.in(x.X)
		case *ast.Ident, *ast.SelectorExpr, *ast.IndexExpr, *ast.IndexListExpr:
			d, s, how := w.named(t)
			if how != resolved {
				return typ{}, nil, how
			}
			owner = &d
			t = typ{d.decl.Type, d.file, s}
		default:
			if owner != nil && ast.Unparen(owner.decl.Type) != t.expr {
				owner = nil
			}
			return t, owner, resolved
		}
	}
	return typ{}, nil, unresolved
}

// field finds the field name selects on t — its own, or promoted through
// embedded fields, shallowest first as the language resolves it — and answers
// its type and the declaration whose struct literal holds it. read says how
// much of t the search read: whole (resolved), with a part of a package this
// walk does not read (foreign), or with a part it could not read at all
// (unresolved).
func (w *identityWorld) field(t typ, name string) (ft typ, owner *declared[*ast.TypeSpec], found bool, read resolution) {
	read = resolved
	level := []typ{t}
	seen := map[*ast.StructType]bool{}
	for depth := 0; depth < 8 && len(level) > 0; depth++ {
		type hit struct {
			t     typ
			owner *declared[*ast.TypeSpec]
		}
		var hits []hit
		var next []typ
		for _, lt := range level {
			lit, own, how := w.under(lt)
			if how != resolved {
				read = max(read, how)
				continue
			}
			st, isStruct := lit.expr.(*ast.StructType)
			if !isStruct || seen[st] {
				continue
			}
			seen[st] = true
			for _, fl := range st.Fields.List {
				ftyp := lit.in(fl.Type)
				if len(fl.Names) == 0 {
					if baseName(fl.Type) == name {
						hits = append(hits, hit{ftyp, own})
					}
					next = append(next, ftyp)
					continue
				}
				for _, n := range fl.Names {
					if n.Name == name {
						hits = append(hits, hit{ftyp, own})
					}
				}
			}
		}
		if len(hits) == 1 {
			return hits[0].t, hits[0].owner, true, read
		}
		if len(hits) > 1 {
			// Ambiguous at one depth: the compiler refuses the selector, so
			// the walk has misread something.
			return typ{}, nil, false, unresolved
		}
		level = next
	}
	return typ{}, nil, false, read
}

// method finds the method name selects on t — declared on its defined type,
// listed by its interface, or promoted through embedding — and answers its
// signature, with a generic receiver's type parameters bound.
func (w *identityWorld) method(t typ, name string) (typ, bool) {
	level := []typ{t}
	for depth := 0; depth < 8 && len(level) > 0; depth++ {
		var next []typ
		for _, lt := range level {
			if d, inst, ok := w.definedType(lt); ok && d.file.pkg != nil {
				if m, ok := d.file.pkg.methods[d.decl.Name.Name][name]; ok {
					return typ{m.decl.Type, m.file, receiverSubst(m.decl, inst)}, true
				}
			}
			lit, _, how := w.under(lt)
			if how != resolved {
				continue
			}
			switch x := lit.expr.(type) {
			case *ast.InterfaceType:
				for _, fl := range x.Methods.List {
					if len(fl.Names) == 0 {
						next = append(next, lit.in(fl.Type))
						continue
					}
					for _, n := range fl.Names {
						if n.Name == name {
							return lit.in(fl.Type), true
						}
					}
				}
			case *ast.StructType:
				for _, fl := range x.Fields.List {
					if len(fl.Names) == 0 {
						next = append(next, lit.in(fl.Type))
					}
				}
			}
		}
		level = next
	}
	return typ{}, false
}

// receiverSubst binds a method's receiver type parameters — the names its
// receiver spells, `func (o op[A, R]) …` — to the instantiation it is
// selected on.
func receiverSubst(m *ast.FuncDecl, inst typ) subst {
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
			s[paramKey{receiverParams{}, id.Name}] = inst.in(args[i])
		}
	}
	return s
}

// definedType resolves t through pointers, bound type parameters and aliases
// to the defined type it names — the one its methods are declared on — and
// the instantiation that named it.
func (w *identityWorld) definedType(t typ) (declared[*ast.TypeSpec], typ, bool) {
	for range 32 {
		var ok bool
		if t, ok = w.bound(t); !ok {
			return declared[*ast.TypeSpec]{}, typ{}, false
		}
		switch x := t.expr.(type) {
		case *ast.ParenExpr:
			t = t.in(x.X)
			continue
		case *ast.StarExpr:
			t = t.in(x.X)
			continue
		}
		d, s, how := w.named(t)
		if how != resolved {
			return declared[*ast.TypeSpec]{}, typ{}, false
		}
		if !d.decl.Assign.IsValid() {
			return d, t, true
		}
		t = typ{d.decl.Type, d.file, s}
	}
	return declared[*ast.TypeSpec]{}, typ{}, false
}

// instance resolves t through bound type parameters and aliases to an
// instantiation of a generic type, and the declaration it instantiates.
func (w *identityWorld) instance(t typ) (declared[*ast.TypeSpec], typ, bool) {
	for range 32 {
		var ok bool
		if t, ok = w.bound(t); !ok {
			return declared[*ast.TypeSpec]{}, typ{}, false
		}
		t = t.in(ast.Unparen(t.expr))
		d, s, how := w.named(t)
		if how != resolved {
			return declared[*ast.TypeSpec]{}, typ{}, false
		}
		if typeArgs(t.expr) != nil || !d.decl.Assign.IsValid() {
			return d, t, true
		}
		t = typ{d.decl.Type, d.file, s}
	}
	return declared[*ast.TypeSpec]{}, typ{}, false
}

// elem is the type of an element of t: a slice's, an array's, a map's value or
// a channel's.
func (w *identityWorld) elem(t typ) typ {
	lit, _, how := w.under(t)
	if how != resolved {
		return typ{}
	}
	switch x := lit.expr.(type) {
	case *ast.ArrayType:
		return lit.in(x.Elt)
	case *ast.MapType:
		return lit.in(x.Value)
	case *ast.ChanType:
		return lit.in(x.Value)
	}
	return typ{}
}

// ranged is the type of a range clause's i-th variable over t.
func (w *identityWorld) ranged(t typ, i int) typ {
	lit, _, how := w.under(t)
	if how != resolved {
		return typ{}
	}
	switch x := lit.expr.(type) {
	case *ast.ArrayType:
		if i == 1 {
			return lit.in(x.Elt)
		}
	case *ast.MapType:
		if i == 0 {
			return lit.in(x.Key)
		}
		return lit.in(x.Value)
	case *ast.ChanType:
		if i == 0 {
			return lit.in(x.Value)
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
	return typ{}
}

// expand lists a field list's types, one per name, spelled where at is.
func expand(fl *ast.FieldList, at typ) []typ {
	if fl == nil {
		return nil
	}
	var out []typ
	for _, field := range fl.List {
		for range max(1, len(field.Names)) {
			out = append(out, at.in(field.Type))
		}
	}
	return out
}

// typeOf is the static type of e, spelled in f, or the zero typ where the walk
// cannot tell.
func (w *identityWorld) typeOf(e ast.Expr, f *identityFile) typ {
	w.depth++
	defer func() { w.depth-- }()
	if w.depth > 64 {
		return typ{}
	}
	switch x := e.(type) {
	case *ast.ParenExpr:
		return w.typeOf(x.X, f)
	case *ast.Ident:
		return w.identType(x, f)
	case *ast.SelectorExpr:
		if p, isPkg := w.packageNamed(x.X, f); isPkg {
			if p == nil {
				return typ{}
			}
			return w.packageValue(p, x.Sel.Name)
		}
		xt := w.typeOf(x.X, f)
		if !xt.ok() {
			return typ{}
		}
		if ft, _, ok, _ := w.field(xt, x.Sel.Name); ok {
			return ft
		}
		if m, ok := w.method(xt, x.Sel.Name); ok {
			return m
		}
	case *ast.CallExpr:
		if rs := w.results(x, f); len(rs) > 0 {
			return rs[0]
		}
	case *ast.StarExpr:
		return w.typeOf(x.X, f)
	case *ast.UnaryExpr:
		switch x.Op {
		case token.AND:
			return w.typeOf(x.X, f)
		case token.ARROW:
			return w.elem(w.typeOf(x.X, f))
		}
	case *ast.IndexExpr:
		return w.elem(w.typeOf(x.X, f))
	case *ast.SliceExpr:
		return w.typeOf(x.X, f)
	case *ast.TypeAssertExpr:
		if x.Type != nil {
			return typ{x.Type, f, nil}
		}
	case *ast.CompositeLit:
		if x.Type != nil {
			return typ{x.Type, f, nil}
		}
	case *ast.FuncLit:
		return typ{x.Type, f, nil}
	}
	return typ{}
}

// identType is the type of a name, by the declaration it names.
func (w *identityWorld) identType(id *ast.Ident, f *identityFile) typ {
	if id.Obj == nil {
		if f.pkg == nil {
			return typ{}
		}
		return w.packageValue(f.pkg, id.Name)
	}
	switch d := id.Obj.Decl.(type) {
	case *ast.Field:
		if id.Obj.Kind != ast.Var {
			return typ{}
		}
		if ell, ok := d.Type.(*ast.Ellipsis); ok {
			return typ{&ast.ArrayType{Elt: ell.Elt}, f, nil}
		}
		return typ{d.Type, f, nil}
	case *ast.ValueSpec:
		return w.specType(d, slices.IndexFunc(d.Names, func(n *ast.Ident) bool { return n.Name == id.Name }), f)
	case *ast.AssignStmt:
		return w.assignType(d, slices.IndexFunc(d.Lhs, func(l ast.Expr) bool {
			n, ok := l.(*ast.Ident)
			return ok && n.Name == id.Name
		}), f)
	case *ast.FuncDecl:
		return typ{d.Type, f, nil}
	}
	return typ{}
}

// packageValue is the type of a package-level var, const or function.
func (w *identityWorld) packageValue(p *identityPkg, name string) typ {
	if v, ok := p.values[name]; ok {
		return w.specType(v.spec, v.index, v.file)
	}
	if fn, ok := p.funcs[name]; ok {
		return typ{fn.decl.Type, fn.file, nil}
	}
	return typ{}
}

// specType is the type of the i-th name a var or const declaration declares.
func (w *identityWorld) specType(s *ast.ValueSpec, i int, f *identityFile) typ {
	switch {
	case i < 0:
		return typ{}
	case s.Type != nil:
		return typ{s.Type, f, nil}
	case len(s.Values) == len(s.Names):
		return w.typeOf(s.Values[i], f)
	case len(s.Values) == 1:
		return w.multi(s.Values[0], i, f)
	}
	return typ{}
}

// assignType is the type of the i-th variable a := declares.
func (w *identityWorld) assignType(a *ast.AssignStmt, i int, f *identityFile) typ {
	if i < 0 {
		return typ{}
	}
	if len(a.Rhs) == 1 {
		// The parser declares a range clause's variables as a := of a
		// unary RANGE over what is ranged.
		if u, ok := a.Rhs[0].(*ast.UnaryExpr); ok && u.Op == token.RANGE {
			return w.ranged(w.typeOf(u.X, f), i)
		}
	}
	if len(a.Rhs) == len(a.Lhs) {
		return w.typeOf(a.Rhs[i], f)
	}
	if len(a.Rhs) == 1 {
		return w.multi(a.Rhs[0], i, f)
	}
	return typ{}
}

// multi is the type of the i-th value of a multi-valued expression: a call's
// i-th result, or the value of a comma-ok form.
func (w *identityWorld) multi(e ast.Expr, i int, f *identityFile) typ {
	switch x := ast.Unparen(e).(type) {
	case *ast.CallExpr:
		if rs := w.results(x, f); i < len(rs) {
			return rs[i]
		}
	case *ast.TypeAssertExpr, *ast.IndexExpr, *ast.UnaryExpr:
		if i == 0 {
			return w.typeOf(x, f)
		}
	}
	return typ{}
}

// results is what a call evaluates to: a conversion's type, a builtin's, or
// the results the called function declares — a generic one's with its type
// parameters bound, explicitly or as the compiler infers them from the
// arguments.
func (w *identityWorld) results(c *ast.CallExpr, f *identityFile) []typ {
	fun := ast.Unparen(c.Fun)
	if w.isType(fun, f) {
		return []typ{{fun, f, nil}}
	}
	if id, ok := fun.(*ast.Ident); ok && id.Obj == nil && !w.declaresValue(f, id.Name) {
		switch id.Name {
		case "new", "make":
			if len(c.Args) > 0 {
				return []typ{{c.Args[0], f, nil}}
			}
		case "append", "min", "max":
			if len(c.Args) > 0 {
				return []typ{w.typeOf(c.Args[0], f)}
			}
		}
		return nil
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
		if lit, _, how := w.under(w.typeOf(x, f)); how == resolved {
			if ft, ok := lit.expr.(*ast.FuncType); ok && ft.TypeParams != nil {
				callee, explicit = x, args
			}
		}
	}
	lit, _, how := w.under(w.typeOf(callee, f))
	ft, ok := lit.expr.(*ast.FuncType)
	if how != resolved || !ok {
		return nil
	}
	s := subst{}
	maps.Copy(s, lit.subst)
	if params := typeParams(ft.TypeParams); len(params) > 0 {
		for i, p := range params {
			if i < len(explicit) {
				s[p] = typ{explicit[i], f, nil}
			}
		}
		w.infer(ft, lit.file, c, f, params, s)
	}
	return expand(ft.Results, typ{ft, lit.file, s})
}

// infer binds a generic function's type parameters from the arguments of a
// call of it, as the compiler infers them: each parameter's type unified with
// its argument's.
func (w *identityWorld) infer(ft *ast.FuncType, fnFile *identityFile, c *ast.CallExpr,
	f *identityFile, params []paramKey, s subst) {

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
		w.unify(p, fnFile, w.typeOf(a, f), want, s)
	}
}

// unify binds the type parameters p spells (those in want) to the parts of a
// they stand for.
func (w *identityWorld) unify(p ast.Expr, pf *identityFile, a typ, want map[paramKey]bool, s subst) {
	if !a.ok() {
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
			w.unify(x.X, pf, a.in(y.X), want, s)
		}
	case *ast.IndexExpr, *ast.IndexListExpr:
		// A generic type's instantiation: the argument instantiates the same
		// type, and its own type arguments bind these.
		pd, _, how := w.named(typ{x, pf, nil})
		ad, inst, ok := w.instance(a)
		if how != resolved || !ok || ad.decl != pd.decl {
			return
		}
		pargs, aargs := typeArgs(x), typeArgs(inst.expr)
		for i := range min(len(pargs), len(aargs)) {
			w.unify(pargs[i], pf, inst.in(aargs[i]), want, s)
		}
	case *ast.ArrayType, *ast.MapType, *ast.ChanType, *ast.FuncType:
		lit, _, how := w.under(a)
		if how != resolved {
			return
		}
		switch y := lit.expr.(type) {
		case *ast.ArrayType:
			if px, ok := x.(*ast.ArrayType); ok {
				w.unify(px.Elt, pf, lit.in(y.Elt), want, s)
			}
		case *ast.MapType:
			if px, ok := x.(*ast.MapType); ok {
				w.unify(px.Key, pf, lit.in(y.Key), want, s)
				w.unify(px.Value, pf, lit.in(y.Value), want, s)
			}
		case *ast.ChanType:
			if px, ok := x.(*ast.ChanType); ok {
				w.unify(px.Value, pf, lit.in(y.Value), want, s)
			}
		case *ast.FuncType:
			if px, ok := x.(*ast.FuncType); ok {
				for _, pair := range [][2]*ast.FieldList{{px.Params, y.Params}, {px.Results, y.Results}} {
					pp, ap := expand(pair[0], typ{}), expand(pair[1], lit)
					for i := range min(len(pp), len(ap)) {
						w.unify(pp[i].expr, pf, ap[i], want, s)
					}
				}
			}
		}
	}
}

// declaresValue reports whether f's package declares name at package level.
func (w *identityWorld) declaresValue(f *identityFile, name string) bool {
	if f.pkg == nil {
		return false
	}
	_, v := f.pkg.values[name]
	_, fn := f.pkg.funcs[name]
	return v || fn
}

// isType reports whether e, in a call's function position, names a type —
// which makes the call a conversion.
func (w *identityWorld) isType(e ast.Expr, f *identityFile) bool {
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
		if f.pkg != nil {
			if _, ok := f.pkg.types[x.Name]; ok {
				return true
			}
		}
		return predeclaredTypes[x.Name]
	case *ast.SelectorExpr:
		p, isPkg := w.packageNamed(x.X, f)
		if !isPkg || p == nil {
			return false
		}
		_, ok := p.types[x.Sel.Name]
		return ok
	}
	return false
}

// predeclaredTypes are the universe's type names, each a conversion when
// called.
var predeclaredTypes = map[string]bool{
	"bool": true, "byte": true, "complex64": true, "complex128": true, "error": true,
	"float32": true, "float64": true, "int": true, "int8": true, "int16": true,
	"int32": true, "int64": true, "rune": true, "string": true, "uint": true,
	"uint8": true, "uint16": true, "uint32": true, "uint64": true, "uintptr": true,
	"any": true, "comparable": true,
}

// declLabel is how a report names the declaration a selector sits in.
func declLabel(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) > 0 {
			return baseName(d.Recv.List[0].Type) + "." + d.Name.Name
		}
		return d.Name.Name
	case *ast.GenDecl:
		for _, spec := range d.Specs {
			switch s := spec.(type) {
			case *ast.ValueSpec:
				if len(s.Names) > 0 {
					return s.Names[0].Name
				}
			case *ast.TypeSpec:
				return s.Name.Name
			}
		}
	}
	return "?"
}

// exprString renders a short expression for a report.
func exprString(e ast.Expr) string {
	var b strings.Builder
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		switch x := e.(type) {
		case *ast.Ident:
			b.WriteString(x.Name)
		case *ast.SelectorExpr:
			walk(x.X)
			b.WriteString("." + x.Sel.Name)
		case *ast.CallExpr:
			walk(x.Fun)
			b.WriteString("(…)")
		case *ast.IndexExpr:
			walk(x.X)
			b.WriteString("[…]")
		case *ast.ParenExpr:
			walk(x.X)
		case *ast.StarExpr:
			b.WriteString("*")
			walk(x.X)
		default:
			b.WriteString("…")
		}
	}
	walk(e)
	return b.String()
}

func countOf(vs []string) map[string]int {
	out := map[string]int{}
	for _, v := range vs {
		out[v]++
	}
	return out
}

func mergeKeys(a, b map[string]int) map[string]int {
	out := maps.Clone(a)
	for k := range b {
		out[k] = 0
	}
	return out
}
