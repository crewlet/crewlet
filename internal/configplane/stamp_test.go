package configplane_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/configplane"
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
