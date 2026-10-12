package statelog_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// heldDomain is a domain whose trim the given inputs hold, blocked since the
// given instant.
func heldDomain(name string, in statelog.TrimInputs, since time.Time) statelog.DomainInputs {
	d := healthyDomain(name, true)
	in.Now = reportAt
	d.Decision = statelog.Trim(in.Terms())
	d.BlockedSince = since
	return d
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
	since := reportAt.Add(-46 * time.Hour)
	var domains []statelog.DomainInputs
	for _, name := range []string{"tracker", "vectors", "pages", "usage"} {
		domains = append(domains, heldDomain(name, noBackup(), since))
	}
	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
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
		if !strings.Contains(d.Detail, "46h") {
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
		Domains:          []statelog.DomainInputs{heldDomain("tracker", in, reportAt.Add(-time.Hour))},
	})
	if got := kinds(rep.Alarms); len(got) != 1 || got[0] != statelog.KindTrimBlocked {
		t.Fatalf("the alarms are %v, want trim_blocked alone", got)
	}
	if d := rep.Alarms[0].Domains; len(d) != 1 || d[0].Term != statelog.TermBackupFloor {
		t.Errorf("trim_blocked names %+v, want tracker held by the backup term", d)
	}
}

// EACH KIND IS ONE ALARM, naming every log it stands on.
//
// The kind is the alarm's identity on the gauge and in the log line, so two
// logs held by two different terms are one `trim_blocked` naming both, led by
// the one held longest, never two alarms the health count adds up.
func TestEachKindIsOneAlarmLedByItsMostPressingLog(t *testing.T) {
	t.Parallel()
	behind := statelog.TrimInputs{
		// Both nodes hold a snapshot, so the one holding the trim is the
		// node that has applied nothing.
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
	unread := behind
	unread.Counted = []statelog.NodePosition{{NodeID: "node-1", Seq: 900, At: reportAt}}
	unread.FeedReadable = false

	rep := statelog.NewReport(statelog.ReportInputs{
		NodeID:           "node-1",
		At:               reportAt,
		RegisterReadable: true,
		Domains: []statelog.DomainInputs{
			heldDomain("pages", unread, reportAt.Add(-10*time.Minute)),
			heldDomain("tracker", behind, reportAt.Add(-3*time.Hour)),
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
	if !strings.Contains(held.Detail, "3h") {
		t.Errorf("the alarm leads with %q, want the measurement of the log held "+
			"longest (tracker, 3h)", held.Detail)
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
		Reading: statelog.Reading{
			ApplyLag:    5 * time.Minute,
			DeferredAge: time.Hour,
		},
		Domains: []statelog.DomainInputs{
			heldDomain("tracker", noBackup(), reportAt.Add(-time.Hour)),
		},
	})
	got := kinds(rep.Alarms)
	want := []statelog.Kind{statelog.KindApplyLag, statelog.KindTrimBlocked, statelog.KindDeferredOld}
	if len(got) != len(want) {
		t.Fatalf("the alarms are %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("alarm %d is %s, want %s (the table's order): %v", i, got[i], want[i], got)
		}
	}
}
