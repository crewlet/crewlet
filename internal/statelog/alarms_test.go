package statelog_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/membership"
	"github.com/crewlet/crewlet/internal/objstore/disk"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// EVERY ALARM FIRES, and every alarm is silent on a healthy node.
//
// Both halves matter and the second is the one that gets lost. An alarm table
// is only useful if a firing row means something, and a condition that is
// true on a node with nothing wrong — the plan's own dashboard tint, lit on
// every row always because its threshold was one apply linger — teaches an
// operator to ignore the whole surface.
//
// So each kind is driven twice: once with the reading that must raise it, and
// once against the zero reading, which is a node that measured nothing and
// must alarm about nothing.
func TestEveryAlarmFiresOnItsConditionAndOnNothingElse(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		kind    statelog.Kind
		reading statelog.Reading
		says    string
	}{
		"a node behind the log": {
			statelog.KindApplyLag,
			statelog.Reading{ApplyLag: 2 * time.Minute},
			"2m0s behind",
		},
		"reads refused for something other than lag": {
			statelog.KindReadRefusals,
			statelog.Reading{RefusalsSince: time.Minute},
			"refused",
		},
		"a barrier spending a quarter of the read budget": {
			statelog.KindBarrierSlow,
			statelog.Reading{BarrierP95: time.Second},
			"read barrier",
		},
		"a log near its ceiling": {
			statelog.KindLogHeadroom,
			statelog.Reading{HeadroomFraction: statelog.Frac(0.05)},
			"5% of the log",
		},
		"a backup past the policy": {
			statelog.KindBackupAge,
			statelog.Reading{
				BackupAge:    statelog.Age(30 * time.Hour),
				BackupMaxAge: 24 * time.Hour,
			},
			"30h0m0s old",
		},
		"no backup at all": {
			statelog.KindBackupAge,
			statelog.Reading{BackupMaxAge: 24 * time.Hour},
			"no verified backup has been recorded",
		},
		"a trim that cannot advance": {
			statelog.KindTrimBlocked,
			statelog.Reading{TrimBlockedFor: time.Hour, TrimBlockedBy: "snapshot_floor"},
			"snapshot_floor",
		},
		"a deferral past the grace": {
			statelog.KindDeferredOld,
			statelog.Reading{DeferredAge: 31 * time.Minute},
			"seats move",
		},
		"a floor nobody can read": {
			statelog.KindFloorUnknown,
			statelog.Reading{FloorUnknownFor: 2 * time.Minute},
			"unreadable",
		},
		"a slow prefetch": {
			statelog.KindPrefetchSlow,
			statelog.Reading{PrefetchP95: time.Second},
			"context assembly",
		},
		"a slow search": {
			statelog.KindSearchSlow,
			statelog.Reading{SearchP95: 3 * time.Second},
			"interactive search",
		},
		"search without its semantic half": {
			statelog.KindSearchDegraded,
			statelog.Reading{SearchDegradedFraction: 0.2},
			"semantic half",
		},
		"search over part of the corpus": {
			statelog.KindSearchScoped,
			statelog.Reading{SearchScopedFraction: 0.1},
			"part of the",
		},
		"recall below its floor": {
			statelog.KindRecallBelowFloor,
			statelog.Reading{SemanticCoverage: statelog.Frac(0.5)},
			"current vectors",
		},
		"an index measured below its floor": {
			statelog.KindIVFRecallBelowFloor,
			statelog.Reading{IVFRecall: statelog.Frac(0.91), IVFRecallFloor: 0.98,
				IVFMeasuredOn: 20_000, IVFShape: "source:page"},
			"0.9100 against the exact scan in the source:page shape over 20000 sources",
		},
		"a gated record": {
			statelog.KindRecordsGated,
			statelog.Reading{RecordsGated: 1},
			"apply gate",
		},
		"a change record no build can read": {
			statelog.KindFeedUnreadable,
			statelog.Reading{FeedUnreadable: 3},
			"could not be translated",
		},
		"maintenance nobody finished": {
			statelog.KindMaintenanceOpen,
			statelog.Reading{MaintenanceOpenFor: 2 * time.Hour, MaintenancePhase: "observe"},
			"observe",
		},
		"a volume with no room for a second copy": {
			statelog.KindVolumeLow,
			statelog.Reading{FreeBytes: 1 << 30, StoreBytes: 4 << 30, StoreVolume: "/var/lib/crewlet"},
			"1.0 GiB free on the volume holding /var/lib/crewlet",
		},
		"a store volume nobody can measure": {
			statelog.KindVolumeLow,
			statelog.Reading{StoreVolumeUnmeasured: "measure the free space on /mnt/estate: input/output error"},
			"/mnt/estate",
		},
		"a write-ahead log nothing is checkpointing": {
			statelog.KindWALLarge,
			statelog.Reading{WALBytes: 2 << 30},
			"write-ahead log is 2.0 GiB",
		},
		"a pool every reader queues on": {
			statelog.KindPoolStarved,
			statelog.Reading{PoolWaitP95: 200 * time.Millisecond},
			"to acquire",
		},
		"a read rate the sizing did not assume": {
			statelog.KindCensusDrift,
			statelog.Reading{LinearizableReads: 5000, LinearizableReadsExpected: 1000,
				CensusLog: "tracker@tracker.007"},
			"on tracker@tracker.007 against the 1000",
		},
		"chunks no member holds": {
			statelog.KindObjectsMissing,
			statelog.Reading{ObjectsMissing: 2},
			"held by no member",
		},
		"copies a completed repair left behind": {
			statelog.KindObjectsDegraded,
			statelog.Reading{ObjectsPending: 7, ObjectsUnreachable: 3},
			"7 chunk(s)",
		},
		"a map epoch no repair has completed at": {
			statelog.KindObjectsDegraded,
			statelog.Reading{ObjectsUnrepairedFor: time.Hour,
				ObjectsRepairInterval: 10 * time.Minute},
			"no repair pass has completed",
		},
		"a repair past its second interval": {
			statelog.KindObjectsDegraded,
			statelog.Reading{ObjectsUnrepairedFor: 20*time.Minute + time.Second,
				ObjectsRepairInterval: 10 * time.Minute},
			"for 20m1s, and one is due every 10m0s",
		},
		"a failed object store": {
			statelog.KindObjectsUnhealthy,
			statelog.Reading{ObjectsHealth: disk.HealthFailed,
				ObjectsHealthDetail: "3 operations in a row failed"},
			"3 operations in a row failed",
		},
		"a full object store": {
			statelog.KindObjectsUnhealthy,
			statelog.Reading{ObjectsHealth: disk.HealthFull, ObjectsUsedPercent: 96},
			"full (96.0% used)",
		},
		"a nearly full object store": {
			statelog.KindObjectsNearFull,
			statelog.Reading{ObjectsHealth: disk.HealthNearFull, ObjectsUsedPercent: 88},
			"88.0% used",
		},
		"a partition no copy can answer for": {
			statelog.KindEstateUnserved,
			statelog.Reading{EstateUnserved: 2, EstateUnservedWhich: "tracker.007, pages.001"},
			"2 partition(s) have no copy that can answer: tracker.007, pages.001",
		},
		"a partition short past the grace": {
			statelog.KindEstateShort,
			statelog.Reading{EstateShort: 3, EstateShortFor: 11 * time.Minute,
				EstateShortWhich: "tracker.007 (1 of 3 copies)"},
			"tracker.007 (1 of 3 copies) has been short for 11m0s, past the 10m0s",
		},
		"a join past the rejoin window": {
			statelog.KindEstateMoveStalled,
			statelog.Reading{EstateJoiningFor: 31 * time.Minute, EstateJoinBudget: 30 * time.Minute,
				EstateJoiningWhich: "tracker.007 on data-c"},
			"tracker.007 on data-c has been joining for 31m0s, past the 30m0s rejoin window",
		},
		"an estate view past the staleness bound": {
			statelog.KindEstateViewStale,
			statelog.Reading{EstateView: &statelog.EstateViewAge{Half: "the estate map",
				Age: 2 * time.Minute, Bound: statelog.FloorCacheStale}},
			"view of the estate map was last confirmed 2m0s ago, past the 1m0s",
		},
		// THE BOUND IS THE VIEW'S, not a minute restated here: leases a
		// second past a 45-second TTL are no answer, and the alarm says so
		// at the same instant the view stops deciding from them.
		"estate leases past their TTL": {
			statelog.KindEstateViewStale,
			statelog.Reading{EstateView: &statelog.EstateViewAge{Half: "the estate leases",
				Age: 46 * time.Second, Bound: 45 * time.Second}},
			"view of the estate leases was last confirmed 46s ago, past the 45s",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got := statelog.Evaluate(tc.reading)
			alarm, found := find(got, tc.kind)
			if !found {
				t.Fatalf("%s did not fire on its own condition; fired: %v",
					tc.kind, kindsOf(got))
			}
			if !strings.Contains(alarm.Detail, tc.says) {
				t.Errorf("detail = %q, want it to say %q — a name is not an "+
					"alarm, the measurement is", alarm.Detail, tc.says)
			}
			if alarm.Remedy == "" {
				t.Errorf("%s fires with no remedy: an operator is told something "+
					"is wrong and not what to do", tc.kind)
			}
			// AND NOTHING ELSE. One reading drives one condition, so a
			// second alarm here is a condition reading a field it does
			// not own or a threshold a zero value crosses.
			if len(got) != 1 {
				t.Errorf("one condition raised %v", kindsOf(got))
			}
		})
	}
}

