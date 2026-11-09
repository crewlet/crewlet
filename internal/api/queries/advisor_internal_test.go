package queries

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
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

// A SOURCE THE ADVISOR CANNOT READ IS NAMED, never read as clear: a failed
// tracker read and an alarm table not yet evaluated each say unread.
func TestTheAdvisorNamesASourceItCouldNotRead(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 9, 12, 0, 0, 0, time.UTC)
	standing := []statelog.Alarm{{Kind: statelog.KindBackupAge}}
	for _, c := range []struct {
		name      string
		work      patternsWork
		evaluated func() bool
		want      advisor.Evidence
	}{
		{"both read", patternsWork{work: worklet.WorkPatterns{AgentComments: 3}}, func() bool { return true },
			advisor.Evidence{Until: now, Work: worklet.WorkPatterns{AgentComments: 3}, Backup: advisor.Backup{Standing: true}}},
		{"the tracker failed", patternsWork{err: errors.New("no data node answered")}, func() bool { return true },
			advisor.Evidence{Until: now, WorkUnread: true, Backup: advisor.Backup{Standing: true}}},
		{"the alarms not yet evaluated", patternsWork{}, func() bool { return false },
			advisor.Evidence{Until: now, AlarmsUnread: true}},
		{"a node with no alarm table", patternsWork{}, nil, advisor.Evidence{Until: now, AlarmsUnread: true}},
	} {
		s := Sources{Work: c.work, AlarmsEvaluated: c.evaluated,
			Alarms: func() []statelog.Alarm { return standing }}
		got := s.advisorEvidence(t.Context(), &config.Company{}, now)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: evidence %+v, want %+v", c.name, got, c.want)
		}
	}
}
