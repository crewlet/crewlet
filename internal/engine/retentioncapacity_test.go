package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/objstore/collect"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE THREE ALARMS ABOUT THE MACHINE ARE FILLED AND FIRE.
//
// `volume_low`, `wal_large` and `pool_starved` each read a field of
// [statelog.Reading]: this asserts the reading fills them and the table fires
// on them, since a field filled and never read is just as silent.
func TestTheCapacityAlarmsFireOnWhatThisNodesDiskIsDoing(t *testing.T) {
	t.Parallel()
	db := storetest.OpenNode(t, t.TempDir()+"/index.db", store.Options{})
	t.Cleanup(func() { _ = db.Close() })

	r := &retention{db: db}
	var reading statelog.Reading
	r.measures().space(&reading, time.Now())

	if reading.StoreBytes <= 0 {
		t.Error("an open store measured zero bytes, so `volume_low` compares " +
			"free space against nothing and can never fire")
	}
	if reading.FreeBytes <= 0 {
		t.Error("the volume measured zero free bytes, which is not a disk any " +
			"test runs on — the statfs did not happen")
	}
	// A fresh store may or may not have a -wal; an error must never
	// become a size.
	if reading.WALBytes < 0 {
		t.Errorf("the write-ahead log measured %d bytes", reading.WALBytes)
	}

	// And the table fires, on forced numbers: what is under test is that
	// the reading reaches the condition, not filling a real volume.
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

// THE VOLUME ALARM IS ABOUT THE VOLUME WITH THE LEAST ROOM, measured per
// volume (`store.replicated_path`). With the replicated estate on a nearly full
// volume and the node estate on an empty one, the reading is the full one's
// free space against its own file alone, and the alarm NAMES it, so an
// operator knows which disk to grow. An unmeasurable volume is alarmed on,
// never skipped for the roomy one.
func TestTheVolumeAlarmReadsEachFilesOwnVolume(t *testing.T) {
	t.Parallel()
	nodeDir, replicatedDir := t.TempDir(), t.TempDir()
	db, estate := storetest.OpenEstate(t, nodeDir+"/company.db", store.Options{
		ReplicatedPath: replicatedDir + "/crewlet-replicated.db"}, 1)
	t.Cleanup(func() { _ = db.Close() })
	replicated := storetest.ReplicatedDB(t, estate)
	nodeSize, err := fileBytes(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	replicatedSize, err := fileBytes(replicated.Path())
	if err != nil {
		t.Fatal(err)
	}
	free := map[string]int64{nodeDir: 1 << 40, replicatedDir: 1 << 10}
	r := &retention{db: db, volumes: func(dir string) (string, int64, error) {
		return dir, free[dir], nil
	}}
	var reading statelog.Reading
	r.measures().space(&reading, time.Now())
	if reading.FreeBytes != 1<<10 || reading.StoreBytes != replicatedSize ||
		reading.StoreVolume != replicatedDir {
		t.Fatalf("the reading is %d free against %d stored on %q, want the replicated "+
			"volume's %d against its own %d on %q", reading.FreeBytes, reading.StoreBytes,
			reading.StoreVolume, 1<<10, replicatedSize, replicatedDir)
	}
	alarms := statelog.Evaluate(reading)
	if len(alarms) != 1 || alarms[0].Kind != statelog.KindVolumeLow {
		t.Fatalf("a nearly full volume raised %v, want volume_low", alarms)
	}
	if !strings.Contains(alarms[0].Detail, replicatedDir) {
		t.Errorf("volume_low does not name the volume to grow: %q", alarms[0].Detail)
	}

	// An unmeasurable volume beside a roomy one is the alarm, naming it.
	r.volumes = func(dir string) (string, int64, error) {
		if dir == replicatedDir {
			return "", 0, fmt.Errorf("engine: measure the free space on %s: input/output error", dir)
		}
		return dir, 1 << 40, nil
	}
	reading = statelog.Reading{}
	r.measures().space(&reading, time.Now())
	if reading.StoreVolume != nodeDir || !strings.Contains(reading.StoreVolumeUnmeasured, replicatedDir) {
		t.Fatalf("the reading is of %q with %q unmeasured, want the node volume read and the "+
			"replicated one named", reading.StoreVolume, reading.StoreVolumeUnmeasured)
	}
	alarms = statelog.Evaluate(reading)
	if len(alarms) != 1 || alarms[0].Kind != statelog.KindVolumeLow ||
		!strings.Contains(alarms[0].Detail, replicatedDir) {
		t.Errorf("an unmeasurable store volume raised %v, want volume_low naming it", alarms)
	}

	// ONE VOLUME UNDER BOTH is one volume's bytes, summed.
	r.volumes = func(string) (string, int64, error) { return "one", 1 << 40, nil }
	reading = statelog.Reading{}
	r.measures().space(&reading, time.Now())
	if reading.StoreBytes != nodeSize+replicatedSize {
		t.Errorf("one volume holding both files is %d stored, want %d", reading.StoreBytes,
			nodeSize+replicatedSize)
	}
}

// A COPY HOLDING THE LOG IS NAMED, AND WHAT IT HELD BACK IS ROOM THE NEXT ONE
// NEEDS.
//
// A copy stops every checkpoint for its whole run (store.DB.Copies). While the
// replicated estate is copied and written, the reading names the file with the
// largest log and the copy holding it, and `wal_large` says so rather than
// blaming a reader. Once done, the estate's volume counts what it held back:
// room for a second copy and its tenth but not that log fires `volume_low`;
// room for both does not.
func TestTheReadingNamesACopyHoldingTheLogAndCountsWhatItHoldsBack(t *testing.T) {
	t.Parallel()
	hold := storetest.NewCopyHold()
	nodeDir, replicatedDir := t.TempDir(), t.TempDir()
	db, estate := storetest.OpenEstate(t, nodeDir+"/company.db", store.Options{
		WrapDriver: hold.Wrap, ReplicatedPath: replicatedDir + "/crewlet-replicated.db"}, 1)
	t.Cleanup(func() { _ = db.Close() })
	replicated := storetest.ReplicatedDB(t, estate)
	ctx := t.Context()
	if err := estate.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`CREATE TABLE crewlet_copy_held (id INTEGER PRIMARY KEY, pad BLOB NOT NULL)`)
		return err
	}); err != nil {
		t.Fatalf("create the table written beside the copy: %v", err)
	}
	r := &retention{db: db}

	var during statelog.Reading
	var nodeLog, estateLog int64
	storetest.HoldCopy(t, replicated, hold, func() {
		for i := range 64 {
			if err := estate.Tx(ctx, func(tx *sql.Tx) error {
				_, err := tx.ExecContext(ctx, `INSERT INTO crewlet_copy_held (id, pad)
					VALUES (?, zeroblob(4096))`, i)
				return err
			}); err != nil {
				t.Fatalf("write row %d beside the copy: %v", i, err)
			}
		}
		nodeLog = mustFileBytes(t, db.Path()+walSuffix)
		estateLog = mustFileBytes(t, replicated.Path()+walSuffix)
		r.measures().space(&during, time.Now())
	})
	if estateLog <= nodeLog {
		t.Fatalf("the estate's log is %d bytes beside the node file's %d after the writes "+
			"its copy held back, so the largest log is not the one the case is about",
			estateLog, nodeLog)
	}
	if during.WALFile != filepath.Base(replicated.Path()) || during.WALBytes != estateLog {
		t.Fatalf("the reading names the log beside %q at %d bytes, want the replicated "+
			"estate's at %d", during.WALFile, during.WALBytes, estateLog)
	}
	if !strings.HasSuffix(during.WALCopy, ".part") || during.WALCopyFor <= 0 {
		t.Fatalf("while a copy held the estate's log the reading named %q holding it for %s, "+
			"want the copy's part file and how long", during.WALCopy, during.WALCopyFor)
	}
	// The table says it, past a threshold no test writes its way to.
	large := during
	large.WALBytes = statelog.WALAlarmBytes + 1
	alarm, fired := alarmOf(statelog.Report{Alarms: statelog.Evaluate(large)}, statelog.KindWALLarge)
	if !fired || !strings.Contains(alarm.Detail, during.WALCopy) ||
		!strings.Contains(alarm.Detail, "folds it back in") {
		t.Fatalf("a large log a copy holds raised %+v (fired %v), want wal_large naming the "+
			"copy and that its end folds the log back in", alarm, fired)
	}

	var after statelog.Reading
	r.measures().space(&after, time.Now())
	if after.WALCopy != "" {
		t.Errorf("a copy that ended is still named holding the log: %q", after.WALCopy)
	}
	size := mustFileBytes(t, replicated.Path())
	held := replicated.CopyHoldsBack(size)
	if held <= 0 {
		t.Fatalf("the estate measured a copy holding back %d after one held its log under "+
			"64 commits", held)
	}
	// Room for a second copy and its tenth, but not the log as well.
	room := int64(math.Ceil(statelog.VolumeHeadroomFactor * float64(size)))
	free := map[string]int64{nodeDir: 1 << 40, replicatedDir: room}
	r.volumes = func(dir string) (string, int64, error) { return dir, free[dir], nil }
	var tight statelog.Reading
	r.measures().space(&tight, time.Now())
	if tight.StoreVolume != replicatedDir || tight.CopyHeldBytes != held {
		t.Fatalf("the reading is of %q counting %d held back, want the estate's volume %q "+
			"counting the %d its copy holds back", tight.StoreVolume, tight.CopyHeldBytes,
			replicatedDir, held)
	}
	alarm, fired = alarmOf(statelog.Report{Alarms: statelog.Evaluate(tight)}, statelog.KindVolumeLow)
	if !fired || !strings.Contains(alarm.Detail, "write-ahead log a copy holds back") {
		t.Fatalf("a volume with room for a second copy and not for the log it holds back "+
			"raised %+v (fired %v), want volume_low naming the log", alarm, fired)
	}
	// And room for both is room.
	free[replicatedDir] = room + held
	var roomy statelog.Reading
	r.measures().space(&roomy, time.Now())
	if _, fired := alarmOf(statelog.Report{Alarms: statelog.Evaluate(roomy)}, statelog.KindVolumeLow); fired {
		t.Errorf("a volume with room for a second copy and the log it holds back raised "+
			"volume_low: %+v", roomy)
	}
}

