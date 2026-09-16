package engine

import (
	"context"
	"testing"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE THREE ALARMS ABOUT THE MACHINE, and the fields nothing filled.
//
// `volume_low`, `wal_large` and `pool_starved` each read a field of
// [statelog.Reading] that no code assigned, so all three were permanently
// silent — which on a node running out of disk is the worst possible silence.
// Filling the field is only half of it: a field filled in and never read is
// the same silence with an extra step, so this asserts the table fires too.
func TestTheCapacityAlarmsFireOnWhatThisNodesDiskIsDoing(t *testing.T) {
	t.Parallel()
	db, err := store.Open(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	r := &retention{db: db}
	var reading statelog.Reading
	r.space(&reading)

	if reading.StoreBytes <= 0 {
		t.Error("an open store measured zero bytes, so `volume_low` compares " +
			"free space against nothing and can never fire")
	}
	if reading.FreeBytes <= 0 {
		t.Error("the volume measured zero free bytes, which is not a disk any " +
			"test runs on — the statfs did not happen")
	}
	// A -wal MAY OR MAY NOT EXIST on a freshly opened store, and both are
	// legitimate. What must never happen is an error becoming a size.
	if reading.WALBytes < 0 {
		t.Errorf("the write-ahead log measured %d bytes", reading.WALBytes)
	}

	// AND THE TABLE FIRES. The numbers are forced rather than provoked:
	// filling a real volume is not a test, and what is under test here is
	// that the reading reaches the condition.
	fired := map[statelog.Kind]bool{}
	for _, alarm := range statelog.Evaluate(statelog.Reading{
		FreeBytes: 1 << 20, StoreBytes: 4 << 30,
		WALBytes:    statelog.WALAlarmBytes + 1,
		PoolWaitP95: statelog.PoolWaitAlarm + time.Millisecond,
	}) {
		fired[alarm.Kind] = true
	}
	for _, kind := range []statelog.Kind{
		statelog.KindVolumeLow, statelog.KindWALLarge, statelog.KindPoolStarved,
	} {
		if !fired[kind] {
			t.Errorf("%s did not fire on a reading that says it should", kind)
		}
	}
}

// A MISSING SIDECAR IS ZERO BYTES, NOT AN UNMEASURABLE ONE.
//
// A database with nothing uncheckpointed has no -wal at all, which is the
// healthiest state `wal_large` has. Reading that as a failure would leave the
// field unset — indistinguishable from the same node with a gibibyte in it.
func TestAnAbsentWriteAheadLogMeasuresZeroRatherThanFailing(t *testing.T) {
	t.Parallel()
	got, err := fileBytes(t.TempDir() + "/nothing-is-here-wal")
	if err != nil {
		t.Fatalf("a missing sidecar reported an error: %v", err)
	}
	if got != 0 {
		t.Errorf("a missing sidecar measured %d bytes", got)
	}
	// THE CONTROL: a file that exists is measured rather than reported as
	// zero, or the assertion above would pass on a helper that always
	// answers zero.
	if got, err := fileBytes("retentioncapacity.go"); err != nil || got <= 0 {
		t.Errorf("this source file measured %d bytes (%v), so the helper "+
			"cannot tell an absent file from a present one", got, err)
	}
}

// THE POOL WAIT IS A DELTA, and a tick in which nobody queued is not a tick in
// which somebody queued for no time.
//
// `sql.DBStats` counts since the process started. Recorded raw, the histogram
// would fill with one ever-growing total and its p95 would be a number that
// only rises — so `pool_starved`, once fired, could never clear. Recorded as a
// delta with no observation when the delta is empty, the series describes the
// interval it was taken over.
func TestAnIdlePoolRecordsNothingRatherThanAZeroWait(t *testing.T) {
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

	r := &retention{db: db, metrics: recorder, pooled: map[string]poolCounters{}}
	for range 3 {
		r.poolWait(db, "index.db")
	}
	for _, snapshot := range recorder.Read() {
		if snapshot.Name == metrics.StorePoolWait {
			t.Fatalf("an idle pool recorded %d observation(s), so the p95 "+
				"`pool_starved` fires on is diluted by ticks nobody waited in",
				snapshot.Count)
		}
	}

	// THE CONTROL, on the arithmetic rather than on a provoked queue: a
	// pool that HAS waited records the interval's mean, which is what a
	// tick's worth of queuing means when the driver reports a total and a
	// count rather than the individual waits.
	r.pooled["index.db"] = poolCounters{count: -4, waited: -2 * time.Second}
	r.poolWait(db, "index.db")
	var observations int
	for _, snapshot := range recorder.Read() {
		if snapshot.Name == metrics.StorePoolWait {
			observations = int(snapshot.Count)
		}
	}
	if observations != 1 {
		t.Errorf("a pool with four waits behind it recorded %d observation(s)",
			observations)
	}
}

// THE FOUR WINDOWED COUNTERS FOUR MORE ALARMS READ, and nothing read them.
//
// `pool_starved`, `records_gated`, `feed_unreadable` and `census_drift` each
// take a field of [statelog.Reading] off this process's own recorder. Every
// one of those fields was left at its zero value, so four conditions in a
// twenty-row table could not fire — and three of them are the only report
// their subject has: a gated record is recoverable by nothing, an
// untranslatable change record blocks every wake behind it, and a company that
// has outgrown its log's sizing has no other symptom until the log is full.
func TestTheWindowedCountersReachTheAlarmsThatFireOnThem(t *testing.T) {
	t.Parallel()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	recorder.Observe(metrics.StorePoolWait, 400*time.Millisecond,
		metrics.Attrs{"file": "index.db"})
	recorder.Add(metrics.StatelogRecordsGated, 2,
		metrics.Attrs{"gate": "deleted", "subject_kind": "task"})
	recorder.Add(metrics.TrackerFeedUnreadable, 3, metrics.Attrs{"source": "tracker"})
	recorder.Add(metrics.StatelogBarrierAppends,
		uint64(3*statelog.LinearizableReadsPerDay), metrics.Attrs{"domain": "tracker"})

	r := &retention{metrics: recorder}
	var reading statelog.Reading
	r.observed(&reading)

	if reading.PoolWaitP95 <= 0 {
		t.Error("a recorded connection wait left the p95 at zero, so " +
			"`pool_starved` has no input")
	}
	if reading.RecordsGated != 2 {
		t.Errorf("two gated records reached the reading as %d", reading.RecordsGated)
	}
	if reading.FeedUnreadable != 3 {
		t.Errorf("three untranslatable records reached the reading as %d",
			reading.FeedUnreadable)
	}
	if reading.LinearizableReadsExpected != statelog.LinearizableReadsPerDay {
		t.Errorf("the declared read rate reached the reading as %d, so "+
			"`census_drift` compares against nothing",
			reading.LinearizableReadsExpected)
	}

	fired := map[statelog.Kind]bool{}
	for _, alarm := range statelog.Evaluate(reading) {
		fired[alarm.Kind] = true
	}
	for _, kind := range []statelog.Kind{
		statelog.KindPoolStarved, statelog.KindRecordsGated,
		statelog.KindFeedUnreadable, statelog.KindCensusDrift,
	} {
		if !fired[kind] {
			t.Errorf("%s did not fire on a reading that says it should", kind)
		}
	}

	// THE CONTROL: an untouched recorder fires none of them, or every
	// assertion above would pass on a table that always fires.
	quiet, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	var empty statelog.Reading
	(&retention{metrics: quiet}).observed(&empty)
	for _, alarm := range statelog.Evaluate(empty) {
		switch alarm.Kind {
		case statelog.KindPoolStarved, statelog.KindRecordsGated,
			statelog.KindFeedUnreadable, statelog.KindCensusDrift:
			t.Errorf("%s fired on a node that has recorded nothing", alarm.Kind)
		}
	}
}

// THE ALARM TABLE HAD ONE SURFACE OF THREE.
//
// [statelog.Tracker] turns each evaluation into the two surfaces that are not
// a screen — the `crewlet.alarm.active{kind}` gauge a collector scrapes, and
// one WARN on entry and one on exit — and it had no production caller at all.
// So an alarm reached whoever happened to be looking at `work_retention` and
// nothing else: no collector series, no log line, no page.
//
// AND IT IS EVALUATED ON A NODE THAT HOLDS NO DUTY, which is the other half. A
// reading describes ONE node, so a table evaluated only where the trim's
// singleton lease happens to sit would report the lease holder's health as the
// fleet's — and the wedged node is the one nobody hears from.
func TestTheAlarmTableIsEvaluatedOnANodeThatHoldsNoDuty(t *testing.T) {
	t.Parallel()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	refused := 0
	r := &retention{
		fleet:   coordmem.NewFleet(),
		state:   &stateLog{},
		nodeID:  "node-a",
		metrics: recorder,
		alarms:  statelog.NewTracker(recorder, nil),
		pooled:  map[string]poolCounters{},
		claim: func(context.Context) (bool, error) {
			refused++
			return false, nil
		},
	}
	r.tick(t.Context())

	if refused != 1 {
		t.Fatalf("the duty was asked for %d time(s); this test is not "+
			"exercising the path it names", refused)
	}
	var series int
	for _, snapshot := range recorder.Read() {
		if snapshot.Name == metrics.AlarmActive {
			series++
		}
	}
	if series != len(statelog.Kinds()) {
		t.Errorf("a tick on a node holding no duty published %d of %d alarm "+
			"series — every kind is written on every observation, firing or "+
			"not, because a series that disappears reads as `no data` on "+
			"every dashboard", series, len(statelog.Kinds()))
	}
}
