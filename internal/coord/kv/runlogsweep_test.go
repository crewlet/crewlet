package kv

import (
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
)

// A PART LEFT BESIDE A REMOVED RUN IS SWEPT, AND ONLY THAT ONE — every part,
// its log, its handed set and its leads alike. The record and each part are
// keys of their own and no write spans them, so a node that stops between the
// record's removal and a part's leaves a part no record holds, which nothing
// else would ever remove ([coord.SandboxRuns]). Staged as that crash leaves
// it — the record purged on the wire and its parts not — beside a live run
// with parts of its own, which the sweep must leave alone.
//
// Mutation: skip [FleetStore.sweepRunParts] in SweepMarkers, and the orphans
// survive; match only the log's name in its candidate filter, and the handed
// set and the leads survive. (The live run's parts are kept by the pass
// itself, which holds its record; the leader's read of the record is held by
// TestALogWhoseRunIsLaunchedAgainAfterTheSweepsPassIsKept.)
func TestAPartLeftBesideARemovedRunIsSwept(t *testing.T) {
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
		for _, part := range coord.RunParts() {
			if _, ok, err := f.MutateSandboxRunPart(ctx, turn, part, func(coord.Record, bool) ([]byte, error) {
				return []byte(`{"launch":"job"}`), nil
			}); err != nil || !ok {
				t.Fatalf("%s %s: (%v, %v)", part, turn, ok, err)
			}
		}
	}
	// THE CRASH: the record goes, and its node stops before its parts do.
	if err := f.runs.Purge(ctx, encodeKey("t-crashed")); err != nil {
		t.Fatalf("purge the record: %v", err)
	}
	for _, part := range coord.RunParts() {
		if _, found, err := f.SandboxRunPart(ctx, "t-crashed", part); err != nil || !found {
			t.Fatalf("the staged crash left no %s to sweep: (%v, %v)", part, found, err)
		}
	}

	if _, err := f.SweepMarkers(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("SweepMarkers: %v", err)
	}
	for _, part := range coord.RunParts() {
		if held, found, err := f.SandboxRunPart(ctx, "t-crashed", part); err != nil || found {
			t.Errorf("the %s of a run with no record survived the sweep: (%s, %v, %v)", part, held.Value, found, err)
		}
		if _, found, err := f.SandboxRunPart(ctx, "t-live", part); err != nil || !found {
			t.Errorf("the sweep removed the %s of a run that has its record: (%v, %v)", part, found, err)
		}
	}
}

// THE REMOVAL OF A RUN'S LOG NEVER TAKES A NEW RUN'S. A run's record is
// removed, then its log; between the two the turn can be launched again, its
// new record created and the log opened to its job. No append creates a log,
// so a removal that took that log would drop every call of the new job. Staged
// as that interleaving leaves the bucket — the old record gone, the new record
// and its log written — before the removal runs.
//
// Mutation: drop removeRunPart's read of the record, and the new run's log is
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
	if _, ok, err := f.MutateSandboxRunPart(ctx, turn, coord.RunLog, func(coord.Record, bool) ([]byte, error) {
		return []byte(`{"launch":"next"}`), nil
	}); err != nil || !ok {
		t.Fatalf("open the next run's log: (%v, %v)", ok, err)
	}

	// THE REMOVAL OF THE RUN BEFORE IT, arriving late.
	f.removeRunPart(ctx, turn, coord.RunLog)

	if log, found, err := f.SandboxRunPart(ctx, turn, coord.RunLog); err != nil || !found || string(log.Value) != `{"launch":"next"}` {
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
// Mutation: drop sweepRunParts' read of the record, and the relaunched run's
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
	if _, ok, err := f.MutateSandboxRunPart(ctx, turn, coord.RunLog, func(coord.Record, bool) ([]byte, error) {
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

	if _, err := f.sweepRunParts(ctx, pass, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("sweepRunParts: %v", err)
	}
	if _, found, err := f.SandboxRunPart(ctx, turn, coord.RunLog); err != nil || !found {
		t.Errorf("the sweep removed the log of a run launched again after its pass: (%v, %v)", found, err)
	}
}
