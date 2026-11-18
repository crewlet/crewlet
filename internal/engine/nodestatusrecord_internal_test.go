package engine

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// recordedRows is r as a peer reads it back off a store: through the bytes its
// record is kept as.
func recordedRows(t *testing.T, r coord.NodeStatusRecord) coord.NodeStatusRecord {
	t.Helper()
	raw, err := r.Encode()
	if err != nil {
		t.Fatalf("encode the status record: %v", err)
	}
	got, err := coord.DecodeNodeStatusRecord(r.Node, raw)
	if err != nil {
		t.Fatalf("decode the status record: %v", err)
	}
	return got
}

// A NODE'S MCP, MODEL AND RUNNER ROWS AND WHAT IT RUNS ON LEAVE ITS PRESENCE
// LEASE FOR ITS STATUS RECORD. The presence carries its placement half and
// live core, nothing a company's configuration can grow: no `mcp`, `models` or
// `runners` in its status and no `infra` beside it. The record, written by the
// node's own writer at its presence's epoch, carries a row per declared model
// entry and the infra summary its file gives.
//
// AND THE ENGINE RUNS THAT WRITER: the presence's claim wakes it
// ([Engine.statusRecordClaimed]), its own loop writes the record (the case
// waits for it rather than driving a pass), and a clean stop removes it.
// TestTheStatusRecordFollowsChangesAndOutlivesNoReconcile holds the schedule. Mutations: start no loop, wire no claim hook, or
// remove nothing at the stop.
func TestANodesStatusRowsLeaveItsPresenceLease(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			e.Stop(context.Background())
		}
	})
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	node := e.nodeID()
	presence, err := back.Coord.Get(t.Context(), coord.NodeResource(node))
	if err != nil || presence == nil {
		t.Fatalf("read the node's presence: %v, %v", presence, err)
	}
	status, ok := presence.Meta[coord.StatusKey].(map[string]any)
	if !ok {
		t.Fatalf("the presence carries no status (%v): the case reads nothing of what it left", presence.Meta)
	}
	for _, key := range []string{"mcp", "models", "runners"} {
		if _, on := status[key]; on {
			t.Errorf("the presence's status carries %q: it is re-sent on every beat, and its rows "+
				"grow with the company's configuration", key)
		}
	}
	for _, key := range []string{"infra", "mcp", "models", "runners"} {
		if _, on := presence.Meta[key]; on {
			t.Errorf("the presence carries %q beside its status", key)
		}
	}

	e.statusRecords.mu.Lock()
	w := e.statusRecords.writer
	e.statusRecords.mu.Unlock()
	if w == nil {
		t.Fatal("a started node runs no status record writer")
	}
	// THE CLAIM WOKE THE WRITER: the host claims the presence inside Start,
	// and the hook is told on that claim, so by Start's return it has been.
	if w.woken.Load() == 0 {
		t.Error("the presence was claimed and the status record's writer was never woken: " +
			"the record waits a renew period after every boot")
	}
	live := func() (coord.NodeStatusRecord, bool) {
		records, err := back.Fleet.NodeStatusRecords(t.Context())
		if err != nil {
			t.Fatalf("list the status records: %v", err)
		}
		leases, err := back.Coord.ListLive(t.Context(), coord.ClassNode)
		if err != nil {
			t.Fatalf("list the presence: %v", err)
		}
		r, counted := coord.LiveNodeStatuses(records, leases)[node]
		return r, counted
	}
	var record coord.NodeStatusRecord
	waitFor(t, "the node's own writer to write its record at its presence's epoch", func() bool {
		var counted bool
		record, counted = live()
		return counted
	})
	var keys []string
	for _, m := range record.Models {
		keys = append(keys, m.Key)
	}
	if !slices.Equal(keys, []string{"zulu"}) {
		t.Errorf("the record's model rows are %v, want the company's one entry", keys)
	}
	if want := NodeInfra(e.boot); record.Infra.Meta() == nil || !slices.Equal(record.Infra.BrokerServers, want.BrokerServers) ||
		record.Infra.ObjectsBackend != want.ObjectsBackend || record.Infra.Address != want.Address {
		t.Errorf("the record says it runs on %+v, want what its file says, %+v", record.Infra, want)
	}

	// A CLEAN STOP REMOVES IT, rather than leaving it for the age.
	stopped = true
	e.Stop(context.Background())
	records, err := back.Fleet.NodeStatusRecords(t.Context())
	if err != nil {
		t.Fatalf("list the status records after the stop: %v", err)
	}
	for _, r := range records {
		if r.Node == node {
			t.Errorf("a node that stopped cleanly left its status record %+v", r)
		}
	}
}

// writes is a status record store that records each write at the instant it
// was made, and refuses them while refuse is set.
type writes struct {
	*coordmemory.Fleet

	mu     sync.Mutex
	at     []time.Time
	got    []coord.NodeStatusRecord
	refuse bool
}

