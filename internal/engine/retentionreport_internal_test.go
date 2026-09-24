package engine

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A NODE WHOSE APPLIER STOPPED WHILE IT WAS BEHIND IS NAMED BY THE HEARTBEAT
// AND BY THE REPORT, from what the applier cannot misreport.
//
// The applier is the thing that has stopped, so nothing it measures can be
// what reports it: a stopped loop keeps the drain it last measured and ends no
// run, so a lag projected from that drain stays as small as the backlog and a
// count sampled as runs end stays where the last run left it. Every figure
// below is read from the log, the waiters and the retained table themselves:
//
//   - the retained records are COUNTED, on the register row and the gauge;
//   - the waiters gauge is sampled on the heartbeat, from the waiters;
//   - the apply lag is the AGE of the oldest record this node has not applied,
//     read from the broker's own timestamp, so `apply_lag` fires one grace
//     after that record whatever the loop is doing;
//   - `deferred_old` and `generation_left` fire on the real age of the state
//     they name, observed on the heartbeat, and a floor at a generation this
//     node has left is not reported as an unreadable one;
//   - a health read feeds the caught-up latch rather than only reading it;
//   - a node on a generation the fleet left gives its seats back, since the
//     answer that says so is about it alone.
//
// Mutation: state the apply lag as the backlog over the drain and `apply_lag`
// never fires on the quiet log; drop the waiters gauge from the heartbeat and
// it is absent; publish the retained flag and the register reads 1; pin the
// deferral or the floor age to its grace and neither alarm fires; answer the
// latch in health() without feeding it and a restarted latch stays clear;
// read a floor at a generation this node left as an unread one and it keeps
// its seats.
func TestAStoppedApplierIsNamedOnTheHeartbeatAndInTheReport(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	b := config.DefaultBootstrap()
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	b.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")
	cfg, err := config.ParseCompany([]byte(nativeCleanupCompany))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	back, err := OpenBackends(ctx, &b, cfg)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	e, err := New(ctx, Options{Bootstrap: &b, Company: cfg, Backends: back, Metrics: rec})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	waitUntil(t, 20*time.Second, "the node to admit seats", e.NativeHydrated)
	// THIS NODE'S OWN TRIM DUTY, stopped: it evaluates the table this case
	// evaluates by hand, and publishes the floor the last phase replaces.
	e.stopRetention()

	s := e.native.log
	name := tracker.Domain{}.Name()
	running := s.Domain(name)
	if running == nil {
		t.Fatal("the tracker domain is not running")
	}
	subject := tracker.Domain{}.Stream().SubjectPrefix + "." + tracker.BarrierSubject().String()
	gen := running.runner.Committed().Generation
	barrier := func(version int) []byte {
		t.Helper()
		body, err := tracker.MutationRecord{RecordEnvelope: tracker.RecordEnvelope{
			V: version, Subject: tracker.BarrierSubject(), Op: tracker.OpBarrier,
			Scope: tracker.ScopeSet{Subject: true}, Gen: gen,
		}}.Encode()
		if err != nil {
			t.Fatalf("encode a barrier: %v", err)
		}
		return body
	}

	// ---- TWO RECORDS THIS BUILD CANNOT READ, retained while it runs.
	for range 2 {
		if _, _, err := running.log.Append(ctx, subject, "", nil,
			barrier(tracker.ReadableRecordVersion+1)); err != nil {
			t.Fatalf("append a record from a newer build: %v", err)
		}
	}
	waitUntil(t, 10*time.Second, "the applier to retain both records", func() bool {
		_, held := running.runner.Deferred()
		return held == 2
	})

	// ---- A HEALTH READ THAT SEES THE NODE DRAINED SETS THE LATCH, so a node
	// that emptied its backlog between two beats is not held out of
	// admission until the next one. Cleared first, as a relaunch clears it;
	// retried because a write landing in the instant of the read is a record
	// behind, which is no drain.
	waitUntil(t, 5*time.Second, "a health read to set the caught-up latch", func() bool {
		running.progress.restart()
		health, err := s.health(ctx, running)
		return err == nil && health.CaughtUp
	})

	// ---- THE APPLIER STOPS, and a record arrives it will not apply.
	s.haltAppliers()
	seq, _, err := running.log.Append(ctx, subject, "", nil, barrier(tracker.RecordVersion))
	if err != nil {
		t.Fatalf("append a record past the stopped applier: %v", err)
	}
	checkpoint := running.runner.Committed()
	if checkpoint.Seq >= seq {
		t.Fatalf("the stopped applier reached %d, at or past the record at %d", checkpoint.Seq, seq)
	}
	_, _, stored, held, err := running.log.At(ctx, checkpoint.Seq+1)
	if err != nil || !held {
		t.Fatalf("read the oldest unapplied record: held=%v err=%v", held, err)
	}

	// ---- THREE CALLERS BLOCKED ON IT.
	waiting, stopWaiting := context.WithCancel(ctx)
	var waiters sync.WaitGroup
	for range 3 {
		waiters.Add(1)
		go func() {
			defer waiters.Done()
			_ = running.runner.WaitCommitted(waiting, statelog.Position{
				Stream: checkpoint.Stream, Generation: gen, Seq: seq,
			})
		}()
	}
	t.Cleanup(func() {
		stopWaiting()
		waiters.Wait()
	})
	waitUntil(t, 5*time.Second, "three callers to block", func() bool {
		return running.runner.Waiting() == 3
	})

	// ---- THE HEARTBEAT.
	s.publishPositions(ctx)
	positions, err := back.Fleet.Positions(ctx)
	if err != nil {
		t.Fatalf("read the register: %v", err)
	}
	var row coord.NodePositions
	for _, p := range positions {
		if p.NodeID == s.nodeID {
			row = p
		}
	}
	if got := row.Domains[name].Deferred; got != 2 {
		t.Errorf("the register row says %d record(s) retained and the node holds 2", got)
	}
	if got, ok := gaugeOf(rec, metrics.StatelogDeferredCount, name); !ok || got != 2 {
		t.Errorf("deferred.count reads %v (set: %v), want the 2 retained", got, ok)
	}
	if got, ok := gaugeOf(rec, metrics.StatelogWaiters, name); !ok || got != 3 {
		t.Errorf("the waiters gauge reads %v (set: %v) with 3 callers blocked on a "+
			"stopped applier", got, ok)
	}
	// A HEARTBEAT TWO MINUTES AFTER THE RECORD: its age, not its backlog.
	// Read as the window's PEAK, because the node's own heartbeat goes on
	// setting the gauge from the real clock, a few seconds in.
	s.positionGauges(ctx, coord.NodePositions{
		At:      stored.Add(2 * time.Minute),
		Domains: map[string]coord.DomainPosition{name: row.Domains[name]},
	})
	if got, ok := peakOf(rec, metrics.StatelogApplyLagSeconds, name); !ok || got != 120 {
		t.Errorf("apply.lag.seconds peaks at %v (set: %v) two minutes after the "+
			"record it has not applied, want 120", got, ok)
	}

	// ---- THE REPORT: a second after the record, and one grace later.
	r := &retention{state: s, metrics: rec}
	quiet := r.reading(ctx, stored.Add(time.Second), coord.BackupPoint{}, false)
	if firedKind(statelog.Evaluate(quiet), statelog.KindApplyLag) {
		t.Errorf("apply_lag fired a second after the record (lag %v)", quiet.ApplyLag)
	}
	late := r.reading(ctx, stored.Add(statelog.StallGrace+time.Second), coord.BackupPoint{}, false)
	if !firedKind(statelog.Evaluate(late), statelog.KindApplyLag) {
		t.Errorf("apply_lag did not fire one grace after a record the stopped "+
			"applier never applied (lag %v)", late.ApplyLag)
	}

	// ---- DEFERRED_OLD, at the real age of the retained records.
	since := running.progress.deferredSinceValue()
	if !since.Held {
		t.Fatal("the heartbeat did not observe the retained records")
	}
	young := r.reading(ctx, since.Since.Add(statelog.DeferralGrace-time.Second), coord.BackupPoint{}, false)
	if firedKind(statelog.Evaluate(young), statelog.KindDeferredOld) {
		t.Errorf("deferred_old fired inside the grace (age %v)", young.DeferredAge)
	}
	old := r.reading(ctx, since.Since.Add(statelog.DeferralGrace+time.Second), coord.BackupPoint{}, false)
	if !firedKind(statelog.Evaluate(old), statelog.KindDeferredOld) {
		t.Errorf("deferred_old did not fire past the grace (age %v)", old.DeferredAge)
	}

	// ---- GENERATION_LEFT: a floor published at a generation this node has
	// left. Every read of it refuses, and it is not an unreadable floor —
	// coordination answered, and what it answered is that this node's rows
	// are in a number space the fleet has left.
	if err := back.Fleet.PutFloor(ctx, coord.TrimFloor{
		Domain: name, Generation: gen + 1, At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish a floor ahead of this node: %v", err)
	}
	// ITS SEATS GO TO A PEER, and admission names the code its reads give:
	// the answer is about this node alone, so its peers are serving.
	if ok, domain := s.Healthy(ctx); ok || domain != name {
		t.Errorf("a node on a generation the fleet left reads as serviceable=%v "+
			"(domain %q) — every read on it refuses while a peer serves the same "+
			"seats", ok, domain)
	}
	if ok, refusal := s.Established(ctx, true); ok || refusal != statelog.RefuseGenerationLeft {
		t.Errorf("admission on a generation the fleet left = (%v, %q), want "+
			"generation_left — the broker answered", ok, refusal)
	}
	s.publishPositions(ctx)
	ahead := r.reading(ctx, time.Now().Add(statelog.FloorCacheStale+time.Second),
		coord.BackupPoint{}, false)
	if fired := statelog.Evaluate(ahead); !firedKind(fired, statelog.KindGenerationLeft) ||
		firedKind(fired, statelog.KindFloorUnknown) {
		t.Errorf("a floor at a generation this node left raised generation_left=%v "+
			"floor_unknown=%v, want only generation_left (left for %v, unreadable for %v)",
			firedKind(fired, statelog.KindGenerationLeft),
			firedKind(fired, statelog.KindFloorUnknown),
			ahead.GenerationLeftFor, ahead.FloorUnknownFor)
	}
	// AND A FLOOR AT THIS NODE'S OWN GENERATION CLEARS IT.
	if err := back.Fleet.PutFloor(ctx, coord.TrimFloor{
		Domain: name, Generation: gen, At: time.Now().UTC(), By: "peer",
	}); err != nil {
		t.Fatalf("publish a readable floor: %v", err)
	}
	s.publishPositions(ctx)
	read := r.reading(ctx, time.Now().Add(statelog.FloorCacheStale+time.Second),
		coord.BackupPoint{}, false)
	if firedKind(statelog.Evaluate(read), statelog.KindGenerationLeft) {
		t.Errorf("generation_left still fires after a floor at this node's own "+
			"generation was read (left for %v)", read.GenerationLeftFor)
	}
}

// A DEFERRAL IS AGED BY WHAT IT COSTS: a domain whose health gates seats feeds
// `deferred_old`, which says the seats move, and one whose gaps are coverage
// feeds `deferred_coverage`, which says they stay.
//
// [stateLog.Healthy] reads only the domains that declare
// [statelog.Domain.ReadinessInput], so the seat move `deferred_old` reports
// happens for those and for no other. A vectors deferral aged into the same
// input would page an operator with a seat move that never comes.
//
// Mutation: age every domain's deferral into DeferredAge and the vectors
// deferral raises `deferred_old`.
func TestADeferralIsAgedByWhatItCosts(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	now := at.Add(statelog.DeferralGrace + time.Minute)
	for _, tc := range []struct {
		name      string
		domain    statelog.Domain
		want, not statelog.Kind
	}{
		{"a domain whose health gates seats", tracker.Domain{},
			statelog.KindDeferredOld, statelog.KindDeferredCoverage},
		{"a domain whose gaps are coverage", search.Domain{},
			statelog.KindDeferredCoverage, statelog.KindDeferredOld},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			running := &runningDomain{domain: tc.domain}
			running.progress.observe(at, 10, owed(0), true)
			var reading statelog.Reading
			series(running, now, &reading)
			fired := statelog.Evaluate(reading)
			if !firedKind(fired, tc.want) || firedKind(fired, tc.not) {
				t.Errorf("a deferral held past the grace in %s raised %s=%v and "+
					"%s=%v, want only %s (seat-gating age %s, coverage age %s)",
					tc.domain.Name(), tc.want, firedKind(fired, tc.want), tc.not,
					firedKind(fired, tc.not), tc.want, reading.DeferredAge,
					reading.CoverageDeferredAge)
			}
		})
	}
}

// gaugeOf is one gauge's current value for one domain, and whether anything
// set it.
func gaugeOf(rec *metrics.Recorder, name, domain string) (float64, bool) {
	return valueOf(rec.Read(), name, domain)
}

// peakOf is the highest value one gauge reached for one domain in the window.
func peakOf(rec *metrics.Recorder, name, domain string) (float64, bool) {
	return valueOf(rec.ReadWindow(), name, domain)
}

func valueOf(reading []metrics.Snapshot, name, domain string) (float64, bool) {
	for _, s := range reading {
		if s.Name == name && s.Attrs["domain"] == domain {
			return s.Value, true
		}
	}
	return 0, false
}
