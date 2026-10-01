package queries_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/tokens"
)

// A TOKENS ANSWER IS LABELLED ON THE SOURCES' CLOCK.
//
// It read the wall clock while `token_series` beside it read this one, so the
// breakdown and the chart a screen draws from the same window were dated by
// two clocks — and a caller that pinned the clock to compare one question
// asked twice got two answers whenever the asks straddled a second, because
// the window's edges are what the answer is labelled with.
func TestATokensAnswerIsLabelledOnTheSourcesClock(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	r := registryOver(t, queries.Sources{
		State: livestate.New(),
		Now:   func() time.Time { return at },
	})
	got := askRaw(t, r, "tokens", nil).(tokens.Rollup)
	if want := at.Format(time.RFC3339); got.Until != want {
		t.Errorf("until = %s, want %s — the window ends at the instant the "+
			"sources' clock reads, not whenever the answer happened to run",
			got.Until, want)
	}
	if want := at.Add(-livestate.LiveSpendWindow).Format(time.RFC3339); got.Since != want {
		t.Errorf("since = %s, want %s", got.Since, want)
	}
}

// EVERY ANSWER READS THE SOURCES' CLOCK, and nothing here reads the wall clock
// but [queries.Sources] itself.
//
// Eight answers read `time.Now` directly — `tokens` and seven of the tracker's
// — so pinning [queries.Sources.Now] pinned some answers and not others, and
// nothing said which. The one place the wall clock may be read is the method
// that falls back to it; every `time.Now`, `time.Since` and `time.Until`
// anywhere else in the package fails this walk, a reference as much as a call,
// since `Now: time.Now` hands the wall clock on just the same.
func TestEveryAnswerReadsTheSourcesClock(t *testing.T) {
	t.Parallel()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	read, allowed := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		read++
		alias := timeImport(file)
		if alias == "" {
			continue
		}
		for _, decl := range file.Decls {
			fn, isFunc := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != alias {
					return true
				}
				switch sel.Sel.Name {
				case "Now", "Since", "Until":
				default:
					return true
				}
				if isFunc && isSourcesClock(fn) {
					allowed++
					return true
				}
				t.Errorf("%s reads the wall clock (time.%s) — read Sources.clock, "+
					"so a caller that pins Sources.Now pins this answer too",
					fset.Position(sel.Pos()), sel.Sel.Name)
				return true
			})
		}
	}
	// A FLOOR ON WHAT WAS READ, because a walk of the wrong directory finds
	// nothing to object to and passes; and the fallback itself must be
	// found, or the walk is not looking where the clock is.
	if read < 10 {
		t.Fatalf("read %d source files — the walk is not in this package", read)
	}
	if allowed != 1 {
		t.Fatalf("found the wall-clock fallback in Sources.clock %d time(s), want "+
			"exactly once", allowed)
	}
}

// timeImport is the name a file refers to package time by, or "" if it does
// not import it.
func timeImport(file *ast.File) string {
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path != "time" {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return "time"
	}
	return ""
}

// isSourcesClock reports whether fn is the Sources method that falls back to
// the wall clock.
func isSourcesClock(fn *ast.FuncDecl) bool {
	if fn.Name.Name != "clock" || fn.Recv == nil || len(fn.Recv.List) != 1 {
		return false
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	ident, ok := recv.(*ast.Ident)
	return ok && ident.Name == "Sources"
}
