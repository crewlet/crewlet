package brokertest_test

import (
	"bytes"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/nats-io/nats-server/v2/conf"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// EVERY NATS-SERVER A CASE BUILDS ITSELF DECLARES ITS SIZE. Every broker this
// module starts through internal/queue/jetstream reaches startEmbedded, where
// a test binary refuses one whose store the runner's disk would size
// (jetstream.ErrUndeclaredSize). A case that builds a server itself — from
// nats-server's own Options, or from a configuration it writes for
// nats-server to read — never gets there, and with JetStream on and no limit
// stated, nats-server takes three quarters of the free space on its store's
// volume as the limit just the same: on the store directory's volume, or,
// with none, on that of the fixed default directory every server on the
// machine shares. So this gate reads every Go file of the module, test files
// included, and fails:
//
//   - a composite literal of nats-server's Options that sets StoreDir, or
//     JetStream to anything but false, and no JetStreamMaxStore;
//   - a function that assigns an Options value's StoreDir, or its JetStream
//     anything but false, and sets that limit nowhere in it — by assignment,
//     or in an Options literal of its own;
//   - a nats-server configuration — a string literal, or a constant
//     concatenation of them, that nats-server's own parser reads — that
//     enables JetStream and states no max_file_store in its jetstream block.
//
// A value's type is decltype's reading of the declarations, in a world that
// reads each package as its tests compile it, never a name list: an Options
// is whatever resolves to nats-server's type, under any import name, an
// element literal whose type its collection elides included, so a store set
// on a value of any other type — Tier A's config.Stream, the broker's
// jetstream.Config — is not this gate's. A literal spelling one of those
// fields, or an assignment to one, whose type the walk cannot establish
// FAILS rather than passing, since a gate asserting an absence that skipped
// what it could not read would certify nothing about it.
//
// The declaration is brokertest.StoreMaxBytes, as for every other test broker
// (the package doc); the engine's own options state the figure Tier A gives
// them, and pass for that.
//
// # Coverage boundary
//
// Not seen: a configuration no literal of the module spells whole — one read
// from a file on disk, or assembled at run time from parts no one literal
// holds — and an Options nats-server's own functions fill from one
// (ConfigureOptions' flags among them); a configuration its parser cannot read
// as written, a template whose verbs sit where the grammar admits none; a
// function that sizes one Options and gives another its store by assignment,
// since that rule holds a function rather than a value; and what decltype
// does not see (its package doc).
//
// Mutation: drop JetStreamMaxStore from externaltest's server, or
// max_file_store from the max_payload case's configuration, and this fails
// naming it.
func TestEveryNATSServerATestBuildsDeclaresItsSize(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	found := serverSizes(t, root, serverDirs(t, root))
	// NOT INERT: the tree holds a declared server of every shape — the
	// engine's own options and externaltest's literal, the engine's memory
	// members and jetstream's raw server by assignment, the max_payload
	// case's configuration — so a shape none of was recognised is one the
	// walk has gone blind to.
	for _, shape := range serverShapes {
		if found.declared[shape] == 0 {
			t.Errorf("found no %s declaring its size, and the tree holds several: the walk has "+
				"gone inert for that shape and certifies nothing about it", shape)
		}
	}
	for _, h := range found.hits {
		t.Errorf("%s:%d in %s: %s — its JetStream takes three quarters of the runner's free disk "+
			"as its limit, so the case passes or fails on the machine. Declare it: "+
			"JetStreamMaxStore: brokertest.StoreMaxBytes in the options, max_file_store in a "+
			"configuration — and give a server with no StoreDir a directory of its own",
			h.file, h.line, h.fn, h.what)
	}
	for _, u := range found.untyped {
		t.Errorf("%s:%d in %s: %s, whose type this walk cannot establish, so it cannot say "+
			"whether that is a nats-server server's.\n\tSpell the value's type where it is "+
			"declared, or teach this gate the shape", u.file, u.line, u.fn, u.what)
	}
	t.Logf("judged %d file(s): %d literal(s), %d function(s) and %d configuration(s) declare "+
		"their size; read %d package(s)", found.files, found.declared[shapeLiteral],
		found.declared[shapeAssignment], found.declared[shapeConfiguration], found.packages)
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN: every shape an undeclared
// server takes must be reported — an Options a test helper of another file,
// or of the external test package, returns among them — every declared one
// passed, a store on a type that is not nats-server's ignored where the walk
// types it through a test helper, what it cannot type reported as such, and
// testdata never read.
func TestTheServerSizeWalkJudgesAKnownTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, body := range serverSizeFixture {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found := serverSizes(t, root, serverDirs(t, sourcetree.Root(t)))
	verdicts := func(sites []serverSite) map[string]int {
		out := map[string]int{}
		for _, s := range sites {
			out[path.Dir(s.file)+"."+s.fn]++
		}
		return out
	}
	for kind, pair := range map[string][2]map[string]int{
		"undeclared server": {verdicts(found.hits), countOf(serverSizeFixtureHits)},
		"untyped store":     {verdicts(found.untyped), countOf(serverSizeFixtureUntyped)},
	} {
		got, want := pair[0], pair[1]
		for _, v := range slices.Sorted(maps.Keys(mergeKeys(got, want))) {
			switch d := want[v] - got[v]; {
			case d > 0:
				t.Errorf("missed the %s in %s (%d time(s))", kind, v, d)
			case d < 0:
				t.Errorf("reported the %s in %s (%d time(s) too many)", kind, v, -d)
			}
		}
	}
	for _, shape := range serverShapes {
		if got, want := found.declared[shape], serverSizeFixtureDeclared[shape]; got != want {
			t.Errorf("recognised %d %s(s) declaring their size, want the fixture's %d", got, shape, want)
		}
	}
}

const (
	serverModule = "github.com/crewlet/crewlet"
	serverPath   = "github.com/nats-io/nats-server/v2/server"
)

// optionsType is nats-server's Options, the one type whose store this gate
// judges.
var optionsType = decltype.TypeName{Package: serverPath, Name: "Options"}

// The shapes a server's options take: a literal, a function's assignments
// and a configuration.
const (
	shapeLiteral       = "Options literal"
	shapeAssignment    = "function assigning an Options value's store"
	shapeConfiguration = "configuration"
)

var serverShapes = []string{shapeLiteral, shapeAssignment, shapeConfiguration}

// serverDirs locates nats-server's package for the walk to read.
func serverDirs(t *testing.T, root string) map[string]string {
	t.Helper()
	dirs, err := decltype.Locate(t.Context(), root, serverPath)
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

// serverWalk is what one walk of a tree found.
type serverWalk struct {
	// files is every file judged; declared every literal, function and
	// configuration, by shape, that gives a server a store and states its
	// size beside it; packages every package whose declarations were read.
	files, packages int
	declared        map[string]int
	hits            []serverSite
	untyped         []serverSite
}

// serverSite is one place the walk reports.
type serverSite struct {
	file string // relative to the root, slash-separated
	line int
	fn   string // the declaration it sits in
	what string
}

// serverSizes walks root's internal/ and cmd/ trees and judges every file
// that spells an Options field this gate reads or a configuration's
// JetStream.
func serverSizes(t *testing.T, root string, dirs map[string]string) serverWalk {
	t.Helper()
	w := decltype.NewWithTests(root, serverModule, dirs)
	out := serverWalk{declared: map[string]int{}}
	// configurations is where a file that spells no field is parsed, for its
	// configurations alone.
	configurations := token.NewFileSet()
	for _, sub := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, sub), func(p string, d fs.DirEntry, err error) error {
			switch {
			case err != nil && p == filepath.Join(root, sub) && errors.Is(err, fs.ErrNotExist):
				// A tree with no such directory, which only a fixture is.
				return fs.SkipDir
			case err != nil:
				return err
			case d.IsDir() && d.Name() == "testdata":
				// testdata is not compiled.
				return fs.SkipDir
			case d.IsDir() || !strings.HasSuffix(p, ".go"):
				return nil
			}
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			// THE PREFILTERS ARE SOUND. A literal's key and an assignment's
			// selector spell the field; a configuration that enables JetStream
			// names it, in whatever case nats-server reads it in.
			fields := bytes.Contains(src, []byte("StoreDir")) || spellsJetStreamField(src)
			configures := namesJetStream(src)
			if !fields && !configures {
				return nil
			}
			out.files++
			if fields {
				f := w.FileAt(p)
				if f == nil {
					// Why is among w.Errors(), which the walk reports.
					return nil
				}
				judgeServers(w, f, &out)
				if configures {
					judgeConfigurations(f.AST, w.FileSet(), f.Rel, &out)
				}
				return nil
			}
			parsed, err := parser.ParseFile(configurations, p, src, parser.SkipObjectResolution)
			if err != nil {
				t.Errorf("parse %s: %v", p, err)
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			judgeConfigurations(parsed, configurations, filepath.ToSlash(rel), &out)
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}
	for _, err := range w.Errors() {
		t.Error(err)
	}
	out.packages = w.Read()
	return out
}

// spellsJetStreamField reports whether src spells JetStream as a key or an
// assignment's left side does: the name, then — past any space — the colon,
// the equals sign or the comma that follows it there.
func spellsJetStreamField(src []byte) bool {
	const name = "JetStream"
	for rest := src; ; {
		i := bytes.Index(rest, []byte(name))
		if i < 0 {
			return false
		}
		rest = rest[i+len(name):]
		if after := bytes.TrimLeft(rest, " \t\r\n"); len(after) > 0 && bytes.IndexByte([]byte(":=,"), after[0]) >= 0 {
			return true
		}
	}
}

// namesJetStream reports whether b holds the word jetstream in any case, as
// nats-server reads a configuration's key: through strings.ToLower, under
// which no rune outside ASCII lowers to one of the word's letters, so ASCII's
// folding is the whole of it — found from each j and J with a byte search
// rather than by lowering a whole file, which the race detector makes the
// slowest step of the walk.
func namesJetStream(b []byte) bool {
	const word = "jetstream"
	for _, first := range []byte{'j', 'J'} {
		for rest := b; ; {
			i := bytes.IndexByte(rest, first)
			if i < 0 || len(rest)-i < len(word) {
				break
			}
			if bytes.EqualFold(rest[i:i+len(word)], []byte(word)) {
				return true
			}
			rest = rest[i+1:]
		}
	}
	return false
}

// judgeServers judges every Options literal of the file f and every function
// of it assigning an Options value's store.
func judgeServers(w *decltype.World, f *decltype.File, out *serverWalk) {
	line := func(n ast.Node) int { return w.FileSet().Position(n.Pos()).Line }
	for _, decl := range f.AST.Decls {
		label := declLabel(decl)
		// What the declaration does to Options values, for the assignment
		// rule: every store it assigns, and whether it sets a limit anywhere.
		var stores []serverSite
		sized := false
		// elided is the type of each literal whose collection elides it, as
		// the collection gives it.
		elided := map[*ast.CompositeLit]decltype.Type{}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				typ, ok := elided[x]
				if !ok && x.Type != nil {
					typ = w.TypeOf(x, f)
				}
				elide(w, x, typ, elided)
				store, enables, limit := keyed(x, "StoreDir"), enabled(x), keyed(x, "JetStreamMaxStore")
				name, named := w.Named(typ)
				switch {
				case !named && (store || enables):
					out.untyped = append(out.untyped, serverSite{file: f.Rel, line: line(x), fn: label,
						what: "a composite literal spells StoreDir or JetStream"})
				case !named || name != optionsType:
				case limit:
					sized = true
					if store || enables {
						out.declared[shapeLiteral]++
					}
				case store:
					out.hits = append(out.hits, serverSite{file: f.Rel, line: line(x), fn: label,
						what: "a nats-server Options literal sets StoreDir and no JetStreamMaxStore"})
				case enables:
					out.hits = append(out.hits, serverSite{file: f.Rel, line: line(x), fn: label,
						what: "a nats-server Options literal enables JetStream and sets no JetStreamMaxStore"})
				}
			case *ast.AssignStmt:
				for i, lhs := range x.Lhs {
					sel, ok := ast.Unparen(lhs).(*ast.SelectorExpr)
					if !ok {
						continue
					}
					field := sel.Sel.Name
					if field != "StoreDir" && field != "JetStream" && field != "JetStreamMaxStore" {
						continue
					}
					var value ast.Expr
					if len(x.Rhs) == len(x.Lhs) {
						value = x.Rhs[i]
					}
					member := w.Select(w.TypeOf(sel.X, f), field)
					switch {
					case member.Kind == decltype.Field && member.Owner == optionsType:
						switch {
						case field == "JetStreamMaxStore":
							sized = true
						case field == "JetStream" && isFalse(value):
							// Turns JetStream off.
						default:
							stores = append(stores, serverSite{file: f.Rel, line: line(sel), fn: label,
								what: "it assigns a nats-server Options value's " + field +
									" and sets no JetStreamMaxStore"})
						}
					case member.Kind == decltype.Field:
						// A field of another type: a store of the module's own.
					case field != "JetStreamMaxStore":
						out.untyped = append(out.untyped, serverSite{file: f.Rel, line: line(sel), fn: label,
							what: field + " is assigned on " + exprString(sel.X)})
					}
				}
			}
			return true
		})
		switch {
		case len(stores) > 0 && !sized:
			out.hits = append(out.hits, stores...)
		case len(stores) > 0:
			out.declared[shapeAssignment]++
		}
	}
}

