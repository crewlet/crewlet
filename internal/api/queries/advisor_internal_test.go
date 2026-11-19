package queries

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/notice"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE ADVISOR READS THE BACKUP ALARM AS THE EVALUATION HOLDS IT, every log it
// stands on counted, and nothing from any other alarm.
func TestTheAdvisorReadsTheBackupAlarmAsItStands(t *testing.T) {
	t.Parallel()
	if got := backupEvidence(nil); got.Standing {
		t.Errorf("no evaluation reads as a standing backup alarm: %+v", got)
	}
	others := []statelog.Alarm{{Kind: statelog.KindVolumeLow}, {Kind: statelog.KindTrimBlocked}}
	if got := backupEvidence(others); got.Standing {
		t.Errorf("other alarms read as a standing backup alarm: %+v", got)
	}
	standing := append(others, statelog.Alarm{Kind: statelog.KindBackupAge, Domains: []statelog.AlarmDomain{
		{Domains: []string{"tracker", "identity"}}, {Domains: []string{"chat"}}}})
	if got := backupEvidence(standing); got != (advisor.Backup{Standing: true, Logs: 3}) {
		t.Errorf("a backup alarm on three logs reads as %+v", got)
	}
}

// patternsWork answers the patterns read and nothing else.
type patternsWork struct {
	WorkReader
	work worklet.WorkPatterns
	err  error
}

func (p patternsWork) WorkPatterns(context.Context, worklet.PatternsQuery) (worklet.WorkPatterns, error) {
	return p.work, p.err
}

// ledgerUsage answers the turn ledger read and nothing else, keeping the range asked.
type ledgerUsage struct {
	UsageReads
	rows  []usage.TurnLedgerRow
	err   error
	asked *usage.SpendQuery
}

func (l ledgerUsage) TurnLedger(_ context.Context, q usage.SpendQuery) ([]usage.TurnLedgerRow, error) {
	if l.asked != nil {
		*l.asked = q
	}
	return l.rows, l.err
}

// A SOURCE THE ADVISOR CANNOT READ IS NAMED, never read as clear: a failed
// tracker read and an alarm table not yet evaluated each say unread.
func TestTheAdvisorNamesASourceItCouldNotRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	standing := []statelog.Alarm{{Kind: statelog.KindBackupAge}}
	turns := []usage.TurnLedgerRow{{AgentID: "a-1", Handle: "app", Wake: "chat:message",
		Disposition: "started", Outcome: "moved", Turns: 2}}
	read := ledgerUsage{rows: turns}
	for _, c := range []struct {
		name      string
		work      patternsWork
		usage     UsageReads
		evaluated func() bool
		want      advisor.Evidence
	}{
		{"every source read", patternsWork{work: worklet.WorkPatterns{AgentComments: 3}}, read,
			func() bool { return true }, advisor.Evidence{Until: now, RunsUnread: true, FiresUnread: true, StopsUnread: true, Work: worklet.WorkPatterns{AgentComments: 3},
				Ledger: turns, Backup: advisor.Backup{Standing: true}}},
		{"the tracker failed", patternsWork{err: errors.New("no data node answered")}, read,
			func() bool { return true }, advisor.Evidence{Until: now, RunsUnread: true, FiresUnread: true, StopsUnread: true, WorkUnread: true, Ledger: turns,
				Backup: advisor.Backup{Standing: true}}},
		{"the ledger failed", patternsWork{}, ledgerUsage{err: errors.New("no data node answered")},
			func() bool { return true }, advisor.Evidence{Until: now, RunsUnread: true, FiresUnread: true, StopsUnread: true, LedgerUnread: true,
				Backup: advisor.Backup{Standing: true}}},
		{"a node with no usage half", patternsWork{}, nil, func() bool { return true },
			advisor.Evidence{Until: now, RunsUnread: true, FiresUnread: true, StopsUnread: true, LedgerUnread: true, Backup: advisor.Backup{Standing: true}}},
		{"the alarms not yet evaluated", patternsWork{}, read, func() bool { return false },
			advisor.Evidence{Until: now, RunsUnread: true, FiresUnread: true, StopsUnread: true, Ledger: turns, AlarmsUnread: true}},
		{"a node with no alarm table", patternsWork{}, read, nil,
			advisor.Evidence{Until: now, RunsUnread: true, FiresUnread: true, StopsUnread: true, Ledger: turns, AlarmsUnread: true}},
	} {
		s := Sources{Work: c.work, Usage: c.usage, AlarmsEvaluated: c.evaluated,
			Alarms: func() []statelog.Alarm { return standing }}
		got := s.advisorEvidence(t.Context(), &config.Company{}, now)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: evidence %+v, want %+v", c.name, got, c.want)
		}
	}
}

