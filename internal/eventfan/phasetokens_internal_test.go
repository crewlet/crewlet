package eventfan

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// spendNode is one member holding spend records and answering the scatter.
func spendNode(t *testing.T, b *memory.Broker, id string) (*store.EventLog, queue.EventQueue) {
	t.Helper()
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), id+".db"), store.Options{})
	if err != nil {
		t.Fatalf("open %s: %v", id, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	q := memberQueue(t, b)
	stop, err := Serve(t.Context(), q, id, db.Events())
	if err != nil {
		t.Fatalf("serve %s: %v", id, err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	return db.Events(), q
}

// memberQueue is one member's client of a shared in-memory broker.
func memberQueue(t *testing.T, b *memory.Broker) queue.EventQueue {
	t.Helper()
	q := b.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	return q
}

func spendRow(t *testing.T, log *store.EventLog, id string, at time.Time) {
	t.Helper()
	if err := log.Append(t.Context(), store.EventRecord{
		ID: id, Type: "agent_phase_completed", Category: "task", Time: at,
		Tags:    map[string]string{"agent_role": "Lead"},
		Spend:   &store.Spend{Phase: "execute", Model: "m", TotalTokens: 1, InputTokens: 1},
		Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("append %s: %v", id, err)
	}
}

// THE SEED'S SPEND IS READ IN PAGES, and every page is the fleet's.
//
// Asked whole, a busy node's day did not fit one reply: the reply was cut to
// the transport, and the merge — which cannot place a record older than the
// last one a cut node sent — cut EVERY node's records at that horizon, so a
// restarted node's live window started hours short of its day. Read in pages,
// each resuming below the last record the page before kept, the window comes
// back whole and in order however the records lie across the nodes, each
// record once.
//
// Mutation: stop after the first page, or resume from the newest record of a
// page rather than its last, and the window comes back short or with repeats.
func TestTheSeedsSpendIsReadInPagesAcrossTheFleet(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	a, qa := spendNode(t, broker, "node-a")
	b, _ := spendNode(t, broker, "node-b")
	at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	var want []string
	// Interleaved across the two nodes, a busy one and a quiet one, with two
	// records sharing one instant.
	for i := range 9 {
		id := fmt.Sprintf("r-%02d", i)
		log := a
		if i%3 == 0 {
			log = b
		}
		instant := at.Add(time.Duration(i) * time.Second)
		if i == 5 {
			instant = at.Add(4 * time.Second)
		}
		spendRow(t, log, id, instant)
		want = append(want, id)
	}
	slices.Reverse(want)

	fleet := &Fleet{
		Self: "node-a", Local: a, Queue: qa,
		Roster: func(context.Context) ([]string, error) { return []string{"node-a", "node-b"}, nil },
		Budget: 5 * time.Second,
	}
	for _, limit := range []int{0, 7} {
		records, coverage, err := fleet.phaseTokens(t.Context(),
			store.PhaseTokenQuery{Since: at.Add(-time.Minute), Limit: limit}, 2)
		if err != nil {
			t.Fatalf("limit %d: %v", limit, err)
		}
		if !coverage.Complete {
			t.Fatalf("limit %d: coverage %+v", limit, coverage)
		}
		var got []string
		for _, r := range records {
			got = append(got, r.EventID)
		}
		expect := want
		if limit > 0 {
			expect = want[:limit]
		}
		// The two records on one instant are in event-id order there; the
		// rest by instant, newest first.
		if !slices.Equal(got, expect) {
			t.Fatalf("limit %d: pages read %v, want %v", limit, got, expect)
		}
	}
}