// mustFileBytes is fileBytes failing the case on an error.
func mustFileBytes(t *testing.T, path string) int64 {
	t.Helper()
	n, err := fileBytes(path)
	if err != nil {
		t.Fatalf("measure %s: %v", path, err)
	}
	return n
}

// THE TIGHTEST VOLUME HAS THE LEAST ROOM FOR THE NEXT COPY OF ITS OWN BYTES AND
// THE LOG THAT COPY HOLDS BACK, and a volume holding nothing is never it.
func TestTheTightestVolumeIsTheOneNearestItsAlarm(t *testing.T) {
	t.Parallel()
	file := func(volume string, free, size int64) storedFile {
		return storedFile{volume: volume, dir: "/" + volume, free: free, size: size}
	}
	holding := func(f storedFile, held int64) storedFile {
		f.held = held
		return f
	}
	for name, tc := range map[string]struct {
		files []storedFile
		want  storeVolume
	}{
		"no files": {nil, storeVolume{}},
		"two files, one volume": {[]storedFile{holding(file("a", 100, 10), 3), holding(file("a", 100, 30), 4)},
			storeVolume{"/a", 100, 40, 7}},
		"the smaller ratio, not the smaller free": {
			[]storedFile{file("big", 1000, 900), file("small", 50, 5)}, storeVolume{"/big", 1000, 900, 0}},
		"an empty volume beside a full one": {
			[]storedFile{file("empty", 1, 0), file("full", 10, 100)}, storeVolume{"/full", 10, 100, 0}},
		"a full volume beside an empty one": {
			[]storedFile{file("full", 10, 100), file("empty", 1, 0)}, storeVolume{"/full", 10, 100, 0}},
		// The same free-to-stored ratio on both; the second is tighter for
		// its held-back log: 1 000 free fits a copy of 500, not 400 of log too.
		"the larger log a copy holds back, at the same ratio": {
			[]storedFile{file("quiet", 1000, 500), holding(file("busy", 1000, 500), 400)},
			storeVolume{"/busy", 1000, 500, 400}},
		// Tens of terabytes: int64 cross products overflow and pick the
		// roomier volume.
		"sizes past what a product of int64s holds": {
			[]storedFile{file("a", 14773780071959, 34330076720781), file("b", 27431250736051, 18740855235886)},
			storeVolume{"/a", 14773780071959, 34330076720781, 0}},
		"sizes past what a product of int64s holds, reversed": {
			[]storedFile{file("b", 27431250736051, 18740855235886), file("a", 14773780071959, 34330076720781)},
			storeVolume{"/a", 14773780071959, 34330076720781, 0}},
	} {
		if got := tightestVolume(tc.files); got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
		}
	}
}

