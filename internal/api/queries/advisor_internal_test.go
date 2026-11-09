package queries

import (
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/statelog"
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
