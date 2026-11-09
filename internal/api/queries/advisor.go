package queries

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/statelog"
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
		return advisor.Answer{Findings: []advisor.Finding{}, Unread: []string{}}, nil
	}
	if err != nil {
		return nil, revisionRefusal("advisor", err)
	}
	in := advisor.Inputs{Company: company.Redact(), Revision: revision}
	// THE INTEGRATIONS' LAST RECONCILE, from the read the integrations answer
	// makes; nil where this node cannot say, which the answer names as unread.
	if states, known := s.reconcileStates(ctx); known {
		in.Integrations = slices.SortedFunc(maps.Values(states), func(a, b integration.State) int {
			return cmp.Compare(a.Kind, b.Kind)
		})
	}
	return advisor.ReadWithEvidence(in, s.advisorEvidence(ctx, company, s.clock())), nil
}

// advisorEvidence is what the company did in the week before now. A SOURCE THAT
// FAILS IS NAMED AS UNREAD, never the whole answer lost: the document's
// findings and the integrations' need no tracker read.
func (s Sources) advisorEvidence(ctx context.Context, company *config.Company, now time.Time) advisor.Evidence {
	ev := advisor.Evidence{Until: now}
	window := advisor.PatternsWindow(now, company)
	window.Level = statelog.ReadStale
	work, err := s.Work.WorkPatterns(ctx, window)
	if err != nil {
		log.WarnContext(ctx, "advisor_work_patterns_unreadable", "error", err.Error())
		ev.WorkUnread = true
	}
	ev.Work = work
	if s.AlarmsEvaluated == nil || !s.AlarmsEvaluated() {
		ev.AlarmsUnread = true
	} else {
		ev.Backup = backupEvidence(s.latestAlarms())
	}
	return ev
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
