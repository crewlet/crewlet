package statelog_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
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
		"a trim blocked past its window, keeping what a working one removes": {
			statelog.KindTrimBlocked,
			statelog.Reading{
				TrimBlockedFor: 8 * 24 * time.Hour, TrimBlockedBy: "snapshot_floor",
				TrimPastWindow: 1200, ReplayWindow: 7 * 24 * time.Hour,
			},
			"snapshot_floor — and the log is keeping up to 1200 record(s)",
		},
		"a deferral past the grace": {
			statelog.KindDeferredOld,
			statelog.Reading{DeferredAge: 31 * time.Minute, DeferredSheds: true,
				DeferredRecord: "at CREWLET_TRACKER_LOG@1:42"},
			"(at CREWLET_TRACKER_LOG@1:42) has been held for 31m, past the 30m " +
				"deferral grace, at which this node's seats move to a peer",
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
		"a binding that has dangled past the stall grace": {
			statelog.KindBindingDangling,
			statelog.Reading{
				DanglingBindings: 2, DanglingBindingFor: 5 * time.Minute,
				DanglingBindingSeat: "platform-lead",
			},
			`"platform-lead"`,
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

// A DANGLING BINDING YOUNGER THAN THE STALL GRACE DOES NOT FIRE.
//
// The residue is legal and it is usually brief: a bind and a seat's removal
// racing on two logs, or a hire this node's chart applier has not reached
// yet, is a dangling binding for seconds and then is not. Alarming on its
// first sighting would page somebody for a state that was already clearing —
// so the rule fires at the grace that already separates a node catching up
// from one that has stopped (ADR-0015), and not a second before.
func TestADanglingBindingYoungerThanTheGraceDoesNotFire(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reading statelog.Reading
		want    bool
	}{
		"just seen": {statelog.Reading{
			DanglingBindings: 1, DanglingBindingSeat: "ops-lead"}, false},
		"half the grace": {statelog.Reading{DanglingBindings: 1,
			DanglingBindingFor: statelog.StallGrace / 2, DanglingBindingSeat: "ops-lead"}, false},
		"exactly the grace": {statelog.Reading{DanglingBindings: 1,
			DanglingBindingFor: statelog.StallGrace, DanglingBindingSeat: "ops-lead"}, false},
		"a second past it": {statelog.Reading{DanglingBindings: 1,
			DanglingBindingFor:  statelog.StallGrace + time.Second,
			DanglingBindingSeat: "ops-lead"}, true},
		// A DURATION WITH NOBODY BEHIND IT IS NOT A RESIDUE: the count
		// is what says one exists, so an age left over from a residue
		// that cleared cannot fire on its own.
		"an age with nobody dangling": {statelog.Reading{
			DanglingBindingFor: time.Hour}, false},
	} {
		t.Run(name, func(t *testing.T) {
			_, fired := find(statelog.Evaluate(tc.reading), statelog.KindBindingDangling)
			if fired != tc.want {
				t.Errorf("fired = %v, want %v", fired, tc.want)
			}
		})
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

// ONE KIND ON TWO LOGS IS TWO ALARMS, each logged when it starts and when it
// ends.
//
// Five conditions are evaluated once per log, so one evaluation can carry
// `trim_blocked` for the tracker's log and again for the knowledge base's.
// Keyed on the kind alone, the tracker folded the second into the first: it
// raised no line of its own, the tracker's clearing while the knowledge base's
// stood said nothing, and the one `alarm_cleared` that finally came carried the
// knowledge base's detail against the tracker's start. The gauge stays one
// series per kind, up while either is.
func TestOneKindOnTwoLogsIsTwoAlarms(t *testing.T) {
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
	blocked := func(domain string) statelog.Alarm {
		return statelog.Alarm{Kind: statelog.KindTrimBlocked, Domain: domain,
			Detail: domain + ": the trim has not advanced"}
	}
	lagging := statelog.Alarm{Kind: statelog.KindApplyLag, Detail: "this node is 2m behind"}

	tracker.Observe(t.Context(), []statelog.Alarm{lagging, blocked("tracker"), blocked("pages")})
	raised := lines("alarm_raised")
	if len(raised) != 3 {
		t.Fatalf("raising one node alarm and one kind on two logs wrote %d line(s), "+
			"want three: %v", len(raised), raised)
	}
	domains := map[string]bool{}
	for _, line := range raised {
		if line["alarm"] != string(statelog.KindTrimBlocked) {
			// A NODE'S ALARM NAMES NO LOG.
			if _, named := line["domain"]; named {
				t.Errorf("the node's own alarm names a log: %v", line)
			}
			continue
		}
		domain, _ := line["domain"].(string)
		domains[domain] = true
		if !strings.HasPrefix(line["detail"].(string), domain+": ") {
			t.Errorf("the %s log's line carries another's detail: %v", domain, line)
		}
	}
	if !domains["tracker"] || !domains["pages"] {
		t.Errorf("trim_blocked was raised for %v, want each log once", domains)
	}

	// THE TRACKER'S LOG CLEARS WHILE THE KNOWLEDGE BASE'S STANDS: its own line,
	// its own age, its own detail — and the kind is still up.
	clock = clock.Add(20 * time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{lagging, blocked("pages")})
	cleared := lines("alarm_cleared")
	if len(cleared) != 1 || cleared[0]["domain"] != "tracker" ||
		cleared[0]["for"] != "20m" || cleared[0]["detail"] != "tracker: the trim has not advanced" {
		t.Fatalf("one log's alarm clearing wrote %v; want one line naming the "+
			"tracker's log, its twenty minutes and its detail", cleared)
	}
	if got := gauge(t, rec, statelog.KindTrimBlocked); got != 1 {
		t.Errorf("the gauge reads %v while the knowledge base's alarm stands, want 1", got)
	}

	clock = clock.Add(10 * time.Minute)
	tracker.Observe(t.Context(), nil)
	cleared = lines("alarm_cleared")
	if len(cleared) != 3 {
		t.Fatalf("clearing everything wrote %d clear line(s) in all, want three: %v",
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

// THE STANDING ALARMS ARE THE LATEST EVALUATION'S, THE OLDEST FIRST — and
// "not evaluated" is not "none firing".
//
// This is what every node's health envelope counts and names, so it has to
// be exactly what the gauge and the log were just told: an alarm that cleared
// is gone from it at once, and the name a health card shows is the condition
// that has gone unanswered longest rather than whichever the table happens to
// list first. Before any evaluation it says it cannot say, because an empty
// list would render a node that has not looked as a node with nothing wrong.
//
// AND A KIND ON TWO LOGS STANDS TWICE, each naming its log: the count is how
// many things an operator has to look at, and `trim_blocked` on the tracker's
// log and on the knowledge base's is two of them with two remedies. Keyed on
// the kind, the second log's would have been folded into the first's and the
// name a health card shows would have sent an operator to one log while the
// other stood.
//
// Mutation: return the table's order rather than the age order, report
// (nil, true) before an evaluation, or key the order on the kind alone, and
// this fails.
func TestTheStandingAlarmsAreTheLatestEvaluationsOldestFirst(t *testing.T) {
	t.Parallel()
	clock := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	tracker := statelog.NewTracker(nil, func() time.Time { return clock })

	if standing, known := tracker.Standing(); known || standing != nil {
		t.Fatalf("before any evaluation Standing = (%v, %v), want (nil, false)", standing, known)
	}

	// The WAL alarm is raised first; the knowledge base's blocked trim
	// joins it two minutes later, and apply lag — which the table lists
	// ahead of both — and the tracker's blocked trim three minutes after
	// that.
	wal := statelog.Alarm{Kind: statelog.KindWALLarge, Detail: "the write-ahead log is 2.0 GiB"}
	lag := statelog.Alarm{Kind: statelog.KindApplyLag, Detail: "this node is 2m behind the log"}
	blocked := func(domain string) statelog.Alarm {
		return statelog.Alarm{Kind: statelog.KindTrimBlocked, Domain: domain,
			Detail: domain + ": the trim has not advanced"}
	}
	tracker.Observe(t.Context(), []statelog.Alarm{wal})
	clock = clock.Add(2 * time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{blocked("pages"), wal})
	clock = clock.Add(3 * time.Minute)
	tracker.Observe(t.Context(), []statelog.Alarm{lag, blocked("tracker"), blocked("pages"), wal})

	standing, known := tracker.Standing()
	want := []statelog.StandingAlarm{
		{Kind: statelog.KindWALLarge},
		{Kind: statelog.KindTrimBlocked, Domain: "pages"},
		{Kind: statelog.KindApplyLag},
		{Kind: statelog.KindTrimBlocked, Domain: "tracker"},
	}
	if !known || !slices.Equal(standing, want) {
		t.Errorf("Standing = (%v, %v), want (%v, true): the longest-standing "+
			"alarm leads whatever the table's order is, and each log's alarm "+
			"stands on its own", standing, known, want)
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