// A MISSING SIDECAR IS ZERO BYTES, NOT AN UNMEASURABLE ONE.
//
// Nothing uncheckpointed means no -wal, the healthiest state `wal_large` has;
// read as a failure it would leave the field unset.
func TestAnAbsentWriteAheadLogMeasuresZeroRatherThanFailing(t *testing.T) {
	t.Parallel()
	got, err := fileBytes(t.TempDir() + "/nothing-is-here-wal")
	if err != nil {
		t.Fatalf("a missing sidecar reported an error: %v", err)
	}
	if got != 0 {
		t.Errorf("a missing sidecar measured %d bytes", got)
	}
	// The control: an existing file is measured, or the assertion above
	// passes on a helper that always answers zero.
	if got, err := fileBytes("retentioncapacity.go"); err != nil || got <= 0 {
		t.Errorf("this source file measured %d bytes (%v), so the helper "+
			"cannot tell an absent file from a present one", got, err)
	}
}

// THE POOL WAIT IS A DELTA, and a tick in which nobody queued is not a tick in
// which somebody queued for no time.
//
// `sql.DBStats` counts since the process started: recorded raw, the p95 only
// rises and `pool_starved` could never clear.
func TestAnIdlePoolRecordsNothingRatherThanAZeroWait(t *testing.T) {
	t.Parallel()
	recorder, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	db := storetest.OpenNode(t, t.TempDir()+"/index.db", store.Options{})
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

	// The control, on the arithmetic rather than a provoked queue: a pool
	// that has waited records the interval's mean.
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

// THE FOUR WINDOWED COUNTERS FOUR MORE ALARMS READ REACH THE READING.
//
// `pool_starved`, `records_gated` and `feed_unreadable` each take a field of
// [statelog.Reading] off this process's recorder, and `census_drift` each log's
// ([retention.census]). Three are their subject's only report: a gated record,
// an untranslatable change record, a company outgrowing its log's sizing.
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
	trackerLog := statelog.Domain(worklet.Domain{})
	trackerCensus := statelog.Census{
		Reads: statelog.ReadModel{PerSeatDay: statelog.LinearizableReadsPerSeatDay}, Seats: 100,
	}
	recorder.Add(metrics.StatelogBarriersApplied,
		uint64(3*trackerCensus.Expected()),
		metrics.Attrs{"domain": "tracker", "stream": worklet.Domain{}.Stream().Name})

	r := &retention{metrics: recorder, state: censusLogs(trackerLog),
		seats: func() int { return 100 }}
	var reading statelog.Reading
	window := r.measures().window()
	r.measures().read(&reading, time.Now(), window)
	r.observed(&reading, window)

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
	// The census is each log's ([statelog.DomainInputs.LinearizableReads]).
	held, counted := r.census(r.state.running())[trackerLog.Name()]
	// The tracker's log counts no readers of its own, so no ceiling
	// enters its census ([clientBarriers]).
	expected, _ := held.expectedAt(0, true, 0)
	if !counted || expected != trackerCensus.Expected() ||
		held.reads != 3*expected {
		t.Errorf("the tracker's log reached its census as %d read against %d (counted "+
			"%v), so `census_drift` compares against nothing", held.reads,
			expected, counted)
	}

	fired := map[statelog.Kind]bool{}
	for _, alarm := range statelog.Evaluate(reading) {
		fired[alarm.Kind] = true
	}
	for _, alarm := range statelog.Evaluate(statelog.Reading{
		LinearizableReads: held.reads, LinearizableReadsExpected: expected,
	}) {
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

	// The control: an untouched recorder fires none of them, or the above
	// passes on a table that always fires.
	quiet, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	var empty statelog.Reading
	silent := &retention{metrics: quiet, state: censusLogs(trackerLog),
		seats: func() int { return 100 }}
	silent.observed(&empty, silent.measures().window())
	alarms := statelog.Evaluate(empty)
	for _, c := range silent.census(silent.state.running()) {
		expected, _ := c.expectedAt(0, true, 0)
		alarms = append(alarms, statelog.Evaluate(statelog.Reading{
			LinearizableReads: c.reads, LinearizableReadsExpected: expected,
		})...)
	}
	for _, alarm := range alarms {
		switch alarm.Kind {
		case statelog.KindPoolStarved, statelog.KindRecordsGated,
			statelog.KindFeedUnreadable, statelog.KindCensusDrift:
			t.Errorf("%s fired on a node that has recorded nothing", alarm.Kind)
		}
	}
}

// THE ALARM TABLE REACHES ALL THREE SURFACES, ON A NODE HOLDING NO DUTY.
//
// [statelog.Tracker] turns each evaluation into the `crewlet.alarm.active{kind}`
// gauge and one WARN on entry and one on exit, beside `work_retention`. A
// reading describes ONE node, so a table evaluated only on the trim's lease
// holder would report its health as the fleet's.
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

// THE VECTOR COVERAGE IS MEASURED ON EVERY TICK, AND ONLY ON A TICK.
//
// A scan gated on the cached figure's age is skipped whenever the last tick
// ran long, leaving `recall_below_floor` an interval stale; and a report runs
// on every dashboard poll, so the scan must never run from one.
func TestTheVectorCoverageIsMeasuredOnEveryTickAndReadByTheReport(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	scans := 0
	fraction := 0.99
	var fail error
	r := &retention{coverage: func(context.Context) (float64, bool, error) {
		scans++
		return fraction, true, fail
	}}
	if got := r.semanticCoverage(); got != nil {
		t.Fatalf("a node that has never ticked read a coverage of %v", *got)
	}

	r.measureCoverage(ctx)
	fraction = 0.5
	// The next tick, however little time drift left between them.
	r.measureCoverage(ctx)
	if scans != 2 {
		t.Fatalf("two ticks scanned %d time(s): a tick skipped its measurement", scans)
	}
	for range 5 {
		if got := r.semanticCoverage(); got == nil || *got != 0.5 {
			t.Fatalf("the report read %v, want the latest tick's 0.5", got)
		}
	}
	if scans != 2 {
		t.Errorf("reading the report scanned the corpus %d more time(s)", scans-2)
	}

	// A failed scan leaves nothing: an unreadable corpus is not the
	// covered one the last tick saw.
	fail = errors.New("store closed")
	r.measureCoverage(ctx)
	if got := r.semanticCoverage(); got != nil {
		t.Errorf("a failed scan left a coverage of %v standing", *got)
	}
}

// censusLogs is a state log running the given domains' logs, with nothing
// behind them but what the census reads: which domain each is, and its stream.
func censusLogs(domains ...statelog.Domain) *stateLog {
	running := make([]*runningLog, 0, len(domains))
	for _, d := range domains {
		running = append(running, &runningLog{domain: d, spec: d.Stream()})
	}
	return runsLogs(&stateLog{}, running...)
}

// A LOG IS HELD TO ITS OWN CENSUS, AT THE RATE IT RECEIVES.
//
// The census scales with the company's seats, and is compared with the rate
// the log receives from every node, which its sizing is about, not this node's
// share. The vector log, which no read appends to, is held to nothing.
//
// The engine's own reads count too: the object collector pins the tracker's
// estate on a fixed cadence, which would push a small company past its seats'
// share.
func TestALogIsHeldToItsOwnCensus(t *testing.T) {
	t.Parallel()
	var trackerLog, vectorLog statelog.Domain = worklet.Domain{}, search.Domain{}
	var pagesLog, iamLog statelog.Domain = doclet.Domain{}, iamdomain.Domain{}
	var chatLog, executorLog statelog.Domain = chatlet.Domain{}, executordomain.Domain{}

	for _, tc := range []struct {
		name       string
		logs       []statelog.Domain
		seats      int
		background map[string]int
		applied    map[statelog.Domain]int
		appended   int
		fires      bool
		log        string
		expected   int
	}{
		{
			name: "one seat, at its census beside the collector",
			// 125 a day from the seat plus the collector's pins,
			// both in the census.
			logs: []statelog.Domain{trackerLog}, seats: 1,
			background: map[string]int{"tracker": collect.PinsPerDay},
			applied:    map[statelog.Domain]int{trackerLog: 125 + collect.PinsPerDay},
			fires:      false, log: "tracker", expected: 125 + collect.PinsPerDay,
		},
		{
			name: "the collector is no cover past twice the whole census",
			logs: []statelog.Domain{trackerLog}, seats: 1,
			background: map[string]int{"tracker": collect.PinsPerDay},
			applied:    map[statelog.Domain]int{trackerLog: 2*(125+collect.PinsPerDay) + 1},
			fires:      true, log: "tracker", expected: 125 + collect.PinsPerDay,
		},
		{
			name: "a 200-seat company at 1.2 times its per-seat census",
			// 30 000 a day: past twice the fixed 12 500, inside twice
			// this company's 25 000.
			logs: []statelog.Domain{trackerLog}, seats: 200,
			applied: map[statelog.Domain]int{trackerLog: 30_000},
			fires:   false, log: "tracker", expected: 25_000,
		},
		{
			name: "a 100-seat company at three times its census",
			logs: []statelog.Domain{trackerLog}, seats: 100,
			applied: map[statelog.Domain]int{trackerLog: 37_501},
			fires:   true, log: "tracker", expected: 12_500,
		},
		{
			name: "this node's own appends, however many, are not the log's rate",
			logs: []statelog.Domain{trackerLog}, seats: 100,
			appended: 100_000,
			fires:    false, log: "tracker", expected: 12_500,
		},
		{
			name: "the log's rate, from every node, past this node's own appends",
			logs: []statelog.Domain{trackerLog}, seats: 100,
			applied: map[statelog.Domain]int{trackerLog: 30_000}, appended: 10_000,
			fires: true, log: "tracker", expected: 12_500,
		},
		{
			name: "Doclet's log, held to the whole per-seat rate",
			logs: []statelog.Domain{pagesLog}, seats: 100,
			applied: map[statelog.Domain]int{pagesLog: 12_500},
			fires:   false, log: "pages", expected: 12_500,
		},
		{
			name: "the identity estate's log, whose domain declares no read, quiet",
			// Hundreds of seats and not one barrier: no read of the
			// estate takes one, so its census is zero.
			logs: []statelog.Domain{iamLog}, seats: 300,
			fires: false, log: "iam", expected: 0,
		},
		{
			name: "the identity estate's log on its first barrier",
			// A declared zero is a census: the first barrier is a
			// reader nobody counted, with no bytes in the ceiling.
			logs: []statelog.Domain{iamLog}, seats: 300,
			applied: map[statelog.Domain]int{iamLog: 1},
			fires:   true, log: "iam", expected: 0,
		},
		{
			name: "the chat log, held to its own per-seat rate",
			// 120 a seat a day, chat's own declaration, not the
			// tracker's 125: 12 000 for 100 seats.
			logs: []statelog.Domain{chatLog}, seats: 100,
			applied: map[statelog.Domain]int{chatLog: 12_500},
			fires:   false, log: "chat", expected: 12_000,
		},
		{
			name: "the chat log past twice its own census, inside twice the tracker's",
			// 24 500 a day: under twice the tracker's 12 500, over twice
			// chat's 12 000, so the tracker's rate would stay quiet.
			logs: []statelog.Domain{chatLog}, seats: 100,
			applied: map[statelog.Domain]int{chatLog: 24_500},
			fires:   true, log: "chat", expected: 12_000,
		},
		{
			name: "the executor log, held to its own per-seat rate, past twice the tracker's",
			// 150 a seat a day, the executor's own declaration:
			// 15 000 for 100 seats. 27 000 a day is over twice the
			// tracker's 12 500 and inside twice the executor's, so
			// the tracker's rate would fire here.
			logs: []statelog.Domain{executorLog}, seats: 100,
			applied: map[statelog.Domain]int{executorLog: 27_000},
			fires:   false, log: "executor", expected: 15_000,
		},
		{
			name: "the executor log past twice its own census",
			logs: []statelog.Domain{executorLog}, seats: 100,
			applied: map[statelog.Domain]int{executorLog: 30_001},
			fires:   true, log: "executor", expected: 15_000,
		},
		{
			name: "the vector log, which no read appends to",
			logs: []statelog.Domain{vectorLog, trackerLog}, seats: 1,
			applied: map[statelog.Domain]int{vectorLog: 1_000_000},
			fires:   false, log: "tracker", expected: 125,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder, err := metrics.New()
			if err != nil {
				t.Fatalf("recorder: %v", err)
			}
			for d, n := range tc.applied {
				recorder.Add(metrics.StatelogBarriersApplied, uint64(n),
					metrics.Attrs{"domain": d.Name(), "stream": d.Stream().Name})
			}
			if tc.appended > 0 {
				recorder.Add(metrics.StatelogBarrierAppends, uint64(tc.appended),
					metrics.Attrs{"domain": "tracker"})
			}
			r := &retention{metrics: recorder, state: censusLogs(tc.logs...),
				seats:      func() int { return tc.seats },
				background: func(domain string) int { return tc.background[domain] }}
			census := r.census(r.state.running())
			held, counted := census[tc.log]
			// None of these logs counts readers of its own, so no ceiling
			// enters a census ([clientBarriers]).
			expected, _ := held.expectedAt(0, true, 0)
			if !counted || expected != tc.expected {
				t.Errorf("the census holds %q to %d a day (counted %v), want %d",
					tc.log, expected, counted, tc.expected)
			}
			if _, counted := census[vectorLog.Name()]; counted {
				t.Error("the vector log, which no read appends to, is held to a census")
			}
			// Per log: each log's reads against its own census, and the
			// alarm names the log.
			fired := false
			for _, c := range census {
				sized, _ := c.expectedAt(0, true, 0)
				for _, alarm := range statelog.Evaluate(statelog.Reading{
					LinearizableReads: c.reads, LinearizableReadsExpected: sized,
				}) {
					fired = fired || alarm.Kind == statelog.KindCensusDrift
				}
			}
			if fired != tc.fires {
				t.Errorf("census_drift fired %v on %d a day against %d, want %v",
					fired, held.reads, expected, tc.fires)
			}
		})
	}
}

