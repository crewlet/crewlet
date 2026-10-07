package livestate

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

// A RECORD ARRIVING AT THE CAP COSTS WHAT ONE RECORD COSTS.
//
// Once a company's day passes [SpendRecordLimit], every spend record that
// arrives drops the oldest — under the projection's lock, which every /agents
// request and every socket snapshot waits on. That trim built a fresh slice of
// the whole window per arrival, a copy of 24 000 entries for each record the
// company published. It is a reslice now, with the backing array replaced only
// by append's own growth, so the bytes allocated per arrival stay a small
// multiple of one entry's rather than the window's.
//
// Mutation: rebuild the window per trim and the arrivals allocate megabytes
// each.
func TestARecordArrivingAtTheCapCostsOneRecord(t *testing.T) {
	t.Parallel()
	s := New()
	fresh := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range SpendRecordLimit {
		s.foldSpend(Envelope{ID: fmt.Sprintf("w%d", i), Timestamp: fresh},
			map[string]any{"total_tokens": 1})
	}
	const arrivals = 200
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for i := range arrivals {
		s.foldSpend(Envelope{ID: fmt.Sprintf("a%d", i), Timestamp: fresh},
			map[string]any{"total_tokens": 1})
	}
	runtime.ReadMemStats(&after)
	if len(s.spend) != SpendRecordLimit || len(s.spendIDs) != SpendRecordLimit {
		t.Fatalf("holding %d records and %d ids, want the cap's %d of each",
			len(s.spend), len(s.spendIDs), SpendRecordLimit)
	}
	if s.spend[len(s.spend)-1].EventID != fmt.Sprintf("a%d", arrivals-1) ||
		s.spend[0].EventID != fmt.Sprintf("w%d", arrivals) {
		t.Fatalf("the window runs %s … %s, want the oldest %d dropped and the newest kept",
			s.spend[0].EventID, s.spend[len(s.spend)-1].EventID, arrivals)
	}
	// One entry is a few hundred bytes; a growth step of the backing array
	// is a quarter of the window, spread over a quarter of the window's
	// arrivals. A rebuild per arrival is the whole window each time.
	window := uint64(SpendRecordLimit) * 400
	if per := (after.TotalAlloc - before.TotalAlloc) / arrivals; per > window/16 {
		t.Fatalf("each arrival at the cap allocated %d bytes, against a window of about "+
			"%d — the trim is copying the window", per, window)
	}
}
