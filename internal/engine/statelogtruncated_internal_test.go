package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// publishLoaded boots a node, waits for its tracker to stand at the checkpoint
// want — the position it publishes, its rows' own — publishes, and stops it.
func publishLoaded(t *testing.T, d divergedBroker, want statelog.Position) {
	t.Helper()
	e, back := bootNode(t, &d.a, d.cfg)
	running := e.core.Load().log.Domain(worklet.Domain{}.Name())
	waitUntil(t, 10*time.Second, "node A's tracker to load its checkpoint", func() bool {
		return running.runner.Committed() == want
	})
	e.core.Load().log.publishPositions(t.Context())
	e.Stop(context.Background())
	back.Close(context.Background())
}

// requireTruncatedRefusal asserts a write refused `log_truncated` over the
// truncation, naming the peer.
func requireTruncatedRefusal(t *testing.T, err error, peer string) {
	t.Helper()
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) || refused.Reason != statelog.ReasonLogTruncated ||
		!errors.Is(err, statelog.ErrLogTruncated) {
		t.Fatalf("the write returned %v, want log_truncated over the truncation", err)
	}
	if !strings.Contains(err.Error(), peer) {
		t.Fatalf("the refusal %q does not name the peer %s", err, peer)
	}
}

// A NODE WHOSE PEER STANDS PAST A RESTORED LOG'S END REFUSES ITS WRITES UNTIL
// THE OPERATOR DECIDES — AND THE PEER'S EVICTION IS ONE WAY TO DECIDE.
//
// Node B's rows are the restored copy's age, so its own log looks consistent,
// yet a reanchor of node A (whose rows hold the tail the copy lost) would
// discard every write B made. B reads A's position past the log's end off the
// register, confirms the log holds no record there, and refuses writes as
// `log_truncated` naming A; reads go on. Evicting A is the one write it still
// makes, and with A evicted its writes are served again.
func TestANodeWhosePeerStandsPastARestoredLogRefusesItsWrites(t *testing.T) {
	t.Parallel()
	d := stageRestoredBroker(t)
	publishLoaded(t, d, d.checkpoint.at)

	eb, _ := bootNode(t, &d.b, d.cfg)
	waitUntil(t, 20*time.Second, "node B to admit seats", hydrated(t, eb))
	running := eb.core.Load().log.Domain(worklet.Domain{}.Name())
	eb.core.Load().log.publishPositions(t.Context())
	if err := running.runner.Truncated(); !errors.Is(err, statelog.ErrLogTruncated) {
		t.Fatalf("node B's truncation is %v, want node A's position past the end", err)
	}
	if err := running.runner.StreamIdentity(); err != nil {
		t.Fatalf("node B's reads refuse over a peer's rows: %v", err)
	}
	title := "refused"
	_, err := eb.native.Load().writer.UpdateTask(t.Context(), "op-b-refused", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Title: &title}, worklet.ChangeFields, nil)
	requireTruncatedRefusal(t, err, d.a.Node.ID)

	// THE OPERATOR EVICTS A, from B.
	res, err := eb.NodeGate().Evict(t.Context(), GateRequest{
		Node: d.a.Node.ID, OpID: statelog.NewOpID(time.Now(), "evict-a"), By: gateOperator,
	})
	if err != nil || !res.Complete() {
		t.Fatalf("the eviction of node A from node B = %+v, %v — it is one of the "+
			"operator's ways out, and a fence that refused it would take it away", res, err)
	}
	eb.core.Load().log.publishPositions(t.Context())
	if err := running.runner.Truncated(); err != nil {
		t.Fatalf("with node A evicted, node B's writes still refuse: %v", err)
	}
	mustApply(t, "node B's write once node A is evicted", func() (worklet.WriteResult, error) {
		title := "after the eviction"
		return eb.native.Load().writer.UpdateTask(t.Context(), "op-b-after", "t-1", "ENG",
			worklet.NoIfMatch, worklet.TaskPatch{Title: &title}, worklet.ChangeFields, nil)
	})
}

