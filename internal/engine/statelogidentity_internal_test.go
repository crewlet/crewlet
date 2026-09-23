package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE APPLIER IS HANDED THE BROKER'S OWN CREATION INSTANT, and a stream
// recreated between two boots stops it.
//
// # Why this is an engine test and not only a framework one
//
// The framework's detector is one comparison, and it was correct. What was
// wrong was the wiring: the engine read the instant back OUT OF THE CURSOR
// ROW and passed that as the stream's, so the loop compared the row against
// itself. Every recreated stream went undetected, a fresh node's cursor row
// recorded the year one, the snapshot manifest carried a zero instant, and
// the reanchor verb asked an operator to confirm 0001-01-01. A framework test
// passing a real instant proves nothing about a caller that passes the wrong
// one, so this boots the engine over its own broker and checks both halves.
func TestTheApplierIsHandedTheBrokersOwnStreamIdentity(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	stream := tracker.Domain{}.Stream().Name

	// FIRST BOOT: the running domain carries what the broker reports.
	back, err := OpenBackends(t.Context(), &b, cfg)
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
		t.Fatal("the broker reports no creation instant, so nothing below can be checked")
	}
	running := e.native.log.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	if !running.createdAt.Equal(stats.CreatedAt.UTC()) {
		t.Fatalf("the running domain carries %s and the broker reports %s — the "+
			"detector compares the checkpoint against this, and a value read "+
			"back out of the checkpoint detects nothing",
			running.createdAt, stats.CreatedAt)
	}
	// Commit a checkpoint under that identity, the way the applier does
	// with every batch, so the second boot has something to compare.
	if err := back.Store.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, 0, 0, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET stream_created_at = excluded.stream_created_at`,
			stream, store.EncodeTime(stats.CreatedAt.UTC()), store.EncodeTime(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("commit a checkpoint: %v", err)
	}
	e.Stop(context.Background())

	// THE STREAM IS DELETED AND REMADE with the same name, which is what a
	// broker-level restore or a hand rebuild does. Its sequences restart
	// and its creation instant moves.
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteStream(t.Context(), stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	back.Close(context.Background())
	// The broker keeps stream creation instants at nanosecond precision
	// and the row keeps microseconds; a second boot inside the same
	// microsecond would compare equal, so it waits one out.
	time.Sleep(2 * time.Millisecond)

	// SECOND BOOT: the applier stops and says why.
	back2, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends again: %v", err)
	}
	t.Cleanup(func() { back2.Close(context.Background()) })
	e2, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back2})
	if err != nil {
		t.Fatalf("New again: %v", err)
	}
	t.Cleanup(func() { e2.Stop(context.Background()) })
	running = e2.native.log.Domain(tracker.Domain{}.Name())
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
	// AND ITS WRITES REFUSE. The stop ends the apply loop and nothing else:
	// the publisher beside it forms its expectations and clears its zero
	// fence from that same checkpoint, so a write at zero is cleared
	// against a history the new stream does not have and lands on it as
	// though it continued the old one.
	before, err := running.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the new stream's end: %v", err)
	}
	_, err = e2.native.writer.EvictNode(t.Context(), "op-after-recreation", "node-x")
	requireRebuiltLogRefusal(t, err)
	if end, err := running.log.End(t.Context()); err != nil || end != before {
		t.Fatalf("the new stream ends at %d (err %v), want %d — a write from a "+
			"node whose checkpoint names another stream landed on it", end, err, before)
	}
	// AND THE OPERATOR SURFACE SAYS SO rather than reporting a caught-up
	// loop: a stopped applier has a lag of zero, and the row read as ready
	// for as long as nobody looked at the error beside the number.
	for _, row := range e2.NativeStatus(t.Context()) {
		if row.Name != (tracker.Domain{}).Name() {
			continue
		}
		if row.Ready || !strings.Contains(row.Detail, "recreated") {
			t.Fatalf("the tracker's status row is %+v, want not ready and naming "+
				"the recreation", row)
		}
	}
}

// AND A STREAM REBUILT UNDER A RUNNING NODE IS NAMED WHILE IT RUNS.
//
// The boot's identity check is the one above, and it was the only one: the
// live creation instant was sampled once in start and never read again, so a
// stream deleted and rebuilt under a node that stayed up was never named as a
// recreation at all.
//
// The sequence terms cannot stand in for it. A rebuilt stream comes back at
// generation 0 counting from 1, so the checkpoint-past-the-end term reports it
// only until the new stream has published past this node's position — after
// which every term reads healthy while the node applies a different history
// into rows keyed by the old one, and the diagnosis an operator gets names
// anything but the rebuild.
//
// The instant arrives in the same answer the heartbeat already reads for the
// stream's bounds; it was being thrown away.
func TestAStreamRebuiltUnderARunningNodeIsNamed(t *testing.T) {
	t.Parallel()
	e, js := aRunningNode(t)
	s := e.native.log
	running := s.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}

	// THE REBUILD, under a node that never stops: the same name, a new
	// creation instant, and sequences counting from 1 again.
	rebuildLog(t, js, tracker.Domain{}.Stream())

	// THE HEARTBEAT IS WHAT SEES IT, on the round trip it already makes.
	s.publishPositions(t.Context())

	health, err := s.health(t.Context(), running)
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
// # What a write did before
//
// The heartbeat named the rebuild and the reads refused, and the write path
// consulted nothing: its expectations are sequences from the old history, and
// the rebuilt log — same name, same generation, counting from 1 — arbitrated
// them as sequences about itself. Two shapes, both below, and both LANDED:
//
//   - A RETRY AT ZERO. The row's anchor names a record the rebuilt log does
//     not hold, so the broker refuses the expectation, the empty subject reads
//     as a trimmed anchor, and the zero fence clears it against a floor and a
//     checkpoint from the old history. The record went onto the rebuilt log
//     at sequence 1, and the write was then reported as a ledger contract
//     violation, because the resolution compared that position with the old
//     checkpoint and read it as already applied.
//   - AN ORDINARY EXPECTATION the rebuilt log happens to satisfy: its own
//     history on the subject ends at exactly the sequence this node's row
//     names. The broker has no way to know the two numbers come from
//     different logs and accepts the write.
//
// Whatever lands is not a local mistake: the recovery follows the rebuilt log
// from its head, so every node applies it as though it continued the history
// it was never arbitrated in.
func TestAStreamRebuiltUnderARunningNodeRefusesItsWrites(t *testing.T) {
	t.Parallel()
	e, js := aRunningNode(t)
	s := e.native.log
	running := s.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	writer := e.native.writer
	if writer == nil {
		t.Fatal("the node runs no tracker writer")
	}

	// TWO OBJECTS WITH HISTORY on the log this node started against, and
	// the first one's record kept byte for byte, to be replayed onto the
	// rebuilt log where this node's row says it is.
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

	rebuildLog(t, js, tracker.Domain{}.Stream())

	// THE RETRY AT ZERO, BEFORE ANY HEARTBEAT HAS SEEN THE REBUILD. The
	// zero fence's own read of the log carries the stream's creation
	// instant, and that is what refuses this write within the call — a
	// check that waited for the next beat would have let it land.
	_, err = writer.ReadmitNode(t.Context(), "op-readmit-y", "node-y")
	requireRebuiltLogRefusal(t, err)
	if end := endOf(t, running); end != 0 {
		t.Fatalf("the rebuilt log ends at %d, want 0 — a retry at zero cleared "+
			"against the old history's floor landed on it", end)
	}

	// THE ORDINARY EXPECTATION the rebuilt log would accept: its own history
	// on node-x's subject ends at exactly the sequence this node's row
	// names, which is the one condition under which the broker says yes.
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

	// AND THE READS, FROM THE SAME ANSWER, and the operator surface with
	// them — including once the rebuilt log has reached this node's own
	// checkpoint, where every sequence term reads caught up.
	for endOf(t, running) < running.runner.Committed().Seq {
		barrierOn(t, running)
	}
	s.publishPositions(t.Context())
	health, err := s.health(t.Context(), running)
	if err != nil {
		t.Fatalf("health: %v", err)
	}
	if code := health.Refusal(time.Now()); code != statelog.RefuseWrongStream {
		t.Errorf("reads on a rebuilt log refuse with %q, want %q", code,
			statelog.RefuseWrongStream)
	}
	for _, row := range e.NativeStatus(t.Context()) {
		if row.Name != (tracker.Domain{}).Name() {
			continue
		}
		if row.Ready || !strings.Contains(row.Detail, "recreated") {
			t.Errorf("the tracker's status row is %+v, want not ready and naming "+
				"the recreation — caught up on a sequence the rebuilt log merely "+
				"reached is not caught up", row)
		}
	}
}

// requireRebuiltLogRefusal is the refusal a write on a rebuilt log earns: the
// framework's `wrong_stream`, recognisable by its cause without switching on
// the reason, and naming what the operator has to do.
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
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(t.Context(), &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
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
// a new creation instant, and sequences counting from 1 again.
//
// THROUGH THE BROKER'S OWN API rather than the queue's, which remembers what
// it has provisioned and would no-op — and a broker-level rebuild is precisely
// a stream this process did not create.
func rebuildLog(t *testing.T, js natsjs.JetStream, spec statelog.StreamSpec) {
	t.Helper()
	if err := js.DeleteStream(t.Context(), spec.Name); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	// The broker keeps creation instants at nanosecond precision and the
	// identity compares microseconds; a rebuild inside the same microsecond
	// as the create would compare equal, so it waits one out.
	time.Sleep(2 * time.Millisecond)
	if _, err := js.CreateStream(t.Context(), natsjs.StreamConfig{
		Name: spec.Name, Subjects: spec.Subjects,
		MaxBytes: 16 << 20, Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("rebuild the stream: %v", err)
	}
}

// endOf is a domain log's last sequence.
func endOf(t *testing.T, running *runningDomain) uint64 {
	t.Helper()
	end, err := running.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	return end
}
