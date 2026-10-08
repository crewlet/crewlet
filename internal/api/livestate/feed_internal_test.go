package livestate

import (
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/tokens"
)

// THE ID INDEX HOLDS EXACTLY THE IDS THE RING HOLDS.
//
// It exists so an event is listed once however it arrived, off the stream, out
// of the store at startup, or both. It has to shrink as the ring does: an index
// that kept every id it had ever seen would be the slow leak the ring's own
// bound exists to prevent, in a process that stays up for weeks. Both ways a
// row leaves are exercised: a live append pushing the oldest out, and a seed
// whose history sorts behind a full ring and is trimmed on arrival.
func TestTheFeedIndexForgetsWhatTheRingDrops(t *testing.T) {
	t.Parallel()
	s := New(WithFeedLimit(3))
	base := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	for i := range 10 {
		s.Apply(&Envelope{
			ID: fmt.Sprint("e", i), Type: "agent_turn_completed", Category: "agent",
			Timestamp: base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano),
			Payload:   map[string]any{"role": "Lead"},
		})
	}
	s.Seed(History{Events: []FeedRow{{
		ID: "older", Type: "agent_turn_completed", Category: "agent",
		Timestamp: base.Add(-time.Hour).Format(time.RFC3339Nano),
	}}})

	held := map[string]struct{}{}
	for _, row := range s.feed {
		held[row.ID] = struct{}{}
	}
	if len(held) != len(s.feed) {
		t.Fatalf("ring = %v, which lists an id twice", s.feed)
	}
	if !maps.Equal(held, s.feedIDs) {
		t.Errorf("index = %v, want exactly the ring's ids %v", s.feedIDs, held)
	}
}

// THE SPEND INDEX HOLDS EXACTLY THE IDS THE WINDOW HOLDS.
//
// It is exact rather than capped, which is what makes it correct where a
// bounded set was not — so what bounds it is that an id leaves with its record
// and never enters without one. An id left behind by a dropped record, or taken
// for a record the window refused, would make this map the one structure in the
// projection that grows for the life of the process: nothing that forgets an id
// (the expiry, the cap) ever reaches one no held record carries. Every way an
// id comes or goes is exercised: an expiry, an arrival already aged and its
// redelivery, a seed of aged history, and the count cap.
func TestTheSpendIndexHoldsExactlyTheWindowsIDs(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	now := start
	s := New(WithClock(func() time.Time { return now }))
	fresh := start.Add(2 * time.Hour).Format(time.RFC3339Nano)
	indexIsWindow := func(step string, outside ...string) {
		t.Helper()
		for _, id := range outside {
			if _, indexed := s.spendIDs[id]; indexed {
				t.Errorf("after %s the window still holds %q, which is older "+
					"than it", step, id)
			}
		}
		held := map[string]struct{}{}
		for _, list := range [][]spendEntry{s.undatedSpend, s.spend} {
			for i := range list {
				held[list[i].EventID] = struct{}{}
			}
		}
		if !maps.Equal(held, s.spendIDs) {
			t.Errorf("after %s the index holds %d ids for the window's %d: "+
				"want exactly the window's", step, len(s.spendIDs), len(held))
		}
	}

	s.foldSpend(Envelope{ID: "early", Timestamp: start.Format(time.RFC3339Nano)},
		map[string]any{"total_tokens": 5})
	s.foldSpend(Envelope{ID: "fresh", Timestamp: fresh}, map[string]any{"total_tokens": 5})
	now = start.Add(LiveSpendWindow + time.Hour) // early has aged, fresh has not
	if !s.ExpireSpend() || len(s.spend) != 1 {
		t.Fatalf("holding %d records, want the aged one expired", len(s.spend))
	}
	indexIsWindow("an expiry", "early")

	// AN ARRIVAL ALREADY AGED is refused without an id, and so is its
	// redelivery: indexed, its id would be carried by no held record, so
	// neither the expiry nor the cap would ever forget it.
	for range 2 {
		s.foldSpend(Envelope{ID: "late", Timestamp: start.Format(time.RFC3339Nano)},
			map[string]any{"total_tokens": 5})
	}
	indexIsWindow("an aged arrival and its redelivery", "late")

	// AND A SEED OF AGED HISTORY admits none of it, and says it moved nothing.
	if s.Seed(History{Spend: []tokens.Record{{
		EventID: "stored", Timestamp: start.Format(time.RFC3339Nano), TotalTokens: 5,
	}}}).Tokens {
		t.Error("a seed of history older than the window reported the rollup moved")
	}
	indexIsWindow("a seed of aged history", "stored")

	// And the count cap prunes the index too, not just the slice.
	for i := range SpendRecordLimit + 10 {
		s.foldSpend(Envelope{ID: fmt.Sprintf("n%d", i), Timestamp: fresh},
			map[string]any{"total_tokens": 1})
	}
	if len(s.spend) != SpendRecordLimit {
		t.Fatalf("holding %d records, want the cap's %d", len(s.spend), SpendRecordLimit)
	}
	indexIsWindow("the count cap")
}
