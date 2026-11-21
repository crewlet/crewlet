package logging_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestNoPackageLevelLoggerIsBoundToTheBootDefault fails the build when a
// non-test file under internal/ or cmd/ builds a FILE-SCOPE logger (outside a
// function literal) from slog's default: `slog.Default().With(…)`,
// `slog.With(…)` or `slog.Default()`.
//
// At init slog.Default is the boot default (info, console, no file), so such
// a logger ignores `-log-level debug`, a log file and a format ([logging.Get]
// resolves per record), and compiles every redact rule at init. Matched as a
// selector on the import: a comment or same-named local is no hit.
// Mutation: drop "Default" from the names bootBoundLoggers matches, and the
// first, third, fourth and fifth controls go red.
func TestNoPackageLevelLoggerIsBoundToTheBootDefault(t *testing.T) {
	t.Parallel()

	root := sourcetree.Root(t)
	found := walkSlogImporters(t, root, bootBoundLoggers)
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
		{`package p; import "log/slog"; var log = slog.Default().With("component", "p")`, true},
		{`package p; import "log/slog"; var log = slog.With("component", "p")`, true},
		{`package p; import s "log/slog"; var log = s.Default()`, true},
		{`package p; import "log/slog"; var (n = 1; log = slog.Default())`, true},
		{`package p; import "log/slog"; var logs = []*slog.Logger{slog.Default()}`, true},
		// Inside a function or a function literal: resolved when it runs.
		{`package p; import "log/slog"; func f() { log := slog.Default(); _ = log }`, false},
		{`package p; import "log/slog"; var get = func() *slog.Logger { return slog.Default() }`, false},
		// The function itself, called when it is needed; another package's
		// Default; and a logger on a handler of its own.
		{`package p; import "log/slog"; var get = slog.Default`, false},
		{`package p; import "example.com/slog"; var log = slog.Default()`, false},
		{`package p; import ("log/slog"; "os"); var log = slog.New(slog.NewTextHandler(os.Stderr, nil))`, false},
	} {
		if got := bootBoundInSource(t, c.src); got != c.want {
			t.Errorf("control: matcher returned %v for %q, want %v", got, c.src, c.want)
		}
	}

	for _, hit := range found.hits {
		t.Errorf("%s builds a package-level logger out of slog's boot default (%s).\n"+
			"\tuse logging.Get(component), which resolves the configured root on "+
			"every record — this one never sees the node's level, log file or "+
			"format, and its bound attributes are redacted at init", hit.pos, hit.what)
	}
	t.Logf("scanned %d non-test files importing log/slog under internal/ and "+
		"cmd/: %d logger(s) bound to the boot default", found.files, len(found.hits))
}

// bootBoundLoggers finds every call of slog's Default or With in the value of
// a file-scope var.
func bootBoundLoggers(file *ast.File) []slogUse {
	slogName, ok := importName(file, "log/slog", "slog")
	if !ok {
		return nil
	}
	var out []slogUse
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.VAR {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for _, v := range value.Values {
				ast.Inspect(v, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.FuncLit:
						return false
					case *ast.CallExpr:
						fn, ok := n.Fun.(*ast.SelectorExpr)
						if !ok {
							return true
						}
						for _, name := range []string{"Default", "With"} {
							if isImportSelector(fn, slogName, name) {
								out = append(out, slogUse{node: n, what: slogName + "." + name})
							}
						}
					}
					return true
				})
			}
		}
	}
	return out
}

// bootBoundInSource runs the matcher over one snippet, for the controls.
func bootBoundInSource(t *testing.T, src string) bool {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), "control.go", src, 0)
	if err != nil {
		t.Fatalf("parse control %q: %v", src, err)
	}
	return len(bootBoundLoggers(file)) > 0
}
