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
	"github.com/crewlet/crewlet/internal/coord/coordtest"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// recordedRows is r as a peer reads it back off a store: through the bytes its
// rows are kept as, filed under a node and an epoch where r names none, as its
// writer stamps them.
func recordedRows(t *testing.T, r coord.NodeStatusRecord) coord.NodeStatusRecord {
	t.Helper()
	if r.Node == "" {
		r.Node = "node-a"
	}
	r.Epoch = max(r.Epoch, 1)
	rows, err := r.Rows()
	if err != nil {
		t.Fatalf("encode the status record: %v", err)
	}
	got := coord.AssembleNodeStatusRecords(rows, nil)
	if len(got) != 1 {
		t.Fatalf("the status record's rows assembled as %+v", got)
	}
	return got[0]
}

// everyPart reads every class of a node's status.
var everyPart = coord.NodeStatusRead{MCP: true, Models: true, Growth: true}

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
		records, err := back.Fleet.NodeStatusRecords(t.Context(), everyPart)
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
	records, err := back.Fleet.NodeStatusRecords(t.Context(), everyPart)
	if err != nil {
		t.Fatalf("list the status records after the stop: %v", err)
	}
	for _, r := range records {
		if r.Node == node {
			t.Errorf("a node that stopped cleanly left its status record %+v", r)
		}
	}
}

// writes is a status record store that records each row written, read back
// as the record it alone says, at the instant it was made, and refuses them
// while refuse is set.
type writes struct {
	*coordmemory.Fleet

	mu     sync.Mutex
	at     []time.Time
	got    []writtenAs
	refuse bool
}

// writtenAs is one row written: its class and the record it alone says, at
// its epoch.
type writtenAs struct {
	class coord.NodeStatusClass
	coord.NodeStatusRecord
}

func (w *writes) PutNodeStatusRow(ctx context.Context, row coord.NodeStatusRow) error {
	w.mu.Lock()
	refuse := w.refuse
	if !refuse {
		epoch, _ := coord.NodeStatusRowEpoch(row.Value)
		own, err := (coord.NodeStatusRecord{Node: row.Key.Node, Epoch: epoch}).Rows()
		read := coord.AssembleNodeStatusRecords(append(own, row), nil)
		if err != nil || len(read) != 1 {
			w.mu.Unlock()
			return errors.New("the row does not read back")
		}
		w.at, w.got = append(w.at, time.Now()), append(w.got, writtenAs{class: row.Key.Class, NodeStatusRecord: read[0]})
	}
	w.mu.Unlock()
	if refuse {
		return errors.New("the coordination store did not answer")
	}
	return w.Fleet.PutNodeStatusRow(ctx, row)
}

// made is each write so far and when it was made.
func (w *writes) made() ([]time.Time, []writtenAs) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.at), slices.Clone(w.got)
}

