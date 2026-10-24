package iam

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// leafSet is everything this package may import beyond the standard library.
//
// ONE ENTRY, and it earns its place the way the tree's dependency rule asks:
// the code convention is that an id is a uuid.UUID, and the alternative — a
// string id, or a uuid type of this package's own — would make [Principal.ID]
// the one id in the engine that does not compare with the others.
//
// NOTHING ELSE GOES IN HERE WITHOUT THE LEAF PROPERTY BEING RE-ARGUED. The
// reason this package holds values only is that config, the tool layer and the
// query registry all have to be able to name a Principal, and the first store
// client, coordination backend or password hasher admitted here is the import
// that stops one of them being able to.
var leafSet = []string{"github.com/google/uuid"}

// TestThePackageImportsNothingBeyondItsLeafSet is the guard that keeps this a
// leaf when somebody later reaches for a store client to "just look the
// principal up here".
//
// A test rather than a comment because a comment has already failed at this
// job elsewhere in the tree: the import is added, everything still compiles,
// and the property is gone with nothing to say so.
func TestThePackageImportsNothingBeyondItsLeafSet(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := token.NewFileSet()
	files, checked := 0, 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" {
			continue
		}
		files++

		parsed, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquoting import %s: %v", name, spec.Path.Value, err)
			}
			checked++
			if stdlib(path) || slices.Contains(leafSet, path) {
				continue
			}
			t.Errorf("%s imports %q, which is neither the standard library nor in the leaf set %v",
				name, path, leafSet)
		}
	}

	// THE CONTROLS. A walk that found no files, or files with no imports,
	// passes every assertion above and protects nothing — which is exactly
	// how a gate reading a path that had moved certified a whole rewrite
	// elsewhere in this tree while reporting a pass.
	if files < 5 {
		t.Fatalf("walked %d .go files in this package; the walk is not reading the package", files)
	}
	if checked < 10 {
		t.Fatalf("checked %d imports; the walk is not reading import blocks", checked)
	}
}

// TestNoTestFileReachesBackIntoTheEngine is the other half. A suite that
// imported the engine to build a fixture would drag every one of its
// dependencies into this package's test binary, and the next person to read
// `go list -deps` would find a leaf that is not one.
func TestNoTestFileReachesBackIntoTheEngine(t *testing.T) {
	const module = "github.com/crewlet/crewlet/"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the package directory: %v", err)
	}

	fset := token.NewFileSet()
	suites := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		suites++

		parsed, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquoting import %s: %v", name, spec.Path.Value, err)
			}
			if strings.HasPrefix(path, module) {
				t.Errorf("%s imports %q — a leaf's own suite may not reach back into the engine", name, path)
			}
		}
	}
	if suites < 4 {
		t.Fatalf("walked %d _test.go files; the walk is not reading the suite", suites)
	}
}

// stdlib reports whether an import path is a standard-library one.
//
// The first path element of every module path contains a dot (it is a domain),
// and no standard-library path's does. That is the rule the toolchain itself
// uses, rather than a list of package names this test would have to be taught
// every time the standard library grows one.
func stdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
