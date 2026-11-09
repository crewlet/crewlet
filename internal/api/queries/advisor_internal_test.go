package queries

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
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
			func() bool { return true }, advisor.Evidence{Until: now, Work: worklet.WorkPatterns{AgentComments: 3},
				Ledger: turns, Backup: advisor.Backup{Standing: true}}},
		{"the tracker failed", patternsWork{err: errors.New("no data node answered")}, read,
			func() bool { return true }, advisor.Evidence{Until: now, WorkUnread: true, Ledger: turns,
				Backup: advisor.Backup{Standing: true}}},
		{"the ledger failed", patternsWork{}, ledgerUsage{err: errors.New("no data node answered")},
			func() bool { return true }, advisor.Evidence{Until: now, LedgerUnread: true,
				Backup: advisor.Backup{Standing: true}}},
		{"a node with no usage half", patternsWork{}, nil, func() bool { return true },
			advisor.Evidence{Until: now, LedgerUnread: true, Backup: advisor.Backup{Standing: true}}},
		{"the alarms not yet evaluated", patternsWork{}, read, func() bool { return false },
			advisor.Evidence{Until: now, Ledger: turns, AlarmsUnread: true}},
		{"a node with no alarm table", patternsWork{}, read, nil,
			advisor.Evidence{Until: now, Ledger: turns, AlarmsUnread: true}},
	} {
		s := Sources{Work: c.work, Usage: c.usage, AlarmsEvaluated: c.evaluated,
			Alarms: func() []statelog.Alarm { return standing }}
		got := s.advisorEvidence(t.Context(), &config.Company{}, now)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: evidence %+v, want %+v", c.name, got, c.want)
		}
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