// A NODE WHOSE PEER SAYS THE LOG DIVERGED FROM ITS ROWS REFUSES ITS WRITES TOO.
//
// Once the restored log is written past A's checkpoint, A's position sits at
// or below the log's end like a lagging node's, so the flag A publishes is all
// that tells B a further write is one the operator's choice would discard.
func TestANodeWhosePeerSaysTheLogDivergedRefusesItsWrites(t *testing.T) {
	t.Parallel()
	d := stageDivergedBroker(t)
	publishLoaded(t, d, d.checkpoint.at)

	eb, _ := bootNode(t, &d.b, d.cfg)
	waitUntil(t, 20*time.Second, "node B to admit seats", hydrated(t, eb))
	running := eb.core.Load().log.Domain(worklet.Domain{}.Name())
	eb.core.Load().log.publishPositions(t.Context())
	err := running.runner.Truncated()
	if !errors.Is(err, statelog.ErrLogTruncated) || !strings.Contains(err.Error(), "another record") {
		t.Fatalf("node B's truncation is %v, want node A's divergence", err)
	}
	title := "refused"
	_, err = eb.native.Load().writer.UpdateTask(t.Context(), "op-b-refused", "t-1", "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{Title: &title}, worklet.ChangeFields, nil)
	requireTruncatedRefusal(t, err, d.a.Node.ID)
}

// WHICH PEERS COUNT, ASKED OF THE REGISTER ROWS DIRECTLY.
//
// Only a peer on this node's keyed stream, in its generation, and either past
// the log's end (confirmed against the log, since a lagging member's end is
// below a healthy peer's position too) or flagging divergence. A position in
// another space says nothing about this log, and a record at the peer's
// checkpoint means the end was a stale member's.
func TestWhichPeersHoldWhatTheLogLost(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNode(t)
	s := e.core.Load().log
	running := s.Domain(worklet.Domain{}.Name())
	for i := range 3 {
		if res, err := e.native.Load().writer.EvictNode(t.Context(), fmt.Sprintf("op-%d", i),
			fmt.Sprintf("gone-%d", i)); err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("a write: %+v, %v", res, err)
		}
	}
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	at := running.runner.Committed()
	end := stats.LastSeq
	keyed := running.runner.KeyedTo()
	peer := func(seq uint64, mutate func(*coord.DomainPosition)) []coord.NodePositions {
		pos := coord.DomainPosition{Seq: seq, Generation: at.Generation, StreamCreatedAt: keyed}
		if mutate != nil {
			mutate(&pos)
		}
		return []coord.NodePositions{{
			NodeID:  "node-peer",
			Domains: map[string]coord.DomainPosition{worklet.Domain{}.Name(): pos},
		}}
	}
	for name, tc := range map[string]struct {
		rows  []coord.NodePositions
		last  uint64
		wants bool
	}{
		"a peer past the log's end": {rows: peer(end+5, nil), last: end, wants: true},
		"a peer past a stale member's end, on a record the log holds": {
			rows: peer(end, nil), last: end - 1,
		},
		"a peer at the end": {rows: peer(end, nil), last: end},
		"a peer saying the log diverged from its rows": {
			rows: peer(end-1, func(p *coord.DomainPosition) { p.LogDiverged = true }),
			last: end, wants: true,
		},
		"a peer past the end in another generation": {
			rows: peer(end+5, func(p *coord.DomainPosition) { p.Generation++ }), last: end,
		},
		"a peer past the end on another stream": {
			rows: peer(end+5, func(p *coord.DomainPosition) {
				p.StreamCreatedAt = keyed.Add(time.Hour)
			}),
			last: end,
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := s.truncation(t.Context(), running, tc.rows, at, tc.last)
			if err != nil {
				t.Fatalf("truncation: %v", err)
			}
			if (got != nil) != tc.wants {
				t.Fatalf("truncation = %+v, want a finding: %v", got, tc.wants)
			}
			if got != nil && got.Peer != "node-peer" {
				t.Fatalf("the finding names %q, want node-peer", got.Peer)
			}
		})
	}
}

