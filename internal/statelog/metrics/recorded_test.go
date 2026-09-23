package metrics_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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
	used := scannedTree(t).referenced

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

// A DIRECT WRITE THE RECORDER WOULD REFUSE IS FOUND HERE, not on a dashboard.
//
// The recorder refuses a write its instrument cannot take — a gauge handed to
// Add, a counter handed to Set, a fraction handed to a counter that is not
// [metrics.Instrument.Fractional] — and a refused write has no symptom: the
// series never appears, and every panel and alarm reading it sees a healthy
// zero. [TestEveryCatalogedInstrumentHasARecordingSite] cannot see it,
// because the name IS referenced.
//
// THE RECORDER IS THE ORACLE. Each call site's own method is applied to its
// own instrument on a fresh recorder, and the write has to leave a series, so
// the rule for which method writes which instrument exists once — in the
// recorder — rather than a second time here, where it could drift.
//
// DIRECT CALLS ONLY: a call that passes the instrument's name through a
// variable, as a package's own counting helper does, is invisible to this walk.
func TestEveryDirectWriteIsOneTheRecorderKeeps(t *testing.T) {
	t.Parallel()

	byValue := instrumentIdentifiers(t)
	byIdent := map[string]metrics.Instrument{}
	for _, instrument := range metrics.Catalogue() {
		if ident, named := byValue[instrument.Name]; named {
			byIdent[ident] = instrument
		}
	}
	writes := scannedTree(t).writes

	// THE WALK, ON INPUT WHOSE VERDICT IS KNOWN: the tracker hands its bulk
	// occupancy a fraction in a direct call, so a walk that did not see it
	// is reading nothing and would pass on any tree.
	sawBulk := false
	for _, w := range writes {
		sawBulk = sawBulk || (w.method == "AddValue" && w.ident == "TrackerBulkApplySeconds")
	}
	if !sawBulk {
		t.Fatal("control: the tracker's AddValue(metrics.TrackerBulkApplySeconds) " +
			"was not found, so this walk is reading nothing")
	}
	// AND THE ORACLE, ON A WRITE IT MUST REFUSE: a fraction handed to a
	// counter exported as an integer. An oracle that kept every write
	// would pass every call site.
	served, catalogued := byIdent["StatelogReadServed"]
	if !catalogued {
		t.Fatal("control: StatelogReadServed is not in the catalogue, so the " +
			"refusal below would be of an unknown name rather than of a fraction")
	}
	if kept(t, "AddValue", served) {
		t.Fatal("control: the recorder kept a fraction handed to an integer " +
			"counter, so this oracle cannot refuse anything")
	}

	for _, w := range writes {
		instrument, isInstrument := byIdent[w.ident]
		if !isInstrument {
			// Not one of the catalogue's names, so not a recorder write:
			// another type's method that happens to share the name.
			continue
		}
		if !kept(t, w.method, instrument) {
			t.Errorf("%s: %s(metrics.%s, …) is a write the recorder refuses — "+
				"%s is a %s (fractional: %v) — so the series never appears "+
				"and everything reading it sees a healthy zero. Write it with "+
				"the method its catalogue entry takes",
				w.path, w.method, w.ident, instrument.Name, instrument.Kind,
				instrument.Fractional)
		}
	}
}

// kept reports whether one write through the named recorder method leaves a
// series for the instrument.
func kept(t *testing.T, method string, instrument metrics.Instrument) bool {
	t.Helper()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	attrs := metrics.Attrs{}
	for _, a := range instrument.Attributes {
		attrs[a] = "probe"
	}
	switch method {
	case "Add":
		rec.Add(instrument.Name, 1, attrs)
	case "AddValue":
		rec.AddValue(instrument.Name, 0.5, attrs)
	case "Set":
		rec.Set(instrument.Name, 1, attrs)
	case "Observe":
		rec.Observe(instrument.Name, time.Millisecond, attrs)
	case "ObserveValue":
		rec.ObserveValue(instrument.Name, 1, attrs)
	default:
		t.Fatalf("%s is matched by the write pattern and not dispatched here", method)
	}
	return len(rec.Read()) > 0
}

// selector matches a reference to one of this package's exported identifiers.
//
// A REGEX RATHER THAN A TYPE-CHECKED WALK. Loading the whole module with
// go/packages to resolve one selector per instrument costs more than the
// question is worth, and the false positives a regex can produce here are all
// harmless: it can only make a name look USED, and a name that is used is
// exactly what this asserts.
var selector = regexp.MustCompile(`\bmetrics\.([A-Z]\w*)\b`)

// writeCall matches a recorder write whose first argument names its instrument
// by constant, across a line break if the call has one.
//
// Its false positives are another type's method of the same name handed one of
// this package's identifiers, and [TestEveryDirectWriteIsOneTheRecorderKeeps]
// skips every identifier that is not an instrument name.
var writeCall = regexp.MustCompile(
	`\.(Add|AddValue|Set|Observe|ObserveValue)\(\s*metrics\.([A-Z]\w*)\b`)

// treeScan is what one walk of the non-test code outside this package found.
type treeScan struct {
	// referenced is every metrics identifier named.
	referenced map[string]bool
	// writes is every direct recorder write that names its instrument by
	// constant.
	writes []write
}

// write is one call site: the recorder method, the identifier it was handed
// and the file it is in.
type write struct {
	method, ident, path string
}

// scanOnce is the one walk every test in this file reads, so parallel tests
// do not each read the whole module.
var scanOnce = sync.OnceValues(scanTheTree)

// scannedTree is the walk's result, or the test's failure when it had none.
func scannedTree(t *testing.T) treeScan {
	t.Helper()
	scan, err := scanOnce()
	if err != nil {
		t.Fatalf("walk the tree: %v", err)
	}
	return scan
}

// scanTheTree reads every non-test Go file outside this package.
func scanTheTree() (treeScan, error) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		return treeScan{}, fmt.Errorf("resolve the module root: %w", err)
	}
	self, err := filepath.Abs(".")
	if err != nil {
		return treeScan{}, fmt.Errorf("resolve this package: %w", err)
	}
	out := treeScan{referenced: map[string]bool{}}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case path == self:
				// THIS PACKAGE IS EXCLUDED, because the question is
				// whether the ENGINE names an instrument, and a mention
				// in the package that declares it is not that.
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
			out.referenced[string(match[1])] = true
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		for _, match := range writeCall.FindAllSubmatch(body, -1) {
			out.writes = append(out.writes, write{
				method: string(match[1]), ident: string(match[2]), path: rel,
			})
		}
		return nil
	})
	if err != nil {
		return treeScan{}, err
	}
	return out, nil
}
