package kv

import (
	"context"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- insight records ----------------------------------------------------- //

// One record per catalogue class and ISO week, filed in the positions register
// under a class of its own: see internal/coord's insightrecord.go.

var insightRecordClass = entryClass[coord.InsightRecord]{
	name:   "the insight records",
	filter: coord.DocumentFilter(coord.InsightRecordClass),
	key:    coord.InsightRecordKey,
	entry:  coord.InsightEntryOf,
	stored: func(r coord.InsightRecord, _ string, version uint64) coord.InsightRecord {
		r.Version, r.SendAt, r.ClaimedAt = version, r.SendAt.UTC(), r.ClaimedAt.UTC()
		return r
	},
}

// InsightRecord reads one class's record for one week.
func (f *FleetStore) InsightRecord(ctx context.Context, class, week string) (coord.InsightRecord, bool, error) {
	return insightRecordClass.read(ctx, f, coord.InsightEntry(class, week))
}

// ListInsightRecords returns every record, ordered by entry.
func (f *FleetStore) ListInsightRecords(ctx context.Context) ([]coord.InsightRecord, error) {
	return insightRecordClass.list(ctx, f)
}

// ClaimInsightRecord writes want where its entry holds no record or open
// admits the one it holds, retried on a lost race as ClaimIssueReport is.
func (f *FleetStore) ClaimInsightRecord(ctx context.Context, want coord.InsightRecord,
	open func(held coord.InsightRecord) bool,
) (coord.InsightRecord, bool, error) {
	if err := want.Validate(); err != nil {
		return coord.InsightRecord{}, false, err
	}
	entry := coord.InsightEntry(want.Class, want.Week)
	var stored coord.InsightRecord
	claimed := false
	settled, err := casRetry(ctx, &f.positionTrips, "claim the insight record "+entry, func() (bool, error) {
		held, found, err := insightRecordClass.read(ctx, f, entry)
		if err != nil {
			return false, err
		}
		if found && !open(held) {
			stored, claimed = held, false
			return true, nil
		}
		var wrote bool
		if found {
			stored, wrote, err = insightRecordClass.update(ctx, f, entry, want, held.Version)
		} else {
			stored, wrote, err = insightRecordClass.create(ctx, f, entry, want)
		}
		claimed = wrote
		return wrote, err
	})
	switch {
	case err != nil:
		return coord.InsightRecord{}, false, err
	case !settled:
		return coord.InsightRecord{}, false, contended("ClaimInsightRecord", entry)
	}
	return stored, claimed, nil
}

// DeleteInsightRecord removes r where it still holds at r.Version.
func (f *FleetStore) DeleteInsightRecord(ctx context.Context, r coord.InsightRecord) (bool, error) {
	return insightRecordClass.remove(ctx, f, coord.InsightEntry(r.Class, r.Week), r.Version)
}