// A HEALTHY NODE IS SILENT, and a node that measured NOTHING is a healthy
// node as far as this table is concerned.
//
// The zero reading is the shape a node with no search backend, no snapshot,
// no maintenance and no backup policy hands in — and every one of those is a
// real deployment rather than a fault. A condition that read a zero as "at
// the floor" would alarm on all of them permanently.
func TestAZeroReadingRaisesNothing(t *testing.T) {
	t.Parallel()
	if got := statelog.Evaluate(statelog.Reading{}); len(got) != 0 {
		t.Errorf("a node that measured nothing raised %v", kindsOf(got))
	}
	// AND A MEASURED ZERO IS THE OPPOSITE OF AN UNMEASURED ONE. This is
	// what the pointer is for: a full log and a node that has not looked
	// at its log are different facts, and a plain float64 gave them one
	// representation — the alarming one, on every node that measured
	// nothing.
	measured := statelog.Reading{
		HeadroomFraction: statelog.Frac(0), SemanticCoverage: statelog.Frac(0),
	}
	got := statelog.Evaluate(measured)
	if len(got) != 2 {
		t.Errorf("a measured zero raised %v, want both fraction alarms", kindsOf(got))
	}
}

// THE INDEX RECALL ALARM FIRES AT THE EVALUATION'S FLOOR, NOT BELOW A NUMBER OF
// ITS OWN (ADR-0015).
//
// The floor is the curve `crewlet search eval` judges a corpus against, at the
// size the training measured, handed in by the engine because the curve is the
// search package's. A recall AT the floor passes that evaluation, so it must
// not alarm; a partition whose index nobody trained measured nothing and must
// not alarm either — zero recall is the alarm, not the absence.
func TestTheIndexRecallAlarmFiresAtTheEvaluationsFloor(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		reading statelog.Reading
		fires   bool
	}{
		"a recall at the floor": {statelog.Reading{
			IVFRecall: statelog.Frac(0.93), IVFRecallFloor: 0.93}, false},
		"a recall above it": {statelog.Reading{
			IVFRecall: statelog.Frac(0.99), IVFRecallFloor: 0.93}, false},
		"a recall a hair below it": {statelog.Reading{
			IVFRecall: statelog.Frac(0.9299), IVFRecallFloor: 0.93}, true},
		"a measured zero": {statelog.Reading{
			IVFRecall: statelog.Frac(0), IVFRecallFloor: 0.93}, true},
		"nothing measured": {statelog.Reading{IVFRecallFloor: 0.93}, false},
	} {
		_, fired := find(statelog.Evaluate(c.reading), statelog.KindIVFRecallBelowFloor)
		if fired != c.fires {
			t.Errorf("%s: fired %v, want %v", name, fired, c.fires)
		}
	}
}

