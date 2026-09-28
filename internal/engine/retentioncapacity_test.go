package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/objstore"
	objplacement "github.com/crewlet/crewlet/internal/objstore/placement"
	"github.com/crewlet/crewlet/internal/objstore/transfer"
	"github.com/crewlet/crewlet/internal/objstore/upkeep"
	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/placement"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/tracker"
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
	db, err := store.OpenNode(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.OpenNode: %v", err)
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
	db, err := store.OpenNode(t.Context(), nodeDir+"/company.db", store.Options{
		ReplicatedPath: replicatedDir + "/crewlet-replicated.db"})
	if err != nil {
		t.Fatalf("store.OpenNode: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	part, err := db.OpenPartition(t.Context(), storetest.LayoutZero(1))
	if err != nil {
		t.Fatalf("open layout 0's partition: %v", err)
	}
	nodeSize, err := fileBytes(db.Path())
	if err != nil {
		t.Fatal(err)
	}
	replicatedSize, err := fileBytes(part.Path())
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
	db, err := store.OpenNode(t.Context(), t.TempDir()+"/index.db", store.Options{})
	if err != nil {
		t.Fatalf("store.OpenNode: %v", err)
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
	trackerLog := estateLog(tracker.Domain{})
	recorder.Add(metrics.StatelogBarriersApplied,
		uint64(3*statelog.Census{Seats: 100, Logs: 1}.Expected()),
		metrics.Attrs{"domain": "tracker", "stream": estateSpec(tracker.Domain{}).Name})

	r := &retention{metrics: recorder, state: censusLogs(LayoutZero(), trackerLog),
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
	if reading.LinearizableReadsExpected != (statelog.Census{Seats: 100, Logs: 1}).Expected() ||
		reading.CensusLog != trackerLog.String() {
		t.Errorf("the declared read rate reached the reading as %d on %q, so "+
			"`census_drift` compares against nothing",
			reading.LinearizableReadsExpected, reading.CensusLog)
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

// censusLogs is a state log running the given logs of layout, with nothing
// behind them but what the census reads: which log each is, and its stream.
func censusLogs(layout statelog.Layout, ids ...statelog.LogID) *stateLog {
	s := &stateLog{layout: layout}
	var running []*runningLog
	for _, id := range ids {
		d, err := registeredDomain(id.Domain)
		if err != nil {
			panic(err)
		}
		running = append(running, &runningLog{domain: d, id: id, key: id.String(),
			spec: layout.StreamSpec(d, id)})
	}
	return runsLogs(s, running...)
}

// A LOG IS HELD TO ITS OWN SHARE OF THE CENSUS, AT THE RATE IT RECEIVES.
//
// Three things the fixed, node-local figure got wrong. It was one number for
// every company, so a two-hundred-seat company doing the reference company's
// work per seat read as drifting. It was compared with the barriers THIS NODE
// appended, which on a fleet is only this node's share of the reads — the
// rate the log receives, from every node, is what its sizing is about, and it
// is what every node's applier sees. And it was one figure for the estate,
// where a partitioned domain divides its census across its logs as it divides
// its ceiling. The vector log, which no read appends to, is held to nothing.
//
// AND THE ENGINE'S OWN READS ARE PART OF WHAT A LOG TAKES. Every data node's
// object passes pin the tracker's estate on a fixed cadence, whatever the
// seats do, so a small company doing exactly its census put more barriers on
// its log than its seats' share allowed — and fired the alarm, louder the
// smaller it was.
func TestALogIsHeldToItsOwnShareOfTheCensus(t *testing.T) {
	t.Parallel()
	zero := LayoutZero()
	trackerLog, vectorLog := estateLog(tracker.Domain{}), estateLog(search.Domain{})
	partitioned := partitionedTestLayout()
	second := statelog.LogID{Domain: "tracker",
		Partition: statelog.PartitionID{Space: statelog.SpaceTracker, Index: 1}}
	stream := func(l statelog.Layout, id statelog.LogID) string { name, _ := l.Stream(id); return name }

	for _, tc := range []struct {
		name       string
		layout     statelog.Layout
		logs       []statelog.LogID
		seats      int
		background map[string]int
		applied    map[statelog.LogID]int
		appended   int
		fires      bool
		log        string
		expected   int
	}{
		{
			name: "one seat on one data node, at its census beside the passes",
			// 125 a day from the seat and 168 from the one data
			// node's passes: 293, which past twice the seat's 125
			// fired on a company whose sizing was right.
			layout: zero, logs: []statelog.LogID{trackerLog}, seats: 1,
			background: map[string]int{"tracker": upkeep.PinsPerDay},
			applied:    map[statelog.LogID]int{trackerLog: 125 + upkeep.PinsPerDay},
			fires:      false, log: "tracker", expected: 125 + upkeep.PinsPerDay,
		},
		{
			name: "an idle two-seat company on three data nodes",
			// 504 a day from the passes alone, past twice the
			// seats' 250.
			layout: zero, logs: []statelog.LogID{trackerLog}, seats: 2,
			background: map[string]int{"tracker": 3 * upkeep.PinsPerDay},
			applied:    map[statelog.LogID]int{trackerLog: 3 * upkeep.PinsPerDay},
			fires:      false, log: "tracker", expected: 250 + 3*upkeep.PinsPerDay,
		},
		{
			name:   "the passes are no cover past twice the whole census",
			layout: zero, logs: []statelog.LogID{trackerLog}, seats: 1,
			background: map[string]int{"tracker": upkeep.PinsPerDay},
			applied:    map[statelog.LogID]int{trackerLog: 2*(125+upkeep.PinsPerDay) + 1},
			fires:      true, log: "tracker", expected: 125 + upkeep.PinsPerDay,
		},
		{
			name: "a 200-seat company at 1.2 times its per-seat census",
			// 30 000 a day: past twice the fixed 12 500, inside twice
			// this company's 25 000.
			layout: zero, logs: []statelog.LogID{trackerLog}, seats: 200,
			applied: map[statelog.LogID]int{trackerLog: 30_000},
			fires:   false, log: "tracker", expected: 25_000,
		},
		{
			name:   "a 100-seat company at three times its census",
			layout: zero, logs: []statelog.LogID{trackerLog}, seats: 100,
			applied: map[statelog.LogID]int{trackerLog: 37_501},
			fires:   true, log: "tracker", expected: 12_500,
		},
		{
			name:   "this node's own appends, however many, are not the log's rate",
			layout: zero, logs: []statelog.LogID{trackerLog}, seats: 100,
			appended: 100_000,
			fires:    false, log: "tracker", expected: 12_500,
		},
		{
			name:   "the log's rate, from every node, past this node's own appends",
			layout: zero, logs: []statelog.LogID{trackerLog}, seats: 100,
			applied: map[statelog.LogID]int{trackerLog: 30_000}, appended: 10_000,
			fires: true, log: "tracker", expected: 12_500,
		},
		{
			name: "a partition past its share while its domain's other log is idle",
			// Three seats: 375 a day, 188 per tracker partition (rounded
			// up) and all 375 for the pages log. tracker.001 at 377 is
			// past twice its 188; pages at 700 is inside twice its 375.
			layout: partitioned,
			logs: []statelog.LogID{
				{Domain: "pages", Partition: statelog.PartitionID{Space: statelog.SpacePages}},
				{Domain: "tracker", Partition: statelog.PartitionID{Space: statelog.SpaceTracker}},
				second,
			},
			seats: 3,
			applied: map[statelog.LogID]int{second: 377,
				{Domain: "pages", Partition: statelog.PartitionID{Space: statelog.SpacePages}}: 700},
			fires: true, log: second.String(), expected: 188,
		},
		{
			name:   "the vector log, which no read appends to",
			layout: zero, logs: []statelog.LogID{vectorLog, trackerLog}, seats: 1,
			applied: map[statelog.LogID]int{vectorLog: 1_000_000},
			fires:   false, log: "tracker", expected: 125,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			recorder, err := metrics.New()
			if err != nil {
				t.Fatalf("recorder: %v", err)
			}
			for id, n := range tc.applied {
				recorder.Add(metrics.StatelogBarriersApplied, uint64(n),
					metrics.Attrs{"domain": id.Domain, "stream": stream(tc.layout, id)})
			}
			if tc.appended > 0 {
				recorder.Add(metrics.StatelogBarrierAppends, uint64(tc.appended),
					metrics.Attrs{"domain": "tracker"})
			}
			r := &retention{metrics: recorder, state: censusLogs(tc.layout, tc.logs...),
				seats:      func() int { return tc.seats },
				background: func(domain string) int { return tc.background[domain] }}
			var reading statelog.Reading
			r.observed(&reading)
			if reading.CensusLog != tc.log || reading.LinearizableReadsExpected != tc.expected {
				t.Errorf("the census names %q expecting %d a day, want %q expecting %d",
					reading.CensusLog, reading.LinearizableReadsExpected, tc.log, tc.expected)
			}
			fired := false
			for _, alarm := range statelog.Evaluate(reading) {
				fired = fired || alarm.Kind == statelog.KindCensusDrift
			}
			if fired != tc.fires {
				t.Errorf("census_drift fired %v on %d a day against %d, want %v",
					fired, reading.LinearizableReads, reading.LinearizableReadsExpected, tc.fires)
			}
		})
	}
}

// THE CENSUS COUNTS THE RUNNING COMPANY'S AGENT SEATS.
//
// A log's share of the census is per seat, so the trim's reading has to be
// handed the company this node is running — and a wiring that handed it none
// would still produce a number, the one seat an empty company is counted as,
// and hold a company of any size to that: an alarm firing on every company past
// two seats' worth of reads.
func TestTheCensusCountsTheRunningCompanysSeats(t *testing.T) {
	t.Parallel()
	e, _ := aRunningNodeOf(t, `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    manages: ["CTO", "Engineer"]
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

// THE ENGINE'S OWN READS ARE THE OBJECT PASSES' PINS, ON EVERY MEMBER.
//
// Each data node's repair and collection pin the estate of every domain a
// declared table names, on a fixed cadence — so the barriers a day the engine
// puts on each of those logs is the passes' steady count for every member of
// the object map, taken out or on probation included, since each still runs
// them; on any other log, nothing; and while no map is known, nothing, since
// no node places and so none pins.
func TestTheEnginesOwnReadsAreThePassesPinsOnEveryMember(t *testing.T) {
	t.Parallel()
	cache := transfer.NewCache(nil)
	e := &Engine{objects: &objectStore{cache: cache}}
	if got := e.backgroundBarriers(tracker.Domain{}.Name()); got != 0 {
		t.Errorf("with no map known the engine puts %d a day on the tracker's log, want none", got)
	}
	cache.Observe(objstore.MapState{Map: objplacement.Map{
		Generation: uuid.New(), Epoch: 1, Replicas: 1, PGBits: objplacement.MinPGBits,
		Members: []placement.Member{
			{Node: "a", Weight: 1, Share: 1 << 16}, {Node: "b", Weight: 1, Share: 1 << 16},
			{Node: "c", Weight: 1, Share: 1 << 16, Out: true},
		},
	}}, 1)
	if got, want := e.backgroundBarriers(tracker.Domain{}.Name()), 3*upkeep.PinsPerDay; got != want {
		t.Errorf("three members put %d a day on the tracker's log, want %d", got, want)
	}
	for _, other := range []statelog.Domain{pages.Domain{}, search.Domain{}} {
		if got := e.backgroundBarriers(other.Name()); got != 0 {
			t.Errorf("the passes put %d a day on the %s log, which no declared "+
				"table names", got, other.Name())
		}
	}
	if upkeep.PinsPerDay != 168 {
		t.Errorf("the passes pin %d times a day, and a repair every ten minutes "+
			"and a collection every hour is 168", upkeep.PinsPerDay)
	}
}
