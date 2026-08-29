package schedule_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestOnlyOneClockReadsTheWallTime fails the build when any engine-side file
// in this package tree calls time.Now() outside the package's single clock
// function.
//
// # Why this is a check and not a convention
//
// Every time-dependent decision the scheduler makes is driven by an instant
// it was HANDED: Tick takes `now`, the cron evaluator takes both window
// bounds, a Run carries its own FiredAt, Describe takes the reference instant.
// That is what makes the DST cases, the catchup-window cases and the
// tick-window cases mean anything — each of them asserts behaviour at an
// instant no clock is going to move underneath it.
//
// A single stray time.Now() several frames down does not fail a test. It makes
// one PASS for the wrong reason: the injected instant decides which fires are
// due, the real clock decides what the fire label says, and the two agree
// perfectly in a test that runs today and disagree on the day a schedule
// crosses a DST boundary. That failure surfaces once a year, in production, in
// whichever timezone an operator happened to configure.
//
// # Why the AST and not a grep
//
// The invariant is about SCOPE — which function a call sits in — and line
// scanning cannot see scope. A fixed-window grep under-reports by stopping
// early; a brace scan over-reports by running past a closure's `}, arg)` into
// the enclosing function. Both have been wrong in both directions in this
// repo. A call expression is a call expression wherever it sits, and a doc
// comment mentioning time.Now (this one does, repeatedly) is not one.
//
// # Coverage boundary, stated rather than assumed
//
// It walks the engine-side packages: internal/schedule and its sqlledger
// backend. It does NOT walk:
//
//   - _test.go files. A test computing a cutoff from the real clock is
//     ordinary and correct.
//   - internal/schedule/scheduletest, which is a test harness in a non-test
//     file. Its cases legitimately read the clock to build a purge cutoff, and
//     it never runs inside the engine.
//   - a clock reached through a different spelling: time.Since, time.Until, a
//     time.Now stored in a variable and called through it, or a clock behind
//     an injected interface. The first two are worth adding if they ever
//     appear; the last is what this rule wants anyway.
func TestOnlyOneClockReadsTheWallTime(t *testing.T) {
	t.Parallel()

	root := moduleRoot(t)
	// The one function permitted to read it, and the file it must live in.
	const clockFunc, clockFile = "now", "ledger.go"

	var (
		scanned   int
		found     int
		inTheOnly int
		strays    []string
	)
	for _, dir := range []string{
		filepath.Join(root, "internal", "schedule"),
		filepath.Join(root, "internal", "schedule", "sqlledger"),
	} {
		for _, path := range goFiles(t, dir) {
			scanned++
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s: %v", shortPos(root, path), err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				permitted := fn.Name.Name == clockFunc &&
					fn.Recv == nil &&
					filepath.Base(path) == clockFile
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					if !isTimeNowCall(n) {
						return true
					}
					found++
					if permitted {
						inTheOnly++
						return true
					}
					strays = append(strays, shortPos(root, fset.Position(n.Pos()).String())+
						" in "+fn.Name.Name)
					return true
				})
			}
		}
	}

	// A guard asserting an absence must assert it MATCHED its subject:
	// "found no violations" and "scanned nothing" are the same green
	// otherwise, and a package rename or a moved directory turns this into
	// the second one silently.
	if scanned < 4 {
		t.Fatalf("scanned only %d engine-side files — this guard is not looking at the "+
			"scheduler any more, so its silence means nothing", scanned)
	}
	if found == 0 {
		t.Fatalf("found no time.Now() call at all across %d files. The package HAS a clock; "+
			"a guard that cannot see it is not proving anything about the ones it forbids",
			scanned)
	}
	if inTheOnly == 0 {
		t.Fatalf("found %d time.Now() call(s) but none inside %s() in %s — either the clock "+
			"moved, in which case move this guard's constants with it, or it is gone and "+
			"every one of these calls is a stray", found, clockFunc, clockFile)
	}
	if len(strays) > 0 {
		t.Fatalf("time.Now() is read outside %s():\n  %s\n\nEvery time-dependent decision here "+
			"takes its instant as a parameter. A second clock does not fail a test — it makes "+
			"one pass for the wrong reason, until a DST boundary separates the injected "+
			"instant from the real one.",
			clockFunc, strings.Join(strays, "\n  "))
	}
}

// isTimeNowCall reports whether n is a call to time.Now().
//
// It matches on the SELECTOR — package identifier `time`, member `Now` — which
// is what a source-level rule can honestly see. A dot-import or an alias would
// slip past; neither appears in this tree, and the import block is right there
// in every file this walks.
func isTimeNowCall(n ast.Node) bool {
	call, ok := n.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Now" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "time"
}

// goFiles lists the non-test .go files directly in dir. Not recursive: each
// directory this guard covers is named explicitly, so a new subpackage has to
// be added here deliberately rather than being swept in and silently exempted
// by a filter nobody re-reads.
func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	if len(out) == 0 {
		t.Fatalf("%s holds no non-test Go file — the guard's subject has moved", dir)
	}
	return out
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	// .../go/internal/schedule/clock_guard_test.go -> .../go
	root := filepath.Dir(filepath.Dir(filepath.Dir(file)))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected the module root at %s: %v", root, err)
	}
	return root
}

func shortPos(root, pos string) string {
	if rel, err := filepath.Rel(root, pos); err == nil {
		return rel
	}
	return pos
}