// THE OBJECT STORE ALARMS FIRE AT THE THRESHOLDS THE STORE ALREADY DECIDED,
// and are silent short of them: a repair still inside the interval the passes
// run on is a repair on schedule, a store the disk calls ok is ok, and a chunk
// a member did not answer for is unreachable rather than missing.
//
// AND A PASS DUE BUT NOT YET DONE IS ON SCHEDULE TOO. The reading runs from
// the last completed pass, so every healthy node passes one interval each
// cycle — the next pass starts an interval after the last one ended and
// finishes only after its own duration — and the alarm waits a second
// interval before it calls that a repair that is not keeping up.
func TestTheObjectStoreAlarmsAreSilentShortOfTheirThresholds(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]statelog.Reading{
		"a repair on schedule": {ObjectsUnrepairedFor: 9 * time.Minute,
			ObjectsRepairInterval: 10 * time.Minute},
		"a pass due and still running": {ObjectsUnrepairedFor: 12 * time.Minute,
			ObjectsRepairInterval: 10 * time.Minute},
		"exactly two intervals": {ObjectsUnrepairedFor: 20 * time.Minute,
			ObjectsRepairInterval: 10 * time.Minute},
		"a node running no passes":    {ObjectsUnrepairedFor: time.Hour},
		"a healthy store":             {ObjectsHealth: disk.HealthOK, ObjectsUsedPercent: 60},
		"a store that has not said":   {ObjectsHealth: "", ObjectsUsedPercent: 99},
		"unreachable but not pending": {ObjectsUnreachable: 4},
	} {
		if got := statelog.Evaluate(r); len(got) != 0 {
			t.Errorf("%s raised %v", name, kindsOf(got))
		}
	}
}

