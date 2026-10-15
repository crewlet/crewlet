package tracing_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// eventsPath is the package whose NewTrace mints a trace id.
const eventsPath = "github.com/crewlet/crewlet/internal/events"

// minter is the one file allowed to call events.NewTrace: TraceOf's own.
const minter = "internal/tracing/span.go"

// ONE PLACE MINTS A ROOT, and it is TraceOf.
//
// events.NewTrace mints ids and nothing else; TraceOf mints through it AND
// DECIDES the root by the operator's sampler. Any other caller mints a root
// nobody decided, which reads as sampled whatever OTEL_TRACES_SAMPLER says —
// and since the decision travels, every turn, resume and box that continues
// that trace is recorded too. The change feed was such a caller, so every wake
// a committed record caused claimed sampled under a ratio of 0.1.
//
// NewTrace's own doc said "do not add a second caller", which held only as
// long as everybody read it. Test files may call it: a fixture's trace decides
// nothing.
//
// Mutation: put `events.NewTrace()` back in the change feed and this goes red.
func TestOnlyTraceOfMintsARoot(t *testing.T) {
	t.Parallel()
	root := sourcetree.Root(t)
	sites, files := mintSites(t, root, "internal", "cmd")
	if files == 0 {
		t.Fatal("read no source files — this guard was certifying nothing")
	}
	// TWO-SIDED: TraceOf's own call is the one this guard allows, and one
	// it no longer finds means it is watching for a shape nobody writes.
	if !slices.ContainsFunc(sites, func(s string) bool { return strings.HasPrefix(s, minter+":") }) {
		t.Errorf("found no events.NewTrace call in %s, so this guard no longer "+
			"knows where a root is minted: fix the allowed file", minter)
	}
	for _, s := range sites {
		if !strings.HasPrefix(s, minter+":") {
			t.Errorf("%s mints a trace with events.NewTrace — a root no sampler "+
				"decided. Use tracing.TraceOf(ctx), which mints the same ids and "+
				"decides them", s)
		}
	}
}

// THE WALK, ON A TREE WHOSE VERDICT IS KNOWN: a plain call, one through an
// aliased import, a same-named function of another package, a call inside the
// events package itself, and a test file — of which the first two and the
// fourth are mint sites and the others are not.
func TestTheMintWalkFindsEveryCaller(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for path, body := range map[string]string{
		"internal/plain/plain.go": "package plain\n\nimport \"" + eventsPath + "\"\n\n" +
			"var _ = events.NewTrace()\n",
		"internal/alias/alias.go": "package alias\n\nimport ev \"" + eventsPath + "\"\n\n" +
			"var _ = ev.NewTrace()\n",
		"internal/other/other.go": "package other\n\nimport \"example.com/events\"\n\n" +
			"var _ = events.NewTrace()\n",
		"internal/events/self.go": "package events\n\nvar _ = NewTrace()\n",
		"internal/plain/plain_test.go": "package plain\n\nimport \"" + eventsPath + "\"\n\n" +
			"var _ = events.NewTrace()\n",
	} {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sites, _ := mintSites(t, root, "internal")
	slices.Sort(sites)
	want := []string{
		"internal/alias/alias.go:5", "internal/events/self.go:3", "internal/plain/plain.go:5",
	}
	if !slices.Equal(sites, want) {
		t.Errorf("found %v, want %v", sites, want)
	}
}

// mintSites is every call of events.NewTrace in the non-test Go files under
// root's dirs, as `path:line` relative to root, and how many files it read.
func mintSites(t *testing.T, root string, dirs ...string) ([]string, int) {
	t.Helper()
	// An identifier has no escapes, so a file that does not spell NewTrace
	// calls no NewTrace and is not parsed.
	spells := sourcetree.MustIdentifiers("NewTrace")
	fset := token.NewFileSet()
	var sites []string
	files := 0
	for _, dir := range dirs {
		err := sourcetree.Walk(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			files++
			src, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			if !spells.In(src) {
				return nil
			}
			file, err := parser.ParseFile(fset, p, src, parser.SkipObjectResolution)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			for _, line := range mintCalls(fset, file) {
				sites = append(sites, filepath.ToSlash(rel)+":"+strconv.Itoa(line))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	return sites, files
}

// mintCalls is the line of every call of the events package's NewTrace in one
// file: through whatever name the file imports the package under, or bare
// inside the package itself.
func mintCalls(fset *token.FileSet, file *ast.File) []int {
	local := ""
	for _, imp := range file.Imports {
		if path, _ := strconv.Unquote(imp.Path.Value); path == eventsPath {
			local = "events"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
	}
	inside := file.Name.Name == "events"
	var lines []int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			if x, ok := fn.X.(*ast.Ident); ok && local != "" && x.Name == local && fn.Sel.Name == "NewTrace" {
				lines = append(lines, fset.Position(call.Pos()).Line)
			}
		case *ast.Ident:
			if inside && fn.Name == "NewTrace" {
				lines = append(lines, fset.Position(call.Pos()).Line)
			}
		}
		return true
	})
	return lines
}
