package decltype_test

import (
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN. Every expression a gate types
// is spelled in the fixture as a probe carrying its own verdict —
// want(EXPR, "verdict") for a value's type, sel(EXPR, "Name", "verdict") for
// what a selector resolves to, elided(EXPR, "verdict", …) for each literal in
// EXPR whose type its collection elides — so the table is the fixture, and a
// probe the walk answers differently names itself. The shapes: locals of every kind
// (parameters, variadics, :=, var, range clauses over every collection and
// an iterator, comma-ok), package values in another file and another
// package, fields and methods promoted through embedding and through an
// interface embedding one of another package, method values, method
// expressions on a type this file, another file of its package, another
// package and an unread one declares, calls of function-typed fields,
// conversions, composite literals — one whose collection's literal elides
// its type, as an element, a value or a key, included — elements, basic
// literals, comparisons, generics bound by instantiation, explicit
// arguments, inference and a generic receiver, and the type parameters
// nothing binds — a function's, and a generic method's receiver's, which the
// parser gives no object and which shadows a package type of its name; a package outside the module
// read from a directory it was given; values only a package the walk does not
// read can type, its import named by every form of the rule; and what it
// cannot type at all. A file this platform does not build is read against
// the scope of the files it does, and never adds to that scope.
func TestTheWalkTypesAKnownTree(t *testing.T) {
	t.Parallel()
	root := writeTree(t, fixture)
	ext := writeTree(t, map[string]string{"lib.go": extLib})
	w := decltype.New(root, fixtureModule, map[string]string{extPath: ext})

	probes := 0
	for _, name := range slices.Sorted(mapKeys(fixture)) {
		f := w.FileAt(filepath.Join(root, filepath.FromSlash(name)))
		if f == nil {
			t.Fatalf("could not read %s: %v", name, w.Errors())
		}
		ast.Inspect(f.AST, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			fn, ok := call.Fun.(*ast.Ident)
			if !ok || (fn.Name != "want" && fn.Name != "sel" && fn.Name != "elided") {
				return true
			}
			probes++
			at := w.FileSet().Position(call.Pos())
			where := name + ":" + strconv.Itoa(at.Line)
			var got, want string
			switch fn.Name {
			case "want":
				got, want = render(w, w.TypeOf(call.Args[0], f)), literal(t, call.Args[1])
			case "sel":
				got = renderSelection(w, w.Select(w.TypeOf(call.Args[0], f), literal(t, call.Args[1])))
				want = literal(t, call.Args[2])
			case "elided":
				// Every literal in the probe whose type its collection
				// elides, in the order the source spells them.
				var gots, wants []string
				ast.Inspect(call.Args[0], func(n ast.Node) bool {
					if lit, ok := n.(*ast.CompositeLit); ok && lit.Type == nil {
						gots = append(gots, render(w, w.TypeOf(lit, f)))
					}
					return true
				})
				for _, v := range call.Args[1:] {
					wants = append(wants, literal(t, v))
				}
				got, want = strings.Join(gots, ", "), strings.Join(wants, ", ")
			}
			if got != want {
				t.Errorf("%s: %s typed %q, want %q", where, types(call.Args[0]), got, want)
			}
			return true
		})
	}
	// A suite that judged nothing would pass: count every probe the fixture
	// spells, by its text, against the ones the walk answered.
	spelled := 0
	for _, src := range fixture {
		spelled += len(probeCall.FindAllString(src, -1))
	}
	if probes != spelled || probes < 100 {
		t.Errorf("answered %d probe(s), and the fixture spells %d", probes, spelled)
	}
	if errs := w.Errors(); len(errs) > 0 {
		t.Errorf("read errors: %v", errs)
	}

	// THE SCOPE IS THIS PLATFORM'S FILES: the file built only on plan9 was
	// read for its own probes above, and declares nothing into the package.
	a := w.Load(fixtureModule + "/a")
	if a == nil {
		t.Fatal("the fixture's package a was not read")
	}
	if slices.Contains(a.TypeNames(), "OnlyOnPlan9") {
		t.Error("a type declared only in a file built on another platform joined the package's scope")
	}
	if a.TypeSpec("Inner") == nil || len(a.Files()) != 2 {
		t.Errorf("package a holds %d file(s) and Inner %v; want a.go and other.go",
			len(a.Files()), a.TypeSpec("Inner"))
	}
	if w.Load("github.com/nats-io/nats.go") != nil {
		t.Error("a package outside the module that no directory was named for was read")
	}
}