// THE ESTATE ALARMS FIRE AT THE THRESHOLDS OTHER DECISIONS MADE, and not a
// moment before (ADR-0015): a shortfall AT membership's grace is the map's own
// repair still within the time it gives a member to come back, a join AT the
// rejoin window is within its budget, and a view AT the staleness bound is one
// every cached coordination fact here still answers from. A node that runs no
// estate view — no join budget, no view age — raises none of them, and a view
// confirmed this instant is a measured zero, not an absence.
func TestTheEstateAlarmsAreSilentShortOfTheirThresholds(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]statelog.Reading{
		"short at the grace": {EstateShort: 1, EstateShortFor: membership.OutGrace,
			EstateShortWhich: "tracker.007"},
		"joining at the window": {EstateJoiningFor: 30 * time.Minute,
			EstateJoinBudget: 30 * time.Minute, EstateJoiningWhich: "tracker.007 on data-c"},
		"joining with no budget": {EstateJoiningFor: time.Hour},
		"a view at the bound": {EstateView: &statelog.EstateViewAge{Half: "the estate map",
			Age: statelog.FloorCacheStale, Bound: statelog.FloorCacheStale}},
		"leases at their TTL": {EstateView: &statelog.EstateViewAge{Half: "the estate leases",
			Age: 45 * time.Second, Bound: 45 * time.Second}},
		"a view confirmed now": {EstateView: &statelog.EstateViewAge{Half: "the estate map",
			Bound: statelog.FloorCacheStale}},
	} {
		if got := statelog.Evaluate(r); len(got) != 0 {
			t.Errorf("%s raised %v", name, kindsOf(got))
		}
	}
	past := statelog.Reading{EstateShort: 1, EstateShortFor: membership.OutGrace + time.Second,
		EstateShortWhich: "tracker.007"}
	if got := statelog.Evaluate(past); len(got) != 1 || got[0].Kind != statelog.KindEstateShort {
		t.Errorf("a shortfall a second past the grace raised %v", kindsOf(got))
	}
}

// THE BACKUP ALARM FIRES AT THE POLICY'S OWN THRESHOLD, once.
//
// The form this replaces had two: a flag set when the newest backup passed
// backup_max_age, and an alarm raised when THAT flag had been true for
// backup_max_age again. A 24-hour policy alarmed at 48 hours — eight missed
// six-hourly runs after the first one that mattered, on a deployment whose
// operator had asked to hear about one.
func TestTheBackupAlarmFiresAtTheAgeThePolicyNames(t *testing.T) {
	t.Parallel()
	const policy = 24 * time.Hour
	for name, tc := range map[string]struct {
		age  time.Duration
		want bool
	}{
		"a backup taken this second":   {0, false},
		"an hour before the threshold": {policy - time.Hour, false},
		"exactly at it":                {policy, false},
		"a minute past it":             {policy + time.Minute, true},
		"at twice it":                  {2 * policy, true},
	} {
		t.Run(name, func(t *testing.T) {
			got := statelog.Evaluate(statelog.Reading{
				BackupAge: statelog.Age(tc.age), BackupMaxAge: policy,
			})
			_, firing := find(got, statelog.KindBackupAge)
			if firing != tc.want {
				t.Errorf("a %s-old backup against a %s policy fires = %v, want %v",
					tc.age, policy, firing, tc.want)
			}
		})
	}

	// AND A DEPLOYMENT WITH NO POLICY IS NOT ALARMED AT. Zero is "we do
	// not take backups", which is a decision rather than a fault.
	stale := statelog.Age(100 * 24 * time.Hour)
	if got := statelog.Evaluate(statelog.Reading{BackupAge: stale}); len(got) != 0 {
		t.Errorf("a node with no backup policy raised %v", kindsOf(got))
	}
}

