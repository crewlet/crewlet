package coord_test

import (
	"bytes"
	"go/ast"
	"go/types"
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

// TestNoNodeJudgesALeaseTheStoreHandedOutOnItsOwnClock fails the build when a
// non-test file of the module selects a coord.Lease's ExpiresAt anywhere but a
// backend (internal/coord/kv/..., internal/coord/memory/...), the contract
// suite (internal/coord/coordtest/...) and coord.TimeLeft, or selects an
// ExpiresAt whose receiver it cannot type (the coord package doc, "A lease a
// read hands out is the store's word"). It must recognise TimeLeft's, the one
// reading of a deadline a reader reaches, or it has stopped seeing any.
func TestNoNodeJudgesALeaseTheStoreHandedOutOnItsOwnClock(t *testing.T) {
	t.Parallel()
	found := leaseDeadlineSelections(t, sourcetree.Root(t))
	if found.exempt[deadlineTimeLeft] == 0 {
		t.Fatalf("judged %d ExpiresAt selector(s), none of them coord.TimeLeft's, the one a reader "+
			"reaches: the walk has gone inert, or TimeLeft moved and this guard needs another anchor",
			found.selectors)
	}
	for _, s := range found.hits {
		t.Errorf("%s:%d selects ExpiresAt on %s, a coord.Lease: a deadline the store set, which held "+
			"against this node's own clock reads a held lease as lapsed, or as running out, wherever "+
			"that clock runs ahead.\n\tTake the store's word: coord.Holder names who holds a resource, "+
			"a lease a read hands out is live as it is, and coord.TimeLeft reads how long it has left "+
			"on the store's clock", s.file, s.line, s.expr)
	}
	for _, s := range found.untyped {
		t.Errorf("%s:%d selects ExpiresAt on %s, whose type this walk cannot establish, so it cannot "+
			"say whether that reads a coord.Lease's deadline.\n\tSpell the value's type where it is "+
			"declared", s.file, s.line, s.expr)
	}
	t.Logf("judged %d ExpiresAt selector(s): coord.Lease's where a store reads one %v; read %d package(s)",
		found.selectors, found.exempt, found.packages)
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN: every shape a reader selecting a
// lease's deadline takes is reported, inside package coord too, among them the
// two the readers of /fleet and fleet_health took; a backend's, the suite's and
// coord.TimeLeft's are recognised and left; another type's ExpiresAt, a
// package's, a test file and testdata are not; a receiver it cannot type is
// reported.
func TestTheLeaseDeadlineWalkJudgesAKnownTree(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for name, body := range leaseDeadlineFixture {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	found := leaseDeadlineSelections(t, root)
	for kind, reported := range map[string][]leaseDeadlineSelection{
		"selection of coord.Lease.ExpiresAt": found.hits,
		"untyped receiver":                   found.untyped,
	} {
		got, want := verdicts(reported), leaseDeadlineFixtureVerdicts[kind]
		for _, v := range slices.Sorted(maps.Keys(merged(got, want))) {
			switch d := want[v] - got[v]; {
			case d > 0:
				t.Errorf("missed the %s %s (%d time(s))", kind, v, d)
			case d < 0:
				t.Errorf("reported the %s %s (%d time(s) too many)", kind, v, -d)
			}
		}
	}
	want := map[string]int{deadlineBackend: 1, deadlineSuite: 1, deadlineTimeLeft: 1}
	if !maps.Equal(found.exempt, want) {
		t.Errorf("recognised %v where a store reads a deadline, want the backend's, the suite's and "+
			"TimeLeft's one each: %v", found.exempt, want)
	}
}

const leaseModule = "github.com/crewlet/crewlet"

// leaseType is the type whose ExpiresAt the walk refuses.
var leaseType = decltype.TypeName{Package: leaseModule + "/internal/coord", Name: "Lease"}

// The places a coord.Lease's deadline may be read, each counted apart.
const (
	deadlineBackend  = "backend"
	deadlineSuite    = "suite"
	deadlineTimeLeft = "TimeLeft"
)

// deadlineReader is the place a store may read a deadline that a selection in
// the file at rel, inside the top-level function fn ("" outside a function or
// in a method), sits in, or "" where none may: the package doc's rule.
func deadlineReader(rel, fn string) string {
	switch {
	case strings.HasPrefix(rel, "internal/coord/kv/"), strings.HasPrefix(rel, "internal/coord/memory/"):
		return deadlineBackend
	case strings.HasPrefix(rel, "internal/coord/coordtest/"):
		return deadlineSuite
	case path.Dir(rel) == "internal/coord" && fn == "TimeLeft":
		return deadlineTimeLeft
	}
	return ""
}

// spellsExpiresAt reports whether src holds ExpiresAt as a whole identifier:
// the walk's prefilter, sound because a selector spells the member's name and
// an identifier has no escapes. A byte of a multi-byte character continues an
// identifier here, which Go allows only of a letter or a digit. It scans for
// the literal: a regular expression led by a character class runs every byte
// through its automaton, forty seconds of such a walk under the race detector.
func spellsExpiresAt(src []byte) bool {
	const name = "ExpiresAt"
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

// leaseDeadlineWalk is what one walk of a tree found: every ExpiresAt selector
// it judged, the coord.Lease ones where a store may read one, by place, and
// those it reports.
type leaseDeadlineWalk struct {
	selectors, packages int
	exempt              map[string]int
	hits, untyped       []leaseDeadlineSelection
}

// leaseDeadlineSelection is one reported selector: where, and its receiver.
type leaseDeadlineSelection struct {
	file string
	line int
	expr string
}

// leaseDeadlineSelections walks every non-test .go file under root, outside
// testdata, and judges each ExpiresAt selector in the files that spell the name.
func leaseDeadlineSelections(t *testing.T, root string) leaseDeadlineWalk {
	t.Helper()
	w := decltype.New(root, leaseModule, nil)
	out := leaseDeadlineWalk{exempt: map[string]int{}}
	err := sourcetree.Walk(root, func(p string, d fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case d.IsDir() && d.Name() == "testdata":
			return fs.SkipDir
		case d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go"):
			return nil
		}
		src, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if spellsExpiresAt(src) {
			judgeLeaseDeadlines(w, p, &out)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	for _, err := range w.Errors() {
		t.Error(err)
	}
	out.packages = w.Read()
	return out
}

// judgeLeaseDeadlines judges every ExpiresAt selector of the file at path, each
// with the top-level function it sits in.
func judgeLeaseDeadlines(w *decltype.World, path string, out *leaseDeadlineWalk) {
	f := w.FileAt(path)
	if f == nil {
		return // why is among w.Errors(), which the walk reports
	}
	for _, decl := range f.AST.Decls {
		fn := ""
		if d, ok := decl.(*ast.FuncDecl); ok && d.Recv == nil {
			fn = d.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "ExpiresAt" {
				judgeLeaseDeadline(w, f, sel, deadlineReader(f.Rel, fn), out)
			}
			return true
		})
	}
}

// judgeLeaseDeadline judges one ExpiresAt selector, sitting where place says a
// store may read a deadline, or "" where none may.
func judgeLeaseDeadline(w *decltype.World, f *decltype.File, sel *ast.SelectorExpr, place string,
	out *leaseDeadlineWalk) {

	if _, isPkg := w.PackageNamed(sel.X, f); isPkg {
		return // a package's own declaration named ExpiresAt
	}
	out.selectors++
	at := leaseDeadlineSelection{file: f.Rel, line: w.FileSet().Position(sel.Pos()).Line,
		expr: types.ExprString(sel.X)}
	member := w.Select(w.TypeOf(sel.X, f), "ExpiresAt")
	switch member.Kind {
	case decltype.Method, decltype.Field:
		switch {
		case member.Owner != leaseType:
		case place != "":
			out.exempt[place]++
		default:
			out.hits = append(out.hits, at)
		}
	case decltype.Beyond:
		// A member of a type of a package the walk does not read, and no such
		// package declares a coord.Lease.
	default:
		out.untyped = append(out.untyped, at)
	}
}

// verdicts counts selections by "file: receiver".
func verdicts(found []leaseDeadlineSelection) map[string]int {
	out := map[string]int{}
	for _, s := range found {
		out[s.file+": "+s.expr]++
	}
	return out
}

// merged is every key of a and b.
func merged(a, b map[string]int) map[string]int {
	out := maps.Clone(a)
	for k := range b {
		out[k] += 0
	}
	return out
}
