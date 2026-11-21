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

	"github.com/crewlet/crewlet/internal/regexlit"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A DECLARED INSTRUMENT WITH NO WRITER IS A DOCUMENTED ZERO: published in
// docs/reference/metrics.md, it exports no series, and every alarm and panel
// an operator built from the page reads its absence as a healthy fleet.
//
// So every catalogued instrument must be named by non-test code outside this
// package. That does not prove it is recorded correctly, only that the
// declaration and the engine are connected at all.
func TestEveryCatalogedInstrumentHasARecordingSite(t *testing.T) {
	t.Parallel()

	byValue := instrumentIdentifiers(t)
	used := identifiersReferencedInTheTree(t)

	// Controls with a known verdict: a presence guard also passes when the
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
// that holds it, read out of names.go itself rather than a second copy of the
// mapping that could stop agreeing with it.
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
// A regex rather than type-checking the whole module, which costs more than
// the question is worth; a false positive can only make a name look used.
var selector = regexp.MustCompile(`\bmetrics\.([A-Z]\w*)\b`)

// selectorNeeds skips a file without `metrics.`: the pattern opens with `\b`,
// leaving the regexp package no prefix to jump to, so it is slow over every
// file ([regexlit.Required]). A prefilter skipping a file holding a reference
// would fail this gate, not pass it, so the gate's verdict covers it.
var selectorNeeds = regexlit.Required(selector)

// identifiersReferencedInTheTree is every metrics identifier named by
// non-test code outside this package.
func identifiersReferencedInTheTree(t *testing.T) map[string]bool {
	t.Helper()
	root := sourcetree.Root(t)
	// From the walk's root, not the working directory, so the two agree. A
	// moved package would make the exclusion match nothing and the test
	// unable to fail, so its absence is fatal.
	self := filepath.Join(root, "internal", "statelog", "metrics")
	if _, err := os.Stat(filepath.Join(self, "catalogue.go")); err != nil {
		t.Fatalf("this package is not at %s, so the walk cannot exclude it: %v", self, err)
	}
	out := map[string]bool{}
	err := sourcetree.Walk(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case path == self:
				// Excluded: the catalogue names every instrument,
				// so including it would make the test unable to fail.
				return fs.SkipDir
			case d.Name() == "dist" || d.Name() == "static":
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
		if !selectorNeeds.Admits(body) {
			return nil
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
