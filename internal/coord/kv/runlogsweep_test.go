package kv

import (
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/coord"
)

// A PART LEFT BESIDE A REMOVED RUN IS SWEPT, AND ONLY THAT ONE, whatever the
// part. No write spans the record and its parts, so a node stopping between
// their removals leaves a part nothing else removes ([coord.SandboxRuns]).
// Staged as that crash leaves it, beside a live run the sweep must leave alone.
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

// THE REMOVAL OF A RUN'S LOG NEVER TAKES A NEW RUN'S. The turn can relaunch
// between its record's removal and its log's, and no append creates a log, so
// taking the new one would drop every call of the new job. Staged with the new
// record and log written before the late removal runs.
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

// A LOG WHOSE RUN IS LAUNCHED AGAIN AFTER THE SWEEP'S PASS IS KEPT: each
// candidate's record is read from the leader before the purge.
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
