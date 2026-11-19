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

// The ledger imports only the standard library and one leaf, as its package doc
// states: the turn loop, the runner and the parked state all hold ledger
// values, so they drag in whatever it imports. internal/plainjson is admitted
// because a test of its own holds it to the standard library. Any other engine
// package ends the property yet still compiles, so only this notices. It reads
// the package's own files, not its suite: that is what the build links.
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
