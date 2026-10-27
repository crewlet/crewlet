package dashspec_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
)

// THE REFERENCE LISTS EVERY KEY THE STRICT DECODE ADMITS, from the same form,
// and rules no key it refuses (Outline panics on one).
func TestTheReferenceListsEveryKey(t *testing.T) {
	t.Parallel()
	ref := dashspec.Reference()
	for _, p := range dashspec.Form.Paths() {
		if !strings.Contains(ref, "`"+p.Path+"`") {
			t.Errorf("the reference does not list %s", p.Path)
		}
	}
}
