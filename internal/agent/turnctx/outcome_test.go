package turnctx_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events/types"
)

// Since is what was committed between two reads of the set: a kind the set
// held before is not the later calls', and a nil set committed nothing.
func TestSinceIsWhatTheCallsBetweenTwoReadsCommitted(t *testing.T) {
	t.Parallel()
	w := &turnctx.Written{}
	w.Commit(types.CommittedComment)
	before := w.Changes()
	w.Commit(types.CommittedComment)
	w.Commit(types.CommittedItem)
	if got := w.Since(before); !slices.Equal(got, []types.CommittedChange{types.CommittedItem}) {
		t.Errorf("since = %v, want only the item committed after the first read", got)
	}
	if got := w.Since(w.Changes()); len(got) != 0 {
		t.Errorf("since now = %v, want nothing", got)
	}
	var none *turnctx.Written
	if got := none.Since(nil); len(got) != 0 {
		t.Errorf("a nil set committed %v", got)
	}
}
