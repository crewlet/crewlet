package kv

import (
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
)

// A LOG LEFT BESIDE A REMOVED RUN IS SWEPT, AND ONLY THAT ONE. The record and
// its log are two keys and no write spans both, so a node that stops between
// the record's removal and the log's leaves a log no record holds, which
// nothing else would ever remove ([coord.SandboxRuns]). Staged as that crash
// leaves it — the record purged on the wire and the log not — beside a live
// run with a log of its own, which the sweep must leave alone.
//
// Mutation: skip [FleetStore.sweepRunLogs] in SweepMarkers, and the orphan
// survives; drop its read of the run's record, and the live run loses its log.
func TestALogLeftBesideARemovedRunIsSwept(t *testing.T) {
	t.Parallel()
	srv, _ := embeddedServer(t)
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	ctx := t.Context()
	for _, turn := range []string{"t-live", "t-crashed"} {
		if _, err := f.CreateSandboxRun(ctx, turn, []byte(`{"status":"running"}`)); err != nil {
			t.Fatalf("create %s: %v", turn, err)
		}
		if _, ok, err := f.MutateSandboxRunLog(ctx, turn, func(coord.Record, bool) ([]byte, error) {
			return []byte(`{"calls":["read_page"]}`), nil
		}); err != nil || !ok {
			t.Fatalf("log %s: (%v, %v)", turn, ok, err)
		}
	}
	// THE CRASH: the record goes, and its node stops before the log does.
	if err := f.runs.Purge(ctx, encodeKey("t-crashed")); err != nil {
		t.Fatalf("purge the record: %v", err)
	}
	if _, found, err := f.SandboxRunLog(ctx, "t-crashed"); err != nil || !found {
		t.Fatalf("the staged crash left no log to sweep: (%v, %v)", found, err)
	}

	if _, err := f.SweepMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SweepMarkers: %v", err)
	}
	if log, found, err := f.SandboxRunLog(ctx, "t-crashed"); err != nil || found {
		t.Errorf("the log of a run with no record survived the sweep: (%s, %v, %v)", log.Value, found, err)
	}
	if _, found, err := f.SandboxRunLog(ctx, "t-live"); err != nil || !found {
		t.Errorf("the sweep removed the log of a run that has its record: (%v, %v)", found, err)
	}
}
