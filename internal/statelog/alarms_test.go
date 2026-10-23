package statelog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

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
			"no verified backup of this log has been recorded",
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
			statelog.Reading{DeferredAge: 31 * time.Minute, DeferredJudgesCopy: true,
				DeferredRecord: "at CREWLET_TRACKER_LOG@1:42"},
			"(at CREWLET_TRACKER_LOG@1:42) has been held for 31m, past the 30m " +
				"deferral grace, at which this node stops serving its copy of the estate",
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
			statelog.Reading{MaintenanceOpenFor: 2 * time.Hour,
				MaintenanceStream: "CREWLET_TRACKER_LOG", MaintenancePhase: "observe"},
			"window on CREWLET_TRACKER_LOG has been open for 2h in phase \"observe\"",
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
			statelog.Reading{LinearizableReads: 5000, LinearizableReadsExpected: 1000},
			"5000 linearizable reads a day against the 1000 this log was sized for",
		},
		"a barrier on a log whose domain declares no read": {
			// A DECLARED ZERO IS A CENSUS, and its first barrier is a
			// reader nobody counted in the log's ceiling.
			statelog.KindCensusDrift,
			statelog.Reading{LinearizableReads: 1},
			"1 linearizable reads a day against the 0 this log was sized for",
		},
		"files the object store lost or damaged": {
			statelog.KindObjectsMissing,
			statelog.Reading{ObjectsMissing: 2},
			"2 file(s) are missing from the object store or damaged in it",
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

// A WINDOW THE ALARM HOLDS FROM AN EARLIER READ SAYS SO.
//
// A capacity window this node can't read now is judged on what it last read
// there, as old as it was then, so a coordination blip neither clears the
// alarm nor raises one. The figure is then the last read's, and the detail has
// to say so, with when that read was where that is known, and that the window
// can't be finished or abandoned through this node until it reads again: an
// operator who reads "open for 2h" and finds it closed would otherwise read the
// alarm as wrong.
//
// Mutation: drop the clause and the held reading reads as a fresh one.
func TestTheMaintenanceAlarmSaysWhenItsFigureIsAnEarlierRead(t *testing.T) {
	t.Parallel()
	base := statelog.Reading{MaintenanceOpenFor: 2 * time.Hour,
		MaintenanceStream: "CREWLET_TRACKER_LOG", MaintenancePhase: "sealing"}
	for name, tc := range map[string]struct {
		unreadable bool
		readAgo    time.Duration
		says       []string
	}{
		"read now": {false, 0, nil},
		"held, read 3m ago": {true, 3 * time.Minute, []string{"had been open for 2h",
			"when this node last read it, 3m ago",
			"can't be finished or abandoned through this node until it can"}},
		"held, read at no known time": {true, 0, []string{"when this node last read it. ",
			"can't read the window now"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := base
			r.MaintenanceUnreadable, r.MaintenanceReadAgo = tc.unreadable, tc.readAgo
			var detail string
			for _, a := range statelog.Evaluate(r) {
				if a.Kind == statelog.KindMaintenanceOpen {
					detail = a.Detail
				}
			}
			if detail == "" {
				t.Fatal("a window open two hours raised nothing")
			}
			if tc.says == nil {
				if strings.Contains(detail, "last read") {
					t.Errorf("a window read now says %q", detail)
				}
				return
			}
			for _, says := range tc.says {
				if !strings.Contains(detail, says) {
					t.Errorf("the alarm says %q, want it to say %q", detail, says)
				}
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
	if want := "no verified backup of this log has been recorded, and the " +
		"policy asks for one every 24h"; alarm.Detail != want {
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

// AN ENTRY NAMING SEVERAL LOGS IS A LINE PAIR PER LOG.
//
// The report carries a measurement taken alike on several logs once, naming
// them ([statelog.AlarmDomain.Domains]) — a young fleet's missing backup, on
// every log. That is how it is carried, and nothing about which log lines are
// written: each log it names raises its own `alarm_raised`, and one leaving it
// while the rest stand writes its own `alarm_cleared`.
//
// Mutation: read only an entry's first log as its instance and this fails.
func TestAnEntryNamingSeveralLogsIsALinePairPerLog(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	tracker := statelog.NewTracker(nil, func() time.Time { return clock })
	var logs bytes.Buffer
	statelog.LogTrackerTo(tracker, slog.New(slog.NewJSONHandler(&logs, nil)))
	domainsOf := func(event string) []string {
		var out []string
		for _, raw := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
			var line map[string]any
			if json.Unmarshal([]byte(raw), &line) == nil && line["msg"] == event {
				domain, _ := line["domain"].(string)
				out = append(out, domain)
			}
		}
		slices.Sort(out)
		return out
	}
	const missing = "no verified backup of this log has been recorded"
	uncovered := func(domains ...string) statelog.Alarm {
		return statelog.Alarm{Kind: statelog.KindBackupAge, Detail: missing,
			Domains: []statelog.AlarmDomain{{Domains: domains, Detail: missing}}}
	}
	tracker.Observe(t.Context(), []statelog.Alarm{uncovered("tracker", "pages", "usage")})
	if got := domainsOf("alarm_raised"); !slices.Equal(got, []string{"pages", "tracker", "usage"}) {
		t.Fatalf("one entry naming three logs raised lines for %v, want one per log", got)
	}
	clock = clock.Add(time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{uncovered("tracker", "usage")})
	if got := domainsOf("alarm_cleared"); !slices.Equal(got, []string{"pages"}) {
		t.Errorf("the pages log leaving the entry cleared %v, want the pages log alone", got)
	}
}

// ONE KIND ON TWO LOGS IS ONE ALARM AND TWO PAIRS OF LOG LINES: each log is
// logged when it comes to stand under the kind and when it leaves it.
//
// Seven conditions are evaluated once per log, and the report lists every log a
// kind stands on under its one alarm, so one `trim_blocked` can stand on the
// tracker's log and the knowledge base's. Keyed on the kind alone, the tracker
// folded the second log into the first: it raised no line of its own, the
// tracker's clearing while the knowledge base's stood said nothing, and the one
// `alarm_cleared` that finally came carried the knowledge base's detail against
// the tracker's start. The gauge stays one series per kind, up while either is.
//
// AND A NODE-WIDE KIND IS ONE PAIR OF LINES naming no log, beside them.
//
// Mutation: key the instances on the kind alone, or report a kind's age from
// whichever of its logs the map hands back last rather than the longest, and
// this fails.
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
		return statelog.AlarmDomain{Domains: []string{domain},
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
	uncovered := statelog.Alarm{Kind: statelog.KindBackupAge,
		Domains: []statelog.AlarmDomain{{Domains: []string{"usage"},
			Detail: "no verified backup of this log has been recorded"}}}
	uncovered.Detail = uncovered.Domains[0].Detail

	// The tracker's log first, alone.
	tracker.Observe(t.Context(), []statelog.Alarm{lagging, blocked("tracker")})
	if raised := lines("alarm_raised"); len(raised) != 2 {
		t.Fatalf("raising a node alarm and one log's trim_blocked wrote %d "+
			"line(s), want two: %v", len(raised), raised)
	}

	// THE KNOWLEDGE BASE'S LOG JOINS a kind that is already up: its own line.
	// And another per-log kind raised on a third log: its own line too.
	clock = clock.Add(5 * time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{lagging, uncovered, blocked("tracker", "pages")})
	raised := lines("alarm_raised")
	if len(raised) != 4 {
		t.Fatalf("a second log joining trim_blocked, and backup_age raised on a "+
			"third, wrote %d raise line(s) in all, want four: %v", len(raised), raised)
	}
	domains := map[string]bool{}
	for _, line := range raised {
		switch line["alarm"] {
		case string(statelog.KindApplyLag):
			// A NODE'S ALARM NAMES NO LOG.
			if _, named := line["domain"]; named {
				t.Errorf("a node-wide alarm's line names a log: %v", line)
			}
		case string(statelog.KindBackupAge):
			if line["domain"] != "usage" || line["detail"] != uncovered.Detail {
				t.Errorf("backup_age's line reads %v; want the usage log's own", line)
			}
		default:
			domain, _ := line["domain"].(string)
			domains[domain] = true
			if line["detail"] != held(domain).Detail {
				t.Errorf("the %s log's line carries another's detail: %v", domain, line)
			}
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
	tracker.Observe(t.Context(), []statelog.Alarm{lagging, uncovered, blocked("pages")})
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
// operator has to look at, and one cause standing on several logs counted once
// per log read as that many problems on the health card while the gauge said
// one. It has stood since its OLDEST log's instance was raised, and a log
// leaving it moves that to the oldest log still standing.
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
			a.Domains = append(a.Domains, statelog.AlarmDomain{Domains: []string{domain},
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
// seat's worth, since its operators read too and an expectation of zero on a
// log they read fires on the first read. The engine's own periodic reads are
// added to it, since each of them is a barrier on every log it reads: without
// them a one-seat company on one data node reading exactly its census put 293
// barriers a day on a log expected to take 125.
//
// AND THE RATE IS THE DOMAIN'S DECLARATION ([statelog.ReadModel]): a log no
// read takes a barrier on declares none, and is expected to take none
// however many seats the company has — plus the engine's own reads, which are
// the engine's and not the seats'.
func TestALogsCensusIsPerSeat(t *testing.T) {
	t.Parallel()
	read := statelog.ReadModel{PerSeatDay: statelog.LinearizableReadsPerSeatDay}
	for _, tc := range []struct {
		name   string
		census statelog.Census
		want   int
	}{
		{"the reference company", statelog.Census{Reads: read, Seats: 100}, 12_500},
		{"twice the company", statelog.Census{Reads: read, Seats: 200}, 25_000},
		{"no agent seat", statelog.Census{Reads: read}, 125},
		{"one seat beside one data node's passes",
			statelog.Census{Reads: read, Seats: 1, Background: 168}, 293},
		{"a small company beside the engine's own reads",
			statelog.Census{Reads: read, Seats: 3, Background: 504}, 879},
		{"a negative background adds nothing",
			statelog.Census{Reads: read, Seats: 1, Background: -5}, 125},
		{"a domain no read takes a barrier on", statelog.Census{Seats: 300}, 0},
		{"a domain no seat reads, beside the engine's own reads",
			statelog.Census{Seats: 300, Background: 25}, 25},
		{"a negative rate reads as none",
			statelog.Census{Reads: statelog.ReadModel{PerSeatDay: -3}, Seats: 10}, 0},
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
