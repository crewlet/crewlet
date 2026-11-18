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

// TestEveryPushTakesTheOneLanding holds a push's steps to the one landing
// (package doc, "One landing, whoever built the push"; ADR-0008): Estate.Propose,
// Estate.Merge (a merge_change lands as a push, M5-D11), the bundle's store
// (gitlet.Putter's Put, or gitlet.StoreBundle, which also seals) and Cache.Adopt
// are each selected once in non-test files, in land or a function only land
// reaches (walkForLandings). The chain (Server.BuildChain) proposes nothing but
// stores and adopts a candidate's pack, each once in what it alone reaches; a
// helper both reach is neither's. A selector is judged by the method it
// resolves to (decltype), any interface's method of a door's name included, as
// it may hold the same implementation; one the walk cannot type or place is
// reported, never passed. Not seen: a concrete store's own method (the host
// holds only the declared types), reflection, a dot import of gitlet.
func TestEveryPushTakesTheOneLanding(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	found := walkForLandings(root, modulePath)
	if found.files == 0 {
		t.Fatal("read no source file under internal/gitlet/host — this gate was certifying nothing")
	}
	for _, f := range found.findings {
		t.Error(f)
	}
	t.Logf("read %d files of internal/gitlet/host: %d function(s) only land reaches, %d only the chain does",
		found.files, found.reached[landFunc], found.reached[chainFunc])
}

// modulePath is this module's import path.
const modulePath = "github.com/crewlet/crewlet"

// hostDir is the package the gate judges, relative to the module root.
const hostDir = "internal/gitlet/host"

// landingDoor is one step of a push: a method of a declared type and, where the
// step has one, the function of owner's package that takes it whole.
type landingDoor struct {
	owner  decltype.TypeName
	method string
	// fn is that function (gitlet.StoreBundle, whose Put is the upload), or ""
	// where the method is the one spelling; a reference to fn selects the door.
	fn string
	// chain is whether chainFunc takes the door too: the store and the
	// adoption, never a proposal.
	chain bool
}

// takers are the functions that take d, each once in what it alone reaches:
// the landing, and the chain where d is one of its steps.
func (d landingDoor) takers() []string {
	if d.chain {
		return []string{landFunc, chainFunc}
	}
	return []string{landFunc}
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
	host, domain, cache := module+"/"+hostDir, module+"/internal/gitlet", module+"/internal/gitlet/cache"
	return []landingDoor{
		{owner: decltype.TypeName{Package: host, Name: "Estate"}, method: "Propose"},
		// A merge_change lands through the same landing as a push (M5-D11).
		{owner: decltype.TypeName{Package: host, Name: "Estate"}, method: "Merge"},
		// The chain stores and adopts a candidate's pack too (chain.go).
		{owner: decltype.TypeName{Package: domain, Name: "Putter"}, method: "Put", fn: "StoreBundle", chain: true},
		{owner: decltype.TypeName{Package: cache, Name: "Cache"}, method: "Adopt", chain: true},
	}
}

// landFunc is the landing's name as the walk names a function.
const landFunc = "Server.land"

// chainFunc is the chain's build as the walk names a function: the one other
// taker of the bundle's store and the adoption, which proposes nothing.
const chainFunc = "Server.BuildChain"

// landingWalk is what the walk concluded.
type landingWalk struct {
	// files is how many non-test files it read.
	files int
	// reached is, for the landing and the chain, how many functions it
	// alone reaches, itself included.
	reached  map[string]int
	findings []string
}

// doorSite is one selection of a door.
type doorSite struct {
	fn, pos string
}

