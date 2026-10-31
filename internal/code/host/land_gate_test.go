package host_test

import (
	"fmt"
	"go/ast"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// TestEveryPushTakesTheOneLanding fails the build when the host reaches the
// steps that make a push — its proposal, its bundle's upload and its pack's
// adoption — or a MERGE'S PROPOSAL (Estate.Merge: a merge_change lands through
// the same landing, M5-D11) anywhere but through the one landing (land.go).
//
// A push's facts are formed by its landing: Command.Forced only by its force
// check, which protected_force turns on; the blob scan, --atomic, the size,
// the seal, the wait for an applied row. A second path to Estate.Propose or
// Estate.Merge, to the bundle's store (code.StoreBundle) or to Cache.Adopt is
// a second implementation of what a push is (ADR-0008) — the first
// engine-built write that proposed by itself would skip whichever check its
// author forgot, and pass every test written for it. So each door is SELECTED EXACTLY
// ONCE in the package's non-test files, and that selection is written in land
// or in a function only land reaches: a function every reference to which — a
// call, a method value, a closure handed on — is written in land or in
// another such function.
//
// # By declaration, never by name
//
// Each selector is typed through internal/sourcetree/decltype, so what is
// judged is the method a selector RESOLVES to: Estate.Propose is the method
// this package's own interface declares, Cache.Adopt the method
// internal/code/cache declares, and the bundle's store code.Putter's Put,
// through whatever field, variable, parameter or embedding the value is held
// in — a field renamed, or a second field of the same type, is the same door.
// A method of that name selected through ANY interface is counted as the door
// too, since an interface value of another type may hold the same
// implementation. The store has a second spelling, and both are the one door:
// code.StoreBundle, the step's one implementation, which makes the Put and
// seals what it stores — a reference to it, a call or a function value, is a
// selection of the store as much as a Put the host made itself, which would be
// the same step with no seal. A selector of one of the four method names the
// walk cannot type is reported rather than passed, and so is a reference it
// cannot place to a method this package declares: a gate asserting where
// something is NOT that skipped what it could not read would certify nothing
// about exactly what it skipped.
//
// # Coverage boundary
//
// A concrete implementation's own method selected directly — an
// *objstore.Store held as one rather than as a code.Putter — is not seen: the
// host is handed its store, its estate and its cache only through the three
// declared types. A value reached through reflection is not seen either, nor
// is code.StoreBundle reached through a dot import.
func TestEveryPushTakesTheOneLanding(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	found := walkForLandings(root, modulePath)
	if found.files == 0 {
		t.Fatal("read no source file under internal/code/host — this gate was certifying nothing")
	}
	for _, f := range found.findings {
		t.Error(f)
	}
	t.Logf("read %d files of internal/code/host: %d function(s) only land reaches", found.files, found.reached)
}

// modulePath is this module's import path.
const modulePath = "github.com/crewlet/crewlet"

// hostDir is the package the gate judges, relative to the module root.
const hostDir = "internal/code/host"

// landingDoor is one step only the landing takes: a method of a declared
// type, and — where the step has one — the function of the same package that
// takes it whole by calling that method.
type landingDoor struct {
	owner  decltype.TypeName
	method string
	// fn is the function of owner's package that IS the step — the
	// bundle's store, code.StoreBundle, whose Put on the code.Putter it is
	// handed is the upload — or "" where the method is its one spelling. A
	// reference to fn and a selection of the method are the same door.
	fn string
}

// String is what a finding calls the door: its function where it has one,
// as a file importing the package names it, else its method.
func (d landingDoor) String() string {
	if d.fn != "" {
		return path.Base(d.owner.Package) + "." + d.fn
	}
	return d.owner.Name + "." + d.method
}

// doorsOf are the four doors under module.
func doorsOf(module string) []landingDoor {
	host, domain, cache := module+"/"+hostDir, module+"/internal/code", module+"/internal/code/cache"
	return []landingDoor{
		{owner: decltype.TypeName{Package: host, Name: "Estate"}, method: "Propose"},
		// A MERGE'S PROPOSAL (M5-D11): a merge_change lands through the
		// same landing — its blobs scanned, its size and pack bound held,
		// its bundle stored and its pack adopted as a push's — so its record
		// is a door only land takes, as a push's proposal is.
		{owner: decltype.TypeName{Package: host, Name: "Estate"}, method: "Merge"},
		{owner: decltype.TypeName{Package: domain, Name: "Putter"}, method: "Put", fn: "StoreBundle"},
		{owner: decltype.TypeName{Package: cache, Name: "Cache"}, method: "Adopt"},
	}
}

// landFunc is the landing's name as the walk names a function.
const landFunc = "Server.land"

// landingWalk is what the walk concluded.
type landingWalk struct {
	// files is how many non-test files it read; reached how many functions
	// only land reaches, land itself included.
	files, reached int
	findings       []string
}

// doorSite is one selection of a door.
type doorSite struct {
	fn, pos string
}

// walkForLandings judges every non-test Go file of module's internal/code/host
// under root.
func walkForLandings(root, module string) landingWalk {
	var found landingWalk
	report := func(format string, args ...any) {
		found.findings = append(found.findings, fmt.Sprintf(format, args...))
	}
	w := decltype.New(root, module, nil)
	dir := filepath.Join(root, filepath.FromSlash(hostDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		report("read %s: %v", hostDir, err)
		return found
	}
	var files []*decltype.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		// Every file, wherever it is built: one this platform does not
		// build is read against the scope of the files it does.
		if f := w.FileAt(filepath.Join(dir, name)); f != nil {
			files = append(files, f)
		}
	}
	found.files = len(files)
	for _, err := range w.Errors() {
		report("%v", err)
	}
	if len(files) == 0 {
		return found
	}
	hostPath := files[0].Package.Path

	// What the package declares: its functions and the names of its
	// methods, which a reference is placed against.
	funcs, methodNames := map[string]bool{}, map[string]bool{}
	for _, f := range files {
		for _, d := range f.AST.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok {
				if fd.Recv == nil || len(fd.Recv.List) == 0 {
					funcs[fd.Name.Name] = true
				} else {
					methodNames[fd.Name.Name] = true
				}
			}
		}
	}
	doors := doorsOf(module)
	sites := map[landingDoor][]doorSite{}
	// refs is, for each function of the package, every function its
	// references are written in ("" is package level).
	refs := map[string][]string{}
	declared := map[string]bool{}

	for _, f := range files {
		pos := func(n ast.Node) string {
			p := w.FileSet().Position(n.Pos())
			return fmt.Sprintf("%s:%d:%d", f.Rel, p.Line, p.Column)
		}
		visit := func(fn string) func(ast.Node) bool {
			var v func(ast.Node) bool
			v = func(n ast.Node) bool {
				switch n := n.(type) {
				case *ast.SelectorExpr:
					ast.Inspect(n.X, v)
					if imported, isPkg := w.PackageNamed(n.X, f); isPkg {
						// A package's function: a door where it is the
						// function that takes a step whole.
						for _, d := range doors {
							if d.fn != "" && d.fn == n.Sel.Name && d.owner.Package == imported {
								sites[d] = append(sites[d], doorSite{fn: fn, pos: pos(n)})
							}
						}
						return false
					}
					name := n.Sel.Name
					sel := w.Select(w.TypeOf(n.X, f), name)
					isDoorName := slices.ContainsFunc(doors, func(d landingDoor) bool { return d.method == name })
					switch {
					case sel.Kind == decltype.Method && isDoorName &&
						(sel.Interface || slices.ContainsFunc(doors, func(d landingDoor) bool { return d.owner == sel.Owner })):
						for _, d := range doors {
							if d.method == name {
								sites[d] = append(sites[d], doorSite{fn: fn, pos: pos(n)})
							}
						}
					case sel.Kind == decltype.Method && sel.Owner.Package == hostPath && !sel.Interface:
						refs[sel.Owner.Name+"."+name] = append(refs[sel.Owner.Name+"."+name], fn)
					case (sel.Kind == decltype.Undetermined || sel.Kind == decltype.Absent) &&
						(isDoorName || methodNames[name]):
						report("%s: the walk cannot type %s.%s, which may be a push's step or a reference "+
							"to a function only the landing may reach: spell its type where it is declared, "+
							"or teach decltype the shape", pos(n), exprString(n.X), name)
					}
					return false
				case *ast.KeyValueExpr:
					// A struct literal's key is a field, not a reference.
					if _, isField := n.Key.(*ast.Ident); !isField {
						ast.Inspect(n.Key, v)
					}
					ast.Inspect(n.Value, v)
					return false
				case *ast.Ident:
					if (n.Obj == nil || n.Obj.Kind == ast.Fun) && funcs[n.Name] {
						refs[n.Name] = append(refs[n.Name], fn)
					}
				}
				return true
			}
			return v
		}
		for _, d := range f.AST.Decls {
			switch d := d.(type) {
			case *ast.FuncDecl:
				fn := d.Name.Name
				if d.Recv != nil && len(d.Recv.List) > 0 {
					fn = decltype.BaseName(d.Recv.List[0].Type) + "." + fn
				}
				declared[fn] = true
				if d.Body != nil {
					ast.Inspect(d.Body, visit(fn))
				}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					if vs, ok := spec.(*ast.ValueSpec); ok {
						for _, value := range vs.Values {
							ast.Inspect(value, visit(""))
						}
					}
				}
			}
		}
	}

	// ONLY LAND REACHES a function every reference to which is written in
	// land or in another function only land reaches — a fixed point from
	// land, a function's references to itself left out.
	if !declared[landFunc] {
		report("%s declares no %s: the one landing every push takes is gone", hostDir, landFunc)
		return found
	}
	only := map[string]bool{landFunc: true}
	for grew := true; grew; {
		grew = false
		for fn := range declared {
			if only[fn] {
				continue
			}
			from := slices.DeleteFunc(slices.Clone(refs[fn]), func(r string) bool { return r == fn })
			if len(from) > 0 && !slices.ContainsFunc(from, func(r string) bool { return !only[r] }) {
				only[fn], grew = true, true
			}
		}
	}
	found.reached = len(only)
	for _, d := range doors {
		got := sites[d]
		switch {
		case len(got) == 0:
			report("%s selects %s nowhere: a push's step that the landing takes is gone, or this walk "+
				"stopped seeing it", hostDir, d)
		case len(got) > 1:
			var where []string
			for _, s := range got {
				where = append(where, s.pos+" ("+orPackageLevel(s.fn)+")")
			}
			report("%s is selected %d times (%s): a push's step is taken once, in the one landing "+
				"(%s), or a second path forms what a push is", d, len(got), strings.Join(where, ", "), landFunc)
		}
		for _, s := range got {
			if !only[s.fn] {
				report("%s: %s is selected in %s, which %s does not reach alone: a push's step is "+
					"taken by the landing, whoever built the push (ADR-0008)", s.pos, d, orPackageLevel(s.fn),
					landFunc)
			}
		}
	}
	return found
}

