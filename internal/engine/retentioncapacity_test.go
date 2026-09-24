package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE ALARMS ABOUT THE MACHINE FIRE ON WHAT THIS NODE'S STORAGE MEASURES.
//
// `volume_low` and `wal_large` each fire on a field of [statelog.Reading] that
// [retention.space] fills from this node's own files and volume, and
// `pool_starved` on one [retention.observed] fills; a field nothing fills is a
// condition that never fires, which on a node running out of disk is the worst
// possible silence. Filling the field is only half of it: a field filled in and
// never read is the same silence with an extra step, so this asserts the table
// fires too.
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

// THE WINDOWED COUNTERS REACH THE ALARMS THAT FIRE ON THEM.
//
// `pool_starved`, `records_gated`, `feed_unreadable` and `census_drift` each
// take a field of [statelog.Reading] off this process's own recorder, through
// [retention.observed], and a field left at its zero value is a condition that
// cannot fire. So both halves are asserted: what the recorder counted reaches
// the reading, and the table fires on it.
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

// THE ALARM TABLE REACHES A COLLECTOR, FROM A NODE THAT HOLDS NO DUTY.
//
// [statelog.Tracker] turns each evaluation into the two surfaces that are not
// a screen — the `crewlet.alarm.active{kind}` gauge a collector scrapes, and
// one WARN on entry and one on exit. An alarm that reached only
// `work_retention` would reach whoever happened to be looking at it and
// nothing else: no collector series and no log line.
//
// AND IT IS EVALUATED ON A NODE THAT HOLDS NO DUTY. A reading describes ONE
// node, so a table evaluated only where the trim's singleton lease happens to
// sit would report the lease holder's health as the fleet's — and the wedged
// node is the one nobody hears from.
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

// A COVERAGE SCAN THAT FAILS IS TRIED AGAIN A TRIM INTERVAL LATER, not on the
// next report.
//
// The scan reads the whole source corpus, and a report asks for it on every
// alarm pass and every operator request. Cached as nothing known, a failure
// costs one scan and one WARN per trim interval while it lasts; uncached, both
// repeat on every report. The answer meanwhile is nil rather than a fraction:
// an unreadable corpus is not an uncovered one, so `recall_below_floor` has
// nothing to judge.
//
// Mutation: leave a failed attempt uncached and the second report scans again.
func TestAFailedCoverageScanIsTriedAgainATrimIntervalLater(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	scans, failing := 0, true
	r := &retention{
		logger: slog.New(slog.NewJSONHandler(&out, nil)),
		coverage: func(context.Context) (float64, bool, error) {
			scans++
			if failing {
				return 0, false, errors.New("the corpus could not be read")
			}
			return 0.5, true, nil
		},
	}
	began := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	for _, after := range []time.Duration{0, AlarmInterval, RetentionInterval - time.Second} {
		if got := r.semanticCoverage(t.Context(), began.Add(after)); got != nil {
			t.Errorf("%v after a failed scan the coverage read %s, want nothing "+
				"known", after, coverageText(got))
		}
	}
	if scans != 1 {
		t.Errorf("three reports inside one trim interval scanned %d time(s), want 1",
			scans)
	}
	if got := countLogged(t, &out, slog.LevelWarn, "vector_coverage_unreadable"); got != 1 {
		t.Errorf("one failed scan was written down %d time(s), want 1", got)
	}

	// ONE INTERVAL ON, the next report scans again, and what it measures is
	// the answer.
	failing = false
	got := r.semanticCoverage(t.Context(), began.Add(RetentionInterval))
	if scans != 2 || got == nil || *got != 0.5 {
		t.Errorf("a report a trim interval after the failure scanned %d time(s) "+
			"in all and read %s, want 2 and 0.5", scans, coverageText(got))
	}
}

// A SCAN ITS CALLER GAVE UP ON IS NOT A FAILED SCAN.
//
// A report runs on the API request's context, so a client that disconnects
// mid-scan ends the scan with its own cancellation — which says nothing about
// the corpus. Cached as a failure it would blank this node's coverage for a
// whole trim interval and write `vector_coverage_unreadable` about a corpus
// that reads perfectly well. So nothing is cached or written, and the next
// report, however soon, scans and answers.
//
// Mutation: drop the arm that reads the caller's context and the cancellation
// is cached as a failure — the next report inside the interval answers nothing
// known without scanning.
func TestACoverageScanItsCallerAbandonedIsNotAFailure(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	scans := 0
	r := &retention{
		logger: slog.New(slog.NewJSONHandler(&out, nil)),
		coverage: func(ctx context.Context) (float64, bool, error) {
			scans++
			if err := ctx.Err(); err != nil {
				return 0, false, err
			}
			return 0.5, true, nil
		},
	}
	now := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	gone, cancel := context.WithCancel(t.Context())
	cancel()
	if got := r.semanticCoverage(gone, now); got != nil {
		t.Errorf("a scan its caller abandoned read %s, want nothing known",
			coverageText(got))
	}
	if got := countLogged(t, &out, slog.LevelWarn, "vector_coverage_unreadable"); got != 0 {
		t.Errorf("an abandoned scan was written down as unreadable %d time(s), "+
			"want 0 — the corpus was never the problem", got)
	}

	got := r.semanticCoverage(t.Context(), now.Add(AlarmInterval))
	if scans != 2 || got == nil || *got != 0.5 {
		t.Errorf("the report after an abandoned scan scanned %d time(s) in all "+
			"and read %s, want 2 and 0.5", scans, coverageText(got))
	}
}

// A REPORT ASSEMBLED WHILE A COVERAGE SCAN RUNS DOES NOT START ANOTHER.
//
// The tick and every API request assemble a report on goroutines of their own,
// and the scan reads the whole source corpus. The attempt is claimed before it
// is made, so a report that finds the cache due while another is scanning
// answers from what was measured before — here, nothing yet — rather than
// scanning the same corpus a second time.
//
// Mutation: stamp the attempt only once the scan returns and the second report
// scans too.
func TestAReportAssembledDuringACoverageScanDoesNotStartAnother(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var scans atomic.Int32
	r := &retention{coverage: func(context.Context) (float64, bool, error) {
		if scans.Add(1) == 1 {
			close(entered)
			<-release
		}
		return 0.5, true, nil
	}}
	now := time.Date(2026, 9, 24, 9, 0, 0, 0, time.UTC)
	first := make(chan *float64, 1)
	go func() { first <- r.semanticCoverage(context.Background(), now) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the first report never started its scan")
	}

	second := r.semanticCoverage(t.Context(), now)
	close(release)
	if second != nil {
		t.Errorf("a report during the first scan read %s, want nothing known yet",
			coverageText(second))
	}
	if got := <-first; got == nil || *got != 0.5 {
		t.Errorf("the report that claimed the scan read %s, want 0.5", coverageText(got))
	}
	if n := scans.Load(); n != 1 {
		t.Errorf("two reports at one instant scanned %d time(s), want 1", n)
	}
}

// coverageText renders a coverage answer for a failure message.
func coverageText(fraction *float64) string {
	if fraction == nil {
		return "nothing known"
	}
	return strconv.FormatFloat(*fraction, 'g', -1, 64)
}
