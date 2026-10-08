package eventfan_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// A FEED-ONLY PAGE AND ITS AXIS ARE THE FEED'S ROWS ON EVERY NODE.
//
// A restarted node seeds its activity feed with one feed-only page from every
// node, and the event log's axis, the Live strip and a seat's latest events
// ask the same filter. Each node leaves the types the feed keeps out of its
// own page and its own bars, so a peer's accounting rows reach neither — and
// a related agent's trace siblings, which a second scatter reads by trace id
// with none of the page's filters, are held to it by the asker as the store's
// own related-agent read holds them.
//
// Mutations: drop `feed_only` from the wire, and the peer's accounting row is
// on the page and in a bar; drop the asker's narrowing of the siblings, and
// the related page lists the peer's trace accounting.
func TestAFeedOnlyPageAndItsAxisAreTheFeedsRowsOnEveryNode(t *testing.T) {
	t.Parallel()
	unfed := events.Unfed()
	if len(unfed) == 0 {
		t.Fatal("no type is kept out of the feed — the case asserts nothing")
	}
	b := memory.NewBroker()
	mine, peer := newNode(t, b, "node-a"), newNode(t, b, "node-b")
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Hour)
	appendTo(t, mine, store.EventRecord{ID: "mine-work", Type: "agent_phase_completed",
		Category: "task", Time: base.Add(time.Minute), TraceID: "trace-1",
		Tags: map[string]string{"agent_role": "Lead"}})
	appendTo(t, peer, store.EventRecord{ID: "peer-cause", Type: "webhook_received",
		Category: "webhook", Time: base.Add(2 * time.Minute), TraceID: "trace-1"})
	appendTo(t, peer, store.EventRecord{ID: "peer-spend", Type: unfed[0],
		Category: "system", Time: base.Add(3 * time.Minute), TraceID: "trace-1"})
	fan := fanFrom(mine, "node-a", "node-b")

	page := func(q store.ListQuery) []string {
		t.Helper()
		got, coverage, err := fan.List(t.Context(), q)
		if err != nil || !coverage.Complete {
			t.Fatalf("List(%+v) = coverage %+v, %v", q, coverage, err)
		}
		ids := idsOf(got.Rows)
		slices.Sort(ids)
		return ids
	}
	if got := page(store.ListQuery{Limit: 10}); !slices.Equal(got, []string{"mine-work", "peer-cause", "peer-spend"}) {
		t.Fatalf("the whole log = %v, want every row: the case exercises nothing otherwise", got)
	}
	if got := page(store.ListQuery{Limit: 10, FeedOnly: true}); !slices.Equal(got, []string{"mine-work", "peer-cause"}) {
		t.Errorf("a feed-only page = %v, want every row but the peer's accounting row", got)
	}
	if got := page(store.ListQuery{Limit: 10, RelatedAgent: "Lead", FeedOnly: true}); !slices.Equal(got, []string{"mine-work", "peer-cause"}) {
		t.Errorf("a related agent's feed-only page = %v, want its work and its cause, "+
			"without the trace's accounting a peer holds", got)
	}

	axis, coverage, err := fan.Histogram(t.Context(), store.HistogramQuery{
		ListQuery: store.ListQuery{FeedOnly: true}, Bucket: store.BucketHour})
	if err != nil || !coverage.Complete {
		t.Fatalf("Histogram = coverage %+v, %v", coverage, err)
	}
	if axis.Total != 2 {
		t.Errorf("a feed-only axis counts %d rows, want 2 — the peer's accounting row "+
			"is a bar the page beside it does not list", axis.Total)
	}
}
