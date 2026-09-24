package usage_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/usage"
)

// fleetNode is one engine's share of the usage domain, wired as the engine
// wires it: its own store, its own consumer and applier on the shared log, and
// the framework's write authority under its own usage publisher.
type fleetNode struct {
	id        string
	db        *store.DB
	runner    *statelog.Runner
	publisher *usage.Publisher
	stop      func()
}

// joinFleet brings a node up on cluster member q, applying the whole log.
func joinFleet(t *testing.T, q *js.Queue, id string, now time.Time) *fleetNode {
	t.Helper()
	spec := usage.Domain{}.Stream()
	db := openStore(t)

	// RETRIED UNTIL THE STREAM HAS A LEADER: a node that joins just after a
	// member was cut off asks a cluster that is still electing one, and the
	// broker answers a deadline rather than a stream. An engine's boot rides
	// the same election under its provisioning budget.
	var (
		log      *js.DomainLog
		stats    js.LogStats
		consumer *js.DomainConsumer
		err      error
	)
	deadline := time.Now().Add(45 * time.Second)
	for {
		if log, err = q.DomainLog(t.Context(), spec.Name); err == nil {
			if stats, err = log.Stats(t.Context()); err == nil {
				consumer, err = q.DomainConsumer(t.Context(), spec.Name, id, 0)
			}
		}
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: open the log, its identity and a consumer: %v", id, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	runner, err := statelog.NewRunner(statelog.RunnerDeps{
		Domain: usage.Domain{}, Applier: usage.NewApplier(), Fetch: consumer,
		DB: db.Replicated(), StreamCreatedAt: stats.CreatedAt.UTC(),
	})
	if err != nil {
		t.Fatalf("%s: build the applier: %v", id, err)
	}
	rows, err := usage.NewRows(db)
	if err != nil {
		t.Fatalf("%s: build the read seam: %v", id, err)
	}
	authority, err := statelog.NewPublisher(statelog.Deps{
		Domain: usage.Domain{}, Log: log, Rows: rows, Fence: usage.NewFence(),
		Gates: usage.NewGates(), Waiter: runner, NodeID: id,
		Generation:    func() uint32 { return runner.Committed().Generation },
		ResolveBudget: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("%s: build the write authority: %v", id, err)
	}
	publisher, err := usage.NewPublisher(usage.PublisherDeps{
		Store: db, Log: authority, NodeID: id,
		Zone: func() *time.Location { return santiago },
		Now:  func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("%s: build the publisher: %v", id, err)
	}

	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan error, 1)
	go func() { done <- runner.Run(ctx) }()
	n := &fleetNode{id: id, db: db, runner: runner, publisher: publisher}
	n.stop = func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("%s: the applier stopped with %v", id, err)
		}
	}
	// REGISTERED AFTER THE STORE, so it runs BEFORE the store closes: an
	// applier still holding its pinned connection would fail the close.
	t.Cleanup(func() {
		if n.stop != nil {
			n.stop()
		}
	})
	return n
}

// halt stops a node's applier for good, idempotently.
func (n *fleetNode) halt() {
	if n.stop != nil {
		n.stop()
		n.stop = nil
	}
}

// spendOf waits until a node's rows hold exactly `want` spend rows for the day
// and returns them.
func (n *fleetNode) spendOf(t *testing.T, day string, want int) []usage.SpendRow {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		rows, err := usage.Spend(t.Context(), n.db.Replicated(), day, day)
		if err != nil {
			t.Fatalf("%s: read the spend: %v", n.id, err)
		}
		if len(rows) == want {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s holds %d spend row(s) for %s after 30s, want %d: %+v",
				n.id, len(rows), day, want, rows)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A DEPARTED NODE'S SPEND IS STILL ANSWERED.
//
// This is the decision adr/0020 records, and the reason the domain exists: a
// node's day was a question only that node's own audit log could answer, so a
// node that left took its history with it and every spend window after it
// under-reported the company by exactly its share. Here the day is replicated
// the moment it is published, and it survives the node twice over — in every
// peer's rows, and on the stream, where a node that joins AFTER the departure
// and never met the node that spent it replays the day from nothing.
func TestADepartedNodesSpendIsStillAnswered(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartPartitionableCluster(t, 3, js.Config{})
	spec := usage.Domain{}.Stream()
	// THE CEILING IS THE ONE FIELD OVERRIDDEN: a member in a temporary
	// directory will not reserve the shipped gibibyte three times over, and
	// the size is not what is under test.
	if err := c.Client(t, 0).EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: 16 << 20,
		MaxPerSubject: spec.MaxPerSubject, MaxAge: spec.MaxAge,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}

	now := time.Date(2026, 9, 23, 17, 0, 0, 0, santiago)
	const day = "2026-09-23"
	departing := joinFleet(t, c.Client(t, 0), "node-a", now)
	staying := joinFleet(t, c.Client(t, 1), "node-b", now)
	// THE LATE JOINER'S CONNECTION IS MADE NOW, while the cluster is whole:
	// a client waits for its member to be routed to every peer, and after
	// the partition one of them is gone for good. The NODE is not built
	// until node-a has left.
	later := c.Client(t, 2)

	// NODE A's SEAT SPENDS, and node B's seat spends too — the answer is a
	// fleet's, and a node's own share is only part of it.
	ev := &events{t: t, db: departing.db}
	ev.phase(now.Add(-3*time.Hour), "seat-a", "ta", "execute", "", 700, 70)
	ev.turn(now.Add(-3*time.Hour+time.Minute), "seat-a", "alice", "ta", false, time.Minute)
	(&events{t: t, db: staying.db}).phase(now.Add(-time.Hour), "seat-b", "tb", "execute", "", 50, 5)
	for _, n := range []*fleetNode{departing, staying} {
		if err := n.publisher.Flush(t.Context()); err != nil {
			t.Fatalf("%s: flush: %v", n.id, err)
		}
	}
	before := staying.spendOf(t, day, 2)

	// NODE A LEAVES: its applier stops, its member is cut off from the
	// rest, and its own store — the only place its events ever lived — is
	// never read again.
	departing.halt()
	c.Partition(t, 0)

	// THE NODE THAT STAYED still answers for it.
	if got := staying.spendOf(t, day, 2); !slices.Equal(got, before) {
		t.Fatalf("after node-a left, node-b answers %+v, was %+v", got, before)
	}
	aliceOn := func(rows []usage.SpendRow) (int64, bool) {
		for _, r := range rows {
			if r.Node == "node-a" && r.AgentID == "seat-a" {
				return r.Input, r.Handle == "alice"
			}
		}
		return 0, false
	}
	if in, named := aliceOn(before); in != 700 || !named {
		t.Fatalf("node-b holds node-a's seat at input %d (named %v), want 700 "+
			"named alice: %+v", in, named, before)
	}

	// AND A NODE THAT JOINS AFTERWARDS, which never met node-a, replays the
	// day from the stream alone.
	joiner := joinFleet(t, later, "node-c", now)
	after := joiner.spendOf(t, day, 2)
	if !slices.Equal(after, before) {
		t.Fatalf("a node that joined after node-a left answers %+v, and the fleet "+
			"answered %+v", after, before)
	}
}
