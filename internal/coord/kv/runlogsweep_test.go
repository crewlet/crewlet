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
// survives. (The live run's log is kept by the pass itself, which holds its
// record; the leader's read of the record is held by
// TestALogWhoseRunIsLaunchedAgainAfterTheSweepsPassIsKept.)
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

// THE REMOVAL OF A RUN'S LOG NEVER TAKES A NEW RUN'S. A run's record is
// removed, then its log; between the two the turn can be launched again, its
// new record created and the log opened to its job. No append creates a log,
// so a removal that took that log would drop every call of the new job. Staged
// as that interleaving leaves the bucket — the old record gone, the new record
// and its log written — before the removal runs.
//
// Mutation: drop removeRunLog's read of the record, and the new run's log is
// purged.
func TestARunsLogRemovalLeavesTheLogOfTheTurnsNextRun(t *testing.T) {
	t.Parallel()
	srv, _ := embeddedServer(t)
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	ctx := t.Context()
	const turn = "t-relaunched"
	if _, err := f.CreateSandboxRun(ctx, turn, []byte(`{"status":"launching"}`)); err != nil {
		t.Fatalf("create the next run: %v", err)
	}
	if _, ok, err := f.MutateSandboxRunLog(ctx, turn, func(coord.Record, bool) ([]byte, error) {
		return []byte(`{"launch":"next"}`), nil
	}); err != nil || !ok {
		t.Fatalf("open the next run's log: (%v, %v)", ok, err)
	}

	// THE REMOVAL OF THE RUN BEFORE IT, arriving late.
	f.removeRunLog(ctx, turn)

	if log, found, err := f.SandboxRunLog(ctx, turn); err != nil || !found || string(log.Value) != `{"launch":"next"}` {
		t.Errorf("the next run's log after the removal of the run before it: (%s, %v, %v), want it kept",
			log.Value, found, err)
	}
}

// A LOG WHOSE RUN IS LAUNCHED AGAIN AFTER THE SWEEP'S PASS IS KEPT. The sweep
// finds its candidates in a pass of the bucket, and the turn can be launched
// again between that pass and the purge: its new record created, the log beside
// it the new run's to open. Each candidate's record is read from the leader
// before the purge, so the log stays.
//
// Mutation: drop sweepRunLogs' read of the record, and the relaunched run's
// log is swept.
func TestALogWhoseRunIsLaunchedAgainAfterTheSweepsPassIsKept(t *testing.T) {
	t.Parallel()
	srv, _ := embeddedServer(t)
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)
	f := openFleetForTest(t, nc, fmt.Sprintf("f%d", bucketSeq.Add(1)))
	ctx := t.Context()
	const turn = "t-launched-again"
	if _, err := f.CreateSandboxRun(ctx, turn, []byte(`{"status":"running"}`)); err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok, err := f.MutateSandboxRunLog(ctx, turn, func(coord.Record, bool) ([]byte, error) {
		return []byte(`{"launch":"first"}`), nil
	}); err != nil || !ok {
		t.Fatalf("log: (%v, %v)", ok, err)
	}
	if err := f.runs.Purge(ctx, encodeKey(turn)); err != nil {
		t.Fatalf("purge the record: %v", err)
	}
	pass, err := walkForSweep(ctx, f.js, f.runs)
	if err != nil {
		t.Fatalf("the sweep's pass: %v", err)
	}
	// THE TURN LAUNCHED AGAIN, after the pass found its log alone.
	if _, err := f.CreateSandboxRun(ctx, turn, []byte(`{"status":"launching"}`)); err != nil {
		t.Fatalf("create again: %v", err)
	}

	if _, err := f.sweepRunLogs(ctx, pass, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("sweepRunLogs: %v", err)
	}
	if _, found, err := f.SandboxRunLog(ctx, turn); err != nil || !found {
		t.Errorf("the sweep removed the log of a run launched again after its pass: (%v, %v)", found, err)
	}
}
