package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE ALARMS A NODE'S HEALTH CARRIES ARE THE RETENTION REPORT'S ALARMS.
//
// [Engine.Alarms] reads the same evaluation the gauge and alarm log lines were
// fed: after one it names exactly the report's alarms, one per kind; before
// one it says it cannot say, since a node that has not looked is not healthy.
//
// Mutation: answer (nil, true) before the first evaluation, or read a list the
// tracker was not handed, and this fails. One kind on two logs is
// [TestTheHealthAlarmsCountAKindOnTwoLogsOnce]'s: this node runs no log.
func TestTheHealthAlarmsAreTheRetentionAlarms(t *testing.T) {
	t.Parallel()
	r := &retention{
		fleet:  coordmem.NewFleet(),
		state:  &stateLog{},
		nodeID: "node-a",
		alarms: statelog.NewTracker(nil, nil),
		pooled: map[string]poolCounters{},
		// A slow search gives the table something to raise: this node runs
		// no log, and a log's alarms are evaluated per log.
		index: func(_ context.Context, out *statelog.Reading) {
			out.SearchP95 = 2 * statelog.InteractiveSearchTarget
		},
	}
	e := &Engine{}
	if standing, evaluated := e.Alarms(); evaluated {
		t.Fatalf("a node running no alarm table answered %v as evaluated", standing)
	}
	e.alarms.Store(r.alarms)
	if standing, evaluated := e.Alarms(); evaluated {
		t.Fatalf("a table not yet evaluated answered %v as evaluated", standing)
	}

	r.beat(t.Context())
	report := r.Report(t.Context())
	want := make([]statelog.Kind, 0, len(report.Alarms))
	for _, a := range report.Alarms {
		want = append(want, a.Kind)
	}
	// A FLOOR: the slow search above raises `search_slow`, so a comparison
	// of two empty lists cannot pass as agreement here.
	if len(want) == 0 {
		t.Fatal("the report raised no alarm, so this compares nothing")
	}
	got, evaluated := e.Alarms()
	if !evaluated {
		t.Fatal("an evaluated table answered as not evaluated")
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the health envelope names %v and the retention report %v", got, want)
	}
}

// THE HEARTBEAT CARRIES WHAT THE NODE'S OWN TABLE HAS FIRING, as a peer reads
// it off the presence lease: the health envelope's alarms, in its order, each
// dated by its raise — and nothing before an evaluation, so a peer never reads
// a node that cannot say as a quiet one.
//
// Mutation: leave the alarms off the status, publish them as evaluated before
// the first evaluation, or date them by the beat rather than the raise, and
// this fails.
func TestTheHeartbeatCarriesTheAlarmsFiringOnTheNode(t *testing.T) {
	t.Parallel()
	raised := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	r := &retention{
		fleet:  coordmem.NewFleet(),
		state:  &stateLog{},
		nodeID: "node-a",
		alarms: statelog.NewTracker(nil, func() time.Time { return raised }),
		pooled: map[string]poolCounters{},
		// A slow search, as the case above has.
		index: func(_ context.Context, out *statelog.Reading) {
			out.SearchP95 = 2 * statelog.InteractiveSearchTarget
		},
	}
	e := &Engine{}
	heard := func() coord.NodeStatus {
		t.Helper()
		status, err := e.nodeStatus(t.Context())
		if err != nil {
			t.Fatalf("the heartbeat's status: %v", err)
		}
		got, ok := coord.StatusFromMeta(map[string]any{coord.StatusKey: status.Meta()})
		if !ok {
			t.Fatal("the heartbeat carried no status")
		}
		return got
	}
	if got := heard(); got.AlarmsEvaluated || got.Alarms != nil {
		t.Fatalf("a node running no alarm table published (%v, %+v)", got.AlarmsEvaluated, got.Alarms)
	}
	e.alarms.Store(r.alarms)
	if got := heard(); got.AlarmsEvaluated || got.Alarms != nil {
		t.Fatalf("a table not yet evaluated published (%v, %+v)", got.AlarmsEvaluated, got.Alarms)
	}

	r.beat(t.Context())
	standing, _ := e.Alarms()
	// A FLOOR: the slow search above raises `search_slow`, so a comparison
	// of two empty lists cannot pass as agreement here.
	if len(standing) == 0 {
		t.Fatal("the beat raised no alarm, so this compares nothing")
	}
	got := heard()
	if !got.AlarmsEvaluated {
		t.Fatal("an evaluated table published its alarms as not evaluated")
	}
	kinds := make([]statelog.Kind, 0, len(got.Alarms))
	for _, m := range got.Alarms {
		kinds = append(kinds, statelog.Kind(m.Kind))
		if !m.Since.Equal(raised) {
			t.Errorf("%s is marked since %v, want %v: the instant the tracker raised it", m.Kind, m.Since, raised)
		}
	}
	if !slices.Equal(kinds, standing) {
		t.Errorf("the heartbeat names %v and the health envelope %v, in that order", kinds, standing)
	}
}

