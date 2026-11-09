package queries

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/notice"
	"github.com/crewlet/crewlet/internal/org"
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
	if ev.Paused, err = s.pausedSeats(ctx, company); err != nil {
		log.WarnContext(ctx, "advisor_seat_pauses_unreadable", "error", err.Error())
		ev.PausesUnread = true
	}
	return ev
}

// SeatPauseLister is the read of every seat's pause ([coord.SeatPauses]').
type SeatPauseLister interface {
	ListSeatPauses(ctx context.Context) ([]coord.SeatPause, error)
}

// pausedSeats is every agent seat of company the engine paused for a failure,
// named by handle: the pause record is keyed by the seat's agent id. A node with
// no pause store has none, since the engine pauses through that store alone.
func (s Sources) pausedSeats(ctx context.Context, company *config.Company) ([]advisor.PausedSeat, error) {
	var out []advisor.PausedSeat
	if s.Pauses == nil {
		return nil, nil
	}
	pauses, err := s.Pauses.ListSeatPauses(ctx)
	if err != nil {
		return nil, fmt.Errorf("queries: list the seats' pauses: %w", err)
	}
	failed := map[uuid.UUID]string{}
	for _, p := range pauses {
		if p.Failure != nil {
			failed[p.Seat] = p.Failure.Kind
		}
	}
	for role := range company.EachRole() {
		id, ok := org.DeriveAgentID(company.Name, role.Seat().Handle())
		if kind, paused := failed[id]; ok && role.Kind != org.KindHuman && paused {
			out = append(out, advisor.PausedSeat{Handle: role.Seat().Handle(), Name: role.Name, Failure: kind})
		}
	}
	return out, nil
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