// probeCall is a probe as the fixture spells it: a statement of its own.
var probeCall = regexp.MustCompile(`(?m)^\t+(want|sel|elided)\(`)

// A WORLD THAT READS TESTS READS A FILE AS ITS TESTS COMPILE IT: an
// in-package test file in its package beside every other in-package test file
// of it — a helper another declares, a type, a method one declares on the
// package's own type — and an external test file in a package of its own,
// beside every other external test file, importing the package with what its
// in-package tests export to it. A test file this platform does not build is
// read against the scope it would join and adds nothing to it, and a package
// that is only imported is read without its tests, as its importers compile
// against it. A world that reads no tests reads every test file against the
// package alone, so it types none of what another test file declares. Each
// probe carries both verdicts: want(EXPR, "with tests", "without") and
// sel(EXPR, "Name", "with tests", "without").
func TestAWorldThatReadsTestsReadsAFileAsItsTestsCompileIt(t *testing.T) {
	t.Parallel()
	root := writeTree(t, testsFixture)
	for _, mode := range []struct {
		name  string
		world func() *decltype.World
		// verdict is which of a probe's verdicts is this world's; files is
		// how many files package p holds once its files have been asked
		// for, and read how many packages the walk has read by then.
		verdict, files, read int
	}{
		{"with tests", func() *decltype.World { return decltype.NewWithTests(root, fixtureModule, nil) }, 0, 4, 3},
		{"without", func() *decltype.World { return decltype.New(root, fixtureModule, nil) }, 1, 1, 2},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Parallel()
			w := mode.world()
			probes, spelled := 0, 0
			for _, name := range slices.Sorted(mapKeys(testsFixture)) {
				// Package q is only imported: none of its files is asked for.
				if !strings.HasPrefix(name, "p/") {
					continue
				}
				spelled += len(probeCall.FindAllString(testsFixture[name], -1))
				f := w.FileAt(filepath.Join(root, filepath.FromSlash(name)))
				if f == nil {
					t.Fatalf("could not read %s: %v", name, w.Errors())
				}
				ast.Inspect(f.AST, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					fn, ok := call.Fun.(*ast.Ident)
					if !ok || (fn.Name != "want" && fn.Name != "sel") {
						return true
					}
					probes++
					where := name + ":" + strconv.Itoa(w.FileSet().Position(call.Pos()).Line)
					var got, want string
					switch fn.Name {
					case "want":
						got, want = render(w, w.TypeOf(call.Args[0], f)), literal(t, call.Args[1+mode.verdict])
					case "sel":
						got = renderSelection(w, w.Select(w.TypeOf(call.Args[0], f), literal(t, call.Args[1])))
						want = literal(t, call.Args[2+mode.verdict])
					}
					if got != want {
						t.Errorf("%s: %s typed %q, want %q", where, types(call.Args[0]), got, want)
					}
					return true
				})
			}
			if probes != spelled || probes < 12 {
				t.Errorf("answered %d probe(s), and the fixture spells %d", probes, spelled)
			}
			if errs := w.Errors(); len(errs) > 0 {
				t.Errorf("read errors: %v", errs)
			}
			p := w.Load(fixtureModule + "/p")
			if p == nil {
				t.Fatal("the fixture's package p was not read")
			}
			if got := len(p.Files()); got != mode.files {
				t.Errorf("package p holds %d file(s), want %d", got, mode.files)
			}
			if slices.Contains(p.TypeNames(), "OnlyOnPlan9InATest") {
				t.Error("a type declared only in a test file built on another platform joined the package's scope")
			}
			if q := w.Load(fixtureModule + "/q"); q == nil || slices.Contains(q.TypeNames(), "OnlyInQsTests") {
				t.Errorf("package q, which is only imported, was read with its tests (or not at all): %v", q)
			}
			if got := w.Read(); got != mode.read {
				t.Errorf("read %d package(s), want %d", got, mode.read)
			}
		})
	}
}

