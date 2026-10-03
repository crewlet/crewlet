package engine

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ALARMS A NODE'S HEALTH CARRIES ARE THE RETENTION REPORT'S ALARMS.
//
// [Engine.Alarms] is what every health probe and push tick reads, and it is the
// evaluation the gauge and the alarm log lines were fed — never a second one.
// So after an evaluation it names exactly the alarms the report that
// evaluation was taken from holds (`work_retention`'s `alarms`), PER LOG — the
// kind and the log it is about, because the same kind on two logs is two
// alarms — and before one it says it cannot say: a node that has not looked at
// its table is not a node with nothing wrong.
//
// Mutation: answer (nil, true) before the first evaluation, read a list the
// tracker was not handed, or drop the domain from a standing alarm, and this
// fails.
func TestTheHealthAlarmsAreTheRetentionAlarms(t *testing.T) {
	t.Parallel()
	r := &retention{
		fleet:  coordmem.NewFleet(),
		state:  &stateLog{},
		nodeID: "node-a",
		alarms: statelog.NewTracker(nil, nil),
		pooled: map[string]poolCounters{},
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
	want := make([]statelog.StandingAlarm, 0, len(report.Alarms))
	for _, a := range report.Alarms {
		want = append(want, statelog.StandingAlarm{Kind: a.Kind, Domain: a.Domain})
	}
	// A FLOOR: a fleet with no backup recorded raises `backup_age`, so a
	// comparison of two empty lists cannot pass as agreement here.
	if len(want) == 0 {
		t.Fatal("the report raised no alarm, so this compares nothing")
	}
	got, evaluated := e.Alarms()
	if !evaluated {
		t.Fatal("an evaluated table answered as not evaluated")
	}
	order := func(a, b statelog.StandingAlarm) int {
		return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Domain, b.Domain))
	}
	slices.SortFunc(got, order)
	slices.SortFunc(want, order)
	if !slices.Equal(got, want) {
		t.Errorf("the health envelope names %v and the retention report %v", got, want)
	}
}

// THE HEARTBEAT LEAVES THE HARDWARE MEASUREMENT TO THE TRIM'S TICK.
//
// The pool-wait delta is ONE observation per interval, so the `pool_starved`
// alarm's day-long p95 is a percentile over intervals of the trim's length;
// taken on every [statelog.AlarmInterval] beat it would be a percentile over
// sixty times as many, shorter intervals — a different alarm nobody decided
// on. The backup-gauge walk reads the coordination register and every announced
// manifest and logs an unreadable directory at WARN each time, so on the beat
// it repeats that line every fifteen seconds for as long as the fault lasts.
//
// Mutation: have [retention.beat] call [retention.capacity], and the first half
// fails; drop it from [retention.evaluate], and the control fails.
func TestTheHeartbeatLeavesTheHardwareMeasurementToTheTrimTick(t *testing.T) {
	t.Parallel()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	r := &retention{
		db:      db,
		fleet:   coordmem.NewFleet(),
		state:   &stateLog{},
		nodeID:  "node-a",
		metrics: recorder,
		alarms:  statelog.NewTracker(recorder, nil),
		// A BASELINE BELOW THE DRIVER'S COUNTERS, so the next pool-wait
		// measurement has a non-empty delta to record — an idle pool
		// records nothing at all, and absence would prove nothing here.
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

	// THE CONTROL: the trim's tick does measure, or the assertion above
	// passes on a node that never measures anything.
	r.tick(t.Context())
	if waits, holds := measured(); waits != 1 || !holds {
		t.Errorf("a trim tick recorded %d pool-wait observation(s) and "+
			"published the holds gauge: %v; want one and true", waits, holds)
	}
}

// A FAILED COVERAGE SCAN IS READ BACK AS UNKNOWN, AND NOT RE-RUN BY A READ.
//
// The scan is of the whole corpus and runs on the trim's tick alone; every
// report — each heartbeat's, each operator request's — reads the tick's figure
// back. A failure that a read re-ran would repeat the scan and its WARN line on
// every beat for as long as the fault lasted, and one that left the previous
// figure behind would report a corpus nobody could read as covered.
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

// THE HISTORY ALARM NAMES THE BUDGET THE FAN-OUT WAITS ON, and that number is
// the fan-out's own. statelog cannot import the layer that defines it, so the
// reading is where it crosses: a reading without it would word the alarm
// around no figure at all, and a copy of it anywhere else would drift the day
// the fan-out was re-tuned.
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
	r.observed(&reading)
	if reading.HistoryReadBudget != eventfan.FleetReadBudget {
		t.Errorf("the reading carries a %s history read budget, want the fan-out's %s",
			reading.HistoryReadBudget, eventfan.FleetReadBudget)
	}
}
