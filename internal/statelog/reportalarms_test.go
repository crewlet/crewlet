package statelog_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// heldDomain is a domain whose trim the given inputs hold, blocked since the
// given instant. Its log holds a hundred records past the replay window (the
// age term keeps from 800 and the log starts at 700), so a block older than
// the window and a tick is one `trim_blocked` stands on.
func heldDomain(name string, in statelog.TrimInputs, since time.Time) statelog.DomainInputs {
	d := healthyDomain(name, true)
	in.Now = reportAt
	d.Decision = statelog.Trim(in.Terms())
	d.BlockedSince = since
	return d
}

// heldPastWindowBy is the instant a block began that has outlasted the replay
// window and the trim tick by over, which is how long a block has to have
// lasted before `trim_blocked` is raised for it.
func heldPastWindowBy(over time.Duration) time.Time {
	return reportAt.Add(-(replayWindow + statelog.TrimInterval + over))
}

// noBackup is a single node's trim inputs with nothing backed up yet.
func noBackup() statelog.TrimInputs {
	return statelog.TrimInputs{
		Counted:         []statelog.NodePosition{{NodeID: "node-1", Seq: 900, At: reportAt}},
		CountedReadable: true,
		HoldsReadable:   true,
		BackupMaxAge:    24 * time.Hour,
		FeedAckFloor:    900,
		HasFeed:         true,
		FeedReadable:    true,
		AgeFloor:        800,
		HoldStale:       statelog.TrimHoldStale,
	}
}

// kinds lists a report's alarm kinds in order.
func kinds(alarms []statelog.Alarm) []statelog.Kind {
	out := make([]statelog.Kind, 0, len(alarms))
	for _, a := range alarms {
		out = append(out, a.Kind)
	}
	return out
}

// A COMPANY WITH NO BACKUP HAS ONE PROBLEM, NOT ONE PER LOG.
//
// Every log's trim waits for the backup the policy asks for, and the backup
// alarm already says the trim does not advance until there is one. Raised as
// a `trim_blocked` per log beside it, the one cause read as five alarms on the
// health count while the gauge and the log line said two.
func TestAHoldTheMissingBackupExplainsIsListedUnderTheBackupAlarm(t *testing.T) {
	t.Parallel()
	since := heldPastWindowBy(46 * time.Hour)
	var domains []statelog.DomainInputs
	for _, name := range []string{"tracker", "vectors", "pages", "usage"} {
		domains = append(domains, heldDomain(name, noBackup(), since))
	}
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		ReplayWindow:     replayWindow,
		Reading:          statelog.Reading{BackupMaxAge: 24 * time.Hour},
		Domains:          domains,
	})
	if got := kinds(rep.Alarms); len(got) != 1 || got[0] != statelog.KindBackupAge {
		t.Fatalf("the alarms are %v, want backup_age alone", got)
	}
	backup := rep.Alarms[0]
	if len(backup.Domains) != 4 {
		t.Fatalf("backup_age names %d logs, want the 4 whose trim it holds: %+v",
			len(backup.Domains), backup.Domains)
	}
	for i, want := range []string{"tracker", "vectors", "pages", "usage"} {
		d := backup.Domains[i]
		if d.Domain != want {
			t.Errorf("log %d is %q, want %q in the report's own order", i, d.Domain, want)
		}
		if d.Term != statelog.TermBackupFloor {
			t.Errorf("%s is held by %q, want the backup term", d.Domain, d.Term)
		}
		if !d.Since.Equal(since) {
			t.Errorf("%s has been held since %v, want %v", d.Domain, d.Since, since)
		}
		if !strings.Contains(d.Detail, "has not advanced for 214h 15m") {
			t.Errorf("%s reads %q, which does not say how long it has been held", d.Domain, d.Detail)
		}
	}
	if !strings.HasPrefix(backup.Detail, "no verified backup has been recorded") {
		t.Errorf("the backup alarm's own measurement became %q; the logs it holds "+
			"are listed beside it, never in place of it", backup.Detail)
	}
}

// A HOLD BY THE BACKUP TERM THAT NO BACKUP ALARM EXPLAINS IS STILL AN ALARM.
//
// A deployment with no backup policy raises no `backup_age`, yet its backup
// term still holds every log until a copy is recorded. Folded into an alarm
// that is not firing, that hold would be named nowhere.
func TestABackupTermHoldWithoutTheBackupAlarmIsATrimAlarm(t *testing.T) {
	t.Parallel()
	in := noBackup()
	in.BackupMaxAge = 0
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		ReplayWindow:     replayWindow,
		Domains:          []statelog.DomainInputs{heldDomain("tracker", in, heldPastWindowBy(time.Hour))},
	})
	if got := kinds(rep.Alarms); len(got) != 1 || got[0] != statelog.KindTrimBlocked {
		t.Fatalf("the alarms are %v, want trim_blocked alone", got)
	}
	if d := rep.Alarms[0].Domains; len(d) != 1 || d[0].Term != statelog.TermBackupFloor {
		t.Errorf("trim_blocked names %+v, want tracker held by the backup term", d)
	}
}