// THE CENSUS COUNTS THE RUNNING COMPANY'S AGENT SEATS.
//
// A log's census is per seat, so the reading must be handed the running
// company: handed none, it counts one seat and fires on any company past two
// seats' worth of reads.
func TestTheCensusCountsTheRunningCompanysSeats(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNodeOf(t, `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    manages: ["cto", "eng"]
  - name: CTO
    handle: cto
    llm: zulu
  - name: Engineer
    handle: eng
    llm: zulu
`)
	r := e.retention.Load()
	if r == nil {
		t.Fatal("a node running the state log started no trim")
	}
	want := len(e.Company().Seats())
	if want < 2 {
		t.Fatalf("the test company has %d agent seat(s), which cannot tell a "+
			"census counting them from one counting the single seat it never "+
			"goes below", want)
	}
	if r.seats == nil {
		t.Fatalf("the trim was handed no seat count, so the census holds a "+
			"%d-seat company to one seat's reads", want)
	}
	if got := r.seats(); got != want {
		t.Errorf("the census counts %d seats, and the running company has %d", got, want)
	}
	if r.background == nil {
		t.Error("the trim was handed nothing the engine reads on its own, so the " +
			"census holds a small company's log to its seats' reads alone")
	}
}

// THE ENGINE'S OWN READS ARE THE OBJECT COLLECTOR'S PINS, ONCE FOR THE FLEET.
//
// The collector pins every domain a declared table names, hourly and in a
// daily audit, and ONE runs in the fleet: its count, never one per member; on
// any other log, or a node running no object store, nothing.
func TestTheEnginesOwnReadsAreTheCollectorsPins(t *testing.T) {
	t.Parallel()
	if got := (&Engine{}).backgroundBarriers(worklet.Domain{}.Name()); got != 0 {
		t.Errorf("a node with no object store puts %d a day on the tracker's log, want none", got)
	}
	e := &Engine{objects: &objectStore{}}
	if got := e.backgroundBarriers(worklet.Domain{}.Name()); got != collect.PinsPerDay {
		t.Errorf("the collector puts %d a day on the tracker's log, want %d", got, collect.PinsPerDay)
	}
	for _, other := range []statelog.Domain{doclet.Domain{}, search.Domain{}} {
		if got := e.backgroundBarriers(other.Name()); got != 0 {
			t.Errorf("the collector puts %d a day on the %s log, which no declared "+
				"table names", got, other.Name())
		}
	}
	perDay := int(24*time.Hour/collect.CollectInterval) + int(24*time.Hour/collect.AuditInterval)
	if collect.PinsPerDay != perDay {
		t.Errorf("the collector pins %d times a day, and a collection every %v and an "+
			"audit every %v is %d", collect.PinsPerDay, collect.CollectInterval,
			collect.AuditInterval, perDay)
	}
}