func (w *writes) PutNodeStatus(ctx context.Context, r coord.NodeStatusRecord) error {
	w.mu.Lock()
	refuse := w.refuse
	if !refuse {
		w.at, w.got = append(w.at, time.Now()), append(w.got, r)
	}
	w.mu.Unlock()
	if refuse {
		return errors.New("the coordination store did not answer")
	}
	return w.Fleet.PutNodeStatus(ctx, r)
}

// made is each write so far and when it was made.
func (w *writes) made() ([]time.Time, []coord.NodeStatusRecord) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.at), slices.Clone(w.got)
}

// A NODE'S STATUS RECORD FOLLOWS WHAT IT SAYS, AND NEVER AGES OUT WHILE IT
// LIVES. On a bubble's clock, at the floor lease's renew period (4 s):
//   - a change other than a model's last answer is written within a period;
//   - a last answer alone waits for the reconcile;
//   - the record is rewritten at least every [coord.ReconcileInterval], so no
//     gap passes the bucket's age, [coord.StatusFreshness];
//   - a new presence epoch is written on the pass that sees it, at once on a
//     claim; a node holding no presence writes nothing;
//   - a refused write is made again on the next pass; a clean stop removes it.
//
// Mutations: skip the reconcile rewrite, compare the last answer too, drop the
// epoch from the fingerprint, or let a claim wake nothing.
func TestTheStatusRecordFollowsChangesAndOutlivesNoReconcile(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const period = 4 * time.Second
		store := &writes{Fleet: coordmemory.NewFleet()}
		// since is the writes made at or after from, and last when the
		// latest was; longestGap the longest the record went unwritten
		// between from and until, counted from the last write before from
		// where there is one, so a span with no write at all is its whole
		// length and never a gap of nothing.
		since := func(from time.Time) []coord.NodeStatusRecord {
			at, got := store.made()
			var out []coord.NodeStatusRecord
			for i := range at {
				if !at[i].Before(from) {
					out = append(out, got[i])
				}
			}
			return out
		}
		last := func() time.Time {
			at, _ := store.made()
			return at[len(at)-1]
		}
		longestGap := func(from, until time.Time) time.Duration {
			at, _ := store.made()
			var longest time.Duration
			var prev time.Time
			for _, when := range at {
				if when.Before(from) {
					prev = when
					continue
				}
				if !prev.IsZero() {
					longest = max(longest, when.Sub(prev))
				}
				prev = when
			}
			if !prev.IsZero() {
				longest = max(longest, until.Sub(prev))
			}
			return longest
		}
		var mu sync.Mutex
		epoch, held := int64(3), true
		rows := coord.NodeStatusRecord{Models: []coord.ModelStatus{{Key: "zulu", SignIn: "token"}}}
		set := func(f func()) {
			mu.Lock()
			defer mu.Unlock()
			f()
		}
		w := newStatusRecordWriter(store, "node-a",
			func() (int64, bool) {
				mu.Lock()
				defer mu.Unlock()
				return epoch, held
			},
			func(context.Context) coord.NodeStatusRecord {
				mu.Lock()
				defer mu.Unlock()
				r := rows
				r.Models = slices.Clone(rows.Models)
				return r
			}, period, time.Now)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			w.run(ctx)
		}()
		synctest.Wait()
		start := time.Now()
		if got := since(start); len(got) != 1 || got[0].Epoch != 3 || got[0].Node != "node-a" {
			t.Fatalf("the first pass wrote %+v, want node-a's record at epoch 3", got)
		}

		// A SIT-OUT is a change: written within a renew period.
		time.Sleep(time.Second)
		changed := time.Now()
		set(func() {
			rows.Models[0].SittingOut = &coord.ModelSitOut{Until: changed.Add(time.Hour), Window: "session"}
		})
		time.Sleep(period)
		synctest.Wait()
		if got := since(changed); len(got) != 1 || got[0].Models[0].SittingOut == nil {
			t.Errorf("a renew period after a sit-out, the writes are %+v, want one carrying it", got)
		}

		// A LAST ANSWER ALONE waits for the reconcile: no pass before it
		// writes, and the reconcile's write carries it.
		answered := time.Now()
		set(func() { rows.Models[0].LastAnswer = answered })
		time.Sleep(time.Until(last().Add(coord.ReconcileInterval)) - time.Millisecond)
		synctest.Wait()
		if got := since(answered); len(got) != 0 {
			t.Errorf("a last answer alone was written before the reconcile: %+v", got)
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		if got := since(answered); len(got) != 1 || !got[0].Models[0].LastAnswer.Equal(answered) {
			t.Errorf("the reconcile wrote %+v, want one write carrying the last answer", got)
		}

		// NOTHING MOVES FOR A MINUTE: rewritten at least every reconcile
		// interval, so never older than the bucket's age.
		quiet := time.Now()
		time.Sleep(time.Minute)
		synctest.Wait()
		if gap := longestGap(quiet, time.Now()); gap > coord.ReconcileInterval || gap >= coord.StatusFreshness {
			t.Errorf("over a quiet minute the record went %v unwritten, want at most %v (the bucket ages "+
				"a record out at %v)", gap, coord.ReconcileInterval, coord.StatusFreshness)
		}

		// A NEW PRESENCE EPOCH is written on the pass that sees it, with
		// nothing else moved and the reconcile not yet due.
		time.Sleep(time.Second)
		moved := time.Now()
		set(func() { epoch = 4 })
		time.Sleep(period)
		synctest.Wait()
		if got := since(moved); len(got) != 1 || got[0].Epoch != 4 {
			t.Errorf("a renew period after the epoch moved, the writes are %+v, want one at epoch 4", got)
		}

		// A CLAIM AT A NEW EPOCH WAKES THE LOOP: written at the instant
		// of the claim, with no tick of the bubble's clock between.
		time.Sleep(time.Second)
		woke := time.Now()
		set(func() { epoch = 5 })
		w.claimed()
		synctest.Wait()
		if got := since(woke); len(got) != 1 || got[0].Epoch != 5 || !last().Equal(woke) {
			t.Errorf("a claim at epoch 5 wrote %+v, want one write at it at the claim's instant", got)
		}

		// NO PRESENCE, NO WRITE, however long; and a refused write is
		// made again on the next pass.
		time.Sleep(time.Second)
		set(func() { held = false })
		gone := time.Now()
		time.Sleep(2 * coord.ReconcileInterval)
		synctest.Wait()
		if got := since(gone); len(got) != 0 {
			t.Errorf("a node holding no presence wrote %+v", got)
		}
		store.mu.Lock()
		store.refuse = true
		store.mu.Unlock()
		set(func() { held, epoch = true, 6 })
		time.Sleep(period)
		synctest.Wait()
		store.mu.Lock()
		store.refuse = false
		store.mu.Unlock()
		retried := time.Now()
		time.Sleep(period)
		synctest.Wait()
		if got := since(retried); len(got) != 1 || got[0].Epoch != 6 {
			t.Errorf("after a refused write, the next pass wrote %+v, want the record at epoch 6", got)
		}

		// A CLEAN STOP removes the record.
		cancel()
		<-done
		w.remove(t.Context())
		if left, err := store.NodeStatusRecords(t.Context()); err != nil || len(left) != 0 {
			t.Errorf("after a clean stop the store holds %+v (%v), want nothing", left, err)
		}
	})
}