// orPackageLevel names a function, or the package level for "".
func orPackageLevel(fn string) string {
	if fn == "" {
		return "a package-level declaration"
	}
	return fn
}

// exprString is an expression as written, for a finding.
func exprString(e ast.Expr) string {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return exprString(e.X) + "." + e.Sel.Name
	case *ast.CallExpr:
		return exprString(e.Fun) + "(…)"
	case *ast.ParenExpr:
		return "(" + exprString(e.X) + ")"
	case *ast.StarExpr:
		return "*" + exprString(e.X)
	}
	return fmt.Sprintf("%T", e)
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN. A fixture host whose landing
// takes each step once — through a helper, a closure, the store's one
// function — passes; and each way a second path to a step can be written must
// be reported: the adoption taken again in the relay, a proposal through a
// second field of the estate's type, a method value taken outside the
// landing, an upload through an interface of its own or a Put on the store
// itself beside the store's function, that function called again or held as
// a value outside the landing, or reached through an import of another name,
// a helper of the landing's that something else reaches too, a step at
// package level, a step gone, the landing gone, and a selector of a door's
// name the walk cannot type. A test file is never read.
func TestTheLandingWalkFindsEverySecondPath(t *testing.T) {
	t.Parallel()
	const module = "example.com/m"
	base := `package host

import (
	"context"

	"example.com/m/internal/code"
	"example.com/m/internal/code/cache"
)

type Estate interface{ Propose(ctx context.Context, n int) (int, error) }

type Options struct {
	Estate  Estate
	Spare   Estate
	Objects code.Putter
	Cache   *cache.Cache
}

type Server struct{ o Options }

func (s *Server) land(ctx context.Context) {
	_, _ = s.o.Estate.Propose(ctx, 1)
	s.store(ctx)
	adopt := func() { _ = s.o.Cache.Adopt(ctx) }
	adopt()
}

func (s *Server) store(ctx context.Context) { _ = code.StoreBundle(ctx, s.o.Objects) }

func (s *Server) serveSession(ctx context.Context) { s.land(ctx) }
`
	for _, tc := range []struct {
		name  string
		edit  func(src string) string
		extra string
		want  []string
	}{
		{name: "one landing", edit: func(s string) string { return s }},
		{name: "the adoption taken again in the relay", edit: func(s string) string {
			return strings.Replace(s, "func (s *Server) serveSession(ctx context.Context) { s.land(ctx) }",
				"func (s *Server) serveSession(ctx context.Context) { s.land(ctx); _ = s.o.Cache.Adopt(ctx) }", 1)
		}, want: []string{"Cache.Adopt is selected 2 times", "selected in Server.serveSession"}},
		{name: "a proposal through a second field", edit: func(s string) string {
			return strings.Replace(s, "func (s *Server) serveSession(ctx context.Context) { s.land(ctx) }",
				"func (s *Server) serveSession(ctx context.Context) { s.land(ctx); _, _ = s.o.Spare.Propose(ctx, 2) }", 1)
		}, want: []string{"Estate.Propose is selected 2 times", "selected in Server.serveSession"}},
		{name: "a method value outside the landing", extra: `
func newServer(o Options) func(context.Context, int) (int, error) { return o.Estate.Propose }
`, want: []string{"Estate.Propose is selected 2 times", "selected in newServer"}},
		{name: "an upload through an interface of its own", extra: `
type putter interface{ Put(ctx context.Context) error }

func (s *Server) sideways(ctx context.Context) {
	var p putter = s.o.Objects
	_ = p.Put(ctx)
}
`, want: []string{"code.StoreBundle is selected 2 times", "selected in Server.sideways"}},
		{name: "a put on the store beside its function", extra: `
func (s *Server) direct(ctx context.Context) { _ = s.o.Objects.Put(ctx) }
`, want: []string{"code.StoreBundle is selected 2 times", "selected in Server.direct"}},
		{name: "the store's function called outside the landing", extra: `
import "example.com/m/internal/code"

func (s *Server) again(ctx context.Context) { _ = code.StoreBundle(ctx, s.o.Objects) }
`, want: []string{"code.StoreBundle is selected 2 times", "selected in Server.again"}},
		{name: "the store's function as a value at package level", extra: `
import "example.com/m/internal/code"

var later = code.StoreBundle
`, want: []string{"code.StoreBundle is selected 2 times", "selected in a package-level declaration"}},
		{name: "the store's function under another name", extra: `
import bundles "example.com/m/internal/code"

func (s *Server) renamed(ctx context.Context) { _ = bundles.StoreBundle(ctx, s.o.Objects) }
`, want: []string{"code.StoreBundle is selected 2 times", "selected in Server.renamed"}},
		{name: "no store anywhere", edit: func(s string) string {
			return strings.Replace(s, "_ = code.StoreBundle(ctx, s.o.Objects)", "", 1)
		}, want: []string{"selects code.StoreBundle nowhere"}},
		{name: "a helper something else reaches", extra: `
func (s *Server) other(ctx context.Context) { s.store(ctx) }
`, want: []string{"selected in Server.store, which Server.land does not reach alone"}},
		{name: "a helper handed on as a value", extra: `
func (s *Server) later() func(context.Context) { return s.store }
`, want: []string{"selected in Server.store, which Server.land does not reach alone"}},
		{name: "a step at package level", extra: `
var spare Options

var sneak = spare.Cache.Adopt
`, want: []string{"Cache.Adopt is selected 2 times", "selected in a package-level declaration"}},
		{name: "no adoption anywhere", edit: func(s string) string {
			return strings.Replace(s, "	adopt := func() { _ = s.o.Cache.Adopt(ctx) }\n	adopt()\n", "", 1)
		}, want: []string{"selects Cache.Adopt nowhere"}},
		{name: "the landing gone", edit: func(s string) string {
			return strings.ReplaceAll(s, "land(", "decide(")
		}, want: []string{"declares no Server.land"}},
		{name: "a door the walk cannot type", extra: `
func (s *Server) opaque(ctx context.Context) { _ = mystery().Adopt(ctx) }
`, want: []string{"cannot type mystery(…).Adopt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			src := base
			if tc.edit != nil {
				src = tc.edit(src)
			}
			write := func(rel, body string) {
				file := filepath.Join(root, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("internal/code/host/host.go", src)
			if tc.extra != "" {
				write("internal/code/host/extra.go", "package host\n\nimport \"context\"\n"+tc.extra)
			}
			write("internal/code/host/host_test.go", "package host\n\nimport \"context\"\n\n"+
				"func (s *Server) tested(ctx context.Context) { _ = s.o.Cache.Adopt(ctx) }\n")
			write("internal/code/cache/cache.go", "package cache\n\nimport \"context\"\n\n"+
				"type Cache struct{}\n\nfunc (c *Cache) Adopt(ctx context.Context) error { return nil }\n")
			// The store's one function, whose Put is the upload — in a package
			// the walk reads for the Putter's type, never judges.
			write("internal/code/code.go", "package code\n\nimport \"context\"\n\n"+
				"type Putter interface{ Put(ctx context.Context) error }\n\n"+
				"func StoreBundle(ctx context.Context, p Putter) error { return p.Put(ctx) }\n")
			found := walkForLandings(root, module)
			if found.files != 1+min(1, len(tc.extra)) {
				t.Errorf("the walk read %d files, want the package's non-test ones", found.files)
			}
			if len(tc.want) == 0 && len(found.findings) > 0 {
				t.Errorf("reported %q on a tree that takes every step once, in its landing", found.findings)
			}
			for _, want := range tc.want {
				if !slices.ContainsFunc(found.findings, func(f string) bool { return strings.Contains(f, want) }) {
					t.Errorf("no finding says %q; found %q", want, found.findings)
				}
			}
		})
	}
}
