package statelog_test

import (
	"bytes"
	"go/ast"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/sourcetree/decltype"
)

// TestOnlyStreamCreatedAtReadsAStreamsCreationInstant fails the build when any
// non-test file under internal/ and cmd/ but internal/statelog/identity.go
// reads the Created field of a stream's information — the BROKER'S creation
// instant — rather than asking statelog.StreamCreatedAt for the stream's
// identity (ADR-0076).
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
// # How X is typed
//
// By internal/sourcetree/decltype, the one walk every gate judging a value by
// its type is built on: from declarations alone, on demand, with no type
// checker — a local by its declaration, a field and a method by the type that
// holds it, promoted through embedding, a call by the results its function
// declares, an element by its collection, generics followed through
// instantiation and inference — across the files of a package and into any
// package of this module and the three NATS packages, which it reads from the
// module cache. Its package doc says what it reads and what it cannot.
//
// # A read the walk cannot type FAILS
//
// A selector whose X it cannot type — a type switch's variable, a value only a
// package it does not read can type (a function's result, a member of one of
// its types) — or whose field it cannot find, or finds only deeper than a part
// it could not read, is reported rather than passed: a gate asserting an
// absence that skipped what it could not read would certify nothing about
// exactly the reads it skipped. Spell the value's type where it is declared,
// or teach decltype the shape.
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
// 0.35–0.39 s with the `go list` that locates NATS, 1.8 s under the race
// detector (four vCPUs).
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
			"(ADR-0076)", h.file, h.line, h.fn, h.owner)
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
	w := decltype.New(root, identityModule, natsDirs(t, root))
	declared := map[decltype.TypeName]bool{}
	for _, p := range []string{jetstreamPath, natsPath, serverPath} {
		pkg := w.Load(p)
		if pkg == nil {
			t.Fatalf("could not read %s", p)
		}
		for _, name := range pkg.TypeNames() {
			if !ast.IsExported(name) {
				continue
			}
			st, ok := pkg.TypeSpec(name).Type.(*ast.StructType)
			if !ok {
				continue
			}
			for _, f := range st.Fields.List {
				if slices.ContainsFunc(f.Names, func(n *ast.Ident) bool { return n.Name == "Created" }) {
					declared[decltype.TypeName{Package: p, Name: name}] = true
				}
			}
		}
	}
	for _, err := range w.Errors() {
		t.Error(err)
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

// streamInstantRoots are the types whose Created field is a stream's creation
// instant as the broker reports it: the client's two views of a stream's
// information, the server's own, the server's monitoring detail of a stream,
// and the server's file store's record of one.
var streamInstantRoots = map[decltype.TypeName]bool{
	{Package: jetstreamPath, Name: "StreamInfo"}:  true,
	{Package: natsPath, Name: "StreamInfo"}:       true,
	{Package: serverPath, Name: "StreamInfo"}:     true,
	{Package: serverPath, Name: "StreamDetail"}:   true,
	{Package: serverPath, Name: "FileStreamInfo"}: true,
}

// notStreamInstants are the other exported types of those packages that
// declare a Created field, and what their instant is.
var notStreamInstants = map[decltype.TypeName]string{
	{Package: jetstreamPath, Name: "ConsumerInfo"}:       "a consumer's creation instant",
	{Package: jetstreamPath, Name: "DesiredClusterInfo"}: "when a placement change was recorded",
	{Package: natsPath, Name: "ConsumerInfo"}:            "a consumer's creation instant",
	{Package: natsPath, Name: "DesiredClusterInfo"}:      "when a placement change was recorded",
	{Package: serverPath, Name: "ConsumerInfo"}:          "a consumer's creation instant",
	{Package: serverPath, Name: "DesiredClusterInfo"}:    "when a placement change was recorded",
	{Package: serverPath, Name: "FileConsumerInfo"}:      "the file store's record of a consumer's creation",
}

// natsDirs locates the NATS packages the module builds against, in the module
// cache: `go list` names each one's directory without building anything.
func natsDirs(t *testing.T, root string) map[string]string {
	t.Helper()
	dirs, err := decltype.Locate(t.Context(), root, jetstreamPath, natsPath, serverPath)
	if err != nil {
		t.Fatal(err)
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
	w := decltype.New(root, identityModule, dirs)
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
			judgeStreamInstants(w, p, &out)
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

// judgeStreamInstants judges every Created selector of the file at path.
func judgeStreamInstants(w *decltype.World, path string, out *streamInstantWalk) {
	f := w.FileAt(path)
	if f == nil {
		// Why is among w.Errors(), which the walk reports.
		return
	}
	called := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f.AST, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if s, ok := ast.Unparen(c.Fun).(*ast.SelectorExpr); ok {
				called[s] = true
			}
		}
		return true
	})
	for _, decl := range f.AST.Decls {
		label := declLabel(decl)
		ast.Inspect(decl, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Created" || called[sel] {
				return true
			}
			if _, isPkg := w.PackageNamed(sel.X, f); isPkg {
				return true
			}
			out.selectors++
			read := streamInstantRead{file: f.Rel, line: w.FileSet().Position(sel.Pos()).Line, fn: label}
			field := w.Select(w.TypeOf(sel.X, f), "Created")
			switch field.Kind {
			case decltype.Field:
				if !streamInstantRoots[field.Owner] {
					// A consumer's instant, or a field of this module's own.
					return true
				}
				read.owner = field.Owner.String()
				if f.Rel == allowedReader {
					out.allowed++
				} else {
					out.hits = append(out.hits, read)
				}
			case decltype.Method, decltype.Beyond:
				// A method value, or a member of a type of a package the walk
				// does not read — none of which declares a stream's
				// information.
			default:
				// Undetermined, Absent, or a value only a package the walk does
				// not read can type: it cannot say what this reads.
				read.owner = exprString(sel.X)
				out.untyped = append(out.untyped, read)
			}
			return true
		})
	}
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

// declLabel is how a report names the declaration a selector sits in.
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
