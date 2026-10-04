package observe_test

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
)

// claims is the fleet's custody record as a keeper reaches it: it can be
// unreachable, and it counts what it was asked.
type claims struct {
	inner  *coordmem.Fleet
	down   atomic.Bool
	called atomic.Int64
}

func (c *claims) ClaimCustody(ctx context.Context, batch, node string) (string, error) {
	c.called.Add(1)
	if c.down.Load() {
		return "", errors.New("the coordination store is unreachable")
	}
	return c.inner.ClaimCustody(ctx, batch, node)
}

// dataNode is one data node: its own event log and its keeper over the
// fleet's shared custody record.
type dataNode struct {
	name   string
	log    *store.EventLog
	keeper *observe.Keeper
}

func newDataNode(t *testing.T, name string, c *claims) *dataNode {
	t.Helper()
	db, err := store.OpenNode(t.Context(), filepath.Join(t.TempDir(), name+".db"), store.Options{})
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	k, err := observe.NewKeeper(db.Events(), c, name)
	if err != nil {
		t.Fatal(err)
	}
	return &dataNode{name: name, log: db.Events(), keeper: k}
}

// holds reports whether this node's log has the event.
func (n *dataNode) holds(t *testing.T, id string) bool {
	t.Helper()
	_, err := n.log.ByID(t.Context(), id)
	switch {
	case err == nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		return false
	}
	t.Fatalf("%s: read %s: %v", n.name, id, err)
	return false
}

// carrier is a stateless node's batch of n persisted events, as it arrives.
func carrier(t *testing.T, n int) (*events.Event, []string) {
	t.Helper()
	var evs []*events.Event
	var ids []string
	for range n {
		ev := events.New(types.AgentPhaseStarted{RoleName: "Engineer"}, events.TraceContext{})
		ev.Node = "seats-1"
		evs = append(evs, ev)
		ids = append(ids, ev.ID.String())
	}
	batch := events.New(types.CustodyBatch{Events: evs}, events.TraceContext{})
	batch.Node = "seats-1"
	return batch, ids
}

// exactlyOne fails unless every event is in exactly one of the nodes' logs.
func exactlyOne(t *testing.T, ids []string, nodes ...*dataNode) {
	t.Helper()
	for _, id := range ids {
		var in []string
		for _, n := range nodes {
			if n.holds(t, id) {
				in = append(in, n.name)
			}
		}
		if len(in) != 1 {
			t.Errorf("event %s is in %v, want exactly one data node's log", id, in)
		}
	}
}

// later moves a keeper's clock past the grace a reconcile waits out.
func later(k *observe.Keeper, by time.Duration) {
	observe.SetKeeperClock(k, func() time.Time { return time.Now().Add(by) })
}

// TWO DATA NODES HANDED ONE BATCH KEEP IT ONCE: the group delivers again a
// batch whose acknowledgement was lost, to whichever member asks next, and
// both acknowledge it — the second having deleted its copy.
func TestABatchDeliveredToTwoDataNodesIsKeptByOne(t *testing.T) {
	t.Parallel()
	c := &claims{inner: coordmem.NewFleet()}
	a, b := newDataNode(t, "data-a", c), newDataNode(t, "data-b", c)
	batch, ids := carrier(t, 3)
	for _, n := range []*dataNode{a, b} {
		if res := n.keeper.Handle(t.Context(), batch); res.Outcome != queue.OutcomeAck {
			t.Fatalf("%s answered %+v, want an acknowledgement", n.name, res)
		}
	}
	exactlyOne(t, ids, a, b)
	if !a.holds(t, ids[0]) {
		t.Error("the node that claimed first does not keep the batch")
	}
}