// wrappedFleet is the coordination store a test wraps, under a name of its own:
// embedded as coord.Fleet, the field would take the name of a method the
// interface declares.
type wrappedFleet = coord.Fleet

// heldFleet is a coordination store whose NEXT write of one node's row at one
// tracker generation is held until released — a heartbeat that read the
// runners and has not yet written — and which records the tracker generation
// of every row that node's writes carried, in order.
type heldFleet struct {
	wrappedFleet

	mu      sync.Mutex
	node    string
	hold    uint32
	armed   bool
	blocked chan struct{}
	release chan struct{}
	written []uint32
}

func (f *heldFleet) arm(node string, gen uint32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.node, f.hold, f.armed = node, gen, true
	f.blocked, f.release = make(chan struct{}), make(chan struct{})
}

func (f *heldFleet) PutPositions(ctx context.Context, row coord.NodePositions) error {
	gen := row.Domains[worklet.Domain{}.Name()].Generation
	f.mu.Lock()
	if f.node == "" || row.NodeID != f.node {
		f.mu.Unlock()
		return f.wrappedFleet.PutPositions(ctx, row)
	}
	hold := f.armed && gen == f.hold
	release := f.release
	if hold {
		f.armed = false
		close(f.blocked)
	}
	f.mu.Unlock()
	if hold {
		<-release
	}
	err := f.wrappedFleet.PutPositions(ctx, row)
	f.mu.Lock()
	f.written = append(f.written, gen)
	f.mu.Unlock()
	return err
}

func (f *heldFleet) last() (uint32, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.written) == 0 {
		return 0, 0
	}
	return f.written[len(f.written)-1], len(f.written)
}

// A HEARTBEAT IN FLIGHT CANNOT PUT A REANCHORED NODE'S OLD GENERATION BACK.
//
// A beat reads the runners then writes this node's row, a reanchor publishes
// the row once its transition commits, and the register keeps the LAST write.
// A beat that read before the re-key and wrote after the reanchor's publish
// would restore the old generation, which re-anchored peers would serve until
// the next beat; so the publishes are serialized, and the last row written is
// read after the reanchor.
func TestAHeartbeatInFlightCannotPutAReanchoredNodesOldGenerationBack(t *testing.T) {
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
	fleet := &heldFleet{wrappedFleet: back.Fleet}
	back.Fleet = fleet
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	js := jetStreamOn(t, back)

	s := e.core.Load().log
	running := s.Domain(worklet.Domain{}.Name())
	spec := running.spec
	if res, err := e.native.Load().writer.EvictNode(t.Context(), "op-before", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write before the rebuild: %+v, %v", res, err)
	}
	gen := running.runner.Committed().Generation
	rebuildLog(t, js, spec)
	view, err := e.ReanchorStatus(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("ReanchorStatus: %v", err)
	}

	// A BEAT IN FLIGHT: it has read the tracker at the old generation and is
	// held at its write.
	fleet.arm(e.core.Load().nodeID, gen)
	beat := make(chan struct{})
	go func() { defer close(beat); s.publishPositions(context.WithoutCancel(t.Context())) }()
	select {
	case <-fleet.blocked:
	case <-time.After(20 * time.Second):
		t.Fatal("no beat reached its write")
	}

	// THE REANCHOR, beside it.
	done := make(chan error, 1)
	go func() {
		_, err := e.Reanchor(context.WithoutCancel(t.Context()), ReanchorRequest{
			Stream: spec.Name, Confirm: statelog.ConfirmationOf(view.CreatedAt), By: reanchorOperator,
		})
		done <- err
	}()
	waitUntil(t, 20*time.Second, "the reanchor to re-key the tracker", func() bool {
		return running.runner.Committed().Generation > gen
	})
	// WHATEVER THE REANCHOR WOULD PUBLISH ON ITS OWN, it has had the time to.
	time.Sleep(300 * time.Millisecond)
	close(fleet.release)
	if err := <-done; err != nil {
		t.Fatalf("Reanchor: %v", err)
	}
	<-beat
	if last, n := fleet.last(); last <= gen {
		t.Fatalf("after %d writes this node's row names generation %d, want the "+
			"reanchored %d — the beat that read the tracker before the reanchor "+
			"wrote after it", n, last, running.runner.Committed().Generation)
	}
}