// elide records, for each element of the literal lit whose type it elides,
// the type lit's own type typ gives it: a slice's or an array's element, a
// map's key or value. A literal whose type the walk does not know gives none,
// and an element it elides is then a literal of no known type.
func elide(w *decltype.World, lit *ast.CompositeLit, typ decltype.Type, elided map[*ast.CompositeLit]decltype.Type) {
	under, how := w.Underlying(typ)
	if how != decltype.Resolved {
		return
	}
	var key, elem decltype.Type
	switch x := under.Expr().(type) {
	case *ast.ArrayType:
		elem = under.In(x.Elt)
	case *ast.MapType:
		key, elem = under.In(x.Key), under.In(x.Value)
	default:
		return
	}
	set := func(e ast.Expr, t decltype.Type) {
		if c, ok := e.(*ast.CompositeLit); ok && c.Type == nil {
			elided[c] = t
		}
	}
	for _, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if key.Known() {
				set(kv.Key, key)
			}
			set(kv.Value, elem)
			continue
		}
		set(e, elem)
	}
}

// judgeConfigurations judges every string literal of file, and every constant
// concatenation of them, that nats-server reads as a configuration naming
// JetStream.
func judgeConfigurations(file *ast.File, fset *token.FileSet, rel string, out *serverWalk) {
	for _, decl := range file.Decls {
		label := declLabel(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			var text string
			switch x := n.(type) {
			case *ast.BinaryExpr:
				folded, ok := concatenated(x)
				if !ok {
					return true
				}
				text = folded
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				unquoted, err := strconv.Unquote(x.Value)
				if err != nil {
					return true
				}
				text = unquoted
			default:
				return true
			}
			if !namesJetStream([]byte(text)) {
				return true
			}
			enables, limited, read := configured(text)
			switch {
			case !read:
				// Not a configuration nats-server reads: its parts may be.
				return true
			case !enables:
			case limited:
				out.declared[shapeConfiguration]++
			default:
				out.hits = append(out.hits, serverSite{file: rel, line: fset.Position(n.Pos()).Line,
					fn: label, what: "a nats-server configuration enables JetStream and states no max_file_store"})
			}
			// A concatenation read whole is judged whole.
			return false
		})
	}
}

