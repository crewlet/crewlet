package artifacts

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
// FOUR ENTRIES, and each is argued where the package doc argues the leaf:
// the uuid an id is, the identity vocabulary a share's grantor and an
// authority's actor are written in, the object a manifest entry names, and
// the words a refusal says a set in (a leaf over the standard library).
// None of them opens a store, dials a broker or reads a clock, which is the
// property that lets internal/authz and internal/config name an artifact.
//
// NOTHING ELSE GOES IN HERE WITHOUT THE LEAF PROPERTY BEING RE-ARGUED: the
// first store client admitted here is the import that stops the authority
// table, the config validator or the dashboard contract from naming a role.
var leafSet = []string{
	"github.com/google/uuid",
	"github.com/crewlet/crewlet/internal/iam",
	"github.com/crewlet/crewlet/internal/objstore",
	"github.com/crewlet/crewlet/internal/words",
}

// importsOf is every import of every .go file in this directory whose name
// keep accepts, with the counts the controls below read.
func importsOf(t *testing.T, keep func(name string) bool) (map[string][]string, int) {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package directory: %v", err)
	}
	fset := token.NewFileSet()
	out := map[string][]string{}
	files := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) != ".go" || !keep(name) {
			continue
		}
		files++
		parsed, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("%s: unquote import %s: %v", name, spec.Path.Value, err)
			}
			out[name] = append(out[name], path)
		}
	}
	return out, files
}

// THE VOCABULARY STAYS A LEAF. A test rather than a comment, because the
// failure is silent: an import is added, everything compiles, and the
// authority table can no longer name a role without dragging a store into its
// build.
func TestThePackageImportsNothingBeyondItsLeafSet(t *testing.T) {
	imports, files := importsOf(t, func(name string) bool {
		return !strings.HasSuffix(name, "_test.go")
	})
	checked := 0
	for name, paths := range imports {
		for _, path := range paths {
			checked++
			if stdlib(path) || slices.Contains(leafSet, path) {
				continue
			}
			t.Errorf("%s imports %q, which is neither the standard library nor in the "+
				"leaf set %v", name, path, leafSet)
		}
	}
	// THE CONTROLS: a walk that found nothing passes every assertion above.
	if files < 8 {
		t.Fatalf("walked %d .go files; the walk is not reading the package", files)
	}
	if checked < 10 {
		t.Fatalf("checked %d imports; the walk is not reading import blocks", checked)
	}
}

// AND ITS OWN SUITE DOES NOT REACH BACK INTO THE ENGINE, beyond the leaf set:
// a fixture that imported a domain would drag its dependencies into this
// package's test binary, and `go list -deps` would show a leaf that is not one.
func TestNoTestFileReachesBackIntoTheEngine(t *testing.T) {
	const module = "github.com/crewlet/crewlet/"
	imports, files := importsOf(t, func(name string) bool {
		return strings.HasSuffix(name, "_test.go")
	})
	for name, paths := range imports {
		for _, path := range paths {
			if strings.HasPrefix(path, module) && !slices.Contains(leafSet, path) {
				t.Errorf("%s imports %q: the leaf's own suite may not reach back "+
					"into the engine", name, path)
			}
		}
	}
	if files < 3 {
		t.Fatalf("walked %d _test.go files; the walk is not reading the suite", files)
	}
}

// stdlib reports a standard-library import path: the first element of every
// module path is a domain and carries a dot, and no standard-library path's
// does: the toolchain's own rule.
func stdlib(path string) bool {
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}