// floorlessFleet is a coordination store whose trim floors cannot be read
// while failing is set — the half of a beat's reading that establishes which
// generation the fleet is on, failing while the register itself answers.
type floorlessFleet struct {
	wrappedFleet
	failing atomic.Bool
}

func (f *floorlessFleet) Floor(ctx context.Context, domain string) (coord.TrimFloor, bool, error) {
	if f.failing.Load() {
		return coord.TrimFloor{}, false, errors.New("the trim floors are unreadable")
	}
	return f.wrappedFleet.Floor(ctx, domain)
}

// THE TRUNCATION FENCE DOES NOT LIFT BEFORE THE PASSED VERDICT REPLACES IT.
//
// The peer holding what the log lost often re-anchors past it, and its row
// moving to the next generation takes it out of the truncation's comparison.
// Judging the truncation on a beat that could not read the fleet's
// generations would lift the fence with no passed verdict set, serving writes
// at a generation the fleet left; both are judged from one successful reading
// or not at all.
func TestTheTruncationFenceStaysUntilThePassedVerdictReplacesIt(t *testing.T) {
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
	register := back.Fleet
	fleet := &floorlessFleet{wrappedFleet: register}
	back.Fleet = fleet
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))

	s := e.core.Load().log
	running := s.Domain(worklet.Domain{}.Name())
	if res, err := e.native.Load().writer.EvictNode(t.Context(), "op-before", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("a write: %+v, %v", res, err)
	}
	stats, err := running.log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the log: %v", err)
	}
	at := running.runner.Committed()
	peerAt := func(gen uint32) {
		t.Helper()
		if err := register.PutPositions(t.Context(), coord.NodePositions{
			NodeID: "node-peer",
			Domains: map[string]coord.DomainPosition{worklet.Domain{}.Name(): {
				Seq: stats.LastSeq + 5, Generation: gen,
				StreamCreatedAt: running.runner.KeyedTo(),
			}},
		}); err != nil {
			t.Fatalf("publish the peer's row: %v", err)
		}
	}

	// A PEER'S ROWS HOLD WHAT THE LOG LOST: the fence goes up.
	peerAt(at.Generation)
	s.publishPositions(t.Context())
	if err := running.runner.Truncated(); !errors.Is(err, statelog.ErrLogTruncated) {
		t.Fatalf("the truncation is %v, want the peer's position past the end", err)
	}

	// THE PEER RE-ANCHORS PAST THIS NODE, on a beat whose generations cannot
	// be read.
	fleet.failing.Store(true)
	peerAt(at.Generation + 1)
	s.publishPositions(t.Context())
	if err := running.runner.Truncated(); !errors.Is(err, statelog.ErrLogTruncated) {
		t.Fatalf("on a beat whose generations were unread the truncation is %v — "+
			"the fence lifted with nothing to replace it", err)
	}
	if err := running.runner.StreamIdentity(); err != nil {
		t.Fatalf("the passed verdict was set on a beat that could not read the "+
			"fleet's generations: %v", err)
	}

	// ONE READING, BOTH VERDICTS: the passed verdict goes up as the fence
	// comes down.
	fleet.failing.Store(false)
	s.publishPositions(t.Context())
	if err := running.runner.StreamIdentity(); !errors.Is(err, statelog.ErrGenerationPassed) {
		t.Fatalf("the reads refuse with %v, want the peer's reanchor past this node", err)
	}
	if err := running.runner.Truncated(); err != nil {
		t.Fatalf("with the peer judged in its own generation the truncation still "+
			"refuses: %v", err)
	}
}