// testsFixture is the tree TestAWorldThatReadsTestsReadsAFileAsItsTestsCompileIt
// reads, as module example.com/m: package p with its in-package and external
// tests, and package q, which p's tests only import.
var testsFixture = map[string]string{
	"p/p.go": `package p

type Opts struct{ StoreDir string }

type T struct{ inner Opts }
`,
	"p/helpers_test.go": `package p

func want(v any, withTests, without string)       {}
func sel(v any, name, withTests, without string) {}

func helper() *Opts { return nil }

type fake struct{ N int }

func (T) fromATest() *fake { return nil }
`,
	"p/use_test.go": `package p

import "example.com/m/q"

func _() {
	want(helper(), "example.com/m/p.Opts", "unknown")
	want(fake{}, "example.com/m/p.fake", "unresolved")
	want(T{}.fromATest(), "example.com/m/p.fake", "unknown")
	sel(T{}, "fromATest", "method example.com/m/p.T", "absent")
	sel(helper(), "StoreDir", "field example.com/m/p.Opts: string", "undetermined")
	want(q.Q{}, "example.com/m/q.Q", "example.com/m/q.Q")
}
`,
	"p/export_test.go": `package p

// Exported is what the package's in-package tests export to its external
// ones.
var Exported = helper
`,
	"p/plan9_test.go": `//go:build plan9

package p

type OnlyOnPlan9InATest struct{}

func _() {
	want(helper(), "example.com/m/p.Opts", "unknown")
}
`,
	"p/ext_test.go": `package p_test

import "example.com/m/p"

func want(v any, withTests, without string)       {}
func sel(v any, name, withTests, without string) {}

type local struct{ O *p.Opts }

func extHelper() *p.Opts { return nil }
`,
	"p/ext_use_test.go": `package p_test

import "example.com/m/p"

func _() {
	want(extHelper(), "example.com/m/p.Opts", "unknown")
	want(p.Exported(), "example.com/m/p.Opts", "unknown")
	want(local{}, "example.com/m/p_test.local", "unresolved")
	sel(local{}, "O", "field example.com/m/p_test.local: example.com/m/p.Opts", "undetermined")
	sel(p.Exported(), "StoreDir", "field example.com/m/p.Opts: string", "undetermined")
}
`,
	"p/xplan9_test.go": `//go:build plan9

package p_test

func _() {
	want(extHelper(), "example.com/m/p.Opts", "unknown")
}
`,
	"q/q.go": `package q

type Q struct{}
`,
	"q/q_test.go": `package q

type OnlyInQsTests struct{}
`,
}

// THE ONE IMPORT-NAME RULE, every form of it: the last element; the one
// before a major version; less a gopkg.in version; less ".go" and "go-".
func TestImportNameIsTheNameAnUnnamedImportIsUsedBy(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]string{
		"net/http":                     "http",
		"math/rand/v2":                 "rand",
		"github.com/golang-jwt/jwt/v5": "jwt",
		"github.com/nats-io/nats-server/v2/server": "server",
		"gopkg.in/yaml.v3":                         "yaml",
		"gopkg.in/natefinch/lumberjack.v2":         "lumberjack",
		"github.com/nats-io/nats.go":               "nats",
		"github.com/nats-io/nats.go/jetstream":     "jetstream",
		"github.com/mattn/go-sqlite3":              "sqlite3",
		"github.com/crewlet/crewlet/internal/api":  "api",
		"example.com/vtwo":                         "vtwo",
		"example.com/thing.v2":                     "thing.v2",
		"v2":                                       "v2",
	} {
		if got := decltype.ImportName(path); got != want {
			t.Errorf("ImportName(%q) = %q, want %q", path, got, want)
		}
	}
}

// A FILE THAT DOES NOT PARSE IS NAMED, never skipped silently: a gate reports
// it, since a file it could not parse is a file it judged nothing in.
func TestAFileThatDoesNotParseIsNamed(t *testing.T) {
	t.Parallel()
	root := writeTree(t, map[string]string{
		"p/good.go":   "package p\n\ntype T struct{}\n",
		"p/broken.go": "package p\n\nfunc {\n",
	})
	w := decltype.New(root, fixtureModule, nil)
	p := w.Load(fixtureModule + "/p")
	if p == nil || p.TypeSpec("T") == nil {
		t.Fatalf("the package's parseable file was not read: %v", p)
	}
	errs := w.Errors()
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "broken.go") {
		t.Errorf("errors %v, want the one naming broken.go", errs)
	}
	if w.FileAt(filepath.Join(root, "q", "x.go")) != nil || len(w.Errors()) != 2 {
		t.Errorf("a file of no package was read, or its failure went unnamed: %v", w.Errors())
	}
}

