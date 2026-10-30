package plainjson_test

import (
	"go/build"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// THIS PACKAGE IMPORTS ONLY THE STANDARD LIBRARY, its own suite included, and
// that is the property every reader of it relies on: the prior-work ledger
// may import nothing that drags a layer behind the values the turn context,
// the prompt builder and the API hold, and a provider backend may import
// nothing from the tool registry above it. The first engine package admitted
// here is the import that stops one of them being able to use it — and
// everything still compiles, so a comment saying so would not notice.
//
// Mutation: add a blank import of github.com/google/uuid to plainjson.go and
// this fails naming it.
func TestThePackageImportsOnlyTheStandardLibrary(t *testing.T) {
	t.Parallel()
	// THE FILES THE BUILD COMPILES, which go/build answers with the build
	// constraints applied, and a directory listing does not.
	pkg, err := build.ImportDir(".", 0)
	if err != nil {
		t.Fatalf("listing the package's files: %v", err)
	}
	fset := token.NewFileSet()
	files, checked := 0, 0
	for _, list := range [][]string{pkg.GoFiles, pkg.TestGoFiles, pkg.XTestGoFiles} {
		for _, name := range list {
			files++
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
				if path == "github.com/crewlet/crewlet/internal/plainjson" && strings.HasSuffix(name, "_test.go") {
					// The external suite importing the package it tests.
					continue
				}
				if !standardLibrary(path) {
					t.Errorf("%s imports %q, which is not the standard library", name, path)
				}
			}
		}
	}
	// THE CONTROLS: a walk that read no file, or no import, passes every
	// assertion above and protects nothing.
	if len(pkg.GoFiles) == 0 || files < 3 {
		t.Fatalf("walked %d files (%d of them the package's own); the walk is not reading the package",
			files, len(pkg.GoFiles))
	}
	if checked < 10 {
		t.Fatalf("checked %d imports; the walk is not reading import blocks", checked)
	}
}

// standardLibrary reports whether an import path is a standard-library one:
// the first element of every module path holds a dot (it is a domain), and no
// standard-library path's does — the toolchain's own rule, rather than a list
// of package names this test would have to be taught as the library grows.
func standardLibrary(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
