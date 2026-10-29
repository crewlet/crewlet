package statelog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/objectives"
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
			"2m behind",
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
			"30h old",
		},
		"no backup at all": {
			statelog.KindBackupAge,
			statelog.Reading{BackupMaxAge: 24 * time.Hour},
			"no verified backup has been recorded",
		},
		// THE TERM IS NOT THE MEASUREMENT: it rides on the log the alarm
		// stands on (AlarmDomain.Term), and the sentence says how long and
		// what the block is keeping.
		"a trim blocked past its window, keeping what a working one removes": {
			statelog.KindTrimBlocked,
			statelog.Reading{
				TrimBlockedFor: 8 * 24 * time.Hour, TrimBlockedBy: "snapshot_floor",
				TrimPastWindow: 1200, ReplayWindow: 7 * 24 * time.Hour,
			},
			"has not advanced for 192h, and the log is keeping up to 1200 record(s)",
		},
		"a deferral past the grace": {
			statelog.KindDeferredOld,
			statelog.Reading{DeferredAge: 31 * time.Minute,
				DeferredRecord: "at CREWLET_TRACKER_LOG@1:42"},
			"(at CREWLET_TRACKER_LOG@1:42) has been held for 31m, past the 30m " +
				"deferral grace, at which this node's copy of this log goes out of service",
		},
		"a floor nobody can read": {
			statelog.KindFloorUnknown,
			statelog.Reading{FloorUnknownFor: 2 * time.Minute,
				FloorUnknownCause: "coordination unreachable"},
			"unreadable here for 2m: coordination unreachable",
		},
		"a slow prefetch": {
			statelog.KindPrefetchSlow,
			statelog.Reading{PrefetchP95: time.Second},
			"the knowledge scans this node ran for turns' prefetches",
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
		"fleet history missing a node": {
			statelog.KindHistoryPartial,
			statelog.Reading{HistoryPartialFraction: 0.25},
			"25% of fleet history reads",
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
		// ROOM FOR A SECOND COPY AND ITS TENTH — 5 GiB against 4.4 — IS NOT
		// ROOM when the copy holds back 2 GiB of log on the same volume.
		"a volume with room for a second copy and not for the log a copy holds back": {
			statelog.KindVolumeLow,
			statelog.Reading{FreeBytes: 5 << 30, StoreBytes: 4 << 30, CopyHeldBytes: 2 << 30,
				StoreVolume: "/var/lib/crewlet"},
			"and for the 2.0 GiB of write-ahead log a copy holds back there while it runs",
		},
		"a write-ahead log nothing is checkpointing": {
			statelog.KindWALLarge,
			statelog.Reading{WALBytes: 2 << 30},
			"write-ahead log is 2.0 GiB",
		},
		"a write-ahead log a copy is holding": {
			statelog.KindWALLarge,
			statelog.Reading{WALBytes: 2 << 30, WALFile: "crewlet-replicated.db",
				WALCopy: "/var/lib/crewlet/snapshots/snapshot.part.db", WALCopyFor: 7 * time.Minute},
			"the write-ahead log beside crewlet-replicated.db is 2.0 GiB, held there for 7m by a " +
				"copy to /var/lib/crewlet/snapshots/snapshot.part.db, which folds it back in when it ends",
		},
		"a pool every reader queues on": {
			statelog.KindPoolStarved,
			statelog.Reading{PoolWaitP95: 200 * time.Millisecond},
			"to acquire",
		},
		"a read rate the sizing did not assume": {
			statelog.KindCensusDrift,
			statelog.Reading{LinearizableReads: 5000, LinearizableReadsExpected: 1000},
			"5000 linearizable reads a day against the 1000 this log was sized for",
		},
		"files the object store lost, damaged or will not answer for": {
			statelog.KindObjectsMissing,
			statelog.Reading{ObjectsMissing: 2},
			"2 file(s) are missing from the object store, damaged in it, or not answered for by it",
		},
		"repositories a git node's memory cannot index": {
			statelog.KindCodeMaintenanceRefused,
			statelog.Reading{CodeMaintenanceRefused: 2, CodeMaintenanceLargest: 19 << 30,
				CodeMaintenanceBudget: 1 << 30},
			"2 repository(ies) on this node are served without a bitmap: the largest's needs about " +
				"19.0 GiB, and the node can give its background work 1.0 GiB",
		},
		"repositories a maintainer has not compacted to under the pack bound": {
			statelog.KindCodeCompactionBehind,
			statelog.Reading{CodeCompactionBehind: 2, CodeCompactionBehindLargest: 2_051,
				CodeCompactionBehindRepo: "web"},
			"2 repository(ies) this node maintains hold the most live packs a repository may — the most, " +
				"web, holds 2051",
		},
		"a service objective burning its budget": {
			statelog.KindObjectiveBurn,
			statelog.Reading{ObjectiveBurns: burning(objectives.ArtifactReady, 90, 10)},
			"artifact_ready is spending its error budget 20.0 times as fast as its " +
				"objective allows over the last 1h and 20.0 times over the last 5m " +
				"(the fast pair fires at 14.4)",
		},
		"an object bucket near its reservation": {
			statelog.KindObjectsBucketNearCeiling,
			statelog.Reading{ObjectsBucketBytes: 9 << 30, ObjectsBucketMaxBytes: 10 << 30},
			"holds 9.0 GiB of its 10.0 GiB reservation",
		},
		"a clock further from the fleet's than the deletions assume": {
			statelog.KindClockSkew,
			statelog.Reading{ClockSkew: time.Hour + time.Minute, ClockSkewNodes: 3,
				ClockSkewBound: time.Hour},
			"this node's clock runs 1h 1m ahead of the fleet's clock (the median of 3 data " +
				"nodes'), past the 1h",
		},
		"a ceiling smaller than the replay window at the measured rate": {
			statelog.KindLogCeilingShort,
			// 300 MiB a day for seven days is 2.05 GiB against a
			// 1 GiB ceiling: the log fills in under four days with
			// every trim term satisfied.
			statelog.Reading{
				LogBytesPerDay: statelog.PerDay(300 << 20),
				LogMaxBytes:    1 << 30, ReplayWindow: 7 * 24 * time.Hour,
			},
			"holds 81h 55m",
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

// EVERY PERCENTILE ALARM IS DECIDED AT A HISTOGRAM BOUNDARY.
//
// A percentile in the reading is read off a histogram (the engine's
// retention.observed), and a histogram's quantile is the upper boundary of the
// bucket the percentile falls in ([metrics.Snapshot.Quantile]) — a boundary
// and nothing else. So an alarm that fires on a percentile past its budget is
// exact only where the budget is a boundary: "the percentile is past the
// budget" and "the boundary above it is" are then one statement. A budget
// between two boundaries is decided at the one above it — under twenty-one
// powers of two, `prefetch_slow`'s 500 ms fired for any p95 past 256 ms and
// said the p95 was 512 ms.
//
// The budgets are FOUND rather than listed: each duration field named for a
// percentile (…P95) is set alone, and the largest duration at which the table
// raises nothing is bisected out of the table itself — so a percentile alarm
// added later, or a budget retuned, is held without a line here, and a
// percentile no alarm reads fails as one.
//
// Mutation: set a budget between two boundaries — PoolWaitAlarm to 150 ms —
// or take a budget's boundary out of the millisecond scale, and this fails.
func TestEveryPercentileAlarmIsDecidedAtABoundary(t *testing.T) {
	t.Parallel()
	bounds, ok := metrics.Bounds(metrics.UnitMilliseconds)
	if !ok {
		t.Fatalf("no histogram boundaries for %q, the unit every percentile here is read in",
			metrics.UnitMilliseconds)
	}
	reading := reflect.TypeFor[statelog.Reading]()
	var held []string
	for i := range reading.NumField() {
		field := reading.Field(i)
		if !strings.HasSuffix(field.Name, "P95") {
			continue
		}
		if field.Type != reflect.TypeFor[time.Duration]() {
			t.Errorf("Reading.%s is a %s, want a time.Duration read off a millisecond histogram", field.Name,
				field.Type)
			continue
		}
		raises := func(d time.Duration) bool {
			var r statelog.Reading
			reflect.ValueOf(&r).Elem().Field(i).SetInt(int64(d))
			return len(statelog.Evaluate(r)) > 0
		}
		quiet, loud := time.Duration(0), 24*time.Hour
		if raises(quiet) || !raises(loud) {
			t.Errorf("Reading.%s raises %v at zero and %v at %v: a percentile no alarm holds to a budget",
				field.Name, raises(quiet), raises(loud), loud)
			continue
		}
		for loud-quiet > 1 {
			if mid := quiet + (loud-quiet)/2; raises(mid) {
				loud = mid
			} else {
				quiet = mid
			}
		}
		budget := float64(quiet) / float64(time.Millisecond)
		if !slices.Contains(bounds, budget) {
			t.Errorf("Reading.%s's alarm fires past %v, which is no boundary of the %s histograms it is read "+
				"from (%v): it fires at the boundary above, for a percentile up to that much under its budget",
				field.Name, quiet, metrics.UnitMilliseconds, bounds)
		}
		held = append(held, field.Name)
	}
	if len(held) == 0 {
		t.Fatal("no percentile field in statelog.Reading: the gate's subject moved")
	}
	t.Logf("held %v to the %s boundaries", held, metrics.UnitMilliseconds)
}

// A NODE THAT HAS NEVER MEASURED ITS RATE DOES NOT ALARM — and that is the
// pointer, not a zero.
//
// `log_ceiling_short` compares a log's ceiling against `min_age` of its own
// measured intake. Every node boots having measured nothing: its trim has not
// ticked, or the log is younger than the day the measurement spans, or the
// log is compacted and has no rate that means anything. Each of those is "no
// idea", and the rule must be silent on all of them — on every boot, on every
// node, for as long as it takes.
//
// The zero is the other half, and it is why the field is a pointer rather
// than a plain number: a log that took in NOTHING yesterday is a
// measurement, the most benign one there is, and it must survive as a value
// rather than collapse into "not measured". What cannot be allowed is either
// meaning borrowing the other's representation.
func TestANodeThatHasNeverMeasuredItsRateDoesNotAlarm(t *testing.T) {
	t.Parallel()
	const ceiling = 1 << 30
	window := 7 * 24 * time.Hour

	never := statelog.Reading{LogMaxBytes: ceiling, ReplayWindow: window}
	if got := statelog.Evaluate(never); len(got) != 0 {
		t.Errorf("a node that has never measured its rate raised %v", kindsOf(got))
	}

	// A MEASURED ZERO IS A VALUE, and it is silent because a log that
	// takes in nothing holds any window — not because it was mistaken for
	// the absent one.
	idle := statelog.Reading{
		LogBytesPerDay: statelog.PerDay(0), LogMaxBytes: ceiling, ReplayWindow: window,
	}
	if got := statelog.Evaluate(idle); len(got) != 0 {
		t.Errorf("a log that took in nothing raised %v", kindsOf(got))
	}

	// THE CONTROL: the same ceiling and window with a MEASURED rate the
	// ceiling cannot hold does fire, so the silences above are the pointer
	// working and not a rule that can never fire.
	short := statelog.Reading{
		LogBytesPerDay: statelog.PerDay(ceiling / 3), LogMaxBytes: ceiling,
		ReplayWindow: window,
	}
	if _, fired := find(statelog.Evaluate(short), statelog.KindLogCeilingShort); !fired {
		t.Error("a third of the ceiling a day against a seven-day window did " +
			"not fire, so the silences above prove nothing")
	}

	// AND AT EXACTLY THE CEILING it is silent: the window fits, just, and
	// the headroom alarm is what speaks for a log that full. A second
	// threshold here would be a second opinion (ADR-0015).
	exact := statelog.Reading{
		LogBytesPerDay: statelog.PerDay(ceiling / 8), LogMaxBytes: ceiling,
		ReplayWindow: 8 * 24 * time.Hour,
	}
	if got := statelog.Evaluate(exact); len(got) != 0 {
		t.Errorf("a window that exactly fits raised %v", kindsOf(got))
	}

	// AN UNKNOWN CEILING IS NOT A SMALL ONE, for the headroom alarm's
	// reason: a measured rate against a ceiling nobody could read says
	// nothing about whether the window fits.
	unread := statelog.Reading{
		LogBytesPerDay: statelog.PerDay(ceiling), ReplayWindow: window,
	}
	if got := statelog.Evaluate(unread); len(got) != 0 {
		t.Errorf("a measured rate against an unread ceiling raised %v", kindsOf(got))
	}
}

// A BLOCKED TRIM ALARMS ONLY ONCE IT HAS KEPT SOMETHING A WORKING TRIM WOULD
// HAVE REMOVED, and not while every young fleet's trim is blocked.
//
// The form this replaced fired on the block itself. Every fresh deployment is
// blocked — on its first backup, on its first snapshot donors, on a log nobody
// has written to — so `trim_blocked` rose within one heartbeat of boot and held
// for days, and `crewlet retention status` exited non-zero on a fleet with
// nothing wrong. The rule now borrows its threshold from `min_age` (ADR-0015),
// the window the configuration already holds every sanctioned block under,
// plus the one tick a block's clearing takes to be seen; and it measures the
// other half, whether the log holds anything past that window at all.
func TestABlockedTrimAlarmsOnlyOnceItHasKeptARecordPastTheWindow(t *testing.T) {
	t.Parallel()
	window := 7 * 24 * time.Hour
	threshold := window + statelog.TrimInterval
	for name, tc := range map[string]struct {
		reading statelog.Reading
		want    bool
	}{
		// THE YOUNG-FLEET CONTROL: blocked since boot on its first
		// backup, for longer than the window by the clock — and holding
		// nothing past the window, because nothing in its log is that
		// old yet.
		"a young fleet blocked on its first backup": {statelog.Reading{
			TrimBlockedBy: "backup_floor", TrimBlockedFor: threshold + time.Hour,
			ReplayWindow: window}, false},
		// A LOG NOBODY WRITES TO is blocked for the life of the
		// deployment — every node sits at position zero — and keeps
		// nothing at all.
		"an empty log blocked for a quarter": {statelog.Reading{
			TrimBlockedBy: "applied", TrimBlockedFor: 90 * 24 * time.Hour,
			ReplayWindow: window}, false},
		// A SANCTIONED BLOCK ON A MATURE LOG: a joiner inside its rejoin
		// window keeps records past the window, and has not been doing
		// it for a window's worth.
		"a mature log blocked for a day": {statelog.Reading{
			TrimBlockedBy: "applied", TrimBlockedFor: 24 * time.Hour,
			TrimPastWindow: 5000, ReplayWindow: window}, false},
		"exactly the window and a tick": {statelog.Reading{
			TrimBlockedBy: "snapshot_floor", TrimBlockedFor: threshold,
			TrimPastWindow: 5000, ReplayWindow: window}, false},
		"a second past it": {statelog.Reading{
			TrimBlockedBy: "snapshot_floor", TrimBlockedFor: threshold + time.Second,
			TrimPastWindow: 5000, ReplayWindow: window}, true},
		// NO WINDOW, NO THRESHOLD: a reading with no replay window has
		// nothing to borrow, and a rule with nothing to borrow is silent
		// rather than firing at a tick.
		"no replay window": {statelog.Reading{
			TrimBlockedBy: "snapshot_floor", TrimBlockedFor: threshold + time.Hour,
			TrimPastWindow: 5000}, false},
		// AND A TRIM THAT IS NOT BLOCKED keeps records past the window
		// all the time — the age term binds last — without alarming.
		"not blocked": {statelog.Reading{
			TrimBlockedFor: threshold + time.Hour, TrimPastWindow: 5000,
			ReplayWindow: window}, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, fired := find(statelog.Evaluate(tc.reading), statelog.KindTrimBlocked)
			if fired != tc.want {
				t.Errorf("fired = %v, want %v", fired, tc.want)
			}
		})
	}
}

// THE INDEX RECALL ALARM FIRES AT THE EVALUATION'S FLOOR, NOT BELOW A NUMBER OF
// ITS OWN (ADR-0015).
//
// The floor is the curve `crewlet search eval` judges a corpus against, at the
// size the training measured, handed in by the engine because the curve is the
// search package's. A recall AT the floor passes that evaluation, so it must
// not alarm; an index nobody trained measured nothing and must not alarm
// either — zero recall is the alarm, not the absence.
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

// THE CODE MAINTENANCE ALARM FIRES PAST THE NODE'S OWN BUDGET, never at a
// number of its own: the budget is what the git cache measured the node's
// memory to leave its background indexing, and a run AT it starts — so only
// an estimate past it is a repository refused. A node that serves no git
// measures nothing and must alarm about nothing, and a node whose lanes leave
// its maintenance nothing at all is the alarm, not the absence.
//
// Mutation: fire at the budget, or on any refused count without comparing,
// and a case below fails.
func TestTheCodeMaintenanceAlarmFiresPastTheNodesBudget(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		reading statelog.Reading
		fires   bool
	}{
		"an estimate at the budget": {statelog.Reading{
			CodeMaintenanceLargest: 1 << 30, CodeMaintenanceBudget: 1 << 30}, false},
		"an estimate a byte past it": {statelog.Reading{CodeMaintenanceRefused: 1,
			CodeMaintenanceLargest: 1<<30 + 1, CodeMaintenanceBudget: 1 << 30}, true},
		"a node that leaves its maintenance nothing": {statelog.Reading{CodeMaintenanceRefused: 1,
			CodeMaintenanceLargest: 320}, true},
		"a budget with nothing refused": {statelog.Reading{CodeMaintenanceBudget: 1 << 30}, false},
		"a node that serves no git":     {statelog.Reading{}, false},
	} {
		_, fired := find(statelog.Evaluate(c.reading), statelog.KindCodeMaintenanceRefused)
		if fired != c.fires {
			t.Errorf("%s: fired %v, want %v", name, fired, c.fires)
		}
	}
}

// THE COMPACTION-BEHIND ALARM BORROWS THE PACK BOUND, never a number of its
// own: the node's compactor counts the repositories it compacts that its
// listing found AT the bound the push decide refuses at (code.MaxPacksPerRepo,
// a package this framework does not import), and the rule fires at any count —
// so a repository the compactor did not count, whatever its live packs, raises
// nothing here. The largest a reading carries is a figure for the detail, and
// a rule reading it against a threshold of its own — the pack count a
// compaction aims for, say — would raise a repository a node merges routinely.
//
// Mutation: fire on the largest's count past a threshold of the rule's own
// (fifty, the target), and the reading that counts nothing raises it.
func TestTheCompactionBehindAlarmBorrowsThePackBound(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		reading statelog.Reading
		fires   bool
	}{
		"one repository at the bound": {statelog.Reading{CodeCompactionBehind: 1,
			CodeCompactionBehindLargest: 2_048, CodeCompactionBehindRepo: "web"}, true},
		"a largest the compactor counted nothing at": {statelog.Reading{
			CodeCompactionBehindLargest: 2_047, CodeCompactionBehindRepo: "web"}, false},
		"a node that compacts nothing": {statelog.Reading{}, false},
	} {
		alarm, fired := find(statelog.Evaluate(c.reading), statelog.KindCodeCompactionBehind)
		if fired != c.fires {
			t.Errorf("%s: fired %v, want %v", name, fired, c.fires)
		}
		if fired && !strings.Contains(alarm.Detail, c.reading.CodeCompactionBehindRepo) {
			t.Errorf("%s: the detail %q names no repository, and an operator is told the node is behind "+
				"on nothing they can find", name, alarm.Detail)
		}
	}
}

