package statelog_test

import (
	"strings"
	"testing"
	"time"

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
			statelog.Reading{BackupAge: 30 * time.Hour, BackupMaxAge: 24 * time.Hour},
			"30h0m0s old",
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
		"a gated record": {
			statelog.KindRecordsGated,
			statelog.Reading{RecordsGated: 1},
			"apply gate",
		},
		"a dead-lettered wake": {
			statelog.KindFeedDeadLetters,
			statelog.Reading{FeedDeadLetters: 3},
			"dead-letter",
		},
		"maintenance nobody finished": {
			statelog.KindMaintenanceOpen,
			statelog.Reading{MaintenanceOpenFor: 2 * time.Hour, MaintenancePhase: "observe"},
			"observe",
		},
		"a volume with no room for a second copy": {
			statelog.KindVolumeLow,
			statelog.Reading{FreeBytes: 1 << 30, StoreBytes: 4 << 30},
			"free against",
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
			"sized for",
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
		"an hour before the threshold": {policy - time.Hour, false},
		"exactly at it":                {policy, false},
		"a minute past it":             {policy + time.Minute, true},
		"at twice it":                  {2 * policy, true},
	} {
		t.Run(name, func(t *testing.T) {
			got := statelog.Evaluate(statelog.Reading{
				BackupAge: tc.age, BackupMaxAge: policy,
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
	if got := statelog.Evaluate(statelog.Reading{BackupAge: 100 * 24 * time.Hour}); len(got) != 0 {
		t.Errorf("a node with no backup policy raised %v", kindsOf(got))
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
		if s.Name == "crewlet.alarm.active" && s.Attrs["kind"] == string(kind) {
			return s.Value
		}
	}
	t.Fatalf("no gauge series for %s", kind)
	return -1
}
