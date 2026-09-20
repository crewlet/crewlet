package livestate

import (
	"fmt"
	"maps"
	"testing"
	"time"
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

// THE SPEND INDEX SHRINKS WITH THE WINDOW IT TRACKS.
//
// It is exact rather than capped, which is what makes it correct where a
// bounded set was not — so the thing that bounds it is the prune. An id left
// behind by a dropped record would make this map the one structure in the
// projection that grows for the life of the process.
func TestTheSpendIndexDropsWhatTheWindowDrops(t *testing.T) {
	t.Parallel()
	s := New()
	aged := time.Now().UTC().Add(-LiveSpendWindow - time.Hour).Format(time.RFC3339Nano)
	fresh := time.Now().UTC().Format(time.RFC3339Nano)

	s.foldSpend(Envelope{ID: "aged", Timestamp: aged}, map[string]any{"total_tokens": 5})
	s.foldSpend(Envelope{ID: "fresh", Timestamp: fresh}, map[string]any{"total_tokens": 5})
	if got := len(s.spend); got != 1 {
		t.Fatalf("holding %d records, want the aged one pruned", got)
	}
	if got := len(s.spendIDs); got != 1 {
		t.Errorf("the index holds %d ids for 1 record: it does not shrink with "+
			"the window, so it grows for the life of the process", got)
	}

	// And the count cap prunes the index too, not just the slice.
	for i := range SpendRecordLimit + 10 {
		s.foldSpend(Envelope{ID: fmt.Sprintf("n%d", i), Timestamp: fresh},
			map[string]any{"total_tokens": 1})
	}
	if len(s.spend) != SpendRecordLimit {
		t.Fatalf("holding %d records, want the cap's %d", len(s.spend), SpendRecordLimit)
	}
	if got := len(s.spendIDs); got != SpendRecordLimit {
		t.Errorf("the index holds %d ids for %d records: the count cap's "+
			"truncation left ids behind", got, SpendRecordLimit)
	}
}