// walkForLandings judges every non-test Go file of module's internal/gitlet/host
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

	// Only land reaches a function every reference to which is in land or
	// another function only land reaches (a fixed point, self-references left
	// out); likewise the chain. A function both reach is neither's alone.
	if !declared[landFunc] {
		report("%s declares no %s: the one landing every push takes is gone", hostDir, landFunc)
		return found
	}
	if !declared[chainFunc] {
		report("%s declares no %s: the chain a queue's candidate is built by is gone, or renamed past "+
			"this gate", hostDir, chainFunc)
	}
	only := map[string]map[string]bool{}
	found.reached = map[string]int{}
	for _, root := range []string{landFunc, chainFunc} {
		reach := map[string]bool{}
		if declared[root] {
			reach[root] = true
		}
		for grew := len(reach) > 0; grew; {
			grew = false
			for fn := range declared {
				if reach[fn] {
					continue
				}
				from := slices.DeleteFunc(slices.Clone(refs[fn]), func(r string) bool { return r == fn })
				if len(from) > 0 && !slices.ContainsFunc(from, func(r string) bool { return !reach[r] }) {
					reach[fn], grew = true, true
				}
			}
		}
		only[root], found.reached[root] = reach, len(reach)
	}
	for _, d := range doors {
		got := sites[d]
		takers := d.takers()
		where := func(sites []doorSite) string {
			var at []string
			for _, s := range sites {
				at = append(at, s.pos+" ("+orPackageLevel(s.fn)+")")
			}
			return strings.Join(at, ", ")
		}
		if len(got) == 0 {
			report("%s selects %s nowhere: a push's step that the landing takes is gone, or this walk "+
				"stopped seeing it", hostDir, d)
			continue
		}
		once, by := "", ""
		if d.chain {
			once, by = ", and a candidate's once, in its chain ("+chainFunc+")", ", and a candidate's by its chain"
		}
		if len(got) > len(takers) {
			report("%s is selected %d times (%s): a push's step is taken once, in the one landing (%s)%s, "+
				"or a second path forms what a push is", d, len(got), where(got), landFunc, once)
		}
		for _, taker := range takers {
			var mine []doorSite
			for _, s := range got {
				if only[taker][s.fn] {
					mine = append(mine, s)
				}
			}
			switch {
			case len(mine) == 0 && declared[taker]:
				report("%s selects %s nowhere that %s alone reaches: its step is taken by a path something "+
					"else reaches too, or is gone from it", hostDir, d, taker)
			case len(mine) > 1:
				report("%s is selected %d times in what %s alone reaches (%s): its step is taken once there",
					d, len(mine), taker, where(mine))
			}
		}
		for _, s := range got {
			if !slices.ContainsFunc(takers, func(taker string) bool { return only[taker][s.fn] }) {
				report("%s: %s is selected in %s, which %s: a push's step is taken by the landing, "+
					"whoever built the push (ADR-0008)%s", s.pos, d, orPackageLevel(s.fn), notReachedAlone(takers),
					by)
			}
		}
	}
	return found
}