// THE BACKUP GAUGE IS THIS NODE'S OWN MANIFESTS, AGED ON THE DUTY'S CLOCK.
//
// `crewlet.backup.age` reads only the points THIS node announced, the only
// disk it can see. A node whose manifest is gone publishes no series, since
// zero is the freshest age. The age is taken on the duty's clock, and a
// manifest from a clock running ahead is zero, not negative.
//
// Mutation: drop the owner check, and the gauge ages node-b's fresher copy;
// read the wall clock instead of [retention.clock], and a manifest finished in
// 2041 is in the wall clock's future and reads zero; set the gauge where no
// manifest was found, and the absent case reads a value.
func TestTheBackupGaugeAgesThisNodesOwnManifestsOnTheDutysClock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// Far from any test machine's wall clock, so an age read off it
	// cannot pass for one read off the duty's.
	at := time.Date(2041, 3, 9, 12, 0, 0, 0, time.UTC)
	manifest := func(t *testing.T, finished time.Time) string {
		t.Helper()
		dir := t.TempDir()
		body, err := json.Marshal(backup.Manifest{
			TakenAt: finished.Add(-time.Minute), FinishedAt: finished,
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, backup.ManifestName), body, 0o600); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	gauge := func(recorder *metrics.Recorder) (float64, bool) {
		for _, snapshot := range recorder.Read() {
			if snapshot.Name == metrics.BackupAge {
				return snapshot.Value, true
			}
		}
		return 0, false
	}
	point := func(owner, dir string) coord.BackupPoint {
		return coord.BackupPoint{Owner: owner, At: at.Add(-time.Hour), Dir: dir,
			Verified: true, Streams: map[string]coord.Position{
				"CREWLET_TRACKER_LOG": {Stream: "CREWLET_TRACKER_LOG", Seq: 1}}}
	}
	measure := func(t *testing.T, points ...coord.BackupPoint) (float64, bool) {
		t.Helper()
		fleet := coordmem.NewFleet()
		for _, p := range points {
			if err := fleet.PutBackupPoint(ctx, p); err != nil {
				t.Fatal(err)
			}
		}
		recorder, err := metrics.New()
		if err != nil {
			t.Fatal(err)
		}
		r := &retention{fleet: fleet, nodeID: "node-a", metrics: recorder,
			now: func() time.Time { return at }}
		r.backupGauges(ctx)
		return gauge(recorder)
	}

	own := manifest(t, at.Add(-6*time.Hour))
	theirs := manifest(t, at.Add(-time.Minute))
	operator := manifest(t, at.Add(-2*time.Minute))
	age, ok := measure(t, point("node-a", own), point("node-b", theirs),
		point(coord.OperatorBackupOwner, operator))
	if !ok || age != (6*time.Hour).Seconds() {
		t.Fatalf("the gauge reads %v (published: %v), want %v — this node's "+
			"own manifest, six hours old on the duty's clock",
			age, ok, (6 * time.Hour).Seconds())
	}

	if age, ok := measure(t, point("node-a", manifest(t, at.Add(time.Hour)))); !ok || age != 0 {
		t.Errorf("a manifest an hour in the duty's future reads %v (published: "+
			"%v), want zero — a negative age compares below every threshold",
			age, ok)
	}

	if age, ok := measure(t, point("node-a", t.TempDir()), point("node-b", theirs)); ok {
		t.Errorf("a node whose own directory holds no manifest published %v, "+
			"want no series: zero is the freshest backup imaginable and "+
			"another node's copy is on a disk this node cannot see", age)
	}
}