// A CLOCK OFF THE FLEET'S BY MORE THAN THE BOUND RAISES `clock_skew`, NAMING
// WHICH CLOCK — this node's own, or the broker member's leading the position
// register — and one inside it, or measured against no bound, does not.
//
// The bound is the one the deletions were sized against —
// objstore.AssumedClockSkew, which the node hands in with its measure, since
// this framework imports neither the object store nor the code host — so the
// alarm fires exactly where a clock stops being one the collector's grace and
// the retired-pack sweep hold on, and is silent AT it, as every rule here is
// silent at its threshold. A node that measured nothing hands in none of the
// numbers, and a skew with no bound is a measure nothing judges.
//
// THE DETAIL NAMES THE CLOCK, because that is the machine an operator has to
// fix: how far it runs, which way, and against the median of how many data
// nodes' clocks — with one, that node's own — and never the other clock when
// only one is past the bound.
//
// Mutation: fire at the bound, or on any skew without comparing it, or on a
// skew past a zero bound, or on one clock while naming the other, or read a
// clock behind as ahead, and a case below fails.
func TestAClockOffTheFleetsRaisesClockSkew(t *testing.T) {
	t.Parallel()
	const bound = time.Hour
	past := time.Hour + time.Minute
	const thisNode, member = "this node's clock", "the clock of the broker member"
	for name, c := range map[string]struct {
		reading statelog.Reading
		says    []string // what a firing alarm's detail says
		never   []string // what it never says
	}{
		"this node's clock an hour and a minute ahead": {
			reading: statelog.Reading{ClockSkew: past, ClockSkewNodes: 3, ClockSkewBound: bound},
			says: []string{"this node's clock runs 1h 1m ahead of the fleet's clock " +
				"(the median of 3 data nodes'), past the 1h"},
			never: []string{member},
		},
		"this node's clock an hour and a minute behind": {
			reading: statelog.Reading{ClockSkew: -past, BrokerClockSkew: time.Minute,
				ClockSkewNodes: 5, ClockSkewBound: bound},
			says: []string{"this node's clock runs 1h 1m behind the fleet's clock " +
				"(the median of 5 data nodes'), past the 1h"},
			never: []string{member},
		},
		"the member's clock an hour and a minute ahead": {
			reading: statelog.Reading{BrokerClockSkew: past, ClockSkewNodes: 3, ClockSkewBound: bound},
			says: []string{"the clock of the broker member leading the position register runs " +
				"1h 1m ahead of the fleet's clock (the median of 3 data nodes'), past the 1h"},
			never: []string{thisNode},
		},
		"both clocks, each past the bound": {
			reading: statelog.Reading{ClockSkew: 90 * time.Minute, BrokerClockSkew: 90 * time.Minute,
				ClockSkewNodes: 3, ClockSkewBound: bound},
			says: []string{"this node's clock runs 1h 30m ahead of the fleet's clock (the median " +
				"of 3 data nodes'), and the clock of the broker member leading the position " +
				"register runs 1h 30m ahead of it, past the 1h"},
		},
		"the member's clock behind a fleet of one": {
			reading: statelog.Reading{BrokerClockSkew: -past, ClockSkewNodes: 1, ClockSkewBound: bound},
			says: []string{"the clock of the broker member leading the position register runs " +
				"1h 1m behind the fleet's clock (this node's own: the register holds no other " +
				"data node's recent row)"},
			never: []string{thisNode},
		},
		"fifty-nine minutes off, either clock and either way": {
			reading: statelog.Reading{ClockSkew: 59 * time.Minute, BrokerClockSkew: -59 * time.Minute,
				ClockSkewNodes: 3, ClockSkewBound: bound},
		},
		"exactly the bound off": {
			reading: statelog.Reading{ClockSkew: -bound, BrokerClockSkew: bound,
				ClockSkewNodes: 3, ClockSkewBound: bound},
		},
		"clocks in time with the fleet's": {
			reading: statelog.Reading{ClockSkewNodes: 3, ClockSkewBound: bound},
		},
		"a skew with no bound to judge it by": {
			reading: statelog.Reading{ClockSkew: 2 * time.Hour, BrokerClockSkew: 2 * time.Hour,
				ClockSkewNodes: 3},
		},
		"a node that measured nothing": {},
	} {
		alarm, fired := find(statelog.Evaluate(c.reading), statelog.KindClockSkew)
		if fires := len(c.says) > 0; fired != fires {
			t.Errorf("%s: fired %v, want %v", name, fired, fires)
			continue
		}
		for _, want := range c.says {
			if !strings.Contains(alarm.Detail, want) {
				t.Errorf("%s: the alarm reads %q, want it to say %q", name, alarm.Detail, want)
			}
		}
		for _, not := range c.never {
			if strings.Contains(alarm.Detail, not) {
				t.Errorf("%s: the alarm reads %q, naming %q, which is inside the bound",
					name, alarm.Detail, not)
			}
		}
	}
}