// A NODE THAT DIED BETWEEN WRITING A BATCH AND CLAIMING IT deletes its copy when
// it comes back, if the redelivery made another node the keeper: written and
// never claimed, the copy is the one a reconcile exists for.
func TestACopyWrittenAndNeverClaimedIsDeletedWhenAnotherNodeKeepsIt(t *testing.T) {
	t.Parallel()
	c := &claims{inner: coordmem.NewFleet()}
	a, b := newDataNode(t, "data-a", c), newDataNode(t, "data-b", c)
	batch, ids := carrier(t, 3)
	// data-a wrote it and died before it could claim.
	writeOnly(t, a, batch)
	// The group gave it to data-b, which keeps it.
	if res := b.keeper.Handle(t.Context(), batch); res.Outcome != queue.OutcomeAck {
		t.Fatalf("data-b answered %+v", res)
	}
	// data-a comes back.
	later(a.keeper, 2*time.Minute)
	if settled, err := a.keeper.Reconcile(t.Context()); err != nil || settled != 1 {
		t.Fatalf("reconcile = (%d, %v), want the one batch settled", settled, err)
	}
	exactlyOne(t, ids, a, b)
	if !b.holds(t, ids[0]) {
		t.Error("the keeper lost its copy")
	}
}

// A NODE THAT DIED AFTER CLAIMING keeps its copy when it comes back, and the node
// the batch was redelivered to deletes its own: the claim, not the order the
// two wrote in, is what decides.
func TestACopyClaimedBeforeACrashIsKeptAndTheRedeliveryLetGo(t *testing.T) {
	t.Parallel()
	c := &claims{inner: coordmem.NewFleet()}
	a, b := newDataNode(t, "data-a", c), newDataNode(t, "data-b", c)
	batch, ids := carrier(t, 3)
	// data-a wrote it and claimed it, and died before settling and
	// acknowledging.
	writeOnly(t, a, batch)
	if keeper, err := c.ClaimCustody(t.Context(), batch.ID.String(), "data-a"); err != nil || keeper != "data-a" {
		t.Fatalf("setup claim = (%q, %v)", keeper, err)
	}
	if res := b.keeper.Handle(t.Context(), batch); res.Outcome != queue.OutcomeAck {
		t.Fatalf("data-b answered %+v", res)
	}
	later(a.keeper, 2*time.Minute)
	if _, err := a.keeper.Reconcile(t.Context()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	exactlyOne(t, ids, a, b)
	if !a.holds(t, ids[0]) {
		t.Error("the node that claimed first does not keep the batch")
	}
}

// A CLAIM THE STORE COULD NOT ANSWER keeps the copy and sends the delivery back:
// neither keeping it nor deleting it is honest while nobody knows who keeps
// the batch. The pass settles it once the store answers.
func TestAnUnansweredClaimKeepsTheCopyUntilThePassSettlesIt(t *testing.T) {
	t.Parallel()
	c := &claims{inner: coordmem.NewFleet()}
	a := newDataNode(t, "data-a", c)
	batch, ids := carrier(t, 2)
	c.down.Store(true)
	if res := a.keeper.Handle(t.Context(), batch); res.Outcome != queue.OutcomeNak {
		t.Fatalf("with the store away the delivery answered %+v, want it sent back", res)
	}
	if !a.holds(t, ids[0]) {
		t.Fatal("the copy was dropped while nobody knew who keeps the batch")
	}
	c.down.Store(false)
	later(a.keeper, 2*time.Minute)
	if settled, err := a.keeper.Reconcile(t.Context()); err != nil || settled != 1 {
		t.Fatalf("reconcile = (%d, %v), want the batch settled", settled, err)
	}
	if !a.holds(t, ids[0]) {
		t.Error("the only copy was deleted")
	}
	if left, err := a.log.UnsettledCustody(t.Context(), time.Now().Add(time.Hour), 10); err != nil || len(left) != 0 {
		t.Errorf("unsettled after the pass = (%v, %v), want none", left, err)
	}
}

// A BATCH FRESHER THAN THE GRACE IS LEFT TO ITS DELIVERY, and one older than the
// event log keeps rows is let go without asking: its keeper's record may have
// aged out with the rows, so there is nothing left to decide.
func TestThePassLeavesWhatIsInFlightAndLetsGoWhatIsPastRetention(t *testing.T) {
	t.Parallel()
	c := &claims{inner: coordmem.NewFleet()}
	a := newDataNode(t, "data-a", c)
	batch, _ := carrier(t, 1)
	writeOnly(t, a, batch)
	if settled, err := a.keeper.Reconcile(t.Context()); err != nil || settled != 0 || c.called.Load() != 0 {
		t.Fatalf("a batch written a moment ago was settled (%d, %v, %d claims)",
			settled, err, c.called.Load())
	}
	later(a.keeper, store.EventRetention+time.Hour)
	if settled, err := a.keeper.Reconcile(t.Context()); err != nil || settled != 1 {
		t.Fatalf("reconcile = (%d, %v), want the old batch let go", settled, err)
	}
	if c.called.Load() != 0 {
		t.Error("a batch past the log's retention was claimed")
	}
}

// A CARRIER NOBODY CAN READ is acknowledged, never redelivered to every node in
// turn until the dead-letter queue takes it.
func TestAnUnreadableBatchIsAcknowledged(t *testing.T) {
	t.Parallel()
	a := newDataNode(t, "data-a", &claims{inner: coordmem.NewFleet()})
	bad := events.New(types.AgentPhaseStarted{}, events.TraceContext{})
	if res := a.keeper.Handle(t.Context(), bad); res.Outcome != queue.OutcomeAck {
		t.Fatalf("an unreadable carrier answered %+v", res)
	}
}

// END TO END, OVER THE BROKER: a stateless node's events reach exactly one of
// two data nodes in one group, and every event is somewhere.
func TestAStatelessNodesEventsReachExactlyOneDataNode(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	start := func(node string) *memory.Queue {
		q := broker.Client(memory.Contract(queue.WithNode(node)))
		if err := q.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = q.Stop(context.Background()) })
		return q
	}
	c := &claims{inner: coordmem.NewFleet()}
	var nodes []*dataNode
	for _, name := range []string{"data-a", "data-b"} {
		n := newDataNode(t, name, c)
		if err := n.keeper.Start(t.Context(), start(name)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(n.keeper.Stop)
		nodes = append(nodes, n)
	}
	seats := start("seats-1")
	custody := observe.NewCustody(seats)
	seats.AddPublishListener(custody.Listen())
	custody.Start(t.Context())

	var ids []string
	var mu sync.Mutex
	for range 300 {
		ev := events.New(types.AgentPhaseStarted{RoleName: "Engineer"}, events.TraceContext{})
		if err := seats.Publish(t.Context(), "crewlet.events.agent_phase_started", ev); err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		ids = append(ids, ev.ID.String())
		mu.Unlock()
	}
	custody.Stop(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for {
		all := true
		for _, id := range ids {
			if !nodes[0].holds(t, id) && !nodes[1].holds(t, id) {
				all = false
				break
			}
		}
		if all {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not every event reached a data node")
		}
		time.Sleep(20 * time.Millisecond)
	}
	exactlyOne(t, ids, nodes...)
}

// writeOnly is a keeper's write without its claim: the state a crash between
// the two leaves.
func writeOnly(t *testing.T, n *dataNode, batch *events.Event) {
	t.Helper()
	data, ok := events.DataAs[*types.CustodyBatch](batch)
	if !ok {
		t.Fatal("not a custody batch")
	}
	var records []store.EventRecord
	for _, ev := range data.Events {
		rec, ok := observe.Record(ev)
		if !ok {
			t.Fatal("a fixture event is not persisted")
		}
		records = append(records, rec)
	}
	if err := n.log.WriteCustody(t.Context(), store.CustodyBatch{
		ID: batch.ID.String(), Origin: batch.Node, Records: records,
	}, time.Now()); err != nil {
		t.Fatal(err)
	}
}