// A NODE'S STATUS RECORD FOLLOWS WHAT IT SAYS, ROW BY ROW, AND NEVER AGES OUT
// WHILE IT LIVES. On a bubble's clock at a 4 s renew period: a change other
// than a last answer rewrites its row alone within a period; a last answer
// waits for its row's reconcile; every row is rewritten at least every
// [coord.ReconcileInterval], inside [coord.StatusFreshness]; a new epoch
// rewrites every row, the own row last, at once on a claim; no presence, no
// write; a refused write is made again; a dropped entry's row is removed; a
// clean stop removes every row.
//
// Mutations: skip the reconcile rewrite, compare the last answer too, drop the
// epoch from the fingerprint, write the own row first, remove no row the
// record dropped, or let a claim wake nothing.
func TestTheStatusRecordFollowsChangesAndOutlivesNoReconcile(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		const period = 4 * time.Second
		store := &writes{Fleet: coordmemory.NewFleet()}
		// since is the writes made at or after from, and last when the
		// latest of a class was; longestGap the longest a class went
		// unwritten between from and until, counted from its last write
		// before from where there is one, so a span with no write at all is
		// its whole length and never a gap of nothing.
		since := func(from time.Time) []writtenAs {
			at, got := store.made()
			var out []writtenAs
			for i := range at {
				if !at[i].Before(from) {
					out = append(out, got[i])
				}
			}
			return out
		}
		last := func(class coord.NodeStatusClass) time.Time {
			at, got := store.made()
			for i := len(at) - 1; i >= 0; i-- {
				if got[i].class == class {
					return at[i]
				}
			}
			return time.Time{}
		}
		longestGap := func(class coord.NodeStatusClass, from, until time.Time) time.Duration {
			at, got := store.made()
			var longest time.Duration
			var prev time.Time
			for i, when := range at {
				if got[i].class != class {
					continue
				}
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
		// both reports whether writes are the model's row and then the
		// node's own, at epoch.
		both := func(writes []writtenAs, epoch int64) bool {
			return len(writes) == 2 && writes[0].class == coord.NodeStatusModel &&
				writes[1].class == coord.NodeStatusOwn && writes[0].Epoch == epoch && writes[1].Epoch == epoch
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
		if got := since(start); !both(got, 3) || got[1].Node != "node-a" {
			t.Fatalf("the first pass wrote %+v, want node-a's model row and then its own at epoch 3", got)
		}

		// A SIT-OUT is a change of the model's row: that row alone is
		// written within a renew period.
		time.Sleep(time.Second)
		changed := time.Now()
		set(func() {
			rows.Models[0].SittingOut = &coord.ModelSitOut{Until: changed.Add(time.Hour), Window: "session"}
		})
		time.Sleep(period)
		synctest.Wait()
		if got := since(changed); len(got) != 1 || got[0].class != coord.NodeStatusModel ||
			got[0].Models[0].SittingOut == nil {
			t.Errorf("a renew period after a sit-out, the writes are %+v, want the model's row alone carrying it",
				got)
		}

		// A LAST ANSWER ALONE waits for its row's reconcile: no pass before
		// it writes the row, and the reconcile's write carries it.
		answered := time.Now()
		set(func() { rows.Models[0].LastAnswer = answered })
		due := last(coord.NodeStatusModel).Add(coord.ReconcileInterval)
		time.Sleep(time.Until(due) - time.Millisecond)
		synctest.Wait()
		for _, got := range since(answered) {
			if got.class == coord.NodeStatusModel {
				t.Errorf("a last answer alone was written before its row's reconcile: %+v", got)
			}
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		var models []writtenAs
		for _, got := range since(due) {
			if got.class == coord.NodeStatusModel {
				models = append(models, got)
			}
		}
		if len(models) != 1 || !models[0].Models[0].LastAnswer.Equal(answered) {
			t.Errorf("the reconcile wrote %+v, want one write of the model's row carrying the last answer", models)
		}

		// NOTHING MOVES FOR A MINUTE: every row rewritten at least every
		// reconcile interval, so never older than the bucket's age.
		quiet := time.Now()
		time.Sleep(time.Minute)
		synctest.Wait()
		for _, class := range []coord.NodeStatusClass{coord.NodeStatusOwn, coord.NodeStatusModel} {
			if gap := longestGap(class, quiet, time.Now()); gap > coord.ReconcileInterval ||
				gap >= coord.StatusFreshness {
				t.Errorf("over a quiet minute the %s row went %v unwritten, want at most %v (the bucket ages "+
					"a row out at %v)", class, gap, coord.ReconcileInterval, coord.StatusFreshness)
			}
		}

		// A NEW PRESENCE EPOCH rewrites every row on the pass that sees it,
		// with nothing else moved and no reconcile due.
		time.Sleep(time.Second)
		moved := time.Now()
		set(func() { epoch = 4 })
		time.Sleep(period)
		synctest.Wait()
		if got := since(moved); !both(got, 4) {
			t.Errorf("a renew period after the epoch moved, the writes are %+v, want both rows at epoch 4", got)
		}

		// A CLAIM AT A NEW EPOCH WAKES THE LOOP: written at the instant
		// of the claim, with no tick of the bubble's clock between.
		time.Sleep(time.Second)
		woke := time.Now()
		set(func() { epoch = 5 })
		w.claimed()
		synctest.Wait()
		if got := since(woke); !both(got, 5) || !last(coord.NodeStatusOwn).Equal(woke) {
			t.Errorf("a claim at epoch 5 wrote %+v, want both rows at it at the claim's instant", got)
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
		if got := since(retried); !both(got, 6) {
			t.Errorf("after a refused write, the next pass wrote %+v, want both rows at epoch 6", got)
		}

		// AN ENTRY NO LONGER CONFIGURED has its row removed on the next
		// pass, and the node's own row stands.
		set(func() { rows.Models = nil })
		time.Sleep(period)
		synctest.Wait()
		if left, err := store.NodeStatusRecords(t.Context(), everyPart); err != nil || len(left) != 1 ||
			left[0].Models != nil || left[0].Epoch != 6 {
			t.Errorf("after the entry went, the store holds %+v (%v), want node-a's own row alone", left, err)
		}

		// A CLEAN STOP removes every row.
		cancel()
		<-done
		w.remove(t.Context())
		if left, err := store.NodeStatusRecords(t.Context(), everyPart); err != nil || len(left) != 0 {
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
	if err := coordtest.PutNodeStatus(t.Context(), fleet, coord.NodeStatusRecord{Node: "node-b", Epoch: first,
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

// landsUnanswered is a status record store whose write of one row lands but
// answers as failed, once: the answer a lost acknowledgement gives.
type landsUnanswered struct {
	*coordmemory.Fleet
	key  coord.NodeStatusRowKey
	lost bool
}

func (s *landsUnanswered) PutNodeStatusRow(ctx context.Context, row coord.NodeStatusRow) error {
	if err := s.Fleet.PutNodeStatusRow(ctx, row); err != nil {
		return err
	}
	if row.Key == s.key && !s.lost {
		s.lost = true
		return errors.New("the answer was lost")
	}
	return nil
}

// A ROW WHOSE WRITE MAY HAVE LANDED IS REMOVED ONCE IT IS NOT CARRIED: a new
// server's row lands but its write answers as failed, the server then leaves
// the configuration, and the next passes remove what landed, so the node never
// reads as running a server it dropped.
//
// Mutation: track a row only once its write answers, and beta is still read
// on node-a after three passes.
func TestARowWhoseWriteLostItsAnswerIsRemovedOnceDropped(t *testing.T) {
	t.Parallel()
	beta := coord.NodeStatusRowKey{Class: coord.NodeStatusMCP, Node: "node-a", Name: "beta"}
	store := &landsUnanswered{Fleet: coordmemory.NewFleet(), key: beta}
	servers := []coord.MCPServerStatus{{Server: "alpha", Started: 1}}
	w := newStatusRecordWriter(store, "node-a", func() (int64, bool) { return 3, true },
		func(context.Context) coord.NodeStatusRecord { return coord.NodeStatusRecord{MCP: servers} },
		time.Second, time.Now)
	if !w.pass(t.Context()) {
		t.Fatal("the first pass did not answer")
	}
	servers = append(servers, coord.MCPServerStatus{Server: "beta", Started: 1})
	if w.pass(t.Context()) {
		t.Fatal("premise: the pass whose write lost its answer reported it written")
	}
	servers = servers[:1]
	for range 3 {
		if !w.pass(t.Context()) {
			t.Fatal("a pass after beta was dropped did not answer")
		}
	}
	got, err := store.NodeStatusRecords(t.Context(), everyPart)
	if err != nil || len(got) != 1 || len(got[0].MCP) != 1 || got[0].MCP[0].Server != "alpha" {
		t.Errorf("after beta was dropped the store reads %+v (%v), want node-a with alpha alone", got, err)
	}
}
