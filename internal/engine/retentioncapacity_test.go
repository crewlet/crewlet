package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// THE THREE ALARMS ABOUT THE MACHINE, and the fields nothing filled.
//
// `volume_low`, `wal_large` and `pool_starved` each read a field of
// [statelog.Reading] that no code assigned, so all three were permanently
// silent — which on a node running out of disk is the worst possible silence.
// Filling the field is only half of it: a field filled in and never read is
// the same silence with an extra step, so this asserts the table fires too.
func TestTheCapacityAlarmsFireOnWhatThisNodesDiskIsDoing(t *testing.T) {
	t.Parallel()
	db := storetest.OpenNode(t, t.TempDir()+"/index.db", store.Options{})
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

// THE VOLUME ALARM IS ABOUT THE VOLUME WITH THE LEAST ROOM, measured per
// volume. `store.replicated_path` may put the replicated estate on a volume of
// its own, and the reading measured the node estate's volume alone against
// both files' bytes: a replicated estate filling its own disk never fired. Here
// the replicated estate sits on a nearly full volume and the node estate on an
// empty one, and the reading is the nearly full one's — its free space against
// its own file, never the other volume's space or both files' bytes — and the
// alarm NAMES it, since an operator has two disks and must know which to grow.
// A volume that cannot be measured is said and alarmed on, never skipped: the
// node would otherwise be judged on its roomy volume alone.
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
	r.space(&reading)
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

	// A VOLUME THAT CANNOT BE MEASURED beside a roomy one is the alarm,
	// naming it — not a node judged on the roomy one alone.
	r.volumes = func(dir string) (string, int64, error) {
		if dir == replicatedDir {
			return "", 0, fmt.Errorf("engine: measure the free space on %s: input/output error", dir)
		}
		return dir, 1 << 40, nil
	}
	reading = statelog.Reading{}
	r.space(&reading)
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
	r.space(&reading)
	if reading.StoreBytes != nodeSize+replicatedSize {
		t.Errorf("one volume holding both files is %d stored, want %d", reading.StoreBytes,
			nodeSize+replicatedSize)
	}
}