// THE FLOOR ALARM NAMES ONLY THE LOGS WHOSE READS IT REFUSES, EACH WITH ITS
// OWN AGE.
//
// A log whose floor began failing inside [statelog.FloorCacheStale] is still
// served from its memo. Read as one longest age beside every failing name, the
// alarm told an operator that log's reads were refused, with another log's age
// as the evidence, and its remedy said so of every name it gave. Each log is
// evaluated on its own measurement, so a log inside the bound is named by no
// alarm and every log past it carries its own age.
//
// Mutation: compare FloorUnknownFor against zero rather than the bound, or
// hand every log the node's longest age, and a case below fails.
func TestTheFloorAlarmNamesOnlyTheLogsPastTheBound(t *testing.T) {
	t.Parallel()
	past := statelog.FloorCacheStale
	for name, c := range map[string]struct {
		unread map[string]time.Duration
		names  map[string]string // each log the alarm names, and the age it says
	}{
		"one log past the bound, one inside it": {
			unread: map[string]time.Duration{"pages": past + time.Minute, "tracker": time.Second},
			names:  map[string]string{"pages": "for 2m"},
		},
		"three past the bound, each with its own age": {
			unread: map[string]time.Duration{
				"tracker": past + 30*time.Second, "pages": past + time.Minute,
				"usage": past + 2*time.Minute,
			},
			names: map[string]string{"tracker": "for 1m 30s", "pages": "for 2m", "usage": "for 3m"},
		},
		"every log inside the bound": {
			unread: map[string]time.Duration{"pages": past, "tracker": time.Second},
		},
	} {
		var domains []statelog.DomainInputs
		for _, log := range slices.Sorted(maps.Keys(c.unread)) {
			d := healthyDomain(log, true)
			d.FloorUnknownFor = c.unread[log]
			d.FloorUnknownCause = "coordination unreachable"
			domains = append(domains, d)
		}
		rep := statelog.NewReport(statelog.ReportInputs{
			NodeID: "node-1", At: reportAt, RegisterReadable: true,
			ReplayWindow: replayWindow, Domains: domains,
		})
		got := map[string]string{}
		for _, a := range rep.Alarms {
			if a.Kind != statelog.KindFloorUnknown {
				continue
			}
			for _, d := range a.Domains {
				got[d.Domain] = d.Detail
			}
		}
		if len(got) != len(c.names) {
			t.Errorf("%s: floor_unknown named %v, want exactly %v", name, got, c.names)
			continue
		}
		for log, age := range c.names {
			if !strings.Contains(got[log], age) {
				t.Errorf("%s: the %s log's floor_unknown reads %q, want its own age, %q",
					name, log, got[log], age)
			}
		}
	}
}

