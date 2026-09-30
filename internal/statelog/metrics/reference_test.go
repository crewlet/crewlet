package metrics_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// referencePath is the generated page, from the module root — where
// `make metrics-doc` writes it.
const referencePath = "docs/reference/metrics.md"

// THE REFERENCE IS GENERATED AND DIFFED, the same idiom schema/ uses.
//
// A reference maintained by hand is one that stops matching the code, and a
// metric whose meaning lives only in the source is one an operator cannot act
// on. Regenerate with `make metrics-doc`.
func TestTheMetricsReferenceIsGenerated(t *testing.T) {
	t.Parallel()
	want := metrics.Reference()

	got, err := os.ReadFile(filepath.Join(sourcetree.Root(t), filepath.FromSlash(referencePath)))
	if err != nil {
		t.Fatalf("read %s: %v — the page is generated from metrics.Catalogue() "+
			"and has to exist for the docs site to link it", referencePath, err)
	}
	if string(got) != want {
		t.Errorf("%s is stale: it is generated from metrics.Catalogue(), so an "+
			"instrument added without regenerating it is one an operator "+
			"cannot look up. Run:\n\n\tmake metrics-doc\n", referencePath)
	}
}
