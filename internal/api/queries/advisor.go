package queries

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// advisor answers `advisor`: every finding about the fleet's company at the
// revision it is, the revision each fix is sent under. One read of the fleet's
// pointer gives both, and the document is redacted before anything is read
// off it, so no change a fix carries can hold a value redaction hides.
//
// AN EMPTY LIST, not an error, when nothing is active: a deployment before
// its first import has nothing to advise about.
func (s Sources) advisor(ctx context.Context, _ Params) (any, error) {
	company, revision, err := s.Config.FleetCompany(ctx)
	if errors.Is(err, configapi.ErrNoActiveRevision) {
		return advisor.Answer{Findings: []advisor.Finding{}}, nil
	}
	if err != nil {
		return nil, revisionRefusal("advisor", err)
	}
	now := s.clock()
	work, err := s.Work.WorkPatterns(ctx, worklet.PatternsQuery{
		Since: now.Add(-advisor.Window), Until: now, Level: statelog.ReadStale,
	})
	if err != nil {
		return nil, fmt.Errorf("advisor: read the company's work patterns: %w", err)
	}
	return advisor.ReadWithEvidence(company.Redact(), revision,
		advisor.Evidence{Until: now, Work: work, Backup: backupEvidence(s.latestAlarms())}), nil
}

// backupEvidence is whether the latest evaluation holds the backup alarm, and
// over how many logs.
func backupEvidence(alarms []statelog.Alarm) advisor.Backup {
	for _, a := range alarms {
		if a.Kind != statelog.KindBackupAge {
			continue
		}
		logs := 0
		for _, d := range a.Domains {
			logs += len(d.Domains)
		}
		return advisor.Backup{Standing: true, Logs: logs}
	}
	return advisor.Backup{}
}
