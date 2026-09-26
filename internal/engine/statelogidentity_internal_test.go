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
	if !running.identity().Equal(stats.CreatedAt.UTC()) {
		t.Fatalf("the running domain carries %s and the broker reports %s — the "+
			"detector compares the checkpoint against this, and a value read "+
			"back out of the checkpoint detects nothing",
			running.identity(), stats.CreatedAt)
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
	// AND THE OPERATOR SURFACE SAYS SO rather than reporting a caught-up
	// loop: a stopped applier has a lag of zero, and the row read as ready
	// for as long as nobody looked at the error beside the number. The
	// heartbeat is what names it — its row states the stream the
	// checkpoint counts on, which is the deleted one — so the row is read
	// after one beat.
	e2.native.log.publishPositions(t.Context())
	for _, row := range e2.NativeStatus(t.Context()) {
		if row.Name != (tracker.Domain{}).Name() {
			continue
		}
		if row.Ready || !strings.Contains(row.Detail, "deleted and rebuilt") {
			t.Fatalf("the tracker's status row is %+v, want not ready and naming "+
				"the rebuild", row)
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
	s := e.native.log
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)

	running := s.Domain(tracker.Domain{}.Name())
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	spec := tracker.Domain{}.Stream()
	q, ok := back.Queue.(interface{ Conn() *nats.Conn })
	if !ok {
		t.Skip("the memory queue has no broker to rebuild a stream on")
	}
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}

	// THE REBUILD, under a node that never stops: the same name, a new
	// creation instant, and sequences counting from 1 again.
	if err := js.DeleteStream(t.Context(), spec.Name); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	// THROUGH THE BROKER'S OWN API rather than the queue's, which
	// remembers what it has provisioned and would no-op — and a
	// broker-level rebuild is precisely a stream this process did not
	// create.
	if _, err := js.CreateStream(t.Context(), natsjs.StreamConfig{
		Name: spec.Name, Subjects: spec.Subjects,
		MaxBytes: 16 << 20, Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("rebuild the stream: %v", err)
	}

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
