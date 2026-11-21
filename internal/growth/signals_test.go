package growth_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/growth"
)

// A CLOCK SKEW STANDS FROM A REFUSAL UNTIL A LATER SIGNED REQUEST IS ACCEPTED:
// a refusal with the vendor's clock within the tolerance is not the clock's
// doing and is not kept; one past it is, keeping when its run began across
// repeats; and an accepted request closes it.
//
// Mutation: keep every refusal and a token refused for its scope reads as the
// node's clock; drop ClockAgreed's delete and the skew never closes.
func TestAClockSkewStandsUntilASignedRequestIsAccepted(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	s := growth.NewSignals(func() time.Time { return at })
	s.ClockRefused("appstoreconnect", "Apple", growth.ClockSkewTolerance)
	s.ClockRefused("appstoreconnect", "Apple", -growth.ClockSkewTolerance)
	if skews, _ := s.Snapshot(); len(skews) != 0 {
		t.Fatalf("refusals within the tolerance stand as %+v", skews)
	}
	s.ClockRefused("appstoreconnect", "Apple", 95*time.Second)
	first := at
	at = at.Add(time.Minute)
	s.ClockRefused("appstoreconnect", "Apple", 97*time.Second)
	skews, _ := s.Snapshot()
	if len(skews) != 1 || skews[0].Offset != 97*time.Second || !skews[0].Since.Equal(first) ||
		skews[0].Vendor != "Apple" {
		t.Fatalf("the skew stands as %+v", skews)
	}
	s.ClockAgreed("appstoreconnect")
	if skews, _ := s.Snapshot(); len(skews) != 0 {
		t.Errorf("an accepted request left %+v", skews)
	}
}

// A DEPRECATION STANDS FOR A DAY AFTER THE VENDOR LAST SAID IT, keyed by its
// product and version, and a node keeps at most MaxSignals of each signal,
// its names bounded, so its status record stays within its bound.
//
// Mutation: never age a deprecation out and a vendor that withdrew it is
// announced for ever; drop the trim and a vendor naming a version per call
// grows the record without bound.
func TestADeprecationStandsADayAfterItWasLastHeard(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	s := growth.NewSignals(func() time.Time { return at })
	retires := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)
	s.Deprecated("metaads", "v23.0", retires)
	at = at.Add(growth.DeprecationLife)
	if _, d := s.Snapshot(); len(d) != 1 || !d[0].Retires.Equal(retires) || d[0].Version != "v23.0" {
		t.Fatalf("a deprecation heard a day ago stands as %+v", d)
	}
	at = at.Add(time.Second)
	if _, d := s.Snapshot(); len(d) != 0 {
		t.Errorf("a deprecation not heard for over a day stands as %+v", d)
	}
	for i := range growth.MaxSignals + 5 {
		at = at.Add(time.Second)
		s.Deprecated("metaads", fmt.Sprintf("v%d.0", i), time.Time{})
		s.ClockRefused(fmt.Sprintf("product%02d", i)+strings.Repeat("x", 2*growth.MaxSignalTextBytes), "V",
			2*time.Minute)
	}
	skews, d := s.Snapshot()
	if len(skews) != growth.MaxSignals || len(d) != growth.MaxSignals {
		t.Errorf("a node keeps %d skews and %d deprecations, want %d each", len(skews), len(d), growth.MaxSignals)
	}
	for _, c := range skews {
		if len(c.Product) > growth.MaxSignalTextBytes {
			t.Fatalf("a product of %d bytes is kept", len(c.Product))
		}
	}
}
