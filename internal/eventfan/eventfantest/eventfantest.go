// Package eventfantest is the certification suite for the fleet's history
// scatter: one set of cases, run over every broker the fleet can be deployed
// on, in the queuetest / coordtest tradition.
//
// A merge that agrees with itself on the in-memory twin proves nothing about a
// real cluster, where a reply crosses routes, a subscription is registered
// when its interest reaches the server rather than when the call returns, and
// a member can stop answering while the rest keep a quorum. So the cases are
// written once and each broker runs them: internal/eventfan runs them on the
// twin, and this package's own test on a three-member embedded cluster.
package eventfantest

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/store"
)

// Factory returns n started queue clients of ONE broker, one per member. The
// suite gives each its own store and its own answerer.
type Factory func(t *testing.T, n int) []queue.EventQueue

// member is one node of the fleet under test.
type member struct {
	id   string
	q    queue.EventQueue
	log  *store.EventLog
	stop queue.Unsubscribe
}

// Budget is what the suite gives a peer to answer: generous, because what is
// under test is the merge and a loaded CI runner's route is not.
const Budget = 10 * time.Second

// Run certifies the scatter over the broker the factory builds.
func Run(t *testing.T, factory Factory) {
	t.Run("a_listing_is_every_nodes_rows_in_one_order", func(t *testing.T) {
		t.Parallel()
		nodes := fleet(t, factory)
		base := time.Now().UTC().Add(-time.Hour)
		for i := range 9 {
			write(t, nodes[i%3], store.EventRecord{
				ID: fmt.Sprintf("e%02d", i), Type: "agent_phase_started", Category: "task",
				Time: base.Add(time.Duration(i) * time.Second), TraceID: "tr-1",
			})
		}
		listing, coverage, err := asker(nodes).List(t.Context(), store.ListQuery{Limit: 4})
		if err != nil {
			t.Fatal(err)
		}
		complete(t, coverage)
		if got := idsOf(listing.Rows); !slices.Equal(got, []string{"e08", "e07", "e06", "e05"}) {
			t.Fatalf("the fleet's newest four are %v, want e08…e05 across all three nodes", got)
		}
		if !listing.More {
			t.Error("a page cut at its size did not say more rows exist")
		}
	})

	t.Run("a_turn_resumed_on_another_node_is_one_turn", func(t *testing.T) {
		t.Parallel()
		nodes := fleet(t, factory)
		// AT THE STORE'S RESOLUTION, which is microseconds.
		at := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
		// THE TURN PARKED ON node 0 and finished on node 2 after a move.
		write(t, nodes[0], phase("p1", "t-1", at, 40, "model-a"))
		write(t, nodes[0], completion("c1", "t-1", at.Add(time.Second), true, 1000))
		write(t, nodes[2], phase("p2", "t-1", at.Add(time.Minute), 60, "model-b"))
		write(t, nodes[2], completion("c2", "t-1", at.Add(time.Minute+time.Second), false, 500))

		page, coverage, err := asker(nodes).Turns(t.Context(), store.TurnQuery{})
		if err != nil {
			t.Fatal(err)
		}
		complete(t, coverage)
		if len(page.Turns) != 1 {
			t.Fatalf("the fleet listed %d rows for one turn: %+v", len(page.Turns), page.Turns)
		}
		turn := page.Turns[0]
		if !turn.Complete || turn.Parked || turn.Phases != 2 || turn.TotalTokens != 100 ||
			turn.DurationMS != 1500 || !turn.StartedAt.Equal(at) {
			t.Fatalf("the merged turn is %+v, want both halves: complete, 2 phases, "+
				"100 tokens, 1500ms, started at its first half", turn)
		}

		detail, coverage, err := asker(nodes).Turn(t.Context(), "t-1")
		if err != nil {
			t.Fatal(err)
		}
		complete(t, coverage)
		if got := idsOf(detail.Rows); !slices.Equal(got, []string{"p1", "c1", "p2", "c2"}) {
			t.Fatalf("the turn's rows are %v, want both nodes' in order", got)
		}
	})

	t.Run("an_event_is_found_on_whichever_node_holds_it", func(t *testing.T) {
		t.Parallel()
		nodes := fleet(t, factory)
		write(t, nodes[1], store.EventRecord{ID: "held-by-1", Type: "org_started",
			Category: "lifecycle", Time: time.Now().UTC().Add(-time.Minute),
			Payload: json.RawMessage(`{"org_name":"acme"}`)})
		rec, coverage, err := asker(nodes).ByID(t.Context(), "held-by-1")
		if err != nil {
			t.Fatalf("an event held by a peer was not found: %v", err)
		}
		complete(t, coverage)
		if len(rec.Payload) == 0 {
			t.Error("the peer's copy arrived without its payload")
		}
	})

	t.Run("a_histogram_is_the_sum_of_every_nodes_bars", func(t *testing.T) {
		t.Parallel()
		nodes := fleet(t, factory)
		at := time.Now().UTC().Add(-2 * time.Hour)
		for i, n := range nodes {
			for j := range i + 1 {
				write(t, n, store.EventRecord{ID: fmt.Sprintf("h%d-%d", i, j),
					Type: "x", Category: "task", Time: at})
			}
		}
		h, coverage, err := asker(nodes).Histogram(t.Context(), store.HistogramQuery{
			ListQuery: store.ListQuery{Since: at.Add(-time.Hour)}, Bucket: store.BucketHour,
		})
		if err != nil {
			t.Fatal(err)
		}
		complete(t, coverage)
		if h.Total != 6 || h.ByCategory["task"] != 6 {
			t.Fatalf("the fleet's axis totals %d (task %d), want 1+2+3 = 6", h.Total, h.ByCategory["task"])
		}
	})

	t.Run("a_node_that_stops_answering_is_named", func(t *testing.T) {
		t.Parallel()
		nodes := fleet(t, factory)
		write(t, nodes[0], store.EventRecord{ID: "here", Type: "x", Category: "task",
			Time: time.Now().UTC().Add(-time.Minute)})
		if err := nodes[2].stop(context.WithoutCancel(t.Context())); err != nil {
			t.Fatal(err)
		}
		fan := asker(nodes)
		fan.Budget = time.Second
		listing, coverage, err := fan.List(t.Context(), store.ListQuery{Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if coverage.Complete || !slices.Equal(coverage.Missing(), []string{nodes[2].id}) {
			t.Fatalf("coverage %+v, want %s named and the answer incomplete", coverage, nodes[2].id)
		}
		if got := idsOf(listing.Rows); !slices.Equal(got, []string{"here"}) {
			t.Errorf("the answering nodes' rows are %v, want them merged anyway", got)
		}
	})
}

// Members is how many nodes every case's fleet has: three, the smallest fleet
// in which one node can stop answering while two still merge.
const Members = 3

// fleet builds the members, each serving from its own store.
func fleet(t *testing.T, factory Factory) []*member {
	t.Helper()
	queues := factory(t, Members)
	out := make([]*member, 0, Members)
	for i, q := range queues {
		db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), fmt.Sprintf("n%d.db", i)), store.Options{})
		if err != nil {
			t.Fatalf("open a store: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		m := &member{id: fmt.Sprintf("node-%d", i), q: q, log: db.Events()}
		stop, err := eventfan.Serve(t.Context(), q, m.id, m.log)
		if err != nil {
			t.Fatalf("serve %s: %v", m.id, err)
		}
		m.stop = stop
		t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
		out = append(out, m)
	}
	return out
}

// asker is the fleet as the first member sees it.
func asker(nodes []*member) *eventfan.Fleet {
	roster := make([]string, 0, len(nodes))
	for _, n := range nodes {
		roster = append(roster, n.id)
	}
	return &eventfan.Fleet{
		Self: nodes[0].id, Local: nodes[0].log, Queue: nodes[0].q,
		Roster: func(context.Context) ([]string, error) { return roster, nil },
		Budget: Budget,
	}
}

func write(t *testing.T, m *member, rec store.EventRecord) {
	t.Helper()
	if rec.Payload == nil {
		rec.Payload = json.RawMessage(`{}`)
	}
	if err := m.log.Append(t.Context(), rec); err != nil {
		t.Fatalf("%s: append %s: %v", m.id, rec.ID, err)
	}
}

func phase(id, turn string, at time.Time, tokens int, model string) store.EventRecord {
	return store.EventRecord{
		ID: id, Type: "agent_phase_completed", Category: "task", Time: at,
		Tags:    map[string]string{"turn_id": turn, "agent_role": "Lead"},
		Payload: json.RawMessage(`{}`),
		Spend: &store.Spend{Phase: "execute", Model: model, TurnID: turn,
			TotalTokens: tokens, InputTokens: tokens},
	}
}

func completion(id, turn string, at time.Time, suspended bool, ms int) store.EventRecord {
	body, _ := json.Marshal(map[string]any{
		"turn_id": turn, "suspended": suspended, "duration_ms": ms,
	})
	return store.EventRecord{
		ID: id, Type: "turn_completed", Category: "task", Time: at,
		Tags:    map[string]string{"turn_id": turn, "agent_role": "Lead"},
		Payload: body,
		Spend:   &store.Spend{TurnID: turn},
	}
}

func complete(t *testing.T, c eventfan.Coverage) {
	t.Helper()
	if !c.Complete || len(c.Nodes) != Members || len(c.Missing()) != 0 {
		t.Fatalf("coverage %+v, want all %d nodes answered", c, Members)
	}
}

func idsOf(rows []store.EventRecord) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}