// THE ADVISOR NOTICES WHAT STANDS ON THE FLEET: every node's heartbeat marks are
// read, a node that has not evaluated its table says nothing and leaves the
// fleet's alarms unread, two nodes holding one alarm past its threshold give one
// notice, and the answer carries the notices beside the findings.
func TestTheAdvisorNoticesTheFleetsAlarms(t *testing.T) {
	t.Parallel()
	backend := coordmemory.New()
	since := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	for node, status := range map[string]coord.NodeStatus{
		"node-a": {AlarmsEvaluated: true, Alarms: []coord.AlarmMark{{Kind: "apply_lag", Since: since}}},
		"node-b": {AlarmsEvaluated: true, Alarms: []coord.AlarmMark{{Kind: "apply_lag", Since: since.Add(time.Minute)}}},
		"node-c": {Alarms: []coord.AlarmMark{{Kind: "volume_low", Since: since}}},
	} {
		if _, _, err := backend.TryAcquire(t.Context(), coord.NodeResource(node), coord.AcquireOptions{
			Owner: node + ":1", TTL: time.Minute, Meta: map[string]any{coord.StatusKey: status.Meta()},
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, known := (Sources{}).fleetAlarms(t.Context()); known {
		t.Error("a node with no coordination store says it read the fleet's alarms")
	}
	fleet, known := Sources{Coord: backend}.fleetAlarms(t.Context())
	if known {
		t.Error("node-c has not evaluated its alarms, and the fleet's read as known")
	}
	got := notice.Read(nil, fleet, nil, since.Add(notice.AlarmAfter))
	title, person, _ := statelog.PersonWords(statelog.KindApplyLag)
	want := []notice.Notice{{Class: notice.ClassAlarm, Subject: advisor.Subject{Kind: notice.SubjectAlarm, Name: "apply_lag"},
		Since: since, Nodes: []string{"node-a", "node-b"}, Title: title, Person: person}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the fleet's notices are\n%+v\nwant\n%+v", got, want)
	}
	raw, err := json.Marshal(AdvisorNotices{Notices: got, Unread: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if keys := slices.Sorted(maps.Keys(fields)); !slices.Equal(keys, []string{"notices", "unread"}) {
		t.Errorf("the notices answer %v", keys)
	}
	// ONCE EVERY LIVE NODE HAS EVALUATED, the fleet's alarms are known.
	settled := coordmemory.New()
	if _, _, err := settled.TryAcquire(t.Context(), coord.NodeResource("node-a"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: time.Minute,
		Meta: map[string]any{coord.StatusKey: coord.NodeStatus{AlarmsEvaluated: true}.Meta()},
	}); err != nil {
		t.Fatal(err)
	}
	if _, known := (Sources{Coord: settled}).fleetAlarms(t.Context()); !known {
		t.Error("every live node has evaluated, and the fleet's alarms read as unknown")
	}
	// A NODE SAYING NOTHING YET, its heartbeat carrying no status, may hold one.
	if _, _, err := settled.TryAcquire(t.Context(), coord.NodeResource("node-d"), coord.AcquireOptions{
		Owner: "node-d:1", TTL: time.Minute}); err != nil {
		t.Fatal(err)
	}
	if _, known := (Sources{Coord: settled}).fleetAlarms(t.Context()); known {
		t.Error("a live node with no status left the fleet's alarms known")
	}
}

// noticeLog is an advisor log whose latest run could not read unread, and
// whose live findings are live.
type noticeLog struct {
	unread []string
	live   []advisordomain.FindingRow
}

func (noticeLog) Findings(context.Context, advisordomain.Filter, statelog.Freshness) ([]advisordomain.FindingRow, string, error) {
	return nil, "", nil
}
func (l noticeLog) NonClosed(context.Context, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return l.live, nil
}
func (noticeLog) Finding(context.Context, string, statelog.Freshness) (advisordomain.FindingRow, []advisordomain.HistoryRow, error) {
	return advisordomain.FindingRow{}, nil, advisordomain.ErrNotFound
}
func (noticeLog) Episodes(context.Context, string, statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return nil, nil
}
func (l noticeLog) LatestRun(context.Context, statelog.Freshness) (advisordomain.RunRow, bool, error) {
	var scopes []duty.ScopeRecord
	for _, word := range l.unread {
		scopes = append(scopes, duty.ScopeRecord{Class: "evidence/" + word, Status: advisordomain.ClassUnread})
	}
	raw, err := json.Marshal(scopes)
	return advisordomain.RunRow{ID: "r", Status: advisordomain.RunDone, Classes: raw}, true, err
}
func (noticeLog) PendingRequests(context.Context, statelog.Freshness) ([]advisordomain.RunRow, error) {
	return nil, nil
}
func (noticeLog) LastRunOfClasses(context.Context, statelog.Freshness) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}
func (noticeLog) Reports(context.Context, string, int, statelog.Freshness) ([]advisordomain.ReportRow, string, error) {
	return nil, "", nil
}
func (noticeLog) Report(context.Context, string, statelog.Freshness) (advisordomain.ReportRow, bool, error) {
	return advisordomain.ReportRow{}, false, nil
}

// `alarms` IS NAMED UNREAD ONCE, whether the latest run could not read this
// node's evaluation, the fleet's marks could not be read, or both; and what
// the latest run could not read is named beside it.
func TestTheAdvisorNamesItsAlarmsUnreadOnce(t *testing.T) {
	t.Parallel()
	settled := coordmemory.New()
	if _, _, err := settled.TryAcquire(t.Context(), coord.NodeResource("node-a"), coord.AcquireOptions{
		Owner: "node-a:1", TTL: time.Minute,
		Meta: map[string]any{coord.StatusKey: coord.NodeStatus{AlarmsEvaluated: true}.Meta()},
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		unread []string
		fleet  coord.Backend
		want   []string
	}{
		{"both read", []string{}, settled, []string{}},
		{"this node not evaluated", []string{advisor.UnreadAlarms}, settled, []string{advisor.UnreadAlarms}},
		{"the fleet unread", []string{advisor.UnreadWork}, nil, []string{advisor.UnreadWork, advisor.UnreadAlarms}},
		{"both unread", []string{advisor.UnreadWork, advisor.UnreadAlarms}, nil,
			[]string{advisor.UnreadWork, advisor.UnreadAlarms}},
	} {
		s := Sources{Advisor: noticeLog{unread: c.unread}, Coord: c.fleet}
		got, err := s.advisorNotices(t.Context(), Params{})
		if err != nil {
			t.Fatal(err)
		}
		if unread := got.(AdvisorNotices).Unread; !slices.Equal(unread, c.want) {
			t.Errorf("%s: unread %v, want %v", c.name, unread, c.want)
		}
	}
}

// A BLOCKING FINDING'S NOTICE NAMES ITS FINDING, so the Inbox opens it and says
// its fix; a finding a person snoozed raises no notice while it sleeps.
func TestABlockingFindingsNoticeNamesItsFinding(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	f := advisor.Finding{Class: advisor.ClassSandboxUnstartable, Severity: advisor.SeverityBlocks,
		Actor: advisor.ActorOwner, Source: advisor.SourceEvidence, Subject: advisor.Subject{Kind: advisor.SubjectSeat, Seat: "dev"},
		Title: "Paused because the sandbox could not start as configured.",
		Fix:   advisor.Fix{Kind: advisor.FixOpenSetting, Sentence: "Fix the sandbox's image.", Options: []advisor.Option{}}}
	body, err := json.Marshal(advisor.Observe(f, advisor.Span{From: now, To: now}, "r1"))
	if err != nil {
		t.Fatal(err)
	}
	row := advisordomain.FindingRow{ID: "f-1", Key: advisor.Key(f), Class: string(f.Class),
		Severity: advisordomain.Severity(f.Severity), State: advisordomain.State(advisor.StateOpen), Body: body}
	notices := func(rows ...advisordomain.FindingRow) []notice.Notice {
		t.Helper()
		got, err := Sources{Advisor: noticeLog{live: rows}, Now: func() time.Time { return now }}.
			advisorNotices(t.Context(), Params{})
		if err != nil {
			t.Fatal(err)
		}
		return got.(AdvisorNotices).Notices
	}
	got := notices(row)
	if len(got) != 1 || got[0].Finding == nil || got[0].Finding.ID != "f-1" || got[0].Finding.Fix != f.Fix.Sentence {
		t.Fatalf("notices %+v, want one naming finding f-1 and its fix", got)
	}
	row.SnoozedUntil = now.Add(time.Hour)
	if got := notices(row); len(got) != 0 {
		t.Errorf("a snoozed finding raised %+v", got)
	}
}

// THE LEDGER IS READ OVER THE WEEK'S COMPANY DAYS, today included, and each
// seat the chart holds is named by the chart's handle, not the one its days
// recorded; a seat the chart no longer holds keeps its recorded handle.
func TestTheAdvisorReadsTheLedgerOverTheWeekByTheChartsHandles(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte("name: Acme\nproviders:\n  llm:\n    p: {model: m}\n  sandbox: {fake: true}\n" +
		"roles:\n  - name: CEO\n    handle: ceo\n    llm: p\n"))
	if err != nil {
		t.Fatal(err)
	}
	roster, err := cfg.Organization()
	if err != nil {
		t.Fatal(err)
	}
	ceo, ok := roster.AgentIDFor(roster.AgentSeatByHandle("ceo"))
	if !ok {
		t.Fatal("the chart holds no ceo seat")
	}
	var asked usage.SpendQuery
	s := Sources{
		Company: func() (*config.Company, *org.Organization) { return cfg, roster },
		Usage: ledgerUsage{asked: &asked, rows: []usage.TurnLedgerRow{
			{AgentID: ceo.String(), Handle: "chief"}, {AgentID: "a-gone", Handle: "gone"}}},
	}
	got, err := s.advisorLedger(t.Context(), time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if asked != (usage.SpendQuery{From: "2030-01-03", To: "2030-01-09"}) {
		t.Errorf("read the ledger over %s..%s, want the 7 company days to 2030-01-09", asked.From, asked.To)
	}
	if len(got) != 2 || got[0].Handle != "ceo" || got[1].Handle != "gone" {
		t.Errorf("the ledger names %+v, want the chart's ceo and the recorded gone", got)
	}
}

// pauseList is a pause store that lists what it holds, or fails.
type pauseList struct {
	pauses []coord.SeatPause
	err    error
}

func (p pauseList) ListSeatPauses(context.Context) ([]coord.SeatPause, error) {
	return p.pauses, p.err
}

// THE ADVISOR READS THE SEATS THE ENGINE PAUSED FOR A FAILURE, by the handle
// their agent id derives from: a person's pause is not one, and a store that
// fails is named unread rather than read as no pauses.
func TestTheAdvisorReadsTheSeatsTheEnginePausedForAFailure(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte("name: Acme\nroles:\n" +
		"  - name: Founder\n    kind: human\n    contact:\n      slack_user_id: U0FOUNDER\n" +
		"  - name: App\n    handle: app\n  - name: Ops\n    handle: ops\n"))
	if err != nil {
		t.Fatal(err)
	}
	app, _ := org.DeriveAgentID("Acme", "app")
	ops, _ := org.DeriveAgentID("Acme", "ops")
	s := Sources{Work: patternsWork{}, Pauses: pauseList{pauses: []coord.SeatPause{
		{Seat: app, Failure: &coord.SeatFailure{Kind: "sandbox_configuration", Basis: "b"}},
		{Seat: ops},
	}}}
	got, err := s.pausedSeats(t.Context(), c)
	want := []advisor.PausedSeat{{Handle: "app", Name: "App", Failure: "sandbox_configuration"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("paused = %+v, %v, want %+v", got, err, want)
	}
	s.Pauses = pauseList{err: errors.New("no fleet store answered")}
	if ev := s.advisorEvidence(t.Context(), c, time.Now()); !ev.PausesUnread {
		t.Error("a pause store that failed was read as no pauses")
	}
}
