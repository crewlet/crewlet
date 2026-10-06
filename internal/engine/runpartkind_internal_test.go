package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY PART OF A RUN'S ACCOUNT IS CONDENSED AS WHAT IT IS. A question
// rewritten as though it were a report keeps findings and loses the options a
// person has to choose between, and an unknown kind is refused outright — so
// each part names a kind of its own, and one the compactor has instructions
// for.
func TestEveryRunPartIsCondensedAsWhatItIs(t *testing.T) {
	t.Parallel()
	want := map[sandbox.RunPart]compact.Kind{
		sandbox.PartReport:   compact.KindReport,
		sandbox.PartFailure:  compact.KindToolError,
		sandbox.PartQuestion: compact.KindQuestion,
	}
	for _, part := range sandbox.RunParts {
		kind := runPartKind(part)
		if !kind.Valid() {
			t.Errorf("%s is condensed as %q, which the compactor has no instructions for", part, kind)
		}
		if w, ok := want[part]; !ok || kind != w {
			t.Errorf("%s is condensed as %q; want %q", part, kind, w)
		}
	}
}
