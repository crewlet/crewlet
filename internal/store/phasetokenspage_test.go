package store_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// A PAGED SPEND READ COVERS EVERY RECORD ONCE, ties on one instant included.
//
// The live spend window's seed reads a busy day in pages, each resuming below
// the last record the one before kept. Records share a microsecond in a burst,
// so a cursor on the instant alone would skip the ones that collided with it or
// read them twice — and a spend record read twice is a double count.
func TestAPagedSpendReadCoversEveryRecordOnce(t *testing.T) {
	t.Parallel()
	log := open(t).Events()
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	var want []string
	for i := range 7 {
		// Three records on each instant but the last.
		id := fmt.Sprintf("p-%d", i)
		seedPhase(t, log, id, at.Add(time.Duration(i/3)*time.Second), "PM", 1, 0)
		want = append(want, id)
	}
	var got []string
	var before *store.Cursor
	for range len(want) + 1 {
		page, err := log.PhaseTokens(t.Context(), store.PhaseTokenQuery{
			Since: at.Add(-time.Minute), Limit: 2, Before: before,
		})
		if err != nil {
			t.Fatalf("phase tokens: %v", err)
		}
		if len(page) == 0 {
			break
		}
		for _, r := range page {
			got = append(got, r.EventID)
		}
		last := page[len(page)-1]
		stamp, err := time.Parse(time.RFC3339Nano, last.Timestamp)
		if err != nil {
			t.Fatalf("stamp %q: %v", last.Timestamp, err)
		}
		before = &store.Cursor{Time: stamp, ID: last.EventID}
	}
	if len(got) != len(want) {
		t.Fatalf("pages read %v, want each of the %d records once", got, len(want))
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("pages read %v, want %v", got, want)
	}
}
