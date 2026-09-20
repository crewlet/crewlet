package livestate

import (
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/tokens"
)

// History is what a projection is seeded with when its process starts.
//
// Without it every surface built from the projection started EMPTY in every
// process: the activity feed, the spend rollup and the per-agent rows folded
// from it all described only what had happened since this process booted, so
// a restart, a deploy or a node joining a fleet showed an operator a company
// that had apparently done nothing, beside a store that said otherwise.
//
// Both halves come from the node's own event store, so on a fleet they are the
// history THIS node published; everything live after the boot is the whole
// company's, as it always was.
type History struct {
	// Events are persisted feed rows, in any order.
	Events []FeedRow

	// Spend is the per-phase spend records inside [LiveSpendWindow], in any
	// order. Past [SpendRecordLimit] only the newest are kept.
	Spend []tokens.Record
}

// Seed folds stored history into the projection and reports what moved.
//
// SAFE AGAINST THE LIVE STREAM IN EITHER ORDER, which is what lets the caller
// subscribe FIRST and read the store second, so no event published between
// the two is lost. An event that arrives both ways is recognised by its id and
// listed and counted once, whichever way reached the projection first, and
// history lands behind the live rows it predates rather than after them.
func (s *LiveState) Seed(h History) Change {
	s.mu.Lock()
	defer s.mu.Unlock()
	var change Change
	change.Events = s.seedFeed(h.Events)
	change.Tokens = s.seedSpend(h.Spend)
	return change
}

// seedFeed merges stored rows into the feed ring, reporting whether any landed.
//
// Through the same id index the live path lists by, which is what makes the
// overlap safe in BOTH orders: a row the stream delivered first is skipped
// here, and an envelope that arrives after its row was seeded is skipped by
// [LiveState.recordEvent].
func (s *LiveState) seedFeed(rows []FeedRow) bool {
	added := false
	for _, row := range rows {
		if !s.admitFeedID(row.ID) {
			continue
		}
		s.feed = append(s.feed, row)
		added = true
	}
	if !added {
		return false
	}
	// The ring is chronological and a snapshot reads it newest-first, so
	// history that arrived after the live rows is put back behind them.
	// Stable, so rows sharing an instant keep the order they arrived in.
	slices.SortStableFunc(s.feed, func(a, b FeedRow) int {
		return newStamp(a.Timestamp).chronological(newStamp(b.Timestamp))
	})
	s.trimFeed()
	return true
}

// seedSpend adds stored spend records to the live window, reporting whether
// any counted.
func (s *LiveState) seedSpend(records []tokens.Record) bool {
	if len(records) == 0 {
		return false
	}
	entries := make([]spendEntry, 0, len(records))
	for _, r := range records {
		entries = append(entries, spendEntry{at: newStamp(r.Timestamp), Record: r})
	}
	// OLDEST FIRST, before a single id is recorded. The dedupe set evicts
	// its oldest insertions past its cap, and the store answers newest
	// first: inserted in that order, a day busier than the cap would evict
	// the ids of the NEWEST records, which are exactly the ones the live
	// stream can still redeliver.
	slices.SortStableFunc(entries, func(a, b spendEntry) int { return a.at.chronological(b.at) })
	// Only the newest records the window can hold are worth an id: the
	// count cap below would drop the rest from the front anyway. The seed's
	// own read already stops at the cap; this holds for any other caller.
	if len(entries) > SpendRecordLimit {
		entries = entries[len(entries)-SpendRecordLimit:]
	}

	counted := false
	for _, entry := range entries {
		// A RECORD WITH NO ID IS DROPPED rather than counted. Unlike a feed
		// row, a spend record is SUMMED, and the dedupe has nothing to hold
		// this one by — so a second seed would add it again and the rollup
		// would grow on every one. A rollup slightly short is better than a
		// rollup that climbs on its own.
		if entry.EventID == "" {
			continue
		}
		if _, counted := s.spendIDs[entry.EventID]; counted {
			continue
		}
		s.spendIDs[entry.EventID] = struct{}{}
		s.spend = append(s.spend, entry)
		counted = true
	}
	if !counted {
		return false
	}
	// The same order the count cap assumes, restored across the live
	// records already held and the history appended behind them.
	slices.SortStableFunc(s.spend, func(a, b spendEntry) int { return a.at.chronological(b.at) })
	// Pruned against the CLOCK rather than an event's own stamp: nothing is
	// arriving here, and the window a seed has to respect is the one ending
	// now.
	s.pruneSpend(s.clock().Format(time.RFC3339Nano))
	return true
}