// ONE ENTRY PER KIND ON A RUNNING NODE, however many logs the kind stands on:
// two logs with an unusable floor raise ONE `floor_unknown` listing both, and
// the health envelope counts it once.
//
// Mutation: have [statelog.Tracker.Standing] answer one entry per firing
// instance (a kind once per log), and this fails.
func TestTheHealthAlarmsCountAKindOnTwoLogsOnce(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e := bootDirectoryNode(t, nil)
	r := quietRetention(t, e)
	logs := []string{worklet.Domain{}.Name(), iamdomain.Domain{}.Name()}
	for _, name := range logs {
		generation := r.state.Domain(name).runner.Committed().Generation
		ahead := coord.TrimFloor{Domain: name, Generation: generation + 1,
			At: time.Now().UTC(), By: "node-b"}
		if err := r.fleet.PutFloor(ctx, ahead); err != nil {
			t.Fatalf("publish a floor ahead of this node on %s: %v", name, err)
		}
	}
	t0 := time.Now().UTC()
	r.now = func() time.Time { return t0 }
	r.beat(ctx)
	r.now = func() time.Time { return t0.Add(statelog.FloorCacheStale + statelog.AlarmInterval) }
	r.beat(ctx)
	report := r.Report(ctx)
	want := make([]statelog.Kind, 0, len(report.Alarms))
	for _, a := range report.Alarms {
		want = append(want, a.Kind)
	}
	fired, ok := alarmOf(report, statelog.KindFloorUnknown)
	if !ok || len(fired.Domains) != len(logs) {
		t.Fatalf("floor_unknown stands on %+v, want the %d logs %v", fired.Domains, len(logs), logs)
	}
	got, evaluated := e.Alarms()
	if !evaluated {
		t.Fatal("an evaluated table answered as not evaluated")
	}
	// Sorted, never deduplicated, so a kind named twice stays visible.
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the health envelope names %v and the retention report %v", got, want)
	}
}

// THE HEARTBEAT LEAVES THE HARDWARE MEASUREMENT TO THE TRIM'S TICK.
//
// On every [statelog.AlarmInterval] beat the pool-wait delta would make
// `pool_starved`'s p95 a percentile over ninety times as many, shorter
// intervals, and the backup-gauge walk would repeat its WARN for an unreadable
// directory on every beat.
//
// Mutation: have [retention.beat] call [retention.capacity], and the first half
// fails; drop it from [retention.evaluate], and the control fails.
func TestTheHeartbeatLeavesTheHardwareMeasurementToTheTrimTick(t *testing.T) {
	t.Parallel()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	db := storetest.OpenNode(t, t.TempDir()+"/index.db", store.Options{})
	t.Cleanup(func() { _ = db.Close() })

	r := &retention{
		db:      db,
		fleet:   coordmem.NewFleet(),
		state:   &stateLog{},
		nodeID:  "node-a",
		metrics: recorder,
		alarms:  statelog.NewTracker(recorder, nil),
		// A baseline below the driver's counters gives the next measurement a
		// delta to record: an idle pool records nothing, which proves nothing.
		pooled: map[string]poolCounters{
			"index.db": {count: -4, waited: -2 * time.Second},
		},
		claim: func(context.Context) (bool, error) { return false, nil },
	}
	measured := func() (poolWaits uint64, holds bool) {
		for _, snapshot := range recorder.Read() {
			switch snapshot.Name {
			case metrics.StorePoolWait:
				poolWaits += snapshot.Count
			case metrics.BackupHolds:
				holds = true
			}
		}
		return poolWaits, holds
	}

	for range 3 {
		r.beat(t.Context())
	}
	if waits, holds := measured(); waits != 0 || holds {
		t.Fatalf("three heartbeats took the hardware measurement "+
			"(%d pool-wait observation(s), holds gauge published: %v), so the "+
			"`pool_starved` p95 would be over heartbeat intervals rather "+
			"than trim ticks", waits, holds)
	}

	// The control: the trim's tick does measure, or the assertion above
	// passes on a node that never measures.
	r.tick(t.Context())
	if waits, holds := measured(); waits != 1 || !holds {
		t.Errorf("a trim tick recorded %d pool-wait observation(s) and "+
			"published the holds gauge: %v; want one and true", waits, holds)
	}
}

// A FAILED COVERAGE SCAN IS READ BACK AS UNKNOWN, AND NOT RE-RUN BY A READ.
//
// The whole-corpus scan runs on the trim's tick alone and every report reads
// its figure back: a read re-running a failure would repeat the scan and its
// WARN on every beat, and keeping the last figure would call an unreadable
// corpus covered.
//
// Mutation: scan inside [retention.semanticCoverage], or keep the last figure
// on the error path, and this fails.
func TestAFailedCoverageScanIsUnknownUntilTheNextTick(t *testing.T) {
	t.Parallel()
	scans, fail := 0, false
	r := &retention{coverage: func(context.Context) (float64, bool, error) {
		scans++
		if fail {
			return 0, false, errors.New("the corpus is unreadable")
		}
		return 0.5, true, nil
	}}
	r.measureCoverage(t.Context())
	if got := r.semanticCoverage(); got == nil || *got != 0.5 {
		t.Fatalf("a measured corpus read back as %v, want 0.5", got)
	}

	fail = true
	r.measureCoverage(t.Context())
	for range 3 {
		if got := r.semanticCoverage(); got != nil {
			t.Fatalf("a failed scan read back as coverage %v rather than unknown", *got)
		}
	}
	if scans != 2 {
		t.Errorf("two ticks and three reads ran %d scans, want one per tick", scans)
	}
}

// THE HISTORY ALARM NAMES THE BUDGET THE FAN-OUT WAITS ON, the fan-out's own
// number. statelog cannot import its layer, so the reading carries it; a copy
// anywhere else would drift when the fan-out is re-tuned.
//
// Mutation: drop the assignment from [retention.observed], and this fails.
func TestTheRetentionReadingCarriesTheFleetReadBudget(t *testing.T) {
	t.Parallel()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	r := &retention{metrics: recorder}
	var reading statelog.Reading
	r.observed(&reading, r.measures().window())
	if reading.HistoryReadBudget != eventfan.FleetReadBudget {
		t.Errorf("the reading carries a %s history read budget, want the fan-out's %s",
			reading.HistoryReadBudget, eventfan.FleetReadBudget)
	}
}
