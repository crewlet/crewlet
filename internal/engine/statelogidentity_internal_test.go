package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
