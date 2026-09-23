package metrics_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// referencePath is the generated page, relative to this package.
const referencePath = "../../../docs/reference/metrics.md"

// regenerate rewrites the page. It is run from the MODULE ROOT, where both of
// its paths resolve — which referencePath, relative to this package, does not.
const regenerate = "go run ./internal/statelog/metrics/gen > docs/reference/metrics.md"

// THE REFERENCE IS GENERATED AND DIFFED, the same idiom schema/ uses.
//
// A reference maintained by hand is one that stops matching the code, and a
// metric whose meaning lives only in the source is one an operator cannot act
// on. Regenerate with [regenerate].
func TestTheMetricsReferenceIsGenerated(t *testing.T) {
	t.Parallel()
	want := metrics.Reference()

	got, err := os.ReadFile(filepath.Clean(referencePath))
	if err != nil {
		t.Fatalf("read %s: %v — the page is generated from metrics.Catalogue() "+
			"and has to exist for the docs site to link it", referencePath, err)
	}
	if string(got) != want {
		t.Errorf("%s is stale: it is generated from metrics.Catalogue(), so an "+
			"instrument added without regenerating it is one an operator "+
			"cannot look up. Run from the module root:\n\n\t%s\n",
			referencePath, regenerate)
	}
}