// THE UNREADABLE-FILES ALARM IS SILENT ON NOTHING MISSING — including a reading
// from a node that holds no collector duty, which reports no audit at all.
func TestTheObjectStoreAlarmIsSilentWithNothingMissing(t *testing.T) {
	t.Parallel()
	if got := statelog.Evaluate(statelog.Reading{}); len(got) != 0 {
		t.Errorf("an empty reading raised %v", kindsOf(got))
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
		"one every 24h"; alarm.Detail != want {
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
	if !firing || !strings.Contains(measured.Detail, "30h old") {
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
// Not on every tick: evaluated on a ten-second heartbeat, a level would
// write the same line six times a minute for as long as the condition holds,
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

// ONE KIND ON TWO LOGS IS ONE ALARM AND TWO PAIRS OF LOG LINES: each log is
// logged when it comes to stand under the kind and when it leaves it.
//
// Six conditions are evaluated once per log, and the report lists every log a
// kind stands on under its one alarm, so one `trim_blocked` can stand on the
// tracker's log and the knowledge base's. Keyed on the kind alone, the tracker
// folded the second log into the first: it raised no line of its own, the
// tracker's clearing while the knowledge base's stood said nothing, and the one
// `alarm_cleared` that finally came carried the knowledge base's detail against
// the tracker's start. The gauge stays one series per kind, up while either is.
//
// AND A NODE-WIDE KIND IS ONE PAIR OF LINES whatever logs it lists: the logs
// `backup_age` holds are one problem with one remedy, so a log joining its list
// writes nothing.
//
// Mutation: key the instances on the kind alone, form one per listed log for a
// node-wide kind, or report a kind's age from whichever of its logs the map
// hands back last rather than the longest, and this fails.
func TestOneKindOnTwoLogsIsOneAlarmWithALinePairPerLog(t *testing.T) {
	t.Parallel()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("recorder: %v", err)
	}
	clock := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	tracker := statelog.NewTracker(rec, func() time.Time { return clock })
	var logs bytes.Buffer
	statelog.LogTrackerTo(tracker, slog.New(slog.NewJSONHandler(&logs, nil)))
	lines := func(event string) []map[string]any {
		var out []map[string]any
		for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var line map[string]any
			if json.Unmarshal([]byte(raw), &line) == nil && line["msg"] == event {
				out = append(out, line)
			}
		}
		return out
	}
	held := func(domain string) statelog.AlarmDomain {
		return statelog.AlarmDomain{Domain: domain,
			Detail: "the " + domain + " log's trim has not advanced"}
	}
	blocked := func(logs ...string) statelog.Alarm {
		a := statelog.Alarm{Kind: statelog.KindTrimBlocked}
		for _, domain := range logs {
			a.Domains = append(a.Domains, held(domain))
		}
		a.Detail = a.Domains[0].Detail
		return a
	}
	lagging := statelog.Alarm{Kind: statelog.KindApplyLag, Detail: "this node is 2m behind"}
	noBackup := statelog.Alarm{Kind: statelog.KindBackupAge,
		Detail: "no verified backup has been recorded"}

	// The tracker's log first, alone.
	tracker.Observe(t.Context(), []statelog.Alarm{lagging, noBackup, blocked("tracker")})
	if raised := lines("alarm_raised"); len(raised) != 3 {
		t.Fatalf("raising two node alarms and one log's trim_blocked wrote %d "+
			"line(s), want three: %v", len(raised), raised)
	}

	// THE KNOWLEDGE BASE'S LOG JOINS a kind that is already up: its own line.
	// And a log joining backup_age's list is no line at all.
	clock = clock.Add(5 * time.Minute)
	withHeld := noBackup
	withHeld.Domains = []statelog.AlarmDomain{held("usage")}
	tracker.Observe(t.Context(), []statelog.Alarm{lagging, withHeld, blocked("tracker", "pages")})
	raised := lines("alarm_raised")
	if len(raised) != 4 {
		t.Fatalf("a second log joining trim_blocked, and a log listed under "+
			"backup_age, wrote %d raise line(s) in all, want four: %v", len(raised), raised)
	}
	domains := map[string]bool{}
	for _, line := range raised {
		if line["alarm"] != string(statelog.KindTrimBlocked) {
			// A NODE'S ALARM NAMES NO LOG, even one that lists logs.
			if _, named := line["domain"]; named {
				t.Errorf("a node-wide alarm's line names a log: %v", line)
			}
			continue
		}
		domain, _ := line["domain"].(string)
		domains[domain] = true
		if line["detail"] != held(domain).Detail {
			t.Errorf("the %s log's line carries another's detail: %v", domain, line)
		}
	}
	if !domains["tracker"] || !domains["pages"] {
		t.Errorf("trim_blocked was raised for %v, want each log once", domains)
	}
	// THE KIND'S AGE IS ITS LONGEST-STANDING LOG'S: the tracker's five
	// minutes, never the knowledge base's none. Asked repeatedly, because
	// which instance a map hands back last is random per call.
	for range 16 {
		if got := tracker.Firing()[statelog.KindTrimBlocked]; got != 5*time.Minute {
			t.Fatalf("trim_blocked on two logs has been up for %v, want the "+
				"tracker's log's 5m", got)
		}
	}

	// THE TRACKER'S LOG LEAVES while the knowledge base's stands: its own line,
	// its own age, its own detail, and the kind is still up.
	clock = clock.Add(20 * time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{lagging, withHeld, blocked("pages")})
	cleared := lines("alarm_cleared")
	if len(cleared) != 1 || cleared[0]["domain"] != "tracker" ||
		cleared[0]["for"] != "25m" || cleared[0]["detail"] != held("tracker").Detail {
		t.Fatalf("one log leaving the kind wrote %v; want one line naming the "+
			"tracker's log, its twenty-five minutes and its detail", cleared)
	}
	if got := gauge(t, rec, statelog.KindTrimBlocked); got != 1 {
		t.Errorf("the gauge reads %v while the knowledge base's log stands, want 1", got)
	}
	if got := tracker.Firing()[statelog.KindTrimBlocked]; got != 20*time.Minute {
		t.Errorf("trim_blocked on the knowledge base's log alone has been up "+
			"for %v, want its own 20m", got)
	}

	clock = clock.Add(10 * time.Minute)
	tracker.Observe(t.Context(), nil)
	cleared = lines("alarm_cleared")
	if len(cleared) != 4 {
		t.Fatalf("clearing everything wrote %d clear line(s) in all, want four: %v",
			len(cleared), cleared)
	}
	for _, line := range cleared[1:] {
		if line["alarm"] == string(statelog.KindTrimBlocked) &&
			(line["domain"] != "pages" || line["for"] != "30m") {
			t.Errorf("the knowledge base's clear reads %v; want its own thirty minutes", line)
		}
	}
	if got := gauge(t, rec, statelog.KindTrimBlocked); got != 0 {
		t.Errorf("the gauge reads %v once every log's alarm cleared, want 0", got)
	}
}

// THE STANDING ALARMS ARE THE LATEST EVALUATION'S, ONE PER KIND, THE OLDEST
// FIRST, and "not evaluated" is not "none firing".
//
// This is what every node's health envelope counts and names, so it has to
// be exactly what the gauge and the log were just told: an alarm that cleared
// is gone from it at once, and the name a health card shows is the condition
// that has gone unanswered longest rather than whichever the table happens to
// list first. Before any evaluation it says it cannot say, because an empty
// list would render a node that has not looked as a node with nothing wrong.
//
// AND A KIND ON TWO LOGS STANDS ONCE: the count is how many problems an
// operator has to look at, and one cause standing on four logs counted as four
// read as four problems on the health card while the gauge said one. It has
// stood since its OLDEST log's instance was raised, and a log leaving it moves
// that to the oldest log still standing.
//
// Mutation: return the table's order rather than the age order, report
// (nil, true) before an evaluation, count a kind once per log, or date a kind
// by its newest log, and this fails.
func TestTheStandingAlarmsAreTheLatestEvaluationsOldestFirst(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	tracker := statelog.NewTracker(nil, func() time.Time { return clock })

	if standing, known := tracker.Standing(); known || standing != nil {
		t.Fatalf("before any evaluation Standing = (%v, %v), want (nil, false)", standing, known)
	}

	// The knowledge base's blocked trim is raised first; the WAL alarm joins
	// it two minutes later, and apply lag (which the table lists ahead of
	// both) and the tracker's blocked trim three minutes after that.
	wal := statelog.Alarm{Kind: statelog.KindWALLarge, Detail: "the write-ahead log is 2.0 GiB"}
	lag := statelog.Alarm{Kind: statelog.KindApplyLag, Detail: "this node is 2m behind the log"}
	blocked := func(logs ...string) statelog.Alarm {
		a := statelog.Alarm{Kind: statelog.KindTrimBlocked, Detail: "the trim has not advanced"}
		for _, domain := range logs {
			a.Domains = append(a.Domains, statelog.AlarmDomain{Domain: domain,
				Detail: "the trim has not advanced"})
		}
		return a
	}
	tracker.Observe(t.Context(), []statelog.Alarm{blocked("pages")})
	clock = clock.Add(2 * time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{blocked("pages"), wal})
	clock = clock.Add(3 * time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{lag, blocked("tracker", "pages"), wal})

	standing, known := tracker.Standing()
	want := []statelog.Kind{statelog.KindTrimBlocked, statelog.KindWALLarge, statelog.KindApplyLag}
	if !known || !slices.Equal(standing, want) {
		t.Errorf("Standing = (%v, %v), want (%v, true): the longest-standing "+
			"alarm leads whatever the table's order is, and a kind on two logs "+
			"stands once", standing, known, want)
	}

	// THE OLDEST LOG LEAVES: the kind now dates from the tracker's log, which
	// joined after the WAL alarm, so it no longer leads.
	clock = clock.Add(time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{lag, blocked("tracker"), wal})
	standing, _ = tracker.Standing()
	want = []statelog.Kind{statelog.KindWALLarge, statelog.KindApplyLag, statelog.KindTrimBlocked}
	if !slices.Equal(standing, want) {
		t.Errorf("after the oldest log left Standing = %v, want %v: the kind has "+
			"stood since its oldest log still standing", standing, want)
	}

	clock = clock.Add(time.Minute)
	tracker.Observe(t.Context(), statelog.Evaluate(statelog.Reading{}))
	standing, known = tracker.Standing()
	if !known || standing == nil || len(standing) != 0 {
		t.Errorf("after an evaluation that raised nothing Standing = (%v, %v), "+
			"want an empty list that is known", standing, known)
	}
}

// A FIRING ALARM IS MARKED SINCE IT WAS RAISED, and the marks are the standing
// alarms: the same kinds in the same order, so the presence heartbeat every
// peer reads can never name a different set, or a different longest-standing
// alarm, from the node's own health card.
//
// A kind standing on several logs is dated by its OLDEST log, which a later log
// joining it does not move and which moves to the oldest log still standing
// when that one leaves; a kind that stays up keeps its start however its detail
// changes; and before any evaluation there is nothing to mark, which is not a
// node with nothing firing.
//
// Mutation: date a kind by its newest log, re-date it on every evaluation, mark
// in the table's order, or answer (nil, true) before an evaluation, and this
// fails.
func TestAFiringAlarmIsMarkedSinceItWasRaised(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	clock := t0
	tracker := statelog.NewTracker(nil, func() time.Time { return clock })

	if marks, known := tracker.Marks(); known || marks != nil {
		t.Fatalf("before any evaluation Marks = (%v, %v), want (nil, false)", marks, known)
	}

	lag := func(detail string) statelog.Alarm {
		return statelog.Alarm{Kind: statelog.KindApplyLag, Detail: detail}
	}
	wal := statelog.Alarm{Kind: statelog.KindWALLarge, Detail: "the write-ahead log is 2.0 GiB"}
	blocked := func(logs ...string) statelog.Alarm {
		a := statelog.Alarm{Kind: statelog.KindTrimBlocked, Detail: "the trim has not advanced"}
		for _, domain := range logs {
			a.Domains = append(a.Domains, statelog.AlarmDomain{Domain: domain,
				Detail: "the trim has not advanced"})
		}
		return a
	}
	marked := func(when string, want []coord.AlarmMark) {
		t.Helper()
		marks, known := tracker.Marks()
		if !known || !slices.Equal(marks, want) {
			t.Errorf("%s: Marks = (%v, %v), want (%v, true)", when, marks, known, want)
		}
		standing, _ := tracker.Standing()
		kinds := make([]statelog.Kind, 0, len(marks))
		for _, m := range marks {
			kinds = append(kinds, statelog.Kind(m.Kind))
		}
		if !slices.Equal(kinds, standing) {
			t.Errorf("%s: the marks name %v and the standing alarms %v", when, kinds, standing)
		}
	}

	// The knowledge base's blocked trim is raised first, the WAL alarm two
	// minutes later, and apply lag (which the table lists ahead of both)
	// with the tracker's blocked trim three minutes after that.
	tracker.Observe(t.Context(), []statelog.Alarm{blocked("pages")})
	clock = t0.Add(2 * time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{blocked("pages"), wal})
	clock = t0.Add(5 * time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{lag("2m behind"), blocked("tracker", "pages"), wal})
	marked("with three alarms up", []coord.AlarmMark{
		{Kind: string(statelog.KindTrimBlocked), Since: t0},
		{Kind: string(statelog.KindWALLarge), Since: t0.Add(2 * time.Minute)},
		{Kind: string(statelog.KindApplyLag), Since: t0.Add(5 * time.Minute)},
	})

	// THE OLDEST LOG LEAVES, and the lag grows: the blocked trim now dates
	// from the tracker's log, and the lag keeps its start.
	clock = t0.Add(6 * time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{lag("3m behind"), blocked("tracker"), wal})
	marked("after the oldest log left", []coord.AlarmMark{
		{Kind: string(statelog.KindWALLarge), Since: t0.Add(2 * time.Minute)},
		{Kind: string(statelog.KindApplyLag), Since: t0.Add(5 * time.Minute)},
		{Kind: string(statelog.KindTrimBlocked), Since: t0.Add(5 * time.Minute)},
	})

	clock = t0.Add(7 * time.Minute)
	tracker.Observe(t.Context(), statelog.Evaluate(statelog.Reading{}))
	marked("after an evaluation that raised nothing", []coord.AlarmMark{})
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

// THE TABLE IS EVALUATED AT A QUARTER OF ITS FINEST THRESHOLD OR FINER.
//
// Every alarm here fires at a duration another decision made, and the table is
// only as punctual as the loop evaluating it: a condition that crossed its
// threshold is named at most one interval later. The table once ran on the
// trim's quarter-hour, which made every sixty-second alarm fire up to fifteen
// minutes late while every case above — each handed a reading directly — went
// on passing. Four evaluations inside the shortest threshold bounds the
// lateness at a quarter of it.
func TestTheTableIsEvaluatedFinerThanItsThresholds(t *testing.T) {
	t.Parallel()
	for name, threshold := range map[string]time.Duration{
		"StallGrace":      statelog.StallGrace,
		"FloorCacheStale": statelog.FloorCacheStale,
		"DeferralGrace":   statelog.DeferralGrace,
	} {
		if 4*statelog.AlarmInterval > threshold {
			t.Errorf("the table is evaluated every %s, which names a condition "+
				"past %s (%s) up to a quarter of it late or worse",
				statelog.AlarmInterval, name, threshold)
		}
	}
}

// THE TABLE IS EVALUATED ON THE POSITION HEARTBEAT, the loop that publishes
// the positions its fastest input — a node's lag behind the log — is measured
// against.
//
// Slower raises every threshold to its own period (the case above); faster
// re-evaluates facts nothing has republished. The cadence was once one
// declaration in the engine bound to the heartbeat, then two literals in two
// packages bound by a comment saying they were the same number — which held
// only until somebody retuned one. This is the binding stated where a retune
// of either fails it.
func TestTheTableIsEvaluatedOnThePositionHeartbeat(t *testing.T) {
	t.Parallel()
	switch every, beat := statelog.AlarmInterval, statelog.PositionHeartbeat; {
	case every > beat:
		t.Errorf("the table is evaluated every %s and the positions it reads "+
			"are republished every %s — a condition the register already shows "+
			"is named up to %s later than the heartbeat could have named it",
			every, beat, every-beat)
	case every < beat:
		t.Errorf("the table is evaluated every %s and the positions it reads "+
			"are republished every %s — evaluations between two beats re-read "+
			"a register that has not moved", every, beat)
	}
}

// A LOG'S CENSUS IS PER SEAT, NEVER ZERO — AND PLUS WHAT THE ENGINE READS ON
// ITS OWN.
//
// The reference company — 100 seats, one tracker log — is the 12 500 the
// sizing was derived from, so that is what its log is expected to take. A
// company twice the size doing the same work per seat takes twice it, which
// the fixed figure read as drift. A company with no agent seat is still one
// seat's worth, since its operators read too and an expectation of zero is an
// alarm that cannot fire. The engine's own periodic reads are added to it,
// since each of them is a barrier on every log it reads: without them a
// one-seat company on one data node reading exactly its census put 293
// barriers a day on a log expected to take 125.
func TestALogsCensusIsPerSeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		census statelog.Census
		want   int
	}{
		{"the reference company", statelog.Census{Seats: 100}, 12_500},
		{"twice the company", statelog.Census{Seats: 200}, 25_000},
		{"no agent seat", statelog.Census{}, 125},
		{"one seat beside one data node's passes",
			statelog.Census{Seats: 1, Background: 168}, 293},
		{"a small company beside the engine's own reads",
			statelog.Census{Seats: 3, Background: 504}, 879},
		{"a negative background adds nothing", statelog.Census{Seats: 1, Background: -5}, 125},
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

// A LOG'S CENSUS COUNTS THE READERS ITS OWN SIZING DECLARES, beside the seats
// and the engine's passes: the code log's git clients — people and their CI,
// whom no seat count predicts — are a term of their own, so a small company's
// git traffic is held to what its code log was sized for rather than to 125 a
// day per agent seat. A negative term adds nothing, like Background.
func TestTheCensusCountsALogsClients(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		census statelog.Census
		want   int
	}{
		{"five seats, the collector and a thousand git reads",
			statelog.Census{Seats: 5, Background: 25, Clients: 1000}, 1650},
		{"clients alone beside no seat", statelog.Census{Clients: 112_788}, 125 + 112_788},
		{"a negative client term adds nothing", statelog.Census{Seats: 5, Clients: -40}, 625},
	} {
		if got := tc.census.Expected(); got != tc.want {
			t.Errorf("%s: %+v expects %d a day, want %d", tc.name, tc.census, got, tc.want)
		}
	}
}

// THE HISTORY ALARM BORROWS THE READ BUDGET (ADR-0015).
//
// What makes a fleet history answer partial is a node that did not answer
// inside the fleet read budget — so that budget IS the threshold, and the
// alarm names the one the reads waited on, which the node that counted them
// states in its reading ([statelog.Reading.HistoryReadBudget]). A second
// number here would be a second opinion about one event, and the two would
// drift: an operator told "every node had 5s" by a node that waited two would
// go looking for a slowness that is not there. A reading that states no
// budget names none, rather than a zero budget nobody waited on.
//
// Mutation: spell a budget as a literal in the detail, or print the zero
// budget, and this fails.
func TestHistoryPartialNamesTheBudgetItsReadsWaitedOn(t *testing.T) {
	t.Parallel()
	for _, budget := range []time.Duration{2 * time.Second, 3500 * time.Millisecond} {
		alarm, found := find(statelog.Evaluate(statelog.Reading{
			HistoryPartialFraction: 0.5, HistoryReadBudget: budget,
		}), statelog.KindHistoryPartial)
		if !found {
			t.Fatal("a partial fleet read raised nothing")
		}
		spelled := map[time.Duration]string{
			2 * time.Second: "the 2s fleet read budget", 3500 * time.Millisecond: "the 3.5s fleet read budget",
		}[budget]
		if !strings.Contains(alarm.Detail, spelled) {
			t.Errorf("detail %q does not name the %s budget the reads waited on as %q",
				alarm.Detail, budget, spelled)
		}
	}
	alarm, found := find(statelog.Evaluate(statelog.Reading{HistoryPartialFraction: 0.5}),
		statelog.KindHistoryPartial)
	if !found {
		t.Fatal("a partial fleet read with no stated budget raised nothing")
	}
	if strings.Contains(alarm.Detail, "0ms") || !strings.Contains(alarm.Detail, "inside the fleet read budget") {
		t.Errorf("detail %q names a budget the reading never stated", alarm.Detail)
	}
	if _, found := find(statelog.Evaluate(statelog.Reading{}), statelog.KindHistoryPartial); found {
		t.Error("a node that read no fleet history alarmed about it")
	}
}

// THE BUCKET ALARM FIRES AT FOUR FIFTHS, TWICE A LOG'S HEADROOM, and an
// unmeasured or unreserved bucket raises nothing.
func TestTheBucketAlarmFiresAtTwiceALogsHeadroom(t *testing.T) {
	t.Parallel()
	if statelog.ObjectsHeadroomAlarmFraction != 2*statelog.HeadroomAlarmFraction {
		t.Fatalf("the bucket's headroom is %v, want twice a log's %v",
			statelog.ObjectsHeadroomAlarmFraction, statelog.HeadroomAlarmFraction)
	}
	for _, c := range []struct {
		bytes, max int64
		fires      bool
	}{
		{79, 100, false}, {81, 100, true}, {100, 100, true}, {5, 0, false}, {0, 0, false},
	} {
		_, fired := find(statelog.Evaluate(statelog.Reading{
			ObjectsBucketBytes: c.bytes << 20, ObjectsBucketMaxBytes: c.max << 20,
		}), statelog.KindObjectsBucketNearCeiling)
		if fired != c.fires {
			t.Errorf("%d MiB of %d MiB: fired %v, want %v", c.bytes, c.max, fired, c.fires)
		}
	}
}

// burning is what a meter judges after good and bad events of objective n
// in the current minute: the burns the engine hands the alarm table.
func burning(n objectives.Name, good, bad int) []objectives.Burn {
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	m := objectives.NewMeter(nil, func() time.Time { return at })
	for range good {
		m.Record(n, 0, true)
	}
	for range bad {
		m.Record(n, 0, false)
	}
	return m.Burning()
}

// THE BURN ALARM BORROWS THE OBJECTIVES' JUDGEMENT WHOLE: it fires for every
// objective the meter judged burning, naming each, and for nothing the meter
// judged inside its budget.
func TestTheBurnAlarmFiresOnWhatTheObjectivesJudge(t *testing.T) {
	t.Parallel()
	inside := burning(objectives.PublishSuccess, 999, 1)
	if len(inside) != 0 {
		t.Fatalf("control: one failure in a thousand is inside a 99.9%% budget, and the "+
			"meter judged %+v", inside)
	}
	if _, fired := find(statelog.Evaluate(statelog.Reading{ObjectiveBurns: inside}),
		statelog.KindObjectiveBurn); fired {
		t.Error("the alarm fired on objectives inside their budgets")
	}
	both := append(burning(objectives.PublishSuccess, 90, 10),
		burning(objectives.ViewFirstByte, 80, 20)...)
	alarm, fired := find(statelog.Evaluate(statelog.Reading{ObjectiveBurns: both}),
		statelog.KindObjectiveBurn)
	if !fired {
		t.Fatal("two burning objectives raised nothing")
	}
	for _, want := range []string{"publish_success is spending", "view_first_byte is spending"} {
		if !strings.Contains(alarm.Detail, want) {
			t.Errorf("detail %q does not name %q", alarm.Detail, want)
		}
	}
}
