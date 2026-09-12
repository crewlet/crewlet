package metrics_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A DECLARED INSTRUMENT WITH NO WRITER IS A DOCUMENTED ZERO.
//
// The recorder refuses a name it has no instrument for, so a typo at a WRITE
// site fails immediately. The opposite has no symptom at all: an instrument
// declared in the catalogue, published in docs/reference/metrics.md and
// recorded by nobody exports a series that never appears, and every surface
// downstream reads its absence as "nothing to report". Eleven of them shipped
// that way — including the three the capacity alarms fire on and the one the
// backup alarm is named after — and each looked exactly like a healthy fleet.
//
// It is worse than an ordinary missing feature because the catalogue is a
// PROMISE: an operator builds a dashboard panel and an alert rule from the
// reference page, and both are silent for ever.
//
// So the rule is that every catalogued instrument is named somewhere outside
// this package by code that is not a test. That is not proof it is recorded
// correctly — nothing structural can be — but it is proof that the
// declaration and the engine are connected at all, which is the half that
// failed.
func TestEveryCatalogedInstrumentHasARecordingSite(t *testing.T) {
	t.Parallel()

	byValue := instrumentIdentifiers(t)
	used := identifiersReferencedInTheTree(t)

	// THE MATCHER, ON INPUT WHOSE VERDICT IS KNOWN. A guard asserting a
	// presence passes identically when everything is present and when the
	// walk found no files at all.
	if !used["StatelogApplyRecords"] {
		t.Fatal("control: the applier's own record counter was not found, so " +
			"this walk is reading nothing and would pass on an empty tree")
	}
	if used["StatelogNoSuchInstrumentExists"] {
		t.Fatal("control: an instrument nobody declares was reported as used, " +
			"so this guard cannot fail")
	}

	for _, instrument := range metrics.Catalogue() {
		ident, named := byValue[instrument.Name]
		if !named {
			t.Errorf("%s is catalogued but is not one of the constants in "+
				"names.go, so no call site can refer to it by name",
				instrument.Name)
			continue
		}
		if !used[ident] {
			t.Errorf("%s (metrics.%s) is catalogued and published in "+
				"docs/reference/metrics.md, and nothing outside this package "+
				"records it — so the series never appears and every alarm, "+
				"panel and operator record reading it sees a healthy zero. "+
				"Record it where the thing it measures happens, or remove it "+
				"from the catalogue with the reason at the instrument it "+
				"duplicates", instrument.Name, ident)
		}
	}
}

// instrumentIdentifiers maps each instrument's wire name to the Go constant
// that holds it, read out of names.go itself.
//
// FROM THE SOURCE rather than from a table written here, because a second copy
// of the mapping is a second thing to keep in step — and this test's whole
// subject is two declarations that stopped agreeing.
func instrumentIdentifiers(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "names.go", nil, 0)
	if err != nil {
		t.Fatalf("parse names.go: %v", err)
	}
	out := map[string]string{}
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok || len(spec.Names) != 1 || len(spec.Values) != 1 {
			return true
		}
		lit, ok := spec.Values[0].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		value, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		out[value] = spec.Names[0].Name
		return true
	})
	if len(out) == 0 {
		t.Fatal("names.go declared no instrument constants, which cannot be " +
			"right — the parse found nothing and this guard would pass on any " +
			"catalogue at all")
	}
	return out
}

// selector matches a reference to one of this package's exported identifiers.
//
// A REGEX RATHER THAN A TYPE-CHECKED WALK. Loading the whole module with
// go/packages to resolve one selector per instrument costs more than the
// question is worth, and the false positives a regex can produce here are all
// harmless: it can only make a name look USED, and a name that is used is
// exactly what this asserts.
var selector = regexp.MustCompile(`\bmetrics\.([A-Z]\w*)\b`)

// identifiersReferencedInTheTree is every metrics identifier named by
// non-test code outside this package.
func identifiersReferencedInTheTree(t *testing.T) map[string]bool {
	t.Helper()
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("resolve the module root: %v", err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatalf("resolve this package: %v", err)
	}
	out := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case path == self:
				// THIS PACKAGE IS EXCLUDED, and it has to be: the
				// catalogue names every instrument, so a walk that
				// included it would report all of them as used and
				// this test would be unable to fail.
				return fs.SkipDir
			case d.Name() == ".git" || d.Name() == "node_modules" ||
				d.Name() == "dist" || d.Name() == "static":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			return err
		}
		for _, match := range selector.FindAllSubmatch(body, -1) {
			out[string(match[1])] = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	return out
}
