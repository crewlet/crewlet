package brokertest_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
	"sync"
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
	found, err := moduleServers()
	if err != nil {
		t.Error(err)
	}
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

// EVERY NATS-SERVER A CASE BUILDS ITSELF KEEPS ITS STORE IN A
// brokertest.TempDir. Every broker this module starts through
// internal/queue/jetstream reaches startEmbedded, where a test binary refuses
// one whose store directory no brokertest.TempDir holds
// (jetstream.ErrStoreOutsideTempDir); a server a case builds from
// nats-server's own Options never gets there. A stopped nats-server can still
// land a consumer's last state flush in its store after Shutdown and
// WaitForShutdown have returned, and a store in a t.TempDir is removed once at
// the end of the test, which that write then fails with "directory not empty"
// on a file the case never wrote (crewlet/crewlet#1079). So this gate judges
// the same walk as the size gate above, and fails an Options StoreDir — in a
// literal, or by assignment — whose value is made by a testing.TB's TempDir
// (or, in a test file, by os.MkdirTemp or os.TempDir, whose removal is the
// case's own and tries once too). What the value is made from is followed
// within its file: through every local it reads to each value that local is
// given, through a parameter to what each of the file's calls of its function
// passes there, and through a field of a type this module declares to each
// value the file gives that field — the shapes the tree's servers take
// (backuptest's restart takes its store as a parameter, externaltest's server
// keeps it in a field). A TempDir call there on a value the walk cannot type
// FAILS rather than passing, for the size gate's reason.
//
// # Coverage boundary
//
// Not seen: a store made in another file than the one that sets it, or
// returned by a function, or copied from another Options; a store
// directory a configuration names, which a verb fills at run time; a store a
// non-test file makes with os.MkdirTemp, which is how the embedded broker
// makes a scratch member's store, removed by its own stop; and what decltype
// does not see (its package doc).
//
// Mutation: give externaltest's server, or backuptest's restored broker, a
// t.TempDir store, and this fails naming the Options it reaches.
func TestEveryNATSServerATestBuildsKeepsItsStoreInATempDir(t *testing.T) {
	t.Parallel()
	found, err := moduleServers()
	if err != nil {
		t.Error(err)
	}
	// NOT INERT: the tree keeps the store of nearly every server a case
	// builds in a brokertest.TempDir, through a literal, a local, a
	// parameter and a field, so a walk that recognised none of them has gone
	// blind to the stores it judges.
	if found.kept == 0 {
		t.Error("found no nats-server store kept in a brokertest.TempDir, and the tree holds " +
			"several: the walk has gone inert and certifies nothing about the stores it judges")
	}
	for _, h := range found.temps {
		t.Errorf("%s:%d in %s: %s — its removal at the end of the test tries once, and a "+
			"stopped nats-server can land a consumer's last state flush there after it stopped, "+
			"failing the case on a file it never wrote (crewlet/crewlet#1079). Keep the store "+
			"in brokertest.TempDir(t), or in a directory under one", h.file, h.line, h.fn, h.what)
	}
	for _, u := range found.tempsUntyped {
		t.Errorf("%s:%d in %s: %s, whose type this walk cannot establish, so it cannot say "+
			"whether that is a test's temporary directory.\n\tSpell the value's type where it "+
			"is declared, or teach this gate the shape", u.file, u.line, u.fn, u.what)
	}
	t.Logf("judged %d file(s): %d nats-server store(s) kept in a brokertest.TempDir",
		found.files, found.kept)
}

