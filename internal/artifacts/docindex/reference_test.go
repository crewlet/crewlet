package docindex_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/docindex"
)

// THE REFERENCE LISTS EVERY KEY THE STRICT DECODE ADMITS, from the same form,
// and rules no key it refuses (Outline panics on one).
func TestTheReferenceListsEveryKey(t *testing.T) {
	t.Parallel()
	ref := docindex.Reference()
	for _, p := range docindex.IndexForm.Paths() {
		if !strings.Contains(ref, "`"+p.Path+"`") {
			t.Errorf("the reference does not list %s", p.Path)
		}
	}
}