// THE MODULE'S ROOT IS A PACKAGE TOO, read from the root itself — by its
// import path, which is the module's, and by its directory.
func TestTheModulesRootIsAPackage(t *testing.T) {
	t.Parallel()
	root := writeTree(t, map[string]string{"m.go": "package m\n\ntype T struct{ N int }\n"})
	w := decltype.New(root, fixtureModule, nil)
	if p := w.Load(fixtureModule); p == nil || p.TypeSpec("T") == nil {
		t.Fatalf("the package at the module's root was not read: %v", p)
	}
	if w.LoadDir(root) != w.Load(fixtureModule) {
		t.Error("the root's directory and the module's import path read two packages")
	}
	if f := w.FileAt(filepath.Join(root, "m.go")); f == nil || f.Rel != "m.go" {
		t.Errorf("the root's file read as %+v", f)
	}
}

// LOCATE ASKS GO LIST, and refuses a package the module cannot find rather
// than answering without it.
func TestLocateNamesWhereAPackageIs(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	dirs, err := decltype.Locate(t.Context(), root, "net/http", "github.com/nats-io/nats.go/jetstream")
	if err != nil {
		t.Fatal(err)
	}
	for p, file := range map[string]string{
		"net/http":                             "server.go",
		"github.com/nats-io/nats.go/jetstream": "jetstream.go",
	} {
		if _, err := os.Stat(filepath.Join(dirs[p], file)); err != nil {
			t.Errorf("Locate put %s at %q, which holds no %s: %v", p, dirs[p], file, err)
		}
	}
	if _, err := decltype.Locate(t.Context(), root, "example.com/no/such/package"); err == nil {
		t.Error("Locate answered for a package the module cannot find")
	}
}

// render is a Type as a verdict: a declared type by its full import path
// and name, a literal by its shape, and the two answers that are not types.
func render(w *decltype.World, t decltype.Type) string {
	return renderAt(w, t, 0)
}

func renderAt(w *decltype.World, t decltype.Type, depth int) string {
	if pkg, unread := t.Unread(); unread {
		return "unread " + pkg
	}
	if !t.Known() {
		return "unknown"
	}
	if depth > 4 {
		return "…"
	}
	if n, ok := w.Named(t); ok {
		return full(n)
	}
	lit, how := w.Underlying(t)
	switch how {
	case decltype.Unresolved:
		if w.Param(t) {
			return "param"
		}
		return "unresolved"
	case decltype.Outside:
		return "outside"
	}
	switch x := lit.Expr().(type) {
	case *ast.ArrayType:
		return "[]" + renderAt(w, lit.In(x.Elt), depth+1)
	case *ast.MapType:
		return "map[" + renderAt(w, lit.In(x.Key), depth+1) + "]" + renderAt(w, lit.In(x.Value), depth+1)
	case *ast.ChanType:
		return "chan " + renderAt(w, lit.In(x.Value), depth+1)
	case *ast.FuncType:
		return "func"
	case *ast.StructType:
		return "struct"
	case *ast.InterfaceType:
		return "interface"
	}
	return "?"
}

// renderSelection is a Selection as a verdict.
func renderSelection(w *decltype.World, s decltype.Selection) string {
	switch s.Kind {
	case decltype.Field:
		return "field " + full(s.Owner) + ": " + render(w, s.Type)
	case decltype.Method:
		if s.Interface {
			return "method " + full(s.Owner) + " (interface)"
		}
		return "method " + full(s.Owner)
	case decltype.Beyond:
		var names []string
		for _, n := range s.Outside {
			names = append(names, full(n))
		}
		return "beyond " + strings.Join(names, ", ")
	case decltype.Absent:
		return "absent"
	case decltype.FromUnread:
		return "fromunread"
	}
	return "undetermined"
}

// full is a type's name with its package's whole import path: "-" for none,
// the bare name for the universe's.
func full(n decltype.TypeName) string {
	switch {
	case n == decltype.TypeName{}:
		return "-"
	case n.Package == "":
		return n.Name
	}
	return n.Package + "." + n.Name
}

func literal(t *testing.T, e ast.Expr) string {
	t.Helper()
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		t.Fatalf("a probe's verdict is a string literal, not %T", e)
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// types renders a probe's expression for a failure.
func types(e ast.Expr) string {
	var b strings.Builder
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if b.Len() > 0 {
				b.WriteString(".")
			}
			b.WriteString(id.Name)
		}
		return true
	})
	return b.String()
}

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

const (
	fixtureModule = "example.com/m"
	extPath       = "ext.example/lib/v2"
)

// extLib is a package outside the module, read from the directory the world
// is given for it.
const extLib = `package lib

type Client struct{ conn *Conn }

type Conn struct{ Addr string }

func New() *Client { return nil }

func (c *Client) Conn() *Conn { return c.conn }
`
