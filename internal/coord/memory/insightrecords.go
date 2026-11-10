package memory

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- insight records ----------------------------------------------------- //

// The model retries' class machinery (modelrecords.go), keyed by class and week.

func newInsightRecords() entryRecords[coord.InsightRecord, coord.InsightRecord] {
	return entryRecords[coord.InsightRecord, coord.InsightRecord]{
		stamp: func(v coord.InsightRecord, version uint64) coord.InsightRecord {
			v.SendAt, v.ClaimedAt, v.Version = v.SendAt.UTC(), v.ClaimedAt.UTC(), version
			return v
		},
		update: func(v *coord.InsightRecord, _ string, _ bool) coord.InsightRecord {
			if v == nil {
				return coord.InsightRecord{}
			}
			return *v
		},
	}
}

// InsightRecord reads one class's record for one week.
func (f *Fleet) InsightRecord(_ context.Context, class, week string) (coord.InsightRecord, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.insightRecords.records[coord.InsightEntry(class, week)]
	return r, ok, nil
}

// ListInsightRecords returns every record, ordered by entry.
func (f *Fleet) ListInsightRecords(context.Context) ([]coord.InsightRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.insightRecords.sortedLocked(), nil
}

// ClaimInsightRecord writes want where its entry holds no record or open
// admits the one it holds.
func (f *Fleet) ClaimInsightRecord(_ context.Context, want coord.InsightRecord,
	open func(held coord.InsightRecord) bool,
) (coord.InsightRecord, bool, error) {
	if err := want.Validate(); err != nil {
		return coord.InsightRecord{}, false, err
	}
	entry := coord.InsightEntry(want.Class, want.Week)
	f.mu.Lock()
	defer f.mu.Unlock()
	if held, ok := f.insightRecords.records[entry]; ok && !open(held) {
		return held, false, nil
	}
	return f.insightRecords.storeLocked(f, entry, want), true, nil
}

// DeleteInsightRecord removes r where it still holds at r.Version.
func (f *Fleet) DeleteInsightRecord(_ context.Context, r coord.InsightRecord) (bool, error) {
	entry := coord.InsightEntry(r.Class, r.Week)
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.insightRecords.records[entry]
	if !ok || r.Version == 0 || held.Version != r.Version {
		return false, nil
	}
	f.insightRecords.removeLocked(entry)
	return true, nil
}
