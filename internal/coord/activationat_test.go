package coord_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// AN ACTIVATION IS PUBLISHED LATER THAN THE ONE IT REPLACES, and still later
// once a node has stored its copy at the store's microseconds.
//
// Later by a few microseconds on the pointer would round to the same instant
// wherever a coarser copy is compared, and a node comparing its stored
// `activated_at` against the pointer's would read two activations as one — or
// the replaced one as the newer.
//
// Mutation: compare in nanoseconds, or keep the request, and a case fails.
func TestTheActivationPointersForwardRuleIsLaterWhereverItIsKept(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 3, 14, 15, 9, 26, 500_000, time.UTC)
	for _, requested := range []time.Time{
		base.Add(-time.Hour), base, base.Add(300 * time.Microsecond),
		base.Add(999 * time.Microsecond),
	} {
		got := coord.ActivationAt(requested, base)
		stored := got.Truncate(time.Microsecond)
		if got.UnixMilli() <= base.UnixMilli() || !stored.After(base.Truncate(time.Microsecond)) {
			t.Errorf("an activation asking for %s after one at %s is published at %s "+
				"— no later than the replaced one where it is kept", requested, base, got)
		}
	}
	if later := base.Add(time.Second); !coord.ActivationAt(later, base).Equal(later) {
		t.Error("a genuinely later activation was moved")
	}
	if first := base; !coord.ActivationAt(first, time.Time{}).Equal(first) {
		t.Error("the first activation was moved, with nothing before it")
	}
}