// heldByANodeBehind is a log's trim inputs held by the applied term: a backup
// an hour old, and of two nodes both holding a snapshot, one that has applied
// nothing.
func heldByANodeBehind() statelog.TrimInputs {
	return statelog.TrimInputs{
		Counted: []statelog.NodePosition{
			{NodeID: "node-1", Seq: 900, At: reportAt, HasSnapshot: true, SnapshotSeq: 900},
			{NodeID: "node-2", Seq: 0, At: reportAt, HasSnapshot: true, SnapshotSeq: 900},
		},
		CountedReadable: true,
		HoldsReadable:   true,
		BackupFloor:     880,
		BackupAt:        reportAt.Add(-time.Hour),
		HasBackupFloor:  true,
		BackupMaxAge:    24 * time.Hour,
		FeedAckFloor:    890,
		HasFeed:         true,
		FeedReadable:    true,
		AgeFloor:        800,
		HoldStale:       statelog.TrimHoldStale,
	}
}

// EVERY PER-LOG KIND LEADS WITH ITS MOST PRESSING LOG, in whichever order the
// report reaches its logs.
//
// An alarm standing on several logs carries ONE detail, and it is the one a
// person reads first: the log nearest its ceiling, the ceiling that holds the
// fewest days, the trim held longest, the record held longest, the floor
// unreadable longest and the read rate furthest past its census. Each row
// fires its kind on two logs, `weak` and `strong`, and the alarm must lead
// with `strong`'s measurement with the logs in either order — the first order
// is what a comparison that never prefers anything fails, the second what an
// inverted one fails. A per-log kind with no row fails too, so the next one
// cannot ship leading with whichever log came first.
//
// Mutation: make any arm of [statelog.morePressing] answer false, or invert
// its comparison, and this fails.
func TestEveryPerLogKindLeadsWithItsMostPressingLog(t *testing.T) {
	t.Parallel()
	const gib = uint64(1) << 30
	rows := map[statelog.Kind]func(weak, strong *statelog.DomainInputs){
		statelog.KindLogHeadroom: func(weak, strong *statelog.DomainInputs) {
			weak.MaxBytes, strong.MaxBytes = gib, gib
			weak.Bytes = gib - 64<<20 // six per cent left
			strong.Bytes = gib - 1    // none
		},
		statelog.KindLogCeilingShort: func(weak, strong *statelog.DomainInputs) {
			// Both well inside their ceilings, so the headroom is quiet;
			// the weak one holds two days of its own writing and the
			// strong one one, against a week's window.
			weak.MaxBytes, strong.MaxBytes = gib, gib
			weak.Bytes, strong.Bytes = 64<<20, 64<<20
			weak.BytesPerDay = statelog.PerDay(gib / 2)
			strong.BytesPerDay = statelog.PerDay(gib)
		},
		statelog.KindTrimBlocked: func(weak, strong *statelog.DomainInputs) {
			*weak = heldDomain(weak.Domain, heldByANodeBehind(), heldPastWindowBy(10*time.Minute))
			*strong = heldDomain(strong.Domain, heldByANodeBehind(), heldPastWindowBy(3*time.Hour))
		},
		statelog.KindDeferredOld: func(weak, strong *statelog.DomainInputs) {
			weak.DeferredAge = statelog.DeferralGrace + time.Minute
			strong.DeferredAge = statelog.DeferralGrace + time.Hour
		},
		statelog.KindFloorUnknown: func(weak, strong *statelog.DomainInputs) {
			weak.FloorUnknownFor = statelog.FloorCacheStale + time.Second
			strong.FloorUnknownFor = statelog.FloorCacheStale + time.Hour
		},
		statelog.KindCensusDrift: func(weak, strong *statelog.DomainInputs) {
			weak.LinearizableReadsExpected, strong.LinearizableReadsExpected = 1_000, 1_000
			weak.LinearizableReads = int(statelog.CensusDriftFactor*1_000) + 1
			strong.LinearizableReads = int(statelog.CensusDriftFactor*1_000) * 2
		},
	}
	for _, kind := range statelog.PerLogKinds() {
		set, ok := rows[kind]
		if !ok {
			t.Errorf("%s is evaluated per log and has no row here, so nothing "+
				"holds which of its logs it leads with", kind)
			continue
		}
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			weak, strong := healthyDomain("weak", true), healthyDomain("strong", true)
			set(&weak, &strong)
			for _, order := range [][]statelog.DomainInputs{{weak, strong}, {strong, weak}} {
				rep := statelog.NewReport(statelog.ReportInputs{
					NodeID:           "node-1",
					At:               reportAt,
					RegisterReadable: true,
					ReplayWindow:     replayWindow,
					Domains:          order,
				})
				var raised []statelog.Alarm
				for _, a := range rep.Alarms {
					if a.Kind == kind {
						raised = append(raised, a)
					}
				}
				if len(raised) != 1 {
					t.Fatalf("logs %s then %s: %s is raised %d time(s) in %v, want once",
						order[0].Domain, order[1].Domain, kind, len(raised), kinds(rep.Alarms))
				}
				fired := raised[0]
				details := map[string]string{}
				for _, d := range fired.Domains {
					details[d.Domain] = d.Detail
				}
				if len(fired.Domains) != 2 || details["weak"] == "" || details["strong"] == "" {
					t.Fatalf("logs %s then %s: %s stands on %+v, want both logs with "+
						"their own measurement", order[0].Domain, order[1].Domain, kind, fired.Domains)
				}
				// A FLOOR: two logs measuring alike would let any lead pass.
				if details["weak"] == details["strong"] {
					t.Fatalf("both logs measure %q, so this compares nothing", details["weak"])
				}
				if fired.Detail != details["strong"] {
					t.Errorf("logs %s then %s: %s leads with %q, want the more pressing "+
						"log's %q", order[0].Domain, order[1].Domain, kind, fired.Detail,
						details["strong"])
				}
			}
		})
	}
}

