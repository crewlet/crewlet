package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE APPLIER IS HANDED THE STREAM'S STAMPED IDENTITY, and a stream recreated
// between two boots stops it.
//
// The identity is the instant [statelog.StreamCreatedAt] reads: the create's
// stamp in the stream's configuration ([statelog.CreatedAtKey]), which a
// snapshot restore hands back — never the broker's own instant, which a
// restore moves (ADR-0083).
//
// An engine test, not only a framework one: the detector is one comparison,
// and what breaks is a caller handing it the wrong instant (one read back out
// of the cursor row compares the row with itself). So this boots the engine
// over its own broker and checks both halves.
func TestTheApplierIsHandedTheStreamsStampedIdentity(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	stream := worklet.Domain{}.Stream().Name

	// FIRST BOOT: the running domain carries the stream's stamped identity.
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		back.Close(context.Background())
		t.Fatalf("New: %v", err)
	}
	q, ok := back.Queue.(*jetstream.Queue)
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", back.Queue)
	}
	log, err := q.DomainLog(t.Context(), stream)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	stats, err := log.Stats(t.Context())
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if stats.CreatedAt.IsZero() {
		t.Fatal("the log reports no creation instant, so nothing below can be checked")
	}
	// THE STAMP, off the stream's own configuration: the log's identity is
	// that instant, which a restore hands back, not the broker's.
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	made, err := js.Stream(t.Context(), stream)
	if err != nil {
		t.Fatalf("read the stream the engine made: %v", err)
	}
	stamp, stamped := made.CachedInfo().Config.Metadata[statelog.CreatedAtKey]
	if !stamped {
		t.Fatalf("the engine made %s with no %s in its configuration — its "+
			"identity is then the broker's instant, which a restore moves",
			stream, statelog.CreatedAtKey)
	}
	if got := stats.CreatedAt.UTC().Format(time.RFC3339Nano); got != stamp {
		t.Fatalf("the log's identity is %s and its stamp %s", got, stamp)
	}
	running := e.core.Load().log.Domain(worklet.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	if !running.runner.StreamCreatedAt().Equal(stats.CreatedAt.UTC()) {
		t.Fatalf("the running domain carries %s and the stream's stamped identity "+
			"is %s — the detector compares the checkpoint against this, and a "+
			"value read back out of the checkpoint detects nothing",
			running.runner.StreamCreatedAt(), stats.CreatedAt)
	}
	// A checkpoint under that identity, as the applier commits one, for the
	// second boot to compare.
	if err := back.Store.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at, applied_version)
			VALUES (?, 0, 0, ?, ?, 0)
			ON CONFLICT (stream) DO UPDATE SET stream_created_at = excluded.stream_created_at`,
			stream, store.EncodeTime(stats.CreatedAt.UTC()), store.EncodeTime(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("commit a checkpoint: %v", err)
	}
	e.Stop(context.Background())

	// THE STREAM IS LOST, and the second boot provisions it again under the
	// same name with a new stamp: the recreation the identity detects (as is
	// a hand rebuild with no stamp). A snapshot restore is not one: it hands
	// back the recorded configuration, stamp included, and every sequence.
	if err := js.DeleteStream(t.Context(), stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	back.Close(context.Background())
	// The stamp has nanoseconds and the row microseconds: a second boot in
	// the same microsecond would compare equal.
	time.Sleep(2 * time.Millisecond)

	// SECOND BOOT: the applier stops and says why.
	back2, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends again: %v", err)
	}
	t.Cleanup(func() { back2.Close(context.Background()) })
	e2, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back2})
	if err != nil {
		t.Fatalf("New again: %v", err)
	}
	t.Cleanup(func() { e2.Stop(context.Background()) })
	running = e2.core.Load().log.Domain(worklet.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running after the second boot")
	}
	deadline := time.Now().Add(10 * time.Second)
	var stopped error
	for time.Now().Before(deadline) {
		if stopped = running.runner.Stopped(); stopped != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !errors.Is(stopped, statelog.ErrStreamRecreated) {
		t.Fatalf("the applier on a recreated stream reports %v, want a stop naming "+
			"the recreation — it would otherwise resume at a checkpoint the new "+
			"stream has not reached, report nothing pending, and apply none of "+
			"the new stream's records", stopped)
	}
	// AND ITS WRITES REFUSE. The stop ends only the apply loop, and the
	// publisher clears its zero fence from the same checkpoint, so a write at
	// zero would land on the new stream as though it continued the old one.
	before, err := running.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the new stream's end: %v", err)
	}
	_, err = e2.native.Load().writer.EvictNode(t.Context(), "op-after-recreation", "node-x")
	requireRebuiltLogRefusal(t, err)
	if end, err := running.log.End(t.Context()); err != nil || end != before {
		t.Fatalf("the new stream ends at %d (err %v), want %d — a write from a "+
			"node whose checkpoint names another stream landed on it", end, err, before)
	}
	// AND THE OPERATOR SURFACE SAYS SO: a stopped applier's lag of zero alone
	// reads as caught up.
	for _, row := range e2.StateLogStatus(t.Context()) {
		if row.Name != (worklet.Domain{}).Name() {
			continue
		}
		if row.Ready || !strings.Contains(row.Detail, "recreated") {
			t.Fatalf("the tracker's status row is %+v, want not ready and naming "+
				"the recreation", row)
		}
	}
}

// AND A STREAM REBUILT UNDER A RUNNING NODE IS NAMED WHILE IT RUNS, not only
// at boot: the heartbeat reads the live creation instant from the answer it
// already fetches for the stream's bounds.
//
// The sequence terms cannot stand in for it: a rebuilt stream restarts at
// generation 0 from 1, so the checkpoint-past-the-end term reports it only
// until the new stream passes this node's position, after which every term
// reads healthy while the node applies a different history into rows keyed by
// the old one.
func TestAStreamRebuiltUnderARunningNodeIsNamed(t *testing.T) {
	t.Parallel()
	e, js := aRunningNode(t)
	s := e.core.Load().log
	running := s.Domain(worklet.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}

	// THE REBUILD, under a node that never stops: the same name, a new
	// creation instant, and sequences counting from 1 again.
	rebuildLog(t, js, worklet.Domain{}.Stream())

	// THE HEARTBEAT IS WHAT SEES IT, on the round trip it already makes.
	s.publishPositions(t.Context())

	health, err := s.health(t.Context(), running, s.readFloors(t.Context()))
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if !health.StreamRecreated {
		t.Fatal("the heartbeat did not name the rebuild — the live instant is " +
			"sampled once at start and never read again, so a node that stays " +
			"up applies a different history into rows keyed by the old one")
	}
	if code := health.Refusal(time.Now()); code != statelog.RefuseWrongStream {
		t.Errorf("a node on a rebuilt stream refuses with %q, want wrong_stream — "+
			"its rows are keyed to a history this log does not have", code)
	}
	if health.Healthy(time.Now(), statelog.DeferredSince{}) {
		t.Error("a node on a rebuilt stream is healthy, so it keeps its seats " +
			"and goes on deciding from rows nothing else in the fleet has")
	}
}

// A NODE WHOSE LOG WAS REBUILT UNDER IT REFUSES TO WRITE, as well as to read —
// on a real embedded stream, deleted and remade under an engine that stays up.
//
// Its expectations are sequences from the old history, which the rebuilt log
// (same name and generation, counting from 1) would arbitrate as its own, and
// every data node would apply what lands. Both shapes must refuse:
//
//   - A RETRY AT ZERO: the missing anchor reads as trimmed, and the zero fence
//     would clear it against the old history's floor and checkpoint.
//   - AN ORDINARY EXPECTATION the rebuilt log satisfies: its history on the
//     subject ends exactly at the sequence this node's row names.
func TestAStreamRebuiltUnderARunningNodeRefusesItsWrites(t *testing.T) {
	t.Parallel()
	e, js := aRunningNode(t)
	s := e.core.Load().log
	running := s.Domain(worklet.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	writer := e.native.Load().writer
	if writer == nil {
		t.Fatal("the node runs no tracker writer")
	}

	// TWO OBJECTS WITH HISTORY on the old log, node-x's record kept byte for
	// byte to replay onto the rebuilt log where this node's row says it is.
	anchors := map[string]uint64{}
	for _, node := range []string{"node-x", "node-y"} {
		res, err := writer.EvictNode(t.Context(), "op-evict-"+node, node)
		if err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Fatalf("the write before the rebuild: %+v, %v", res, err)
		}
		anchors[node] = res.Position.Seq
	}
	subjectX, recordX, _, held, err := running.log.At(t.Context(), anchors["node-x"])
	if err != nil || !held {
		t.Fatalf("read node-x's record back (held %v): %v", held, err)
	}

	rebuildLog(t, js, worklet.Domain{}.Stream())

	// THE RETRY AT ZERO, BEFORE ANY HEARTBEAT SEES THE REBUILD: the zero
	// fence's own read of the log carries the creation instant, so it refuses
	// within the call rather than at the next beat.
	_, err = writer.ReadmitNode(t.Context(), "op-readmit-y", "node-y")
	requireRebuiltLogRefusal(t, err)
	if end := endOf(t, running); end != 0 {
		t.Fatalf("the rebuilt log ends at %d, want 0 — a retry at zero cleared "+
			"against the old history's floor landed on it", end)
	}

	// THE ORDINARY EXPECTATION: node-x's history on the rebuilt log ends
	// exactly at the sequence this node's row names, so the broker accepts it.
	for endOf(t, running)+1 < anchors["node-x"] {
		barrierOn(t, running)
	}
	replayed, _, err := running.log.Append(t.Context(), subjectX, "", nil, recordX)
	if err != nil || replayed != anchors["node-x"] {
		t.Fatalf("the replayed record landed at %d (err %v), want %d — this case "+
			"needs the rebuilt log to satisfy the expectation exactly",
			replayed, err, anchors["node-x"])
	}
	_, err = writer.ReadmitNode(t.Context(), "op-readmit-x", "node-x")
	requireRebuiltLogRefusal(t, err)
	if end := endOf(t, running); end != replayed {
		t.Fatalf("the rebuilt log ends at %d, want %d — an expectation from the "+
			"old history was accepted by the new one", end, replayed)
	}

	// AND THE READS, FROM THE SAME ANSWER, and the operator surface — even
	// once the rebuilt log reaches this node's checkpoint and every sequence
	// term reads caught up.
	for endOf(t, running) < running.runner.Committed().Seq {
		barrierOn(t, running)
	}
	s.publishPositions(t.Context())
	health, err := s.health(t.Context(), running, s.readFloors(t.Context()))
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if code := health.Refusal(time.Now()); code != statelog.RefuseWrongStream {
		t.Errorf("reads on a rebuilt log refuse with %q, want %q", code,
			statelog.RefuseWrongStream)
	}
	for _, row := range e.StateLogStatus(t.Context()) {
		if row.Name != (worklet.Domain{}).Name() {
			continue
		}
		if row.Ready || !strings.Contains(row.Detail, "recreated") {
			t.Errorf("the tracker's status row is %+v, want not ready and naming "+
				"the recreation — caught up on a sequence the rebuilt log merely "+
				"reached is not caught up", row)
		}
	}
}

// A NODE WHOSE CHECKPOINT IS PAST THE LOG'S END REFUSES TO WRITE, as well as to
// read — on a real embedded stream that KEEPS its creation instant, as a
// broker restored from an older copy does: the shape the rebuild check above
// cannot see. Both shapes must refuse:
//
//   - AN ORDINARY EXPECTATION on a record the restored log holds lands at a
//     sequence below the checkpoint, which this node's applier never applies.
//   - AN EXPECTATION OF ZERO: a fence asking only whether the cursor is HIGH
//     ENOUGH passes one that is too high.
func TestACheckpointPastTheLogsEndRefusesTheNodesWrites(t *testing.T) {
	t.Parallel()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	stream := worklet.Domain{}.Stream().Name

	// FIRST BOOT: an object with history on the log, and the log's end.
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		back.Close(context.Background())
		t.Fatalf("New: %v", err)
	}
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	if res, err := e.native.Load().writer.EvictNode(t.Context(), "op-evict-x", "node-x"); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		e.Stop(context.Background())
		back.Close(context.Background())
		t.Fatalf("the write before the restore: %+v, %v", res, err)
	}
	end := endOf(t, e.core.Load().log.Domain(worklet.Domain{}.Name()))
	e.Stop(context.Background())

	// THE CHECKPOINT MOVES PAST THE LOG'S END and nothing else does: the row
	// keeps its instant, so by every identity check this is the stream it
	// started against — rows newer than the broker the node came back to.
	ahead := end + 5
	// Staged through the store, which still holds the replicated estate open
	// after e.Stop: a stopped state log leaves the file to it.
	if err := back.Store.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`UPDATE statelog_cursor SET seq = ? WHERE stream = ?`, ahead, stream)
		return err
	}); err != nil {
		back.Close(context.Background())
		t.Fatalf("stage the checkpoint: %v", err)
	}
	back.Close(context.Background())

	// SECOND BOOT.
	back2, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends again: %v", err)
	}
	t.Cleanup(func() { back2.Close(context.Background()) })
	e2, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back2})
	if err != nil {
		t.Fatalf("New again: %v", err)
	}
	t.Cleanup(func() { e2.Stop(context.Background()) })
	running := e2.core.Load().log.Domain(worklet.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running after the second boot")
	}

	// THE BOOT ESTABLISHED IT, before any heartbeat read the end: otherwise
	// an ordinary write has a whole interval to land.
	if identity := running.runner.StreamIdentity(); !errors.Is(identity, statelog.ErrAheadOfLog) {
		t.Fatalf("right after boot the runner's identity is %v, want %v",
			identity, statelog.ErrAheadOfLog)
	}
	if err := running.runner.Stopped(); err != nil {
		t.Fatalf("the applier stopped (%v) — the stream is the same stream, and "+
			"what refuses here is the write path, not the loop", err)
	}
	waitUntil(t, 10*time.Second, "the applier to load its checkpoint", func() bool {
		return running.runner.Committed().Seq == ahead
	})

	// AN ORDINARY EXPECTATION the log would accept: the restored log holds
	// node-x's last record at exactly the sequence this node's row names.
	_, err = e2.native.Load().writer.EvictNode(t.Context(), "op-evict-x-again", "node-x")
	requireAheadOfLogRefusal(t, err)
	if got := endOf(t, running); got != end {
		t.Fatalf("the log ends at %d, want %d — an ordinary write from a node "+
			"past the end landed at a sequence its own applier had passed", got, end)
	}

	// AN EXPECTATION OF ZERO, WITH NOTHING ESTABLISHED BEFORE IT: the verdict
	// is cleared as a reading at the checkpoint would, so only the zero
	// fence's own read of the log, within the call, can refuse this write.
	running.runner.ObserveEnd(statelog.Stance{}, ahead)
	if err := running.runner.StreamIdentity(); err != nil {
		t.Fatalf("the verdict did not clear for the zero case: %v", err)
	}
	_, err = e2.native.Load().writer.EvictNode(t.Context(), "op-evict-z", "node-z")
	requireAheadOfLogRefusal(t, err)
	if got := endOf(t, running); got != end {
		t.Fatalf("the log ends at %d, want %d — a retry at zero was cleared by a "+
			"fence that asked only whether the cursor was high enough", got, end)
	}

	// AND THE READS, FROM THE SAME FACT, and the operator surface with them.
	s2 := e2.core.Load().log
	health, err := s2.health(t.Context(), running, s2.readFloors(t.Context()))
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if code := health.Refusal(time.Now()); code != statelog.RefuseWrongStream {
		t.Errorf("reads past the log's end refuse with %q, want %q", code,
			statelog.RefuseWrongStream)
	}
	if health.StreamRecreated {
		t.Error("the health names a rebuild — this stream kept its instant, and " +
			"an operator told it was recreated looks for a delete that never happened")
	}
	for _, row := range e2.StateLogStatus(t.Context()) {
		if row.Name != (worklet.Domain{}).Name() {
			continue
		}
		if row.Ready || !strings.Contains(row.Detail, "past the log's end") {
			t.Errorf("the tracker's status row is %+v, want not ready and naming "+
				"the checkpoint past the end", row)
		}
	}
}

// THE BOOT HANDS THE APPLIER WHERE THE LOG ENDS, beside the checkpoint, before
// its loop or any write has run.
//
// Boot is when a broker restored from an older copy is met (the embedded one
// restarts with this process), and the first heartbeat is an interval away, so
// an ordinary write could arrive with nothing refusing it. Driven through
// [stateLog.start], which launches no loop or heartbeat, so only the boot's
// own reading can establish it.
func TestTheBootReadsTheLogsEndBesideTheCheckpoint(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		past uint64
		want error
	}{
		"control: a checkpoint at the log's end": {past: 0},
		"a checkpoint past the log's end":        {past: 5, want: statelog.ErrAheadOfLog},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s, q, appendTo := aProvisionedTrackerLog(t)
			// The loop never starts, so only where the log ends matters.
			subject := worklet.Domain{}.Stream().SubjectPrefix + ".probe.x"
			for i := range 2 {
				if _, _, err := appendTo.Append(t.Context(), subject,
					fmt.Sprintf("op-%d", i), nil, []byte("{}")); err != nil {
					t.Fatalf("append: %v", err)
				}
			}
			stats, err := appendTo.Stats(t.Context())
			if err != nil {
				t.Fatalf("stats: %v", err)
			}
			if err := s.db.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.ExecContext(t.Context(), `
					INSERT INTO statelog_cursor
						(stream, generation, seq, stream_created_at, updated_at, applied_version)
					VALUES (?, 0, ?, ?, ?, 0)`,
					worklet.Domain{}.Stream().Name, stats.LastSeq+tc.past,
					store.EncodeTime(stats.CreatedAt.UTC()),
					store.EncodeTime(time.Now().UTC()))
				return err
			}); err != nil {
				t.Fatalf("stage the checkpoint: %v", err)
			}

			running, err := s.start(t.Context(), t.Context(), q, worklet.Domain{}, appendTo)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			identity := running.runner.StreamIdentity()
			if tc.want == nil {
				if identity != nil {
					t.Fatalf("a node at the log's end reads as %v — it would refuse "+
						"every write it makes", identity)
				}
				return
			}
			if !errors.Is(identity, tc.want) {
				t.Fatalf("after boot, before any loop or heartbeat, the identity is "+
					"%v, want %v — every ordinary write would land until a later "+
					"reading caught up", identity, tc.want)
			}
		})
	}
}

// aProvisionedTrackerLog is the tracker's log provisioned on a fresh embedded
// broker, over a fresh store, by a state log that has started nothing.
//
// The broker is sized ([brokertest.StoreMaxBytes]) because the ceilings divide
// its reported limit; unsized, that is three quarters of the machine's free
// disk, which may be short of even the log's floor.
func aProvisionedTrackerLog(t *testing.T) (*stateLog, *jetstream.Queue, *jetstream.DomainLog) {
	t.Helper()
	q, err := jetstream.Open(t.Context(), jetstream.Config{
		StoreDir: brokertest.TempDir(t), StoreMaxBytes: brokertest.StoreMaxBytes,
	})
	if err != nil {
		t.Fatalf("open the broker: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	path := filepath.Join(t.TempDir(), "crewlet.db")
	db := storetest.OpenNode(t, path, store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	boot := testBootstrap(t)
	// The estate the runtime's start opens below, migrated already.
	storetest.Seed(t, store.EstateReplicated, store.ReplicatedPath(path, ""))
	ceilings, err := sizeCeilings(t.Context(), q, boot.Stream,
		64<<30, "/var/lib/crewlet/stream")
	if err != nil {
		t.Fatalf("sizeCeilings: %v", err)
	}
	// A runner verifies every record against the keyring and refuses to
	// start without one.
	s := &stateLog{
		mode: statelog.ModeNormal, nodeID: "node-a", db: db,
		ceilings: ceilings, run: t.Context(), ring: recordKeyring(&boot),
	}
	// The replicated estate first, as the runtime's start opens it before
	// reading any checkpoint.
	if err := s.reopenEstate(t.Context()); err != nil {
		t.Fatalf("open the replicated estate: %v", err)
	}
	appendTo, err := s.provision(t.Context(), q, worklet.Domain{})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	return s, q, appendTo
}

// requireAheadOfLogRefusal is the refusal a write from a node past the log's
// end earns: `wrong_stream`, caused by [statelog.ErrAheadOfLog] and not a
// rebuild, naming the verb that repairs it.
func requireAheadOfLogRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonWrongStream {
		t.Fatalf("write = %v, want a %s refusal", err, statelog.ReasonWrongStream)
	}
	if !errors.Is(err, statelog.ErrAheadOfLog) || errors.Is(err, statelog.ErrStreamRecreated) {
		t.Fatalf("write = %v, want errors.Is(%v) and not a rebuild",
			err, statelog.ErrAheadOfLog)
	}
	if !strings.Contains(err.Error(), "crewlet retention reanchor") {
		t.Fatalf("write = %v, which does not name the verb that repairs it", err)
	}
}

// requireRebuiltLogRefusal is the refusal a write on a rebuilt log earns:
// `wrong_stream`, caused by [statelog.ErrStreamRecreated], naming the verb
// that repairs it.
func requireRebuiltLogRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal *statelog.Unavailable
	if !errors.As(err, &refusal) || refusal.Reason != statelog.ReasonWrongStream {
		t.Fatalf("write = %v, want a %s refusal", err, statelog.ReasonWrongStream)
	}
	if !errors.Is(err, statelog.ErrStreamRecreated) {
		t.Fatalf("write = %v, which does not answer errors.Is(%v)",
			err, statelog.ErrStreamRecreated)
	}
	if !strings.Contains(err.Error(), "crewlet retention reanchor") {
		t.Fatalf("write = %v, which does not name the verb that repairs it", err)
	}
}

// aRunningNode boots one engine on its own embedded broker, waits for it to
// admit seats, and answers a JetStream handle on that broker.
func aRunningNode(t *testing.T) (*Engine, natsjs.JetStream) {
	t.Helper()
	return aRunningNodeOf(t, nativeCleanupCompany)
}

// aRunningNodeOf is [aRunningNode] running the given company.
func aRunningNodeOf(t *testing.T, company string) (*Engine, natsjs.JetStream) {
	t.Helper()
	return aRunningNodeWith(t, company, Options{})
}

// aRunningNodeWith is [aRunningNodeOf] built with opts, for a node needing one
// more option ([Options.Metrics], say); it supplies the bootstrap, company and
// backends itself.
func aRunningNodeWith(t *testing.T, company string, opts Options) (*Engine, natsjs.JetStream) {
	t.Helper()
	return aRunningNodeOver(t, company, opts, nil)
}

// aRunningNodeOver is [aRunningNodeWith] over backends amend changes before the
// node boots on them — a coordination store wrapped to stand in for a broker
// the case cannot run, say. Nil amends nothing.
func aRunningNodeOver(t *testing.T, company string, opts Options,
	amend func(*Backends)) (*Engine, natsjs.JetStream) {

	t.Helper()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	brokertest.GitletCacheIn(&b.Gitlet)
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(company))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	if amend != nil {
		amend(back)
	}
	opts.Bootstrap, opts.Company, opts.Backends = &b, cfg, back
	RequireNodeGit(t, &b, cfg)
	e, err := New(t.Context(), opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	waitUntil(t, 20*time.Second, "the node to admit seats", hydrated(t, e))
	q, ok := back.Queue.(interface{ Conn() *nats.Conn })
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend — there is no "+
			"broker to rebuild a log on", back.Queue)
	}
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return e, js
}

// rebuildLog deletes a domain's stream and makes it again under the same name:
// a new identity, and sequences counting from 1 again.
//
// Through the broker's API, not the queue's, which would no-op on a stream it
// provisioned. From a configuration with no stamp, as a hand rebuild makes
// one, so the identity is the broker's new instant ([statelog.StreamCreatedAt]);
// the original's configuration would carry its stamp and be the same stream.
// At the old MaxBytes and MaxMsgSize, which the node sized: at an arbitrary
// size the gate reserve (seven largest appends and a mebibyte) may be all of
// it, refusing every write for a reason the rebuild is not about.
func rebuildLog(t *testing.T, js natsjs.JetStream, spec statelog.StreamSpec) {
	t.Helper()
	old, err := js.Stream(t.Context(), spec.Name)
	if err != nil {
		t.Fatalf("read the stream before rebuilding it: %v", err)
	}
	was := old.CachedInfo().Config
	if err := js.DeleteStream(t.Context(), spec.Name); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	// Creation instants have nanoseconds and the identity compares
	// microseconds: a rebuild in the create's microsecond would compare equal.
	time.Sleep(2 * time.Millisecond)
	if _, err := js.CreateStream(t.Context(), natsjs.StreamConfig{
		Name: spec.Name, Subjects: spec.Subjects,
		MaxBytes: was.MaxBytes, MaxMsgSize: was.MaxMsgSize,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("rebuild the stream: %v", err)
	}
}

// endOf is a domain log's last sequence.
func endOf(t *testing.T, running *runningLog) uint64 {
	t.Helper()
	end, err := running.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	return end
}
