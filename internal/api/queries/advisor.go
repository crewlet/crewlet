package queries

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/notice"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
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
	company, revision, err := s.Config.FleetCompany(ctx)
	if errors.Is(err, configapi.ErrNoActiveRevision) {
		return noticed(advisor.Answer{Findings: []advisor.Finding{}, Unread: []string{}}, fleet, fleetKnown, now), nil
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
	return noticed(advisor.ReadWithEvidence(in, s.advisorEvidence(ctx, company, now)), fleet, fleetKnown, now), nil
}

// noticed is the answer with the notices its findings and the fleet's alarms
// raise. `alarms` is named unread once, whether this node's evaluation or the
// fleet's marks could not be read.
func noticed(a advisor.Answer, fleet []notice.NodeAlarms, fleetKnown bool, now time.Time) advisorAnswer {
	if !fleetKnown && !slices.Contains(a.Unread, advisor.UnreadAlarms) {
		a.Unread = append(a.Unread, advisor.UnreadAlarms)
	}
	return advisorAnswer{Answer: a, Notices: notice.Read(a.Findings, fleet, now)}
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
	if ev.Ledger, err = s.advisorLedger(ctx, now); err != nil {
		log.WarnContext(ctx, "advisor_turn_ledger_unreadable", "error", err.Error())
		ev.LedgerUnread = true
	}
	if s.AlarmsEvaluated == nil || !s.AlarmsEvaluated() {
		ev.AlarmsUnread = true
	} else {
		ev.Backup = backupEvidence(s.latestAlarms())
	}
	return ev
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

// AccessFixAnswer is `advisor_access`: the change that grants an agent one
// access it asked for, built from the fleet's company, never applied here.
type AccessFixAnswer struct {
	Seat     string            `json:"seat"`
	Access   advisor.AccessKey `json:"access"`
	Revision string            `json:"revision"`
	Fix      advisor.Fix       `json:"fix"`
}

// advisorAccess answers `advisor_access`: the grant change for `seat` and
// `access`, read off the redacted document. A key the catalogue does not
// name, a seat that holds it or cannot take it, or no such agent is refused
// in its own words.
func (s Sources) advisorAccess(ctx context.Context, p Params) (any, error) {
	seat, key := strings.TrimSpace(p.String("seat")), advisor.AccessKey(strings.TrimSpace(p.String("access")))
	if !key.Valid() {
		return nil, badParams("access", string(key), accessKeyNames())
	}
	company, revision, err := s.Config.FleetCompany(ctx)
	if err != nil {
		return nil, revisionRefusal("advisor_access", err)
	}
	fix, err := advisor.AccessFix(company.Redact(), seat, key)
	switch {
	case errors.Is(err, advisor.ErrAccessHeld):
		return nil, fmt.Errorf("%w: %s can already %s", ErrBadParams, seat, key.Does())
	case errors.Is(err, advisor.ErrAccessNotTakable):
		return nil, fmt.Errorf("%w: %s has nothing this access could be added to", ErrBadParams, seat)
	case err != nil:
		return nil, fmt.Errorf("%w: %s is not an agent seat of this company", ErrBadParams, seat)
	}
	return AccessFixAnswer{Seat: seat, Access: key, Revision: revision, Fix: fix}, nil
}

func accessKeyNames() []string {
	out := []string{}
	for _, k := range advisor.AccessKeys() {
		out = append(out, string(k))
	}
	return out
}

// advisorLedger is the turn ledger over [advisor.LedgerDays], each seat named
// by the chart's handle while the chart holds it.
func (s Sources) advisorLedger(ctx context.Context, now time.Time) ([]usage.TurnLedgerRow, error) {
	if s.Usage == nil {
		return nil, errors.New("queries: no usage half to read the turn ledger from")
	}
	window := tokens.LastDays(advisor.LedgerDays, now, s.zone())
	rows, err := s.Usage.TurnLedger(ctx, usage.SpendQuery{From: window.First.Label, To: window.Last.Label})
	if err != nil {
		return nil, err
	}
	chart := s.chartHandles()
	for i := range rows {
		if seat, ok := chart[rows[i].AgentID]; ok && seat.handle != "" {
			rows[i].Handle = seat.handle
		}
	}
	return rows, nil
}
