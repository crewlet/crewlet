package logging_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestNoLoggerOutsideATestIsBuiltToDiscard fails the build when any non-test
// file under internal/ or cmd/ builds a logger that writes nowhere.
//
// # Why this is a rule and not a review comment
//
// Nine constructors read an absent logger as `slog.New(slog.DiscardHandler)`
// — six in internal/statelog, two in internal/tracker and one in
// internal/search — and the engine handed a logger to fewer than half of the
// ones it built. So the state log's applier, its write authority and its
// re-anchor, and the tracker's housekeeping duty, logged into nothing on every
// production node: `statelog_apply_faulted`, `statelog_record_gated`,
// `statelog_write_gated`, `statelog_publish_unknown` and
// `tracker_one_sided_repair_failed` among them. Nothing failed and nothing
// looked wrong, because a missing warning is indistinguishable from a quiet
// system — which is the one property that makes this worth a guard: the
// omission it rules out has no other symptom.
//
// A package's absent logger is its own [logging.Get] logger instead, the
// convention every other optional logger in the tree already follows. A TEST
// may still discard, and does, which is why _test.go files are not read.
//
// # What it flags
//
//   - slog.DiscardHandler, named anywhere;
//   - slog.NewTextHandler or slog.NewJSONHandler handed io.Discard as its
//     writer, which is the same handler spelled the long way.
//
// Each is matched as a SELECTOR on the file's own import of log/slog or io,
// never as text, so a comment naming slog.DiscardHandler — this one, and the
// package docs that explain why it is gone — is not a hit, and a renamed
// import is still covered.
//
// # Coverage boundary
//
// It does not see a handler whose level is set above every record, a writer
// that discards by construction under another name, or a nil logger stored
// and never defaulted — which panics on its first line rather than dropping
// it, and so announces itself. What it closes is the shape the tree actually
// had, written the way it was written.
func TestNoLoggerOutsideATestIsBuiltToDiscard(t *testing.T) {
	t.Parallel()

	root := sourcetree.Root(t)
	found := walkForDiscardingLoggers(t, root)

	if found.files == 0 {
		t.Fatal("parsed no non-test source importing log/slog under internal/ " +
			"and cmd/ — this guard was certifying nothing. Check the module " +
			"root and the internal/ and cmd/ globs")
	}
	// A guard asserting an ABSENCE passes identically when the thing is
	// absent and when the matcher has gone inert, so the matcher's verdicts
	// on known sources are asserted first.
	for _, c := range []struct {
		src  string
		want bool
	}{
		{`package p; import "log/slog"; var _ = slog.New(slog.DiscardHandler)`, true},
		{`package p; import s "log/slog"; var _ = s.DiscardHandler`, true},
		{`package p; import ("io"; "log/slog"); var _ = slog.NewTextHandler(io.Discard, nil)`, true},
		{`package p; import ("io"; "log/slog"); var _ = slog.NewJSONHandler(io.Discard, nil)`, true},
		{`package p; import (x "io"; "log/slog"); var _ = slog.NewJSONHandler(x.Discard, nil)`, true},
		// A real writer, another package's Discard, and a local that
		// merely shares the import's name.
		{`package p; import ("os"; "log/slog"); var _ = slog.NewTextHandler(os.Stderr, nil)`, false},
		{`package p; import ("example.com/io"; "log/slog"); var _ = slog.NewTextHandler(io.Discard, nil)`, false},
		{`package p; import "example.com/slog"; var _ = slog.DiscardHandler`, false},
		{`package p; import "log/slog"; func f(slog struct{ DiscardHandler int }) { _ = slog.DiscardHandler }`, false},
		{`package p; import ("io"; "log/slog"); func f() { _, _ = io.Copy(io.Discard, nil); _ = slog.Default() }`, false},
	} {
		if got := discardsInSource(t, c.src); got != c.want {
			t.Errorf("control: matcher returned %v for %q, want %v", got, c.src, c.want)
		}
	}

	for _, hit := range found.hits {
		t.Errorf("%s builds a logger that writes nowhere (%s).\n"+
			"\tdefault an absent logger to the package's own logging.Get one — "+
			"a warning written to a discarding handler is indistinguishable "+
			"from a system with nothing to warn about", hit.pos, hit.what)
	}
	t.Logf("scanned %d non-test files importing log/slog under internal/ and "+
		"cmd/: %d discarding logger(s)", found.files, len(found.hits))
}