// THE TIGHTEST VOLUME IS THE ONE WITH THE SMALLEST ROOM FOR ITS OWN BYTES, and a
// volume holding nothing is never it.
func TestTheTightestVolumeIsTheOneNearestItsAlarm(t *testing.T) {
	t.Parallel()
	file := func(volume string, free, size int64) storedFile {
		return storedFile{volume: volume, dir: "/" + volume, free: free, size: size}
	}
	for name, tc := range map[string]struct {
		files []storedFile
		want  storeVolume
	}{
		"no files": {nil, storeVolume{}},
		"two files, one volume": {[]storedFile{file("a", 100, 10), file("a", 100, 30)},
			storeVolume{"/a", 100, 40}},
		"the smaller ratio, not the smaller free": {
			[]storedFile{file("big", 1000, 900), file("small", 50, 5)}, storeVolume{"/big", 1000, 900}},
		"an empty volume beside a full one": {
			[]storedFile{file("empty", 1, 0), file("full", 10, 100)}, storeVolume{"/full", 10, 100}},
		"a full volume beside an empty one": {
			[]storedFile{file("full", 10, 100), file("empty", 1, 0)}, storeVolume{"/full", 10, 100}},
		// Tens of terabytes each: the cross products overflow an int64,
		// and wrapped they choose the roomier volume in either order.
		"sizes past what a product of int64s holds": {
			[]storedFile{file("a", 14773780071959, 34330076720781), file("b", 27431250736051, 18740855235886)},
			storeVolume{"/a", 14773780071959, 34330076720781}},
		"sizes past what a product of int64s holds, reversed": {
			[]storedFile{file("b", 27431250736051, 18740855235886), file("a", 14773780071959, 34330076720781)},
			storeVolume{"/a", 14773780071959, 34330076720781}},
	} {
		if got := tightestVolume(tc.files); got != tc.want {
			t.Errorf("%s: %+v, want %+v", name, got, tc.want)
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
// `pool_starved`, `records_gated` and `feed_unreadable` each take a field of
// [statelog.Reading] off this process's own recorder, and `census_drift` takes
// each log's from the same recorder ([retention.census]). Every
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
	// THE CENSUS IS EACH LOG'S, as the report evaluates it
	// ([statelog.DomainInputs.LinearizableReads]).
	held, counted := r.census(r.state.running())[trackerLog.Name()]
	if !counted || held.expected != trackerCensus.Expected() ||
		held.reads != 3*held.expected {
		t.Errorf("the tracker's log reached its census as %d read against %d (counted "+
			"%v), so `census_drift` compares against nothing", held.reads,
			held.expected, counted)
	}

	fired := map[statelog.Kind]bool{}
	for _, alarm := range statelog.Evaluate(reading) {
		fired[alarm.Kind] = true
	}
	for _, alarm := range statelog.Evaluate(statelog.Reading{
		LinearizableReads: held.reads, LinearizableReadsExpected: held.expected,
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

	// THE CONTROL: an untouched recorder fires none of them, or every
	// assertion above would pass on a table that always fires.
	quiet, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	var empty statelog.Reading
	silent := &retention{metrics: quiet, state: censusLogs(trackerLog),
		seats: func() int { return 100 }}
	silent.observed(&empty)
	alarms := statelog.Evaluate(empty)
	for _, c := range silent.census(silent.state.running()) {
		alarms = append(alarms, statelog.Evaluate(statelog.Reading{
			LinearizableReads: c.reads, LinearizableReadsExpected: c.expected,
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

// THE VECTOR COVERAGE IS MEASURED ON EVERY TICK, AND ONLY ON A TICK.
//
// It used to be measured inside the report whenever the cached figure was a
// whole interval old — which the next tick's report usually was not, because
// the ticker fires on a fixed period and the report starts after the tick's
// other measurements. Whenever the previous tick's took longer, the figure was
// a few milliseconds short of an interval old, the scan was skipped, and
// `recall_below_floor` read a coverage half an hour stale beside inputs a
// quarter of an hour old. And a report is assembled on every dashboard poll,
// so the scan must never run from one.
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
	// THE NEXT TICK, however little time the drift left between them.
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

	// A FAILED SCAN LEAVES NOTHING: an unreadable corpus is not the
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
// Two things the fixed, node-local figure got wrong. It was one number for
// every company, so a two-hundred-seat company doing the reference company's
// work per seat read as drifting. And it was compared with the barriers THIS
// NODE appended, which on a fleet is only this node's share of the reads — the
// rate the log receives, from every node, is what its sizing is about, and it
// is what every data node's applier sees. The vector log, which no read
// appends to, is held to nothing.
//
// AND THE ENGINE'S OWN READS ARE PART OF WHAT A LOG TAKES. The object store's
// collector pins the tracker's estate on a fixed cadence, whatever the seats
// do, so a small company doing exactly its census would put more barriers on
// its log than its seats' share allowed — and fire the alarm, louder the
// smaller it was.
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
			// 125 a day from the seat and the collector's pins
			// beside it: counted in the census, so the seat's own
			// reads are measured against what the log really
			// carries.
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
			// HUNDREDS OF SEATS AND NOT ONE BARRIER: no read of the
			// estate takes one, so its census is zero however many
			// seats the company has — the whole company's figure,
			// charged here, was a census its ceiling was never sized for.
			logs: []statelog.Domain{iamLog}, seats: 300,
			fires: false, log: "iam", expected: 0,
		},
		{
			name: "the identity estate's log on its first barrier",
			// A DECLARED ZERO IS A CENSUS: the first barrier is a
			// linearizable reader nobody counted, and the log's ceiling
			// has no bytes for it.
			logs: []statelog.Domain{iamLog}, seats: 300,
			applied: map[statelog.Domain]int{iamLog: 1},
			fires:   true, log: "iam", expected: 0,
		},
		{
			name: "the chat log, held to its own per-seat rate",
			// 120 a seat a day, chat's own declaration, rather than the
			// tracker's 125 borrowed: 12 000 for 100 seats.
			logs: []statelog.Domain{chatLog}, seats: 100,
			applied: map[statelog.Domain]int{chatLog: 12_500},
			fires:   false, log: "chat", expected: 12_000,
		},
		{
			name: "the chat log past twice its own census, inside twice the tracker's",
			// 24 500 a day is under twice the tracker's 12 500 and over
			// twice chat's 12 000, so a census that held the chat log to
			// the tracker's rate stays quiet here.
			logs: []statelog.Domain{chatLog}, seats: 100,
			applied: map[statelog.Domain]int{chatLog: 24_500},
			fires:   true, log: "chat", expected: 12_000,
		},
		{
			name: "the executor log, held to its own per-seat rate",
			// 60 a seat a day, the executor's own declaration (a head
			// read at a turn's first launch, a row read at a
			// relaunch): 6 000 for 100 seats, and 12 000 a day is
			// still inside twice it.
			logs: []statelog.Domain{executorLog}, seats: 100,
			applied: map[statelog.Domain]int{executorLog: 12_000},
			fires:   false, log: "executor", expected: 6_000,
		},
		{
			name: "the executor log past twice its own census, inside twice the tracker's",
			// 12 001 a day is under twice the tracker's 12 500 and
			// over twice the executor's 6 000, so a census that held
			// this log to the tracker's rate stays quiet here.
			logs: []statelog.Domain{executorLog}, seats: 100,
			applied: map[statelog.Domain]int{executorLog: 12_001},
			fires:   true, log: "executor", expected: 6_000,
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
			if !counted || held.expected != tc.expected {
				t.Errorf("the census holds %q to %d a day (counted %v), want %d",
					tc.log, held.expected, counted, tc.expected)
			}
			if _, counted := census[vectorLog.Name()]; counted {
				t.Error("the vector log, which no read appends to, is held to a census")
			}
			// PER LOG, as the report evaluates it: each log's reads against
			// its own census, and the alarm names the log.
			fired := false
			for _, c := range census {
				for _, alarm := range statelog.Evaluate(statelog.Reading{
					LinearizableReads: c.reads, LinearizableReadsExpected: c.expected,
				}) {
					fired = fired || alarm.Kind == statelog.KindCensusDrift
				}
			}
			if fired != tc.fires {
				t.Errorf("census_drift fired %v on %d a day against %d, want %v",
					fired, held.reads, held.expected, tc.fires)
			}
		})
	}
}

// THE CENSUS COUNTS THE RUNNING COMPANY'S AGENT SEATS.
//
// A log's census is per seat, so the trim's reading has to be handed the
// company this node is running — and a wiring that handed it none would still
// produce a number, the one seat an empty company is counted as, and hold a
// company of any size to that: an alarm firing on every company past two
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
// The collector pins the estate of every domain a declared table names, on a
// fixed cadence — a collection hourly and an audit daily — and ONE collector
// runs in the fleet, so the barriers a day the engine puts on each of those
// logs is its count, never a count per member; on any other log, nothing; and
// on a node running no object store, nothing.
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
// `crewlet.backup.age` reads the directories of the points THIS node announced
// — the only ones whose disk this process can see — and nothing else: another
// node's fresher point and the operator's acknowledgement name directories on
// other hosts. A node whose own manifest is gone publishes no series rather
// than an age, since zero is the freshest backup imaginable. And the age is
// taken against the duty's clock, which every report beside it reads, so a
// manifest from a host whose clock runs ahead is zero rather than negative.
//
// Mutation: drop the owner check, and the gauge ages node-b's fresher copy;
// read the wall clock instead of [retention.clock], and a manifest finished in
// 2041 is in the wall clock's future and reads zero; set the gauge where no
// manifest was found, and the absent case reads a value.
func TestTheBackupGaugeAgesThisNodesOwnManifestsOnTheDutysClock(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	// FAR FROM ANY WALL CLOCK a test machine has, so an age read off the
	// wall clock cannot pass for one read off the duty's.
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
