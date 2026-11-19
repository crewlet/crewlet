package schedule_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestOnlyOneClockReadsTheWallTime fails the build when any engine-side file
// in this package tree calls time.Now() outside the package's single clock
// function. Every time-dependent decision is driven by an instant it was HANDED
// (Tick takes `now`, a Run carries its FiredAt), so DST, catchup and tick-window
// cases assert behaviour at an instant no clock moves under; a stray time.Now()
// makes a test pass for the wrong reason, then disagree on a DST boundary.
//
// AST, NOT GREP: the invariant is about scope, which line scanning cannot see (a
// window grep under-reports, a brace scan runs past a closure), and a doc
// comment mentioning time.Now is not a call. It walks internal/schedule and its
// sqlledger backend, NOT _test.go files, internal/schedule/scheduletest (never
// run in the engine), or a clock spelled otherwise (time.Since, time.Until).
func TestOnlyOneClockReadsTheWallTime(t *testing.T) {
	t.Parallel()

	root := sourcetree.Root(t)
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

func shortPos(root, pos string) string {
	if rel, err := filepath.Rel(root, pos); err == nil {
		return rel
	}
	return pos
}