// concatenated is the text a constant concatenation of string literals
// spells; ok is false for any other expression.
func concatenated(e ast.Expr) (string, bool) {
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
		a, ok := concatenated(x.X)
		if !ok {
			return "", false
		}
		b, ok := concatenated(x.Y)
		return a + b, ok
	}
	return "", false
}

// configured reads text as nats-server's own parser does: whether it enables
// JetStream at the top level, and whether its jetstream block states a file
// store's limit. read is false for a text that parser does not read.
//
// nats-server reads the jetstream key as a bool, as "enabled" or "disabled",
// or as a block, which enables it unless its enable key says otherwise; and
// the limit as max_file_store or max_file within that block — each key and
// word lowered first (server/opts.go, parseJetStream).
func configured(text string) (enables, limited, read bool) {
	m, err := conf.Parse(text)
	if err != nil {
		return false, false, false
	}
	for k, v := range m {
		if strings.ToLower(k) != "jetstream" {
			continue
		}
		switch js := v.(type) {
		case bool:
			return js, false, true
		case string:
			word := strings.ToLower(js)
			return word == "enabled" || word == "enable", false, true
		case map[string]any:
			enables = true
			for bk, bv := range js {
				switch strings.ToLower(bk) {
				case "enable", "enabled":
					if on, ok := bv.(bool); ok && !on {
						enables = false
					}
				case "max_file_store", "max_file":
					limited = true
				}
			}
			return enables, limited, true
		}
		return false, false, true
	}
	return false, false, true
}

