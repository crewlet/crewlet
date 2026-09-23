package configplane_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
)

// TWO ACTIVATIONS INSIDE ONE SECOND GET TWO STAMPS, ordered as they were made,
// and one instant gets one stamp whichever zone it was read in.
//
// At a second's resolution the two share a stamp, and every guard reading it
// lets an equal stamp through — so the earlier configuration, applied late,
// walks the later one back.
func TestAnActivationStampResolvesMilliseconds(t *testing.T) {
	t.Parallel()
	first := time.Date(2026, 3, 2, 10, 0, 0, 100_000_000, time.UTC)
	second := first.Add(300 * time.Millisecond)
	if a, b := configplane.ActivationStamp(first), configplane.ActivationStamp(second); a >= b {
		t.Fatalf("stamps %d and %d for activations 300ms apart, want the later one higher", a, b)
	}
	elsewhere := first.In(time.FixedZone("UTC+5", 5*60*60))
	if a, b := configplane.ActivationStamp(first), configplane.ActivationStamp(elsewhere); a != b {
		t.Fatalf("one instant stamps %d in UTC and %d in another zone", a, b)
	}
	// AND A STORE'S MICROSECONDS AGREE WITH THE POINTER'S NANOSECONDS, which
	// is what makes a boot's reading the same as the reconcile's.
	pointer := first.Add(123_456_789 * time.Nanosecond)
	stored := pointer.Truncate(time.Microsecond)
	if a, b := configplane.ActivationStamp(pointer), configplane.ActivationStamp(stored); a != b {
		t.Fatalf("the pointer's instant stamps %d and the store's copy %d", a, b)
	}
}

// THE POINTER'S FORWARD RULE AND THE STAMP AGREE ON A RESOLUTION.
//
// The activation pointer publishes an instant later than the one it replaces
// (coord.ActivationAt), and "later" has to mean a later STAMP: an instant a
// few microseconds on is the same stamp here, and every guard reading it lets
// an equal stamp through. Two definitions of one resolution, held together by
// this.
func TestTheActivationPointersForwardRuleIsLaterInStamps(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 3, 14, 15, 9, 26, 500_000, time.UTC)
	for _, requested := range []time.Time{
		base.Add(-time.Hour), base, base.Add(300 * time.Microsecond),
		base.Add(999 * time.Microsecond),
	} {
		got := coord.ActivationAt(requested, base)
		if configplane.ActivationStamp(got) <= configplane.ActivationStamp(base) {
			t.Errorf("an activation asking for %s after one at %s is published at "+
				"%s, stamp %d — no later than the replaced one's %d", requested,
				base, got, configplane.ActivationStamp(got), configplane.ActivationStamp(base))
		}
	}
	if later := base.Add(time.Second); !coord.ActivationAt(later, base).Equal(later) {
		t.Error("a genuinely later activation was moved")
	}
	if first := base; !coord.ActivationAt(first, time.Time{}).Equal(first) {
		t.Error("the first activation was moved, with nothing before it")
	}
}
