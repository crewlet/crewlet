package store_test

import (
	"database/sql"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/store"
)

// NOTHING REACHES A CLOSED PEER THROUGH A DOOR THAT CANNOT ANSWER.
//
// A closed replicated peer is a LEGITIMATE, DOCUMENTED state rather than a
// fault: an adoption closes it between its rename and its reopen, and `Close`
// leaves it nil. So `Replicated()` answers nil, and what a caller does with
// that nil decides whether the process survives.
//
// It began as one rule — do not take `Replicated().SQL()`, whose nil pool
// dereferences three frames inside database/sql — after six call sites had
// that shape and the one that fired did so in the trim's own tick, which runs
// on a timer against an estate a shutdown is closing:
//
//	database/sql.(*DB).conn(0x0, …)
//	tracker.Evictions(…)
//	engine.(*retention).tombstones(…)
//
// ONE METHOD WAS NEVER THE RULE, which is what the second incident proved:
// the same tick crashed again at `store.(*DB).Read(0x0, …)` under the
// identical stack, and a third site reached `Replicated().Path()` — an
// accessor with no nil guard — from `crewlet retention status`, the REST
// route and the dashboard socket. So the guard is a WHITELIST now: through
// `Replicated()`, only the doors that answer on a handle that is not open.
// Everything else is a crash waiting for the window to open.
//
// A STRUCTURAL TEST, because the defect is structural: what it asserts is
// that no call site in the tree has this shape, and only a walk can say that.
//
// It used to add that a behavioural test was out of reach — "a race a test can
// only lose reliably" — and that was the half worth checking, because it is
// false and it cost a second incident. Closing an estate underneath a running
// loop is indeed a race; the STATE that loop then holds is not one. A closed
// peer is a nil `*store.DB`, which a test can simply declare, so every door
// into it is deterministically reachable — see
// [TestAClosedEstateAnswersRatherThanCrashing], which is that test, and which
// catches what this one structurally cannot.
func TestNothingIssuesAStatementOnTheReplicatedPool(t *testing.T) {
	t.Parallel()

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN. A guard asserting an
	// absence passes identically when the thing is absent and when the
	// guard has gone inert.
	for _, positive := range []string{
		"x.db.Replicated().SQL().QueryContext(ctx, q)",
		"db := g.db.Replicated().SQL()",
		// The second incident's third site, verbatim as it stood.
		"if info, err := os.Stat(r.db.Replicated().Path()); err == nil { _ = info }",
		"e := x.db.Replicated().Estate()",
	} {
		if len(poolSites(t, snippet(positive))) == 0 {
			t.Errorf("control: %q reaches the pool and the matcher did not "+
				"flag it", positive)
		}
	}
	for _, negative := range []string{
		"x.db.Replicated().Read(ctx, fn)",
		"x.db.Replicated().Tx(ctx, fn)",
		"w, err := db.Replicated().Writer(ctx)",
		"info, err := s.deps.DB.Replicated().Backup(ctx, part)",
		"d.SQL().QueryContext(ctx, q)", // the NODE estate, which one Open owns
		// PROSE IS NOT CODE, and this guard was line-based text matching
		// until it flagged the doc comment on store.open() for quoting the
		// shape it forbids. A guard that dictates how its own rule may be
		// written down is one that gets worked around rather than read,
		// and internal/httpx's process-pool guard had already settled the
		// question the other way: it "matches a selector resolved against
		// the file's own import, so a doc comment naming the symbol is not
		// a hit."
		"// six call sites took Replicated().SQL() and issued on a nil pool",
	} {
		if sites := poolSites(t, snippet(negative)); len(sites) != 0 {
			t.Errorf("control: %q was flagged, so this guard fails on the "+
				"correct shape", negative)
		}
	}

	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	var found []string
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "static":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for _, line := range poolSites(t, string(body)) {
			found = append(found, rel+":"+itoa(line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	for _, site := range found {
		t.Errorf("%s reaches through Replicated() to a method that cannot answer "+
			"on a handle which is not open — and nil is exactly what Replicated() "+
			"returns during an adoption's rename and after Close, so this is a "+
			"segfault waiting for that window. Use one of %v, which answer "+
			"ErrNoEstate (or the handle's own nil-safe helper, e.g. "+
			"ReplicatedPath instead of Replicated().Path)",
			site, slices.Sorted(maps.Keys(answersOnAClosedPeer)))
	}
}

// EVERY DOOR INTO A CLOSED ESTATE ANSWERS, AND NONE OF THEM CRASHES.
//
// The structural guard above is necessary and was not sufficient. It sends
// every caller through [store.DB.Read] and [store.DB.Tx] on the promise that
// those answer [store.ErrNoEstate] — and for a NIL handle they did not.
// Both built their retry budget from a field on the receiver before reaching
// the nil check three frames in, so the process still died; only the frame in
// the stack changed, from `database/sql.(*DB).conn(0x0, …)` to
// `store.(*DB).Read(0x0, …)`. CI run 35312291605 on main is that crash, in
// the trim's own tick, under the same `tracker.Evictions` ->
// `(*retention).tombstones` stack as the incident the structural guard was
// written after. The caller had `errors.Is(err, store.ErrNoEstate)` the whole
// time.
//
// AND IT NEEDS NO RACE. The comment above says a behavioural test "would have
// to close an estate underneath a running loop at the instant it issues a
// statement, which is a race a test can only lose reliably" — true of a
// handle being closed, and not true of the state that reaches these doors. A
// closed peer IS a nil `*store.DB`, and a nil receiver is a value a test can
// simply hold. Every case here is deterministic.
func TestAClosedEstateAnswersRatherThanCrashing(t *testing.T) {
	t.Parallel()

	// The state itself, established rather than assumed: this is what a
	// caller holds after an adoption's rename or a Close.
	var closed *store.DB
	if peer := closed.Replicated(); peer != nil {
		t.Fatalf("Replicated() on a closed estate = %v, want nil", peer)
	}
	if pool := closed.SQL(); pool != nil {
		t.Fatalf("SQL() on a closed estate = %v, want nil", pool)
	}

	ctx := t.Context()
	ran := func(*sql.Tx) error {
		t.Error("the body ran against an estate that is not open")
		return nil
	}

	for _, c := range []struct {
		door string
		call func() error
	}{
		{"Read", func() error { return closed.Read(ctx, ran) }},
		{"Tx", func() error { return closed.Tx(ctx, ran) }},
		{"Writer", func() error { _, err := closed.Writer(ctx); return err }},
	} {
		t.Run(c.door, func(t *testing.T) {
			// NOT t.Parallel(): the recover below has to belong to this
			// goroutine, and a panic in a parallel subtest takes the
			// binary down before any assertion here can name the door.
			defer func() {
				if p := recover(); p != nil {
					t.Fatalf("%s panicked on an estate that is not open (%v). "+
						"A closed replicated peer is a documented state, not a "+
						"caller bug — an adoption's rename and Close both "+
						"produce it — so a tick already in flight reaches here "+
						"legitimately and must be told so", c.door, p)
				}
			}()
			if err := c.call(); !errors.Is(err, store.ErrNoEstate) {
				t.Errorf("%s on a closed estate = %v, want ErrNoEstate; every "+
					"caller of these doors branches on that sentinel", c.door, err)
			}
		})
	}
}

// answersOnAClosedPeer is every method of [store.DB] that a caller holding a
// CLOSED peer may call: each one checks the handle before it touches a field
// and reports rather than crashing. Read, Tx and Writer answer
// [store.ErrNoEstate]; Backup answers an error of its own; Close is a no-op.
//
// Anything absent from this set touches `d` unguarded, so calling it through
// [store.DB.Replicated] is a crash the moment that window opens. Adding a name
// here is a claim about that method, and the claim is checked one test down in
// [TestAClosedEstateAnswersRatherThanCrashing].
var answersOnAClosedPeer = map[string]bool{
	"Read":   true,
	"Tx":     true,
	"Writer": true,
	"Backup": true,
	"Close":  true,
}

// poolSites reports the lines of one Go source file that reach through
// [store.DB.Replicated] to a method which cannot answer on a closed peer.
//
// AN AST WALK RATHER THAN A SUBSTRING, for the reason internal/httpx's
// process-pool guard gives for the same shape: a comment naming the forbidden
// call is not a use of it. As text matching, this guard flagged the doc
// comment on store.open() — prose whose whole subject is why the rule exists
// — and a guard that forbids writing its own rule down is one the next reader
// routes around instead of reading.
//
// The predicate is a call whose RECEIVER is a call to Replicated and whose
// own name is not in [answersOnAClosedPeer], so it is exact in both
// directions: `d.SQL()` on the node estate is not a hit, because its receiver
// is not a Replicated() call, and neither is `Replicated().Read(…)`.
//
// WHAT IT DOES NOT SEE, stated rather than left to be discovered: binding the
// peer to a variable first — `h := db.Replicated(); h.Foo()` — escapes it,
// because following that would be dataflow analysis rather than a shape
// match. Every such site in the tree was read by hand when this became a
// whitelist and each is either guarded (statelog.go, backup.go,
// retentioncapacity.go) or holds a handle store.Open has just brought up with
// nothing able to close it (cmd/crewlet/ops.go). A reader adding one is on
// their own, which is why it is written here.
func poolSites(t *testing.T, src string) []int {
	t.Helper()

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "src.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var lines []int
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		outer, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || answersOnAClosedPeer[outer.Sel.Name] {
			return true
		}
		recv, isCall := outer.X.(*ast.CallExpr)
		if !isCall {
			return true
		}
		inner, isSel := recv.Fun.(*ast.SelectorExpr)
		if isSel && inner.Sel.Name == "Replicated" {
			lines = append(lines, fset.Position(call.Pos()).Line)
		}
		return true
	})
	return lines
}

// snippet makes one line of a control case into a parsable file.
func snippet(line string) string {
	return "package p\nfunc f() {\n\t" + line + "\n}\n"
}

// itoa avoids importing strconv for one call in a test whose subject is a walk.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