// moduleServers is the walk of this module's tree both gates above judge,
// taken once for the test binary: they read the same files, and the walk is
// nearly all of either one's time.
var moduleServers = sync.OnceValues(func() (serverWalk, error) {
	root, err := sourcetree.ModuleRoot()
	if err != nil {
		return serverWalk{}, err
	}
	dirs, err := decltype.Locate(context.Background(), root, serverPath)
	if err != nil {
		return serverWalk{}, err
	}
	return serverSizes(root, dirs)
})

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
	found, err := serverSizes(root, serverDirs(t, sourcetree.Root(t)))
	if err != nil {
		t.Error(err)
	}
	verdicts := func(sites []serverSite) map[string]int {
		out := map[string]int{}
		for _, s := range sites {
			out[path.Dir(s.file)+"."+s.fn]++
		}
		return out
	}
	for kind, pair := range map[string][2]map[string]int{
		"undeclared server":        {verdicts(found.hits), countOf(serverSizeFixtureHits)},
		"untyped store":            {verdicts(found.untyped), countOf(serverSizeFixtureUntyped)},
		"store in a temporary dir": {verdicts(found.temps), countOf(serverStoreFixtureTemps)},
		"untyped temporary dir":    {verdicts(found.tempsUntyped), countOf(serverStoreFixtureUntyped)},
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
	if found.kept != serverStoreFixtureKept {
		t.Errorf("recognised %d store(s) kept in a brokertest.TempDir, want the fixture's %d",
			found.kept, serverStoreFixtureKept)
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
	// kept is every Options store the walk followed to a
	// brokertest.TempDir; temps every one it followed to a temporary
	// directory of any other maker; tempsUntyped every TempDir call it met
	// on that way on a value it could not type.
	kept         int
	temps        []serverSite
	tempsUntyped []serverSite
}

// serverSite is one place the walk reports.
type serverSite struct {
	file string // relative to the root, slash-separated
	line int
	fn   string // the declaration it sits in
	what string
}

// serverSizes walks root's internal/ and cmd/ trees and judges every file
// that spells an Options field these gates read or a configuration's
// JetStream. What it could not read it returns as the error, beside all it
// judged.
func serverSizes(root string, dirs map[string]string) (serverWalk, error) {
	w := decltype.NewWithTests(root, serverModule, dirs)
	out := serverWalk{declared: map[string]int{}}
	var errs []error
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
				errs = append(errs, fmt.Errorf("parse %s: %w", p, err))
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
			return out, errors.Join(append(errs, fmt.Errorf("walk %s: %w", sub, err))...)
		}
	}
	out.packages = w.Read()
	return out, errors.Join(append(errs, w.Errors()...)...)
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
// of it assigning an Options value's store: the size each declares, and what
// made the store each is given.
func judgeServers(w *decltype.World, f *decltype.File, out *serverWalk) {
	line := func(n ast.Node) int { return w.FileSet().Position(n.Pos()).Line }
	// origins follows a store's value through the file, read the first time
	// an Options store is met: most files judged spell only the store of a
	// type of the module's own.
	var origins *storeOrigins
	judgeStore := func(value ast.Expr, label string) {
		if origins == nil {
			origins = originsIn(w, f)
		}
		made := origins.made(value)
		if made.kept {
			out.kept++
		}
		for _, m := range made.makers {
			site := serverSite{file: f.Rel, line: line(value), fn: label}
			if m.untyped {
				site.what = fmt.Sprintf("a nats-server Options value's StoreDir is made at line %d "+
					"by a TempDir() call on %s", line(m.at), exprString(m.receiver))
				out.tempsUntyped = append(out.tempsUntyped, site)
				continue
			}
			site.what = fmt.Sprintf("a nats-server Options value's StoreDir is made by %s at line %d",
				m.what, line(m.at))
			out.temps = append(out.temps, site)
		}
	}
	for _, decl := range f.AST.Decls {
		label := declLabel(decl)
		// What the declaration does to Options values, for the assignment
		// rule: every store it assigns, and whether it sets a limit anywhere.
		var stores []serverSite
		sized := false
		ast.Inspect(decl, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CompositeLit:
				store, enables, limit := keyed(x, "StoreDir"), enabled(x), keyed(x, "JetStreamMaxStore")
				name, named := w.Named(w.TypeOf(x, f))
				if named && name == optionsType && store {
					judgeStore(keyedValue(x, "StoreDir"), label)
				}
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
						if field == "StoreDir" {
							// The value paired with it, or the one call
							// whose results the whole left side takes.
							made := value
							if made == nil {
								made = x.Rhs[0]
							}
							judgeStore(made, label)
						}
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

// keyedValue is the value the composite literal lit gives the field named
// key, which it sets ([keyed]).
func keyedValue(lit *ast.CompositeLit, key string) ast.Expr {
	for _, e := range lit.Elts {
		if kv, ok := e.(*ast.KeyValueExpr); ok {
			if id, ok := kv.Key.(*ast.Ident); ok && id.Name == key {
				return kv.Value
			}
		}
	}
	return nil
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

	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

type own struct{ StoreDir string }

func undeclaredLiteral(t *testing.T) {
	_, _ = natsserver.NewServer(&natsserver.Options{JetStream: true, StoreDir: brokertest.TempDir(t)})
}

func undeclaredJetStream() {
	_, _ = natsserver.NewServer(&natsserver.Options{JetStream: true})
}

func storeOnly(t *testing.T) {
	_, _ = natsserver.NewServer(&natsserver.Options{StoreDir: brokertest.TempDir(t)})
}

func declaredLiteral(t *testing.T) {
	_, _ = natsserver.NewServer(&natsserver.Options{JetStream: true, StoreDir: brokertest.TempDir(t),
		JetStreamMaxStore: 1 << 34})
}

func noJetStream() {
	_, _ = natsserver.NewServer(&natsserver.Options{Port: -1})
	_, _ = natsserver.NewServer(&natsserver.Options{JetStream: false})
}

func elidedUndeclared(t *testing.T) {
	_ = []natsserver.Options{{JetStream: true, StoreDir: brokertest.TempDir(t)}}
}

func elidedDeclared(t *testing.T) {
	_ = map[string]*natsserver.Options{"a": {StoreDir: brokertest.TempDir(t), JetStreamMaxStore: 1 << 34}}
}

func undeclaredAssignment(t *testing.T, opts *natsserver.Options) {
	opts.JetStream, opts.StoreDir = true, brokertest.TempDir(t)
}

func jetStreamTurnedOff(opts *natsserver.Options) {
	opts.JetStream = false
}

func declaredAssignment(t *testing.T, opts *natsserver.Options) {
	opts.StoreDir = brokertest.TempDir(t)
	opts.JetStreamMaxStore = 1 << 34
}

func declaredByItsLiteral(t *testing.T) {
	opts := &natsserver.Options{JetStream: true, JetStreamMaxStore: 1 << 34}
	opts.StoreDir = brokertest.TempDir(t)
}

func limitOnlyLiteral(t *testing.T) {
	opts := &natsserver.Options{JetStreamMaxStore: 1 << 34}
	opts.JetStream, opts.StoreDir = true, brokertest.TempDir(t)
}

func viaTestHelper(t *testing.T) {
	opts := helperOptions()
	opts.StoreDir = brokertest.TempDir(t)
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

import (
	natsserver "github.com/nats-io/nats-server/v2/server"

	"example.com/unread/elsewhere"
)

func untypedStore() {
	v := elsewhere.Options()
	v.StoreDir = "/tmp/x"
}

func untypedLiteral() {
	_ = undeclaredType{JetStream: true}
}

func untypedTempDir() {
	v := elsewhere.Fixture()
	_ = &natsserver.Options{StoreDir: v.TempDir(), JetStreamMaxStore: 1 << 34}
}
`,
	"internal/a/stores_test.go": `package a

import (
	"os"
	"path/filepath"
	"testing"

	natsserver "github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

type wrapped struct{ testing.TB }

type tempDirer interface{ TempDir() string }

type ownTempDir struct{}

func (ownTempDir) TempDir() string { return "/var/lib/crewlet/stream" }

type member struct{ store string }

func storeInATempDir(t *testing.T) {
	_ = &natsserver.Options{JetStream: true, StoreDir: t.TempDir(), JetStreamMaxStore: 1 << 34}
}

func storeUnderATempDir(t *testing.T) {
	_ = &natsserver.Options{StoreDir: filepath.Join(t.TempDir(), "store"), JetStreamMaxStore: 1 << 34}
}

func storeThroughLocals(t *testing.T) {
	dir := t.TempDir()
	var store = filepath.Join(dir, "store")
	_ = &natsserver.Options{StoreDir: store, JetStreamMaxStore: 1 << 34}
}

func storeAssigned(t *testing.T, opts *natsserver.Options) {
	opts.StoreDir, opts.JetStreamMaxStore = t.TempDir(), 1<<34
}

func storeOfABenchmark(b *testing.B) {
	_ = &natsserver.Options{StoreDir: b.TempDir(), JetStreamMaxStore: 1 << 34}
}

func storeOfATB(tb testing.TB) {
	_ = &natsserver.Options{StoreDir: tb.TempDir(), JetStreamMaxStore: 1 << 34}
}

func storeOfAWrapper(t *testing.T) {
	w := wrapped{t}
	_ = &natsserver.Options{StoreDir: w.TempDir(), JetStreamMaxStore: 1 << 34}
}

func storeOfAnInterface(t tempDirer) {
	_ = &natsserver.Options{StoreDir: t.TempDir(), JetStreamMaxStore: 1 << 34}
}

func storeMadeByOS() {
	dir, _ := os.MkdirTemp("", "store")
	_ = &natsserver.Options{StoreDir: dir, JetStreamMaxStore: 1 << 34}
}

func storeThroughAParameter(t *testing.T) {
	serveIn(t.TempDir())
}

func serveIn(dir string) {
	_ = &natsserver.Options{StoreDir: dir, JetStreamMaxStore: 1 << 34}
}

func storeThroughAField(t *testing.T) {
	m := &member{store: t.TempDir()}
	m.serve()
}

func (m *member) serve() {
	_ = &natsserver.Options{StoreDir: m.store, JetStreamMaxStore: 1 << 34}
}

func keptInATempDir(t *testing.T) {
	_ = &natsserver.Options{StoreDir: brokertest.TempDir(t), JetStreamMaxStore: 1 << 34}
}

func keptThroughLocals(t *testing.T) {
	dir := filepath.Join(brokertest.TempDir(t), "store")
	opts := &natsserver.Options{JetStreamMaxStore: 1 << 34}
	opts.StoreDir = dir
}

func keptThroughAParameter(t *testing.T) {
	keepIn(t, brokertest.TempDir(t))
}

func keepIn(t *testing.T, dir string) {
	_ = &natsserver.Options{StoreDir: dir, JetStreamMaxStore: 1 << 34}
}

type keeper struct{ store string }

func keptThroughAField(t *testing.T) {
	k := &keeper{}
	k.store = brokertest.TempDir(t)
	k.serve()
}

func (k *keeper) serve() {
	_ = &natsserver.Options{StoreDir: k.store, JetStreamMaxStore: 1 << 34}
}

func keptShadowingATempDir(t *testing.T) {
	dir := t.TempDir()
	_ = dir
	for _, dir := range []string{brokertest.TempDir(t)} {
		_ = &natsserver.Options{StoreDir: dir, JetStreamMaxStore: 1 << 34}
	}
}

type holder struct{ dir string }

func pick(h holder) string { return h.dir }

func keptPastAKeyNamedLikeALocal(t *testing.T) {
	dir := t.TempDir()
	_ = dir
	_ = &natsserver.Options{StoreDir: pick(holder{dir: brokertest.TempDir(t)}), JetStreamMaxStore: 1 << 34}
}

func ownTempDirMethod() {
	var o ownTempDir
	_ = &natsserver.Options{StoreDir: o.TempDir(), JetStreamMaxStore: 1 << 34}
}

func aTempDirThatIsNoStore(t *testing.T) {
	_ = &natsserver.Options{JetStream: true, JetStreamMaxStore: 1 << 34,
		ConfigFile: filepath.Join(t.TempDir(), "nats.conf")}
}
`,
	"internal/c/scratch.go": `package c

import (
	"os"

	natsserver "github.com/nats-io/nats-server/v2/server"
)

func scratchStore() {
	dir, _ := os.MkdirTemp("", "scratch")
	opts := &natsserver.Options{JetStream: true, JetStreamMaxStore: -1}
	opts.StoreDir = dir
}
`,
	"internal/c/testdata/never_test.go": `package c

import natsserver "github.com/nats-io/nats-server/v2/server"

func undeclaredButNeverRead() {
	_, _ = natsserver.NewServer(&natsserver.Options{JetStream: true, StoreDir: "/tmp/x"})
}
`,
}

// serverStoreFixtureTemps and serverStoreFixtureUntyped are the fixture's
// verdicts on its stores, a site as directory.function, and
// serverStoreFixtureKept how many of its stores are kept in a
// brokertest.TempDir.
var (
	serverStoreFixtureTemps = []string{
		"internal/a.storeInATempDir",
		"internal/a.storeUnderATempDir",
		"internal/a.storeThroughLocals",
		"internal/a.storeAssigned",
		"internal/a.storeOfABenchmark",
		"internal/a.storeOfATB",
		"internal/a.storeOfAWrapper",
		"internal/a.storeOfAnInterface",
		"internal/a.storeMadeByOS",
		// Reported where the store is set, which is where it is made
		// from the caller's directory.
		"internal/a.serveIn",
		"internal/a.member.serve",
	}
	serverStoreFixtureUntyped = []string{
		"internal/b.untypedTempDir",
	}
	// servers_test.go's ten stores, and stores_test.go's six kept ones.
	serverStoreFixtureKept = 16
)

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
		// declaredLiteral, elidedDeclared and declaredByItsLiteral's own;
		// every literal of stores_test.go that gives a store or enables
		// JetStream, seventeen, all sized, since that file's subject is the
		// store; untypedTempDir's; and scratchStore's own.
		shapeLiteral: 3 + 17 + 1 + 1,
		// declaredAssignment, declaredByItsLiteral and limitOnlyLiteral, the
		// last by a literal that gives no store itself; storeAssigned and
		// keptThroughLocals; and scratchStore.
		shapeAssignment: 3 + 2 + 1,
		// declaredConfiguration and declaredTemplate.
		shapeConfiguration: 2,
	}
)