// notReachedAlone says that none of takers reaches a function alone, naming
// them: "Server.land does not reach alone", or "neither Server.land nor
// Server.BuildChain reaches alone".
func notReachedAlone(takers []string) string {
	if len(takers) == 1 {
		return takers[0] + " does not reach alone"
	}
	return "neither " + strings.Join(takers, " nor ") + " reaches alone"
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

// TestTheLandingWalkFindsEverySecondPath runs the walk on a fixture whose
// verdict is known: a landing taking each step once (through a helper, a
// closure, the store's function) passes, and every second path is reported —
// the adoption again in the relay, a proposal through a second estate field, a
// merge outside the landing, a method value, an upload through an interface of
// its own or a direct Put, the store's function called again, held as a value
// or imported under another name, a landing helper something else reaches, a
// step at package level, a step, the landing or the chain gone, an untypeable
// door selector; and from the chain, a proposal, a second store, a helper
// shared with the landing, or the landing's store gone with the chain's kept.
// A test file is never read.
func TestTheLandingWalkFindsEverySecondPath(t *testing.T) {
	t.Parallel()
	const module = "example.com/m"
	base := `package host

import (
	"context"

	"example.com/m/internal/gitlet"
	"example.com/m/internal/gitlet/cache"
)

type Estate interface {
	Propose(ctx context.Context, n int) (int, error)
	Merge(ctx context.Context, n int) (int, error)
}

type Options struct {
	Estate  Estate
	Spare   Estate
	Objects gitlet.Putter
	Cache   *cache.Cache
}

type Server struct{ o Options }

func (s *Server) land(ctx context.Context) {
	_, _ = s.o.Estate.Propose(ctx, 1)
	_, _ = s.o.Estate.Merge(ctx, 1)
	s.store(ctx)
	adopt := func() { _ = s.o.Cache.Adopt(ctx) }
	adopt()
}

func (s *Server) store(ctx context.Context) { _ = gitlet.StoreBundle(ctx, s.o.Objects) }

func (s *Server) serveSession(ctx context.Context) { s.land(ctx) }

func (s *Server) BuildChain(ctx context.Context) {
	s.storeChain(ctx)
	settle := func() { _ = s.o.Cache.Adopt(ctx) }
	settle()
}

func (s *Server) storeChain(ctx context.Context) { _ = gitlet.StoreBundle(ctx, s.o.Objects) }
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
		}, want: []string{"Cache.Adopt is selected 3 times", "selected in Server.serveSession"}},
		{name: "a proposal through a second field", edit: func(s string) string {
			return strings.Replace(s, "func (s *Server) serveSession(ctx context.Context) { s.land(ctx) }",
				"func (s *Server) serveSession(ctx context.Context) { s.land(ctx); _, _ = s.o.Spare.Propose(ctx, 2) }", 1)
		}, want: []string{"Estate.Propose is selected 2 times", "selected in Server.serveSession"}},
		{name: "a merge proposed outside the landing", edit: func(s string) string {
			return strings.Replace(s, "func (s *Server) serveSession(ctx context.Context) { s.land(ctx) }",
				"func (s *Server) serveSession(ctx context.Context) { s.land(ctx); _, _ = s.o.Estate.Merge(ctx, 2) }", 1)
		}, want: []string{"Estate.Merge is selected 2 times", "selected in Server.serveSession"}},
		{name: "no merge anywhere", edit: func(s string) string {
			return strings.Replace(s, "	_, _ = s.o.Estate.Merge(ctx, 1)\n", "", 1)
		}, want: []string{"selects Estate.Merge nowhere"}},
		{name: "a method value outside the landing", extra: `
func newServer(o Options) func(context.Context, int) (int, error) { return o.Estate.Propose }
`, want: []string{"Estate.Propose is selected 2 times", "selected in newServer"}},
		{name: "an upload through an interface of its own", extra: `
type putter interface{ Put(ctx context.Context) error }

func (s *Server) sideways(ctx context.Context) {
	var p putter = s.o.Objects
	_ = p.Put(ctx)
}
`, want: []string{"gitlet.StoreBundle is selected 3 times", "selected in Server.sideways"}},
		{name: "a put on the store beside its function", extra: `
func (s *Server) direct(ctx context.Context) { _ = s.o.Objects.Put(ctx) }
`, want: []string{"gitlet.StoreBundle is selected 3 times", "selected in Server.direct"}},
		{name: "the store's function called outside the landing", extra: `
import "example.com/m/internal/gitlet"

func (s *Server) again(ctx context.Context) { _ = gitlet.StoreBundle(ctx, s.o.Objects) }
`, want: []string{"gitlet.StoreBundle is selected 3 times", "selected in Server.again"}},
		{name: "the store's function as a value at package level", extra: `
import "example.com/m/internal/gitlet"

var later = gitlet.StoreBundle
`, want: []string{"gitlet.StoreBundle is selected 3 times", "selected in a package-level declaration"}},
		{name: "the store's function under another name", extra: `
import bundles "example.com/m/internal/gitlet"

func (s *Server) renamed(ctx context.Context) { _ = bundles.StoreBundle(ctx, s.o.Objects) }
`, want: []string{"gitlet.StoreBundle is selected 3 times", "selected in Server.renamed"}},
		{name: "no store anywhere", edit: func(s string) string {
			return strings.ReplaceAll(s, "_ = gitlet.StoreBundle(ctx, s.o.Objects)", "")
		}, want: []string{"selects gitlet.StoreBundle nowhere: a push's step"}},
		{name: "the landing's store gone, the chain's kept", edit: func(s string) string {
			return strings.Replace(s, "_ = gitlet.StoreBundle(ctx, s.o.Objects)", "", 1)
		}, want: []string{"selects gitlet.StoreBundle nowhere that Server.land alone reaches"}},
		{name: "a proposal from the chain", edit: func(s string) string {
			return strings.Replace(s, "	s.storeChain(ctx)\n",
				"	s.storeChain(ctx)\n	_, _ = s.o.Estate.Propose(ctx, 3)\n", 1)
		}, want: []string{"Estate.Propose is selected 2 times",
			"selected in Server.BuildChain, which Server.land does not reach alone"}},
		{name: "the chain's store taken twice", edit: func(s string) string {
			return strings.Replace(s, "	s.storeChain(ctx)\n",
				"	s.storeChain(ctx)\n	_ = gitlet.StoreBundle(ctx, s.o.Objects)\n", 1)
		}, want: []string{"gitlet.StoreBundle is selected 3 times",
			"gitlet.StoreBundle is selected 2 times in what Server.BuildChain alone reaches"}},
		{name: "a helper the landing and the chain share", edit: func(s string) string {
			return strings.Replace(s, "	s.storeChain(ctx)\n", "	s.store(ctx)\n", 1)
		}, want: []string{"selected in Server.store, which neither Server.land nor Server.BuildChain reaches alone"}},
		{name: "the chain gone", edit: func(s string) string {
			return strings.ReplaceAll(s, "BuildChain(", "buildCandidate(")
		}, want: []string{"declares no Server.BuildChain"}},
		{name: "a helper something else reaches", extra: `
func (s *Server) other(ctx context.Context) { s.store(ctx) }
`, want: []string{"selected in Server.store, which neither Server.land nor Server.BuildChain reaches alone"}},
		{name: "a helper handed on as a value", extra: `
func (s *Server) later() func(context.Context) { return s.store }
`, want: []string{"selected in Server.store, which neither Server.land nor Server.BuildChain reaches alone"}},
		{name: "a step at package level", extra: `
var spare Options

var sneak = spare.Cache.Adopt
`, want: []string{"Cache.Adopt is selected 3 times", "selected in a package-level declaration"}},
		{name: "no adoption anywhere", edit: func(s string) string {
			s = strings.Replace(s, "	adopt := func() { _ = s.o.Cache.Adopt(ctx) }\n	adopt()\n", "", 1)
			return strings.Replace(s, "	settle := func() { _ = s.o.Cache.Adopt(ctx) }\n	settle()\n", "", 1)
		}, want: []string{"selects Cache.Adopt nowhere: a push's step"}},
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
			write("internal/gitlet/host/host.go", src)
			if tc.extra != "" {
				write("internal/gitlet/host/extra.go", "package host\n\nimport \"context\"\n"+tc.extra)
			}
			write("internal/gitlet/host/host_test.go", "package host\n\nimport \"context\"\n\n"+
				"func (s *Server) tested(ctx context.Context) { _ = s.o.Cache.Adopt(ctx) }\n")
			write("internal/gitlet/cache/cache.go", "package cache\n\nimport \"context\"\n\n"+
				"type Cache struct{}\n\nfunc (c *Cache) Adopt(ctx context.Context) error { return nil }\n")
			// The store's one function, whose Put is the upload — in a package
			// the walk reads for the Putter's type, never judges.
			write("internal/gitlet/code.go", "package gitlet\n\nimport \"context\"\n\n"+
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
