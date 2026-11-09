package queries

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/notice"
	"github.com/crewlet/crewlet/internal/statelog"
)

// advisorAnswer is the advisor's findings with the platform notices they and
// the fleet's alarms raise: what reaches the administrators' Inbox.
type advisorAnswer struct {
	advisor.Answer
	Notices []notice.Notice `json:"notices"`
}

// advisor answers `advisor`: every finding about the fleet's company at the
// revision it is, the revision each fix is sent under. One read of the fleet's
// pointer gives both, and the document is redacted before anything is read
// off it, so no change a fix carries can hold a value redaction hides.
//
// AN EMPTY LIST, not an error, when nothing is active: a deployment before
// its first import has nothing to advise about, and its alarms are still noticed.
func (s Sources) advisor(ctx context.Context, _ Params) (any, error) {
	now := s.clock()
	fleet, fleetKnown := s.fleetAlarms(ctx)
	answered := func(a advisor.Answer) advisorAnswer {
		if !fleetKnown {
			a.Unread = append(a.Unread, notice.UnreadAlarms)
		}
		return advisorAnswer{Answer: a, Notices: notice.Read(a.Findings, fleet, now)}
	}
	company, revision, err := s.Config.FleetCompany(ctx)
	if errors.Is(err, configapi.ErrNoActiveRevision) {
		return answered(advisor.Answer{Findings: []advisor.Finding{}, Unread: []string{}}), nil
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
	window := advisor.PatternsWindow(now, company)
	window.Level = statelog.ReadStale
	work, err := s.Work.WorkPatterns(ctx, window)
	if err != nil {
		return nil, fmt.Errorf("advisor: read the company's work patterns: %w", err)
	}
	return answered(advisor.ReadWithEvidence(in, advisor.Evidence{Until: now, Work: work,
		Backup: backupEvidence(s.latestAlarms())})), nil
}

// fleetAlarms is every live node's alarm marks as its last heartbeat carried
// them, from the node whose table has run; false where the fleet cannot be read.
func (s Sources) fleetAlarms(ctx context.Context) ([]notice.NodeAlarms, bool) {
	if s.Coord == nil {
		return nil, false
	}
	leases, err := s.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		log.WarnContext(ctx, "advisor_fleet_alarms_unread", "error", err.Error())
		return nil, false
	}
	var out []notice.NodeAlarms
	for _, lease := range leases {
		status, ok := coord.StatusFromMeta(lease.Meta)
		if !ok || !status.AlarmsEvaluated {
			continue
		}
		out = append(out, notice.NodeAlarms{Node: nameIn(coord.ClassNode, lease.Resource), Marks: status.Alarms})
	}
	return out, true
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
