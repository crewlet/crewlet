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
// what a selector resolves to — so the table is the fixture, and a probe the
// walk answers differently names itself. The shapes: locals of every kind
// (parameters, variadics, :=, var, range clauses over every collection and
// an iterator, comma-ok), package values in another file and another
// package, fields and methods promoted through embedding and through an
// interface embedding one of another package, method values, method
// expressions on a type this file, another file of its package, another
// package and an unread one declares, calls of function-typed fields,
// conversions, composite literals, elements, basic
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
			if !ok || (fn.Name != "want" && fn.Name != "sel") {
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
var probeCall = regexp.MustCompile(`(?m)^\t+(want|sel)\(`)

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
