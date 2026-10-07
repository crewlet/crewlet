package store_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/store"
)

// THE FEED'S SEED READS WHAT THE FEED CARRIES.
//
// The live projection rebuilds its activity feed from the store at boot with
// one page of the newest rows. A page read whole and filtered afterwards would
// come back short by every accounting row among the newest — a compaction's
// burst could leave it nearly empty — so the read itself leaves out the types
// [events.Unfed] keeps out of the feed, and reads every other row as it was.
func TestAFeedOnlyListingLeavesOutWhatTheFeedDoesNotCarry(t *testing.T) {
	t.Parallel()
	unfed := events.Unfed()
	if len(unfed) == 0 {
		t.Fatal("no type is kept out of the feed — this case is asserting nothing")
	}
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	for i, rec := range []store.EventRecord{
		{ID: "turn", Type: "agent_turn_completed", Category: "system"},
		{ID: "spend", Type: unfed[0], Category: "system"},
		{ID: "phase", Type: "agent_phase_completed", Category: "system"},
	} {
		rec.Time = base.Add(time.Duration(i) * time.Minute)
		rec.Payload = []byte(`{}`)
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	ids := func(q store.ListQuery) []string {
		t.Helper()
		rows, err := log.List(t.Context(), q)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(store.ListQuery{Limit: 10, FeedOnly: true}); !slices.Equal(got, []string{"phase", "turn"}) {
		t.Errorf("feed-only listing = %v, want every row but the accounting one", got)
	}
	if got := ids(store.ListQuery{Limit: 10}); !slices.Equal(got, []string{"phase", "spend", "turn"}) {
		t.Errorf("listing = %v, want every row: the accounting row is stored and listed", got)
	}
}

// A RELATED AGENT'S FEED-ONLY PAGE KEEPS ITS TRACE'S ACCOUNTING OUT TOO.
//
// The related-agent read pulls in every row sharing a trace with the agent's
// own — the cause beside the effect — and carries none of the page's filters
// onto them. A turn's trace holds every auxiliary_spend record of the turn, so
// a feed-only page of an agent's activity filled with them through the back
// door.
func TestARelatedAgentsFeedOnlyPageLeavesOutItsTracesAccounting(t *testing.T) {
	t.Parallel()
	unfed := events.Unfed()
	log := open(t).Events()
	base := time.Now().UTC().Add(-time.Hour)
	for i, rec := range []store.EventRecord{
		{ID: "work", Type: "agent_phase_completed", Category: "task",
			Tags: map[string]string{"agent_role": "Lead"}},
		{ID: "cause", Type: "webhook_received", Category: "webhook"},
		{ID: "spend", Type: unfed[0], Category: "system"},
	} {
		rec.Time = base.Add(time.Duration(i) * time.Minute)
		rec.TraceID = "trace-1"
		rec.Payload = []byte(`{}`)
		if err := log.Append(t.Context(), rec); err != nil {
			t.Fatalf("append %s: %v", rec.ID, err)
		}
	}
	ids := func(q store.ListQuery) []string {
		t.Helper()
		rows, err := log.List(t.Context(), q)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		var out []string
		for _, r := range rows {
			out = append(out, r.ID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(store.ListQuery{Limit: 10, RelatedAgent: "Lead"}); !slices.Equal(got, []string{"cause", "spend", "work"}) {
		t.Fatalf("related listing = %v, want the agent's row and its trace's — the case "+
			"exercises nothing otherwise", got)
	}
	if got := ids(store.ListQuery{Limit: 10, RelatedAgent: "Lead", FeedOnly: true}); !slices.Equal(got, []string{"cause", "work"}) {
		t.Errorf("feed-only related listing = %v, want the agent's row and its cause, "+
			"without the trace's accounting", got)
	}
}
