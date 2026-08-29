package livestate

import (
	"fmt"
	"testing"
)

// Internal because the eviction contract is the point and the type is not
// exported: what a caller sees of it is only that a dedupe stops holding very
// old ids, which is true of any cap.

func TestABoundedSetEvictsOldestFirst(t *testing.T) {
	t.Parallel()
	b := newBoundedSet[int](3)
	for i := range 5 {
		b.put(fmt.Sprint(i), i)
	}
	if b.len() != 3 {
		t.Fatalf("len = %d, want the cap of 3", b.len())
	}
	for _, gone := range []string{"0", "1"} {
		if b.has(gone) {
			t.Errorf("%q survived past the cap", gone)
		}
	}
	for _, kept := range []string{"2", "3", "4"} {
		if !b.has(kept) {
			t.Errorf("%q was evicted before an older key", kept)
		}
	}
}

func TestRePuttingAKeyDoesNotPromoteIt(t *testing.T) {
	t.Parallel()
	// The order is an EVICTION order, not a recency ranking. Promoting on
	// every write would let a hot key hold the map open while the cap
	// silently stopped bounding anything.
	b := newBoundedSet[int](3)
	b.put("a", 1)
	b.put("b", 2)
	b.put("a", 9) // re-put, and still the oldest
	b.put("c", 3)
	b.put("d", 4)

	if b.has("a") {
		t.Error("a re-put key was promoted past its eviction slot")
	}
	if b.len() != 3 {
		t.Errorf("len = %d, want 3", b.len())
	}
}

func TestARePutReplacesTheValue(t *testing.T) {
	t.Parallel()
	b := newBoundedSet[int](3)
	b.put("a", 1)
	b.put("a", 2)
	if got, _ := b.get("a"); got != 2 {
		t.Errorf("value = %d, want the replacement", got)
	}
	if b.len() != 1 {
		t.Errorf("len = %d: a re-put added a second entry", b.len())
	}
}

// TestTheBackingArrayStaysBoundedAcrossManyEvictions is the measurement that
// retired a compaction step.
//
// The concern was real — a forward re-slice keeps pointing into the same array
// — but the conclusion was not: the re-slice also REDUCES the remaining
// capacity, so the next append past it reallocates and drops the old array.
// This pins that, so a change to the eviction that broke it would show up as a
// growing array rather than as memory nobody measures.
func TestTheBackingArrayStaysBoundedAcrossManyEvictions(t *testing.T) {
	t.Parallel()
	b := newBoundedSet[struct{}](4)
	for i := range 10_000 {
		b.put(fmt.Sprint(i), struct{}{})
	}
	if got := cap(b.order); got > 8*b.limit {
		t.Errorf("eviction order capacity = %d after 10k puts, want it bounded "+
			"near the limit of %d", got, b.limit)
	}
	if b.len() != 4 {
		t.Errorf("len = %d, want the cap", b.len())
	}
}
