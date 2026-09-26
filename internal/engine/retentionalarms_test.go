package engine

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ALARM COUNT A NODE'S HEALTH CARRIES IS THE RETENTION REPORT'S ALARMS.
//
// [Engine.Alarms] is what every health probe and push tick reads, and it is the
// evaluation the gauge and the alarm log lines were fed — never a second one.
// So after an evaluation it names exactly the alarms the report that
// evaluation was taken from holds (`work_retention`'s `alarms`), and before
// one it says it cannot say: a node that has not looked at its table is not a
// node with nothing wrong.
//
// Mutation: answer (nil, true) before the first evaluation, or read a list the
// tracker was not handed, and this fails.
func TestTheAlarmCountEqualsTheRetentionAlarms(t *testing.T) {
	t.Parallel()
	r := &retention{
		fleet:  coordmem.NewFleet(),
		state:  &stateLog{},
		nodeID: "node-a",
		alarms: statelog.NewTracker(nil, nil),
		pooled: map[string]poolCounters{},
	}
	e := &Engine{}
	if kinds, evaluated := e.Alarms(); evaluated {
		t.Fatalf("a node running no alarm table answered %v as evaluated", kinds)
	}
	e.alarms.Store(r.alarms)
	if kinds, evaluated := e.Alarms(); evaluated {
		t.Fatalf("a table not yet evaluated answered %v as evaluated", kinds)
	}

	r.evaluate(t.Context())
	report := r.Report(t.Context())
	want := make([]statelog.Kind, 0, len(report.Alarms))
	for _, a := range report.Alarms {
		want = append(want, a.Kind)
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
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the health count names %v and the retention report %v", got, want)
	}
}

// AN ALARM IS EVALUATED INSIDE THE GRACE IT FIRES AT.
//
// The table was evaluated only on the trim's fifteen-minute tick, so
// `apply_lag` — a one-minute grace whose remedy promises a stopped node is
// named within a minute — reached the gauge, the log and the health count up
// to sixteen minutes late. The evaluation runs on the position heartbeat, the
// cadence its fastest input is measured at.
//
// Mutation: set AlarmInterval back to RetentionInterval, and this fails.
func TestAnAlarmIsEvaluatedInsideTheGraceItFiresAt(t *testing.T) {
	t.Parallel()
	if AlarmInterval >= statelog.StallGrace {
		t.Errorf("alarms are evaluated every %s, which is not inside the %s "+
			"stall grace `apply_lag` fires at", AlarmInterval, statelog.StallGrace)
	}
	if AlarmInterval < PositionHeartbeat {
		t.Errorf("alarms are evaluated every %s, faster than the %s heartbeat "+
			"their inputs are measured at, re-reading facts that have not moved",
			AlarmInterval, PositionHeartbeat)
	}
}

// THE HARDWARE MEASUREMENT STAYS ON THE TRIM'S TICK when the alarms move off it.
//
// The pool-wait delta is ONE observation per interval, so the `pool_starved`
// alarm's day-long p95 is a percentile over intervals of the trim's length;
// taken on the ten-second alarm ticker it would be a percentile over ninety
// times as many, shorter intervals — a different alarm nobody decided on. The
// backup-gauge walk reads the coordination register and every announced
// manifest and logs an unreadable directory at WARN each time, so on the alarm
// ticker it repeats that line every ten seconds for as long as the fault lasts.
//
// Mutation: have [retention.evaluate] call [retention.capacity] again, and the
// first half fails; drop it from [retention.tick], and the control fails.
func TestTheAlarmEvaluationLeavesTheHardwareMeasurementToTheTrimTick(t *testing.T) {
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
		r.evaluate(t.Context())
	}
	if waits, holds := measured(); waits != 0 || holds {
		t.Fatalf("three alarm evaluations took the hardware measurement "+
			"(%d pool-wait observation(s), holds gauge published: %v), so the "+
			"`pool_starved` p95 would be over ten-second intervals rather "+
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

// A FAILED COVERAGE SCAN IS REMEMBERED FOR A TRIM TICK, like a successful one.
//
// The reading is assembled on every alarm evaluation — every ten seconds — as
// well as on every operator request, and the scan is of the whole corpus. A
// failure that was not cached re-ran that scan and repeated its WARN line on
// each of them for as long as the fault lasted.
//
// Mutation: return before caching on the error path, and this fails.
func TestAFailedCoverageScanIsNotRepeatedEveryEvaluation(t *testing.T) {
	t.Parallel()
	scans := 0
	r := &retention{coverage: func(context.Context) (float64, bool, error) {
		scans++
		return 0, false, errors.New("the corpus is unreadable")
	}}
	at := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for i := range 3 {
		if got := r.semanticCoverage(t.Context(), at.Add(time.Duration(i)*AlarmInterval)); got != nil {
			t.Fatalf("a failed scan answered coverage %v rather than unknown", *got)
		}
	}
	if scans != 1 {
		t.Errorf("three readings inside one trim tick ran %d scans after the "+
			"first failed", scans)
	}
	// AND IT IS RETRIED once the tick has passed, or a transient fault
	// would leave coverage unknown for the life of the process.
	r.semanticCoverage(t.Context(), at.Add(RetentionInterval))
	if scans != 2 {
		t.Errorf("a reading a whole trim tick later ran %d scans in total, "+
			"want the failure retried", scans)
	}
}