// A COMPANY WITH NO BACKUP AT ALL IS ALARMED AT, AND IS NOT TOLD AN AGE.
//
// Both halves are the bug this closes. The alarm has to fire — the trim does
// not advance one sequence until a backup exists, so an operator who never
// hears about it watches the log grow to its ceiling — and the sentence has to
// be true, which the form this replaces was not: the reading was filled with
// `backup_max_age + 1h` to reach the threshold, so a company four seconds old
// was told "the newest verified backup is 25h0m0s old" one line above the trim
// term reporting that no backup had been recorded at all. Two sentences about
// one state, and the louder of them invented a backup.
func TestNoBackupAtAllFiresAndSaysSoRatherThanNamingAnAge(t *testing.T) {
	t.Parallel()
	const policy = 24 * time.Hour

	got := statelog.Evaluate(statelog.Reading{BackupMaxAge: policy})
	alarm, firing := find(got, statelog.KindBackupAge)
	if !firing {
		t.Fatalf("a company with no backup raised %v — the trim will not "+
			"advance until one exists", kindsOf(got))
	}
	if want := "no verified backup has been recorded, and the policy asks for " +
		"one every 24h0m0s"; alarm.Detail != want {
		t.Errorf("detail = %q, want %q", alarm.Detail, want)
	}
	// AND IT NAMES NO AGE. The word the fabricated sentence turned on was
	// "old", and any age at all here is a measurement nobody took.
	if strings.Contains(alarm.Detail, " old") {
		t.Errorf("detail = %q: it reports an age for a backup that does not "+
			"exist", alarm.Detail)
	}

	// THE MEASURED CASE STILL NAMES ITS AGE, so the two states are told
	// apart by what the operator reads rather than only by a nil.
	got = statelog.Evaluate(statelog.Reading{
		BackupAge: statelog.Age(30 * time.Hour), BackupMaxAge: policy,
	})
	measured, firing := find(got, statelog.KindBackupAge)
	if !firing || !strings.Contains(measured.Detail, "30h0m0s old") {
		t.Errorf("a 30h backup reported %q, want its own age", measured.Detail)
	}
}

// EVERY KIND IS IN THE TABLE EXACTLY ONCE, and every one has a remedy.
//
// A duplicate kind would raise the same alarm twice on every surface; a kind
// with no remedy is a row that tells an operator something is wrong and
// nothing about what to do, which is the state the table was written to end.
func TestTheAlarmTableIsWellFormed(t *testing.T) {
	t.Parallel()
	kinds := statelog.Kinds()
	if len(kinds) == 0 {
		t.Fatal("the alarm table is empty")
	}
	seen := map[statelog.Kind]bool{}
	for _, kind := range kinds {
		if seen[kind] {
			t.Errorf("%s appears twice in the table", kind)
		}
		seen[kind] = true
		if strings.TrimSpace(string(kind)) == "" {
			t.Error("an alarm has no name, so nothing can be searched for it")
		}
	}
}

