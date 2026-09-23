package engine

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tracker"
)

// ONE STOP IS ONE LINE, AND THE ENGINE DOES NOT ADD ITS OWN.
//
// The runner's `statelog_applier_stopped` went to a discarding logger, so the
// engine wrote one of its own when the apply loop returned. Once the runner's
// reached the log, every stop read as two — the second under
// `component=engine`, which is not where the replication guide sends anybody,
// and carrying the one sentence saying what resumes the applier. The runner's
// line carries that sentence now, and its own test pins it; this is the
// engine's half, and it has to boot one, because only a booted engine can
// show what the engine adds around the loop.
//
// NOT PARALLEL: it swaps the process-wide logger, which this package
// otherwise leaves at its boot default, and a parallel case stopping an
// applier of its own would be counted here.
func TestAnApplierThatStopsIsWrittenOnceAndByTheStateLog(t *testing.T) {
	logs := &stopLineBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	stream := tracker.Domain{}.Stream().Name

	// FIRST BOOT, and a checkpoint committed under the stream it ran on.
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
	created := e.native.log.Domain(tracker.Domain{}.Name()).runner.StreamCreatedAt()
	if err := back.Store.Replicated().Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			INSERT INTO statelog_cursor
				(stream, generation, seq, stream_created_at, updated_at)
			VALUES (?, 0, 0, ?, ?)
			ON CONFLICT (stream) DO UPDATE SET stream_created_at = excluded.stream_created_at`,
			stream, store.EncodeTime(created), store.EncodeTime(time.Now().UTC()))
		return err
	}); err != nil {
		t.Fatalf("commit a checkpoint: %v", err)
	}
	e.Stop(context.Background())

	// THE LOG IS DELETED under the stopped node, so the next boot makes a
	// new one under the same name, and the applier stops on it.
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	if err := js.DeleteStream(t.Context(), stream); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	back.Close(context.Background())
	// The broker keeps creation instants at nanosecond precision and the
	// row keeps microseconds; a second boot inside the same microsecond
	// would compare equal, so it waits one out.
	time.Sleep(2 * time.Millisecond)

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
	running := e2.native.log.Domain(tracker.Domain{}.Name())
	waitUntil(t, 10*time.Second, "the tracker's applier to stop on the recreated log",
		func() bool { return running.runner.Stopped() != nil })
	// EVERY APPLY LOOP ENDED AND WAITED FOR, so whatever the engine writes
	// once a loop returns has been written by now.
	e2.native.log.haltAppliers()

	var stops []map[string]any
	for _, record := range logs.records(t) {
		if record["msg"] == "statelog_applier_stopped" &&
			record["domain"] == (tracker.Domain{}).Name() {
			stops = append(stops, record)
		}
	}
	if len(stops) != 1 {
		t.Fatalf("one stop wrote %d statelog_applier_stopped lines, want one — "+
			"anything counting the event counts a stop per line: %v", len(stops), stops)
	}
	for key, want := range map[string]any{
		"component": "statelog",
		"stream":    stream,
	} {
		if stops[0][key] != want {
			t.Errorf("statelog_applier_stopped %s = %v, want %v", key, stops[0][key], want)
		}
	}
	if detail, _ := stops[0]["detail"].(string); !strings.Contains(detail, "resumes") {
		t.Errorf("the one statelog_applier_stopped does not say what resumes the "+
			"applier: %q", detail)
	}
}

// stopLineBuffer is the process log's destination for one test, safe to write
// from every goroutine the engine runs.
type stopLineBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *stopLineBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// records decodes every line written so far.
func (b *stopLineBuffer) records(t *testing.T) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(b.buf.String()), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("a log line is not JSON: %q: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}
