package notice_test

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/notice"
	"github.com/crewlet/crewlet/internal/statelog"
)

var t0 = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

func marks(node string, kinds map[string]time.Time) notice.NodeAlarms {
	n := notice.NodeAlarms{Node: node}
	for kind, since := range kinds {
		n.Marks = append(n.Marks, coord.AlarmMark{Kind: kind, Since: since})
	}
	return n
}

func alarm(kind string, since time.Time, nodes ...string) notice.Notice {
	return notice.Notice{Class: notice.ClassAlarm, Subject: advisor.Subject{Kind: notice.SubjectAlarm, Name: kind},
		Since: since, Nodes: nodes}
}

// AN ALARM PAST ITS THRESHOLD REACHES THE ADMINISTRATORS ONCE: one notice per
// kind however many nodes report it and however often it is read, opened at the
// earliest raise; below the threshold, or with no known raise, it is no notice.
func TestAnAlarmPastItsThresholdReachesTheAdministratorsOnce(t *testing.T) {
	t.Parallel()
	past := t0.Add(notice.AlarmAfter)
	fleet := []notice.NodeAlarms{
		marks("node-b", map[string]time.Time{"apply_lag": t0.Add(time.Minute)}),
		marks("node-a", map[string]time.Time{"apply_lag": t0, "volume_low": t0.Add(time.Minute), "wal_large": {}}),
	}
	want := []notice.Notice{alarm("apply_lag", t0, "node-a", "node-b")}
	for range 100 {
		if got := notice.FromAlarms(fleet, past); !reflect.DeepEqual(got, want) {
			t.Fatalf("at the threshold the notices are\n%+v\nwant\n%+v", got, want)
		}
	}
	if got := notice.FromAlarms(fleet, past.Add(-time.Second)); len(got) != 0 {
		t.Errorf("a second before the threshold the notices are %+v, want none", got)
	}
	later := past.Add(time.Minute)
	want = []notice.Notice{alarm("apply_lag", t0, "node-a", "node-b"), alarm("volume_low", t0.Add(time.Minute), "node-a")}
	if got := notice.FromAlarms(fleet, later); !reflect.DeepEqual(got, want) {
		t.Errorf("past both thresholds the notices are\n%+v\nwant\n%+v", got, want)
	}
}

// A PLATFORM NOTICE CLEARS WITH ITS CONDITION: the next read of a fleet whose
// alarm cleared holds no notice, and a later raise is a new notice from its own
// instant once it is past the threshold.
func TestAPlatformNoticeClearsWithItsCondition(t *testing.T) {
	t.Parallel()
	now := t0.Add(notice.AlarmAfter)
	if got := notice.FromAlarms([]notice.NodeAlarms{marks("node-a", map[string]time.Time{"apply_lag": t0})}, now); len(got) != 1 {
		t.Fatalf("a standing alarm gives %d notices, want 1", len(got))
	}
	if got := notice.FromAlarms([]notice.NodeAlarms{{Node: "node-a"}}, now); len(got) != 0 {
		t.Errorf("a cleared alarm gives %+v, want no notice", got)
	}
	raised := now.Add(time.Minute)
	again := []notice.NodeAlarms{marks("node-a", map[string]time.Time{"apply_lag": raised})}
	if got := notice.FromAlarms(again, raised.Add(time.Minute)); len(got) != 0 {
		t.Errorf("a new raise below the threshold gives %+v, want none", got)
	}
	want := []notice.Notice{alarm("apply_lag", raised, "node-a")}
	if got := notice.FromAlarms(again, raised.Add(notice.AlarmAfter)); !reflect.DeepEqual(got, want) {
		t.Errorf("a new raise past the threshold gives %+v, want %+v", got, want)
	}
}

// THE BACKUP ALARM IS THE ADVISOR'S FINDING, NOT A SECOND NOTICE: its notice
// carries the class and subject the advisor's own finding has, so the dashboard
// attaches that fix, and the finding read off the same alarm adds no notice.
func TestTheBackupAlarmIsTheAdvisorsFinding(t *testing.T) {
	t.Parallel()
	found := advisor.Detect(advisor.Evidence{Until: t0, Backup: advisor.Backup{Standing: true, Logs: 2}}, nil)
	if len(found) != 1 {
		t.Fatalf("a standing backup alarm raises %d findings, want 1", len(found))
	}
	backup := string(statelog.KindBackupAge)
	got := notice.FromAlarms([]notice.NodeAlarms{marks("node-a", map[string]time.Time{backup: t0})}, t0.Add(notice.AlarmAfter))
	want := []notice.Notice{{Class: string(found[0].Class), Subject: found[0].Subject, Since: t0, Nodes: []string{"node-a"}}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the backup alarm's notice is\n%+v\nwant the finding's class and subject\n%+v", got, want)
	}
	if got := notice.FromFindings(found); len(got) != 0 {
		t.Errorf("the backup finding adds %+v beside the alarm's notice, want nothing", got)
	}
}

// ONLY A FINDING THAT BLOCKS THE COMPANY AND IS THE OWNER'S TO FIX IS A NOTICE:
// what costs or is worth knowing stays on the advisor's page, and the engine's
// own work never reaches the owner as a to-do.
func TestOnlyAnOwnersBlockingFindingIsANotice(t *testing.T) {
	t.Parallel()
	github := advisor.Subject{Kind: advisor.SubjectIntegration, Name: "GitHub"}
	findings := []advisor.Finding{
		{Class: "integration.awaiting_admin", Severity: advisor.SeverityBlocks, Actor: advisor.ActorOwner, Subject: github},
		{Class: "integration.awaiting_admin", Severity: advisor.SeverityBlocks, Actor: advisor.ActorOwner, Subject: github},
		{Class: "integration.unconfigured", Severity: advisor.SeverityBlocks, Actor: advisor.ActorEngine, Subject: github},
		{Class: "grant.health_unheld", Severity: advisor.SeverityCosts, Actor: advisor.ActorOwner},
		{Class: "schedule.frequent", Severity: advisor.SeverityNote, Actor: advisor.ActorOwner},
	}
	want := []notice.Notice{{Class: "integration.awaiting_admin", Subject: github}}
	if got := notice.FromFindings(findings); !reflect.DeepEqual(got, want) {
		t.Errorf("the notices are %+v, want %+v", got, want)
	}
}

// A PLATFORM NOTICE OFFERS NO ANSWER: it says what holds and since when, and
// carries nothing a reader could answer or decide; it ends when its condition does.
func TestAPlatformNoticeOffersNoAnswer(t *testing.T) {
	t.Parallel()
	raw, err := json.Marshal(alarm("apply_lag", t0, "node-a"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(fields))
	if want := []string{"class", "nodes", "since", "subject"}; !slices.Equal(got, want) {
		t.Errorf("a notice carries %v, want only %v", got, want)
	}
}