type discardHit struct {
	pos  string
	what string
}

type discardWalk struct {
	files int
	hits  []discardHit
}

// walkForDiscardingLoggers parses every non-test .go file under internal/ and
// cmd/ that imports log/slog.
//
// THE PRODUCTION HALF ONLY, which is the opposite of the pool guard in
// internal/httpx: there the test half carries the bug, and here the test half
// is where discarding is the right call.
func walkForDiscardingLoggers(t *testing.T, root string) discardWalk {
	t.Helper()
	fset := token.NewFileSet()
	var out discardWalk
	for _, tree := range []string{"internal", "cmd"} {
		err := sourcetree.Walk(filepath.Join(root, tree), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if filepath.Base(path) == "testdata" {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Errorf("parse %s: %v", path, err)
				return nil
			}
			if _, ok := importName(file, "log/slog", "slog"); !ok {
				return nil
			}
			out.files++
			for _, use := range discardingUses(file) {
				out.hits = append(out.hits, discardHit{
					pos:  relativePos(root, fset.Position(use.node.Pos()).String()),
					what: use.what,
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", tree, err)
		}
	}
	return out
}

// importName is the local name path is imported under in file, if it is.
func importName(file *ast.File, path, def string) (string, bool) {
	for _, imp := range file.Imports {
		if imp.Path == nil || imp.Path.Value != `"`+path+`"` {
			continue
		}
		if imp.Name != nil {
			if imp.Name.Name == "_" || imp.Name.Name == "." {
				// A blank import names nothing, and a dot import would
				// need identifier resolution rather than a selector —
				// none exists in this tree, and the controls would say
				// so if the matcher stopped seeing the shape it has.
				return "", false
			}
			return imp.Name.Name, true
		}
		return def, true
	}
	return "", false
}

type discardUse struct {
	node ast.Node
	what string
}

// discardingUses finds every discarding handler file builds.
func discardingUses(file *ast.File) []discardUse {
	slogName, ok := importName(file, "log/slog", "slog")
	if !ok {
		return nil
	}
	ioName, hasIO := importName(file, "io", "io")

	var out []discardUse
	ast.Inspect(file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			if isImportSelector(n, slogName, "DiscardHandler") {
				out = append(out, discardUse{node: n, what: slogName + ".DiscardHandler"})
			}
		case *ast.CallExpr:
			if !hasIO || len(n.Args) == 0 {
				return true
			}
			fn, ok := n.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			for _, ctor := range []string{"NewTextHandler", "NewJSONHandler"} {
				if isImportSelector(fn, slogName, ctor) {
					if w, ok := n.Args[0].(*ast.SelectorExpr); ok && isImportSelector(w, ioName, "Discard") {
						out = append(out, discardUse{node: n,
							what: slogName + "." + ctor + "(" + ioName + ".Discard, …)"})
					}
				}
			}
		}
		return true
	})
	return out
}

// isImportSelector reports whether sel is `name.field` with name the file's
// import rather than anything declared in it. ident.Obj is set for a name the
// parser resolved to a declaration in this file — a variable or a parameter —
// and nil for an import, which is what tells the two apart.
func isImportSelector(sel *ast.SelectorExpr, name, field string) bool {
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == name && ident.Obj == nil && sel.Sel.Name == field
}

// discardsInSource runs the matcher over one snippet, for the controls.
func discardsInSource(t *testing.T, src string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "control.go", src, 0)
	if err != nil {
		t.Fatalf("parse control %q: %v", src, err)
	}
	return len(discardingUses(file)) > 0
}

func relativePos(root, pos string) string {
	if rel, err := filepath.Rel(root, pos); err == nil {
		return rel
	}
	return pos
}