// EACH KIND IS ONE ALARM, naming every log it stands on.
//
// The kind is the alarm's identity on the gauge and in the log line, so two
// logs held by two different terms are one `trim_blocked` naming both, led by
// the one held longest, never two alarms the health count adds up.
func TestEachKindIsOneAlarmLedByItsMostPressingLog(t *testing.T) {
	t.Parallel()
	behind := heldByANodeBehind()
	unread := behind
	unread.Counted = []statelog.NodePosition{{NodeID: "node-1", Seq: 900, At: reportAt}}
	unread.FeedReadable = false

	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		ReplayWindow:     replayWindow,
		Domains: []statelog.DomainInputs{
			heldDomain("pages", unread, heldPastWindowBy(10*time.Minute)),
			heldDomain("tracker", behind, heldPastWindowBy(3*time.Hour)),
		},
	})
	seen := map[statelog.Kind]bool{}
	for _, a := range rep.Alarms {
		if seen[a.Kind] {
			t.Fatalf("%s is raised twice in %v", a.Kind, kinds(rep.Alarms))
		}
		seen[a.Kind] = true
	}
	if len(rep.Alarms) != 1 || rep.Alarms[0].Kind != statelog.KindTrimBlocked {
		t.Fatalf("the alarms are %v, want one trim_blocked", kinds(rep.Alarms))
	}
	held := rep.Alarms[0]
	terms := map[string]statelog.TermName{}
	for _, d := range held.Domains {
		terms[d.Domain] = d.Term
	}
	if terms["pages"] != statelog.TermFeedAckFloor || terms["tracker"] != statelog.TermApplied {
		t.Errorf("the held logs and their terms are %v", terms)
	}
	var tracker statelog.AlarmDomain
	for _, d := range held.Domains {
		if d.Domain == "tracker" {
			tracker = d
		}
	}
	if held.Detail == "" || held.Detail != tracker.Detail {
		t.Errorf("the alarm leads with %q, want the measurement of the log held "+
			"longest (tracker: %q)", held.Detail, tracker.Detail)
	}
}

// THE ALARMS ARE IN THE TABLE'S ORDER, node-wide and per-log kinds alike, which
// is the order the reference, the gauge and every surface read them in.
func TestTheReportsAlarmsAreInTheTablesOrder(t *testing.T) {
	t.Parallel()
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		ReplayWindow:     replayWindow,
		Reading: statelog.Reading{
			ApplyLag:    5 * time.Minute,
			PrefetchP95: statelog.PrefetchScanBudget + time.Second,
		},
		Domains: []statelog.DomainInputs{
			heldDomain("tracker", noBackup(), heldPastWindowBy(time.Hour)),
		},
	})
	got := kinds(rep.Alarms)
	want := []statelog.Kind{statelog.KindApplyLag, statelog.KindTrimBlocked, statelog.KindPrefetchSlow}
	if len(got) != len(want) {
		t.Fatalf("the alarms are %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("alarm %d is %s, want %s (the table's order): %v", i, got[i], want[i], got)
		}
	}
}