// AN ALARM IS LOGGED ONCE WHEN IT STARTS AND ONCE WHEN IT ENDS.
//
// Not on every tick: evaluated on a fifteen-second heartbeat, a level would
// write the same line four times a minute for as long as the condition holds,
// and the one thing an operator needs from a log — when it STARTED — would be
// buried under thousands of repetitions of the fact that it is still true.
func TestAnAlarmIsLoggedOnItsTransitionsAndTheGaugeIsALevel(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	clock := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	tracker := statelog.NewTracker(rec, func() time.Time { return clock })
	lagging := statelog.Reading{ApplyLag: 2 * time.Minute}

	tracker.Observe(t.Context(), statelog.Evaluate(lagging))
	if got := tracker.Firing()[statelog.KindApplyLag]; got != 0 {
		t.Errorf("an alarm just raised has been up for %v", got)
	}
	if got := gauge(t, rec, statelog.KindApplyLag); got != 1 {
		t.Errorf("the gauge reads %v while the alarm is up, want 1", got)
	}
	// EVERY KIND IS WRITTEN, not only the firing one: a series that is
	// never written reads as no data, and a series left at 1 is an alarm
	// that never clears for anybody reading the metric.
	if got := gauge(t, rec, statelog.KindWALLarge); got != 0 {
		t.Errorf("a quiet alarm's gauge reads %v, want 0", got)
	}

	clock = clock.Add(5 * time.Minute)
	tracker.Observe(t.Context(), statelog.Evaluate(lagging))
	if got := tracker.Firing()[statelog.KindApplyLag]; got != 5*time.Minute {
		t.Errorf("the alarm has been up for %v, want 5m — its start moved", got)
	}

	clock = clock.Add(time.Minute)
	tracker.Observe(t.Context(), statelog.Evaluate(statelog.Reading{}))
	if _, still := tracker.Firing()[statelog.KindApplyLag]; still {
		t.Error("a cleared alarm is still firing")
	}
	if got := gauge(t, rec, statelog.KindApplyLag); got != 0 {
		t.Errorf("the gauge reads %v after the alarm cleared, want 0", got)
	}
}

func find(alarms []statelog.Alarm, kind statelog.Kind) (statelog.Alarm, bool) {
	for _, a := range alarms {
		if a.Kind == kind {
			return a, true
		}
	}
	return statelog.Alarm{}, false
}

func kindsOf(alarms []statelog.Alarm) []statelog.Kind {
	out := make([]statelog.Kind, 0, len(alarms))
	for _, a := range alarms {
		out = append(out, a.Kind)
	}
	return out
}

func gauge(t *testing.T, rec *metrics.Recorder, kind statelog.Kind) float64 {
	t.Helper()
	for _, s := range rec.Read() {
		if s.Name == metrics.AlarmActive && s.Attrs["kind"] == string(kind) {
			return s.Value
		}
	}
	t.Fatalf("no gauge series for %s", kind)
	return -1
}

// A LOG'S SHARE OF THE CENSUS IS PER SEAT, PER DOMAIN LOG, NEVER ZERO — AND
// PLUS WHAT THE ENGINE READS ON ITS OWN, WHOLE.
//
// The reference company — 100 seats, one tracker log — is the 12 500 the
// sizing was derived from, so that is what its log is expected to take. A
// company twice the size doing the same work per seat takes twice it, which
// the fixed figure read as drift. A partitioned domain divides its figure
// across its own logs, ROUNDING UP, so no partition of a small company is told
// to expect nothing; and a company with no agent seat is still one seat's
// worth, since its operators read too and an expectation of zero is an alarm
// that cannot fire. The engine's own periodic reads are added after the
// division and not divided, since each of them is a barrier on every log it
// reads: without them a one-seat company on one data node reading exactly its
// census put 293 barriers a day on a log expected to take 125.
func TestALogsCensusIsPerSeatAndPerDomainLog(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		census statelog.Census
		want   int
	}{
		{"the reference company on layout 0", statelog.Census{Seats: 100, Logs: 1}, 12_500},
		{"twice the company", statelog.Census{Seats: 200, Logs: 1}, 25_000},
		{"no agent seat", statelog.Census{Logs: 1}, 125},
		{"a small company on two partitions", statelog.Census{Seats: 3, Logs: 2}, 188},
		{"the reference company on 256 partitions", statelog.Census{Seats: 100, Logs: 256}, 49},
		{"no log to share it across", statelog.Census{Seats: 5, Background: 168}, 0},
		{"one seat beside one data node's passes",
			statelog.Census{Seats: 1, Logs: 1, Background: 168}, 293},
		{"the engine's own reads are not divided across partitions",
			statelog.Census{Seats: 3, Logs: 2, Background: 504}, 692},
		{"a negative background adds nothing", statelog.Census{Seats: 1, Logs: 1, Background: -5}, 125},
	} {
		if got := tc.census.Expected(); got != tc.want {
			t.Errorf("%s: %+v expects %d a day, want %d", tc.name, tc.census, got, tc.want)
		}
	}
	if statelog.LinearizableReadsPerSeatDay*100 != 12_500 {
		t.Errorf("the per-seat census is %d, and the reference company's 12 500 "+
			"over its 100 seats is 125", statelog.LinearizableReadsPerSeatDay)
	}
}
