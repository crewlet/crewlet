package eventfan

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// olderPeer answers every listing as a build that does not know `feed_only`:
// it reads the request at whatever version it came in, ignores the field, and
// sends the rows it holds, accounting rows among them.
func olderPeer(t *testing.T, q queue.EventQueue, node string, rows []store.EventRecord, full bool) {
	t.Helper()
	stop, err := q.Serve(t.Context(), Subject, func(_ context.Context, raw []byte) ([]byte, error) {
		var req request
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		if req.Asker == node {
			return nil, errAsker
		}
		if req.Version > 4 {
			return encodeError(node, "this node speaks history protocol up to v4")
		}
		return fit(node, listPart{Rows: rows, Full: full}, queue.MaxPayloadBytes, req.Version)
	})
	if err != nil {
		t.Fatalf("serve %s: %v", node, err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
}

func memberQueue(t *testing.T, b *memory.Broker) queue.EventQueue {
	t.Helper()
	q := b.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	return q
}

// A FEED-ONLY LISTING IS ANSWERED BY AN OLDER PEER, AND NARROWED BY THE ASKER.
//
// A restarted node seeds its activity feed with one feed-only page from every
// node. Asked at a version only this build speaks, every peer on the build
// before refused it, and the feed came back without their recent events for
// the length of an upgrade — to keep out rows the asker can drop itself,
// since every row names its own type. So the listing goes out at its other
// filters' version, the older peer answers wider, and nothing it sent of a type
// the feed does not carry reaches the page.
//
// And a page whose every row the asker dropped is still a page: its cursor is
// past what it covered, so a reader paging the log goes on to the rows below
// rather than taking it for the start of history.
func TestAnOlderPeersFeedListingIsNarrowedNotRefused(t *testing.T) {
	t.Parallel()
	unfed := events.Unfed()
	if len(unfed) == 0 {
		t.Fatal("no type is kept out of the feed — the case asserts nothing")
	}
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), "new.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	base := time.Now().UTC().Add(-time.Hour)
	if err := db.Events().Append(t.Context(), store.EventRecord{
		ID: "mine", Type: "agent_turn_completed", Category: "lifecycle",
		Time: base, Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	broker := memory.NewBroker()
	olderPeer(t, memberQueue(t, broker), "old", []store.EventRecord{
		{ID: "old-spend", Type: unfed[0], Category: "system", Time: base.Add(3 * time.Minute)},
		{ID: "old-turn", Type: "agent_turn_completed", Category: "lifecycle", Time: base.Add(2 * time.Minute)},
	}, false)
	fleet := &Fleet{
		Self: "new", Local: db.Events(), Queue: memberQueue(t, broker),
		Roster: func(context.Context) ([]string, error) { return []string{"new", "old"}, nil },
		Budget: 5 * time.Second,
	}
	got, coverage, err := fleet.List(t.Context(), store.ListQuery{Limit: 10, FeedOnly: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !coverage.Complete {
		t.Fatalf("coverage = %+v — the older peer was not answered", coverage)
	}
	var ids []string
	for _, r := range got.Rows {
		ids = append(ids, r.ID)
	}
	if len(ids) != 2 || ids[0] != "old-turn" || ids[1] != "mine" {
		t.Fatalf("rows = %v, want the older peer's turn and this node's, and no accounting row", ids)
	}

	// EVERY ROW DROPPED, the page still covered them.
	broker2 := memory.NewBroker()
	olderPeer(t, memberQueue(t, broker2), "old", []store.EventRecord{
		{ID: "burst-2", Type: unfed[0], Category: "system", Time: base.Add(5 * time.Minute)},
		{ID: "burst-1", Type: unfed[0], Category: "system", Time: base.Add(4 * time.Minute)},
	}, true)
	fleet.Queue = memberQueue(t, broker2)
	page, _, err := fleet.List(t.Context(), store.ListQuery{Limit: 2, FeedOnly: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(page.Rows) != 0 {
		t.Fatalf("rows = %+v, want none: the page was all accounting rows", page.Rows)
	}
	if page.Next == nil || page.Next.ID != "burst-1" {
		t.Fatalf("next = %+v, want the cursor past the dropped rows — nil reads as the end of history",
			page.Next)
	}
}
