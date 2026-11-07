package logging_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
)

// TestNoPackageLevelLoggerIsBoundToTheBootDefault fails the build when any
// non-test file under internal/ or cmd/ builds a package-level logger out of
// slog's default: `var log = slog.Default().With(…)`, `slog.With(…)`, or
// `slog.Default()` itself.
//
// # Why this is a rule and not a review comment
//
// A package-level var is set at init, before main has parsed a flag or read
// its config, so what slog.Default returns then is the boot default this
// package's own init installs: info, the console, no file. A logger built from
// it keeps that handler for the life of the process, so `-log-level debug`, a
// log file and a format never reach its lines. [logging.Get] resolves the root
// on every record instead, which is why it is the convention.
//
// And every process pays for one at init: the root's redaction reads an
// attribute as it is bound ([slog.Handler.WithAttrs]), so the first such
// logger in init order compiled every redact rule during its own package's
// init. The tree's one such logger, internal/assistant/proposal/approval's,
// allocated 189 KB at init in the plain build once redact's rule table was
// built at first use, which put the module past the bound cmd/crewlet's
// TestTheHookPaysLittleForTheEnginesInit holds. That test caught it by its
// cost; this names the shape, whatever it costs.
//
// # What it flags
//
// A call of the file's log/slog import's Default or With anywhere in the
// value of a FILE-SCOPE var, outside a function literal, which runs when it is
// called rather than at init. Each is matched as a selector on the import, as
// the discard guard matches, so a comment naming slog.Default or a local that
// shares the import's name is not a hit.
//
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