// keyed reports whether the composite literal lit sets the field named key.
func keyed(lit *ast.CompositeLit, key string) bool {
	return slices.ContainsFunc(lit.Elts, func(e ast.Expr) bool {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		id, ok := kv.Key.(*ast.Ident)
		return ok && id.Name == key
	})
}

// enabled reports whether the composite literal lit sets JetStream to
// anything but false.
func enabled(lit *ast.CompositeLit) bool {
	return slices.ContainsFunc(lit.Elts, func(e ast.Expr) bool {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			return false
		}
		id, ok := kv.Key.(*ast.Ident)
		return ok && id.Name == "JetStream" && !isFalse(kv.Value)
	})
}

// isFalse reports whether e is the constant false.
func isFalse(e ast.Expr) bool {
	if e == nil {
		return false
	}
	id, ok := ast.Unparen(e).(*ast.Ident)
	return ok && id.Name == "false"
}

// declLabel names the declaration a site sits in.
func declLabel(decl ast.Decl) string {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Recv != nil && len(d.Recv.List) > 0 {
			return decltype.BaseName(d.Recv.List[0].Type) + "." + d.Name.Name
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

// serverSizeFixture is a tree whose every shape has a known verdict; the
// files never compile, and need not — the walk reads declarations alone.
var serverSizeFixture = map[string]string{
	"internal/a/servers_test.go": `package a

import (
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

type own struct{ StoreDir string }

func undeclaredLiteral(t *testing.T) {
	_, _ = natsserver.NewServer(&natsserver.Options{JetStream: true, StoreDir: t.TempDir()})
}

func undeclaredJetStream() {
	_, _ = natsserver.NewServer(&natsserver.Options{JetStream: true})
}

func storeOnly(t *testing.T) {
	_, _ = natsserver.NewServer(&natsserver.Options{StoreDir: t.TempDir()})
}

func declaredLiteral(t *testing.T) {
	_, _ = natsserver.NewServer(&natsserver.Options{JetStream: true, StoreDir: t.TempDir(),
		JetStreamMaxStore: 1 << 34})
}

func noJetStream() {
	_, _ = natsserver.NewServer(&natsserver.Options{Port: -1})
	_, _ = natsserver.NewServer(&natsserver.Options{JetStream: false})
}

func elidedUndeclared(t *testing.T) {
	_ = []natsserver.Options{{JetStream: true, StoreDir: t.TempDir()}}
}

func elidedDeclared(t *testing.T) {
	_ = map[string]*natsserver.Options{"a": {StoreDir: t.TempDir(), JetStreamMaxStore: 1 << 34}}
}

func undeclaredAssignment(t *testing.T, opts *natsserver.Options) {
	opts.JetStream, opts.StoreDir = true, t.TempDir()
}

func jetStreamTurnedOff(opts *natsserver.Options) {
	opts.JetStream = false
}

func declaredAssignment(t *testing.T, opts *natsserver.Options) {
	opts.StoreDir = t.TempDir()
	opts.JetStreamMaxStore = 1 << 34
}

func declaredByItsLiteral(t *testing.T) {
	opts := &natsserver.Options{JetStream: true, JetStreamMaxStore: 1 << 34}
	opts.StoreDir = t.TempDir()
}

func limitOnlyLiteral(t *testing.T) {
	opts := &natsserver.Options{JetStreamMaxStore: 1 << 34}
	opts.JetStream, opts.StoreDir = true, t.TempDir()
}

func viaTestHelper(t *testing.T) {
	opts := helperOptions()
	opts.StoreDir = t.TempDir()
}

func ownStore(t *testing.T) {
	var o own
	o.StoreDir = t.TempDir()
}

func tierAViaTestHelper() {
	b := bootstrap()
	b.Stream.StoreDir = "/var/lib/crewlet/stream"
}

func undeclaredConfiguration() string {
	return "port: -1\njetstream { store_dir: \"/tmp/x\" }\n"
}

func enabledWithoutABlock() string {
	return "port: -1\nstore_dir: \"/tmp/x\"\njetstream: enabled\n"
}

func concatenated() string {
	return "port: -1\n" + "JetStream {\n" + "  store_dir: \"/tmp/x\"\n" + "}\n"
}

func declaredConfiguration() string {
	return "port: -1\njetstream: {\n  store_dir: \"/tmp/x\"\n  max_file_store: 17179869184\n}\n"
}

func declaredTemplate() string {
	return "host: 127.0.0.1\nport: -1\nmax_payload: %s\njetstream {\n  store_dir: %q\n  max_file: %d\n}\n" +
		"accounts {\n  ENGINE {\n    jetstream: enabled\n    users: [ { user: crewlet, password: %q } ]\n  }\n}\n"
}

func disabledConfiguration() string {
	return "port: -1\njetstream: disabled\nstore_dir: \"/tmp/x\"\n"
}

func tierA() string {
	return "stream:\n  store_dir: /var/lib/crewlet/stream # where jetstream keeps it\n"
}
`,
	"internal/a/helpers_test.go": `package a

import natsserver "github.com/nats-io/nats-server/v2/server"

type stream struct{ StoreDir string }

type tierABootstrap struct{ Stream stream }

func helperOptions() *natsserver.Options { return &natsserver.Options{Port: -1} }

func bootstrap() *tierABootstrap { return &tierABootstrap{} }
`,
	"internal/a/external_test.go": `package a_test

import "testing"

func externalUndeclared(t *testing.T) {
	opts := externalOptions()
	opts.JetStream = true
}
`,
	"internal/a/externalhelpers_test.go": `package a_test

import natsserver "github.com/nats-io/nats-server/v2/server"

func externalOptions() *natsserver.Options { return &natsserver.Options{} }
`,
	"internal/b/untyped.go": `package b

import "example.com/unread/elsewhere"

func untypedStore() {
	v := elsewhere.Options()
	v.StoreDir = "/tmp/x"
}

func untypedLiteral() {
	_ = undeclaredType{JetStream: true}
}
`,
	"internal/c/testdata/never_test.go": `package c

import natsserver "github.com/nats-io/nats-server/v2/server"

func undeclaredButNeverRead() {
	_, _ = natsserver.NewServer(&natsserver.Options{JetStream: true, StoreDir: "/tmp/x"})
}
`,
}

// serverSizeFixtureHits, serverSizeFixtureUntyped and
// serverSizeFixtureDeclared are the fixture's verdicts, a site as
// directory.function.
var (
	serverSizeFixtureHits = []string{
		"internal/a.undeclaredLiteral",
		"internal/a.undeclaredJetStream",
		"internal/a.storeOnly",
		"internal/a.elidedUndeclared",
		// One statement, two stores.
		"internal/a.undeclaredAssignment", "internal/a.undeclaredAssignment",
		"internal/a.viaTestHelper",
		"internal/a.externalUndeclared",
		"internal/a.undeclaredConfiguration",
		"internal/a.enabledWithoutABlock",
		"internal/a.concatenated",
	}
	serverSizeFixtureUntyped = []string{
		"internal/b.untypedStore",
		"internal/b.untypedLiteral",
	}
	serverSizeFixtureDeclared = map[string]int{
		// declaredLiteral, elidedDeclared and declaredByItsLiteral's own.
		shapeLiteral: 3,
		// declaredAssignment, declaredByItsLiteral and limitOnlyLiteral, the
		// last by a literal that gives no store itself.
		shapeAssignment: 3,
		// declaredConfiguration and declaredTemplate.
		shapeConfiguration: 2,
	}
)
