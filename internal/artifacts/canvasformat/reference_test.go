package canvasformat_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
)

// THE REFERENCE LISTS EVERY KEY THE STRICT DECODE ADMITS, from the same form,
// and rules no key it refuses (Outline panics on one).
func TestTheReferenceListsEveryKey(t *testing.T) {
	t.Parallel()
	ref := canvasformat.Reference()
	for _, p := range canvasformat.Form.Paths() {
		if !strings.Contains(ref, "`"+p.Path+"`") {
			t.Errorf("the reference does not list %s", p.Path)
		}
	}
}