// A PEER'S SIT-OUT IS READ OFF ITS STATUS RECORD, AT ITS PRESENCE'S EPOCH
// ([Engine.fleetSitsOut]): a live peer's record holds this node's retry of a
// model, and one its earlier incarnation left holds nothing, since the
// process that met the limit is gone.
//
// Mutation: read the records without the presence join, and the stale record
// holds the retry.
func TestAFleetSitOutIsReadOffALivePeersRecord(t *testing.T) {
	t.Parallel()
	leases, fleet := coordmemory.New(), coordmemory.NewFleet()
	e := &Engine{backends: &Backends{Coord: leases, Fleet: fleet}, testNode: "node-a"}
	claim := func(owner string) int64 {
		t.Helper()
		l, _, err := leases.TryAcquire(t.Context(), coord.NodeResource("node-b"), coord.AcquireOptions{
			Owner: owner, TTL: time.Minute,
		})
		if err != nil || l == nil {
			t.Fatalf("claim node-b's presence: %v, %v", l, err)
		}
		return l.Epoch
	}
	first := claim("node-b:1")
	if err := fleet.PutNodeStatus(t.Context(), coord.NodeStatusRecord{Node: "node-b", Epoch: first,
		Models: []coord.ModelStatus{{Key: "zulu", SittingOut: &coord.ModelSitOut{
			Until: time.Now().Add(time.Hour), Window: "session"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if out, err := e.fleetSitsOut(t.Context(), "zulu"); err != nil || !out {
		t.Fatalf("fleetSitsOut = %v, %v with node-b's live record sitting zulu out, want true", out, err)
	}
	if out, err := e.fleetSitsOut(t.Context(), "alpha"); err != nil || out {
		t.Errorf("fleetSitsOut(alpha) = %v, %v, want false: nobody sits it out", out, err)
	}
	if ok, err := leases.Release(t.Context(), coord.NodeResource("node-b"), "node-b:1", first); err != nil || !ok {
		t.Fatalf("release node-b's presence: %v, %v", ok, err)
	}
	if again := claim("node-b:2"); again == first {
		t.Fatalf("node-b's presence came back at epoch %d: the stale arm reads nothing", first)
	}
	if out, err := e.fleetSitsOut(t.Context(), "zulu"); err != nil || out {
		t.Errorf("fleetSitsOut = %v, %v off node-b's earlier incarnation's record, want false", out, err)
	}
}
