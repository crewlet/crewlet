package observe_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/observe"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
)

// claims is the fleet's custody record as a keeper reaches it: it can be
// unreachable, it can answer one batch's claim with an error of its own, and
// it counts what it was asked.
type claims struct {
	inner  *coordmem.Fleet
	down   atomic.Bool
	called atomic.Int64
	// answers holds, by batch id, the error a claim on that batch is
	// answered with instead of the store's keeper.
	answers sync.Map
}

func (c *claims) ClaimCustody(ctx context.Context, batch, node string) (string, error) {
	c.called.Add(1)
	if c.down.Load() {
		return "", errors.New("the coordination store is unreachable")
	}
	if answer, ok := c.answers.Load(batch); ok {
		return "", answer.(error)
	}
	return c.inner.ClaimCustody(ctx, batch, node)
}

// undecidable is the store's answer for a batch whose keeper record exists and
// nothing can read — what the KV backend answers for a record this engine
// never writes (internal/coord/kv's custody_test.go).
func undecidable(batch string) error {
	return fmt.Errorf("%w: batch %s: it names no node", coord.ErrCustodyUndecidable, batch)
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
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), name+".db"), store.Options{})
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
	_, err := n.log.ByID(t.Context(), id, time.Now())
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
	if pass, err := a.keeper.Reconcile(t.Context()); err != nil || pass.Settled != 1 {
		t.Fatalf("reconcile = (%+v, %v), want the one batch settled", pass, err)
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
	if pass, err := a.keeper.Reconcile(t.Context()); err != nil || pass.Settled != 1 {
		t.Fatalf("reconcile = (%+v, %v), want the batch settled", pass, err)
	}
	if !a.holds(t, ids[0]) {
		t.Error("the only copy was deleted")
	}
	if left, err := a.log.UnsettledCustody(t.Context(), time.Now().Add(time.Hour), store.UnsettledBatch{}, 10); err != nil || len(left) != 0 {
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
	if pass, err := a.keeper.Reconcile(t.Context()); err != nil || pass.Settled != 0 || c.called.Load() != 0 {
		t.Fatalf("a batch written a moment ago was settled (%+v, %v, %d claims)",
			pass, err, c.called.Load())
	}
	later(a.keeper, store.EventRetention+time.Hour)
	if pass, err := a.keeper.Reconcile(t.Context()); err != nil || pass.Settled != 1 {
		t.Fatalf("reconcile = (%+v, %v), want the old batch let go", pass, err)
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
		t.Cleanup(func() { n.keeper.Stop(context.Background()) })
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

// A KEEPER THAT STOPS HAS LEFT THE GROUP: what is published afterwards is kept
// by the data nodes still in it, and the stopped node writes none of it.
//
// The engine stops the keeper before its usage publisher's last flush, because
// a batch settled as kept makes rows count on its day: a keeper whose
// attachment outlived its stop went on writing and settling batches until the
// queue closed — after that flush, into a day a node that never starts again
// never publishes.
//
// Mutation: end only the settling pass in Stop, and the stopped node goes on
// taking batches.
func TestAStoppedKeeperHasLeftTheGroup(t *testing.T) {
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
	stopped, staying := newDataNode(t, "data-a", c), newDataNode(t, "data-b", c)
	for _, n := range []*dataNode{stopped, staying} {
		if err := n.keeper.Start(t.Context(), start(n.name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { staying.keeper.Stop(context.Background()) })
	stopped.keeper.Stop(t.Context())

	seats := start("seats-1")
	custody := observe.NewCustody(seats)
	seats.AddPublishListener(custody.Listen())
	custody.Start(t.Context())
	var ids []string
	for range 50 {
		ev := events.New(types.AgentPhaseStarted{RoleName: "Engineer"}, events.TraceContext{})
		if err := seats.Publish(t.Context(), "crewlet.events.agent_phase_started", ev); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, ev.ID.String())
	}
	custody.Stop(context.Background())
	deadline := time.Now().Add(10 * time.Second)
	for _, id := range ids {
		for !staying.holds(t, id) {
			if time.Now().After(deadline) {
				t.Fatalf("event %s never reached the data node still in the group", id)
			}
			time.Sleep(20 * time.Millisecond)
		}
		if stopped.holds(t, id) {
			t.Errorf("event %s was written by a keeper that had stopped", id)
		}
	}
}

// writeOnly is a keeper's write without its claim: the state a crash between
// the two leaves.
func writeOnly(t *testing.T, n *dataNode, batch *events.Event) {
	t.Helper()
	writeAt(t, n, batch, time.Now())
}

// writeAt is [writeOnly] for a batch first written at `at`, which is what the
// pass lists its batches in the order of.
func writeAt(t *testing.T, n *dataNode, batch *events.Event, at time.Time) {
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
	}, at); err != nil {
		t.Fatal(err)
	}
}

// A STATELESS NODE'S DELIVERY REACHES A DATA NODE, FILED AS A DELIVERY.
//
// The Mattermost socket runs on every node, and a node without `data` keeps no
// event log: a delivery row written straight into its own store vanished with
// its scratch database, which is why the socket PUBLISHES its record. Through
// custody that record crosses the wire inside a batch and is filed by the data
// node keeping it — under the delivery's label, with the bytes that arrived,
// tagged with who it was for and where it came in — exactly as the node that
// published it would have filed it.
//
// Mutation: file the record under its event type in observe.Record, and the
// row reads `inbound_delivery` rather than what the socket called it; leave
// the envelope's node off the record, and the row names no origin.
func TestAStatelessNodesDeliveryIsKeptAsADelivery(t *testing.T) {
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
	data := newDataNode(t, "data-a", &claims{inner: coordmem.NewFleet()})
	if err := data.keeper.Start(t.Context(), start("data-a")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { data.keeper.Stop(context.Background()) })
	seats := start("seats-1")
	custody := observe.NewCustody(seats)
	seats.AddPublishListener(custody.Listen())
	custody.Start(t.Context())

	body := []byte(`{"event":"posted","post":{"id":"p1","channel_id":"C1"}}`)
	ev := events.New(types.InboundDelivery{
		Label: "socket:posted", Route: "mattermost", Text: "post for @agent-swe in channel C1",
		Recipient: "swe", DeliveryKey: "p1", Channel: "C1", Body: body,
	}, events.TraceContext{})
	ev.Source = "mattermost"
	if err := seats.Publish(t.Context(), "crewlet.events.inbound_delivery", ev); err != nil {
		t.Fatal(err)
	}
	custody.Stop(context.Background())

	deadline := time.Now().Add(10 * time.Second)
	for !data.holds(t, ev.ID.String()) {
		if time.Now().After(deadline) {
			t.Fatal("the stateless node's delivery never reached the data node")
		}
		time.Sleep(20 * time.Millisecond)
	}
	row, err := data.log.ByID(t.Context(), ev.ID.String(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if row.Type != "socket:posted" || row.Category != events.WebhookCategory || row.Source != "mattermost" {
		t.Errorf("row filed as (%q, %q, %q), want (socket:posted, %s, mattermost)",
			row.Type, row.Category, row.Source, events.WebhookCategory)
	}
	if string(row.Payload) != string(body) {
		t.Errorf("row payload = %s, want the post as it arrived %s", row.Payload, body)
	}
	if row.Tags["recipient"] != "swe" || row.Tags["delivery_key"] != "p1" || row.Tags["route"] != "mattermost" {
		t.Errorf("row tags = %v, want the seat, the post id and the socket route", row.Tags)
	}
	// AND IT NAMES WHERE IT ARRIVED: the node that published it, off the
	// envelope — not the data node keeping it, and not the vendor's body,
	// which is all its payload holds.
	if row.Node != "seats-1" {
		t.Errorf("the row kept on data-a names %q as its origin, want seats-1, the node "+
			"that published it", row.Node)
	}
}

// unsettledIDs is every batch the node has written and not settled, oldest
// first.
func unsettledIDs(t *testing.T, n *dataNode) []string {
	t.Helper()
	var ids []string
	after := store.UnsettledBatch{}
	for {
		page, err := n.log.UnsettledCustody(t.Context(), time.Now().Add(time.Hour), after, 100)
		if err != nil {
			t.Fatalf("%s: list the unsettled batches: %v", n.name, err)
		}
		for _, b := range page {
			ids = append(ids, b.ID)
		}
		if len(page) < 100 {
			return ids
		}
		after = page[len(page)-1]
	}
}

// A BATCH WHOSE KEEPER CANNOT BE DECIDED IS STEPPED OVER, and every batch written
// after it is settled in the same pass.
//
// The fleet's record of who keeps a batch can be one nothing reads — written
// into the bucket by something that is not this engine, reshaped by a later
// build, corrupted — and the store says so definitely. The pass lists the
// oldest first, and it used to stop at that answer: every batch behind the
// record stayed unsettled on every pass and every boot until the record's
// batch aged past the event log's retention — a batch this node keeps counted
// in none of its days for a month, and a copy another node keeps held here
// beside it. Here the record is the oldest, and behind it are a batch this
// node claimed before a crash, one another node keeps, and one nobody has
// claimed.
//
// Mutation: stop the pass on every claim error in Reconcile, and it settles
// nothing.
func TestThePassStepsOverABatchWhoseKeeperCannotBeDecided(t *testing.T) {
	t.Parallel()
	c := &claims{inner: coordmem.NewFleet()}
	a, b := newDataNode(t, "data-a", c), newDataNode(t, "data-b", c)
	base := time.Now().Add(-time.Hour)

	poisoned, poisonedIDs := carrier(t, 2)
	c.answers.Store(poisoned.ID.String(), undecidable(poisoned.ID.String()))
	writeAt(t, a, poisoned, base)

	mine, mineIDs := carrier(t, 1)
	writeAt(t, a, mine, base.Add(time.Minute))
	if keeper, err := c.ClaimCustody(t.Context(), mine.ID.String(), "data-a"); err != nil || keeper != "data-a" {
		t.Fatalf("setup claim = (%q, %v)", keeper, err)
	}

	theirs, theirIDs := carrier(t, 1)
	writeAt(t, a, theirs, base.Add(2*time.Minute))
	if res := b.keeper.Handle(t.Context(), theirs); res.Outcome != queue.OutcomeAck {
		t.Fatalf("data-b answered %+v", res)
	}

	unclaimed, unclaimedIDs := carrier(t, 1)
	writeAt(t, a, unclaimed, base.Add(3*time.Minute))

	pass, err := a.keeper.Reconcile(t.Context())
	if err != nil {
		t.Fatalf("the pass stopped: %v — every batch behind the undecidable one "+
			"stays unsettled until it ages out", err)
	}
	if pass.Settled != 3 {
		t.Errorf("the pass settled %d batch(es), want the three behind the undecidable one", pass.Settled)
	}
	if len(pass.Undecidable) != 1 || pass.Undecidable[0].ID != poisoned.ID.String() {
		t.Errorf("the pass reported %+v as undecidable, want the poisoned batch alone", pass.Undecidable)
	}
	if got := unsettledIDs(t, a); !slices.Equal(got, []string{poisoned.ID.String()}) {
		t.Errorf("unsettled after the pass = %v, want the undecidable batch alone", got)
	}
	// Neither kept nor deleted: the rows of a batch nobody can decide stay.
	for _, id := range poisonedIDs {
		if !a.holds(t, id) {
			t.Errorf("the undecidable batch's event %s was deleted on a guess", id)
		}
	}
	exactlyOne(t, append(mineIDs, theirIDs...), a, b)
	exactlyOne(t, unclaimedIDs, a, b)
	if !a.holds(t, mineIDs[0]) || !a.holds(t, unclaimedIDs[0]) || !b.holds(t, theirIDs[0]) {
		t.Error("a batch settled to the wrong keeper")
	}
}

// A PASS READS PAST A WHOLE PAGE OF UNDECIDABLE BATCHES: a pass lists the
// unsettled batches a page at a time, and a page of batches it steps over
// would be the next page too if it listed from the oldest again — so a pass
// that only stepped over them would never end, and never reach the batch
// behind them. Under a deadline, so that pass fails rather than hangs.
//
// Mutation: list every page from the oldest in Reconcile, and the pass runs
// until the deadline ends it.
func TestAPassReadsPastAFullPageOfUndecidableBatches(t *testing.T) {
	t.Parallel()
	c := &claims{inner: coordmem.NewFleet()}
	a := newDataNode(t, "data-a", c)
	base := time.Now().Add(-time.Hour)
	undecided := observe.KeeperPassBatches + 1
	for i := range undecided {
		batch, _ := carrier(t, 1)
		c.answers.Store(batch.ID.String(), undecidable(batch.ID.String()))
		writeAt(t, a, batch, base.Add(time.Duration(i)*time.Millisecond))
	}
	behind, ids := carrier(t, 1)
	writeAt(t, a, behind, base.Add(time.Minute))

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	pass, err := a.keeper.Reconcile(ctx)
	if err != nil {
		t.Fatalf("the pass ended with %v, after settling %d and stepping over %d",
			err, pass.Settled, len(pass.Undecidable))
	}
	if pass.Settled != 1 || len(pass.Undecidable) != undecided {
		t.Errorf("the pass settled %d and stepped over %d, want 1 and %d",
			pass.Settled, len(pass.Undecidable), undecided)
	}
	if got := unsettledIDs(t, a); len(got) != undecided || slices.Contains(got, behind.ID.String()) {
		t.Errorf("%d batch(es) unsettled after the pass, the one behind among them: %v — "+
			"want only the %d undecidable ones", len(got), slices.Contains(got, behind.ID.String()), undecided)
	}
	if !a.holds(t, ids[0]) {
		t.Error("the batch behind the undecidable ones lost its only copy")
	}
}

// AN UNKNOWN ANSWER STILL ENDS THE PASS. A store that did not answer for one
// batch will not answer for the next either, and asking each in turn would
// wait out a request timeout per batch; only the definite answer about one
// batch's record is stepped over.
//
// Mutation: step over every claim error in Reconcile, and the second batch is
// asked about with the store away.
func TestAnUnknownAnswerStillEndsThePass(t *testing.T) {
	t.Parallel()
	c := &claims{inner: coordmem.NewFleet()}
	a := newDataNode(t, "data-a", c)
	base := time.Now().Add(-time.Hour)
	first, _ := carrier(t, 1)
	c.answers.Store(first.ID.String(), fmt.Errorf("%w: the leader did not answer", coord.ErrUnavailable))
	writeAt(t, a, first, base)
	second, _ := carrier(t, 1)
	writeAt(t, a, second, base.Add(time.Minute))

	pass, err := a.keeper.Reconcile(t.Context())
	if !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("the pass ended with %v, want the store's unknown answer", err)
	}
	if pass.Settled != 0 || len(pass.Undecidable) != 0 || c.called.Load() != 1 {
		t.Errorf("the pass settled %d, stepped over %d and asked %d time(s), want "+
			"it to stop at the first, unanswered claim", pass.Settled, len(pass.Undecidable),
			c.called.Load())
	}
	if got := unsettledIDs(t, a); len(got) != 2 {
		t.Errorf("unsettled after the pass = %v, want both batches", got)
	}
}

// A DELIVERY WHOSE KEEPER CANNOT BE DECIDED IS ACKNOWLEDGED, and its copy stays.
// Sent back, the group would hand it to each data node in turn, every one told
// the same — the record is the fleet's — and every one writing one more copy
// nobody can settle. Acknowledged, the copy here is the batch's one copy, and
// the pass names it every time it runs.
//
// Mutation: send the delivery back on every settle error in Handle.
func TestADeliveryWhoseKeeperCannotBeDecidedIsAcknowledged(t *testing.T) {
	t.Parallel()
	c := &claims{inner: coordmem.NewFleet()}
	a := newDataNode(t, "data-a", c)
	batch, ids := carrier(t, 2)
	c.answers.Store(batch.ID.String(), undecidable(batch.ID.String()))
	if res := a.keeper.Handle(t.Context(), batch); res.Outcome != queue.OutcomeAck {
		t.Fatalf("the delivery answered %+v, want it acknowledged", res)
	}
	for _, id := range ids {
		if !a.holds(t, id) {
			t.Errorf("event %s was dropped on a guess", id)
		}
	}
	if got := unsettledIDs(t, a); !slices.Equal(got, []string{batch.ID.String()}) {
		t.Errorf("unsettled = %v, want the delivered batch, which nothing has decided", got)
	}
}
