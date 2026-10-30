package ledger

import (
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// THE LEDGER IMPORTS NOTHING BUT THE STANDARD LIBRARY AND ONE LEAF over it,
// which is the property its package doc states: the turn context, the prompt
// builder and the API layer all hold ledger values, so whatever the ledger
// imports, all three drag behind them. internal/plainjson is admitted because
// it is held to the standard library by a test of its own, so admitting it
// admits nothing further. Every other engine package is the import that ends
// the property — and everything would still compile, so the doc alone would
// not notice.
//
// The package's own files, not its suite: what the holders drag is what the
// build links into them.
//
// Mutation: add a blank import of internal/logging to doc.go and this fails
// naming it.
func TestTheLedgerImportsNothingButALeaf(t *testing.T) {
	t.Parallel()
	const leaf = "github.com/crewlet/crewlet/internal/plainjson"

	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("listing the package's files: %v", err)
	}
	fset := token.NewFileSet()
	checked, sawLeaf := 0, false
	for _, name := range pkg.GoFiles {
		parsed, err := parser.ParseFile(fset, filepath.Join(pkg.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquoting import %s: %v", name, spec.Path.Value, err)
			}
			checked++
			if path == leaf {
				sawLeaf = true
				continue
			}
			// The first element of every module path holds a dot (it is a
			// domain) and no standard-library path's does.
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q, which is neither the standard library nor %s", name, path, leaf)
			}
		}
	}
	// THE CONTROLS: a walk that read no file, or no import, passes every
	// assertion above and protects nothing; and the leaf it admits must be
	// one it actually read, or the exception is a name nothing checks.
	if len(pkg.GoFiles) < 3 || checked < 5 {
		t.Fatalf("read %d files and %d imports; the walk is not reading the package",
			len(pkg.GoFiles), checked)
	}
	if !sawLeaf {
		t.Fatalf("no file imports %s; the admitted leaf is stale", leaf)
	}
}
