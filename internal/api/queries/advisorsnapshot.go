package queries

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tokens"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE ADVISOR'S PAGE NUMBERS (ADR-0166): the company's stats and charts over
// the last 7, 14 or 30 company days, today included, each stat beside the
// range before it, read from the records the detectors read. A source that
// fails is named in `unread`, and its numbers left out.

// advisorSnapshot answers `advisor_snapshot`: the snapshot over `range`
// (7d, 14d or 30d; 7d where absent).
func (s Sources) advisorSnapshot(ctx context.Context, p Params) (any, error) {
	days, err := snapshotDays(p.String("range"))
	if err != nil {
		return nil, err
	}
	return s.AdvisorSnapshot(ctx, s.clock(), days), nil
}

// snapshotDays is a range parameter's days.
func snapshotDays(raw string) (int, error) {
	if raw == "" {
		return advisor.SnapshotRanges[0], nil
	}
	n, err := strconv.Atoi(strings.TrimSuffix(raw, "d"))
	if err != nil || !strings.HasSuffix(raw, "d") || !slices.Contains(advisor.SnapshotRanges, n) {
		return 0, fmt.Errorf("%w: range is one of 7d, 14d or 30d, and %q is not", ErrBadParams, raw)
	}
	return n, nil
}

// AdvisorSnapshot is the snapshot over the days company days that end at now,
// today included.
func (s Sources) AdvisorSnapshot(ctx context.Context, now time.Time, days int) advisor.Snapshot {
	zone := s.zone()
	window := tokens.LastDays(days, now, zone)
	local := now.In(zone)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, zone)
	from := today.AddDate(0, 0, 1-days)
	before := from.AddDate(0, 0, -days)
	in := advisor.SnapshotInputs{Range: advisor.SnapshotRange{From: from.UTC(), To: now.UTC(), Days: days},
		Names: map[string]string{}}
	company := s.company()
	if company != nil {
		for role := range company.EachRole() {
			in.Names[role.Seat().Handle()] = role.Name
		}
	}
	var err error
	if in.Ledger, err = s.ledgerOver(ctx, window); err == nil {
		in.LedgerBefore, err = s.ledgerOver(ctx, tokens.LastDays(days, from.Add(-time.Nanosecond), zone))
	}
	if err != nil {
		log.WarnContext(ctx, "advisor_snapshot_ledger_unreadable", "error", err.Error())
		in.LedgerUnread = true
	}
	switch kept, kerr := keptHere(s.Work); {
	case kerr != nil:
		log.WarnContext(ctx, "advisor_snapshot_flow_unreadable", "error", kerr.Error())
		in.FlowUnread = true
	case kept:
		in.FlowKept = true
		if in.Flow, in.Pickups, err = s.flowOver(ctx, from, now); err == nil {
			in.FlowBefore, in.PickupsBefore, err = s.flowOver(ctx, before, from)
		}
		if err != nil {
			log.WarnContext(ctx, "advisor_snapshot_flow_unreadable", "error", err.Error())
			in.FlowUnread = true
		}
	}
	if in.Capacity, err = s.advisorCapacityOf(ctx, company, now, days); err == nil {
		in.CapacityBefore, err = s.advisorCapacityOf(ctx, company, from.Add(-time.Nanosecond), days)
	}
	if err != nil {
		log.WarnContext(ctx, "advisor_snapshot_capacity_unreadable", "error", err.Error())
		in.Capacity, in.CapacityBefore, in.CapacityUnread = nil, nil, true
	}
	if in.Open, err = s.openHolders(ctx, company, now); err != nil {
		log.WarnContext(ctx, "advisor_snapshot_work_unreadable", "error", err.Error())
		in.WorkUnread = true
	}
	return advisor.ComputeSnapshot(in, now.UTC())
}

// ledgerOver is the turn ledger over window, each seat named by the chart's
// handle while the chart holds it.
func (s Sources) ledgerOver(ctx context.Context, window tokens.Range) ([]usage.TurnLedgerRow, error) {
	if s.Usage == nil {
		return nil, errors.New("queries: no usage half to read the turn ledger from")
	}
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

// flowOver is the flow record's hours and pickup waits over [from, to), read
// in windows the flow read takes at once.
func (s Sources) flowOver(ctx context.Context, from, to time.Time) ([]worklet.FlowHours, []time.Duration, error) {
	var hours []worklet.FlowHours
	pickups := []time.Duration{}
	for since := from; since.Before(to); {
		until := since.Add(worklet.MaxFlowHoursWindow)
		if until.After(to) {
			until = to
		}
		q := worklet.FlowHoursQuery{Since: since, Until: until, Level: statelog.ReadStale}
		h, err := s.Work.FlowHours(ctx, q)
		if err != nil {
			return nil, nil, err
		}
		p, err := s.Work.FlowPickups(ctx, q)
		if err != nil {
			return nil, nil, err
		}
		hours, pickups = append(hours, h...), append(pickups, p...)
		since = until
	}
	slices.Sort(pickups)
	return hours, pickups, nil
}

// openHolders is every holder's open items now.
func (s Sources) openHolders(ctx context.Context, company *config.Company, now time.Time) ([]worklet.HeldOpen, error) {
	if company == nil || s.Work == nil {
		return nil, errors.New("queries: this node reads no company work")
	}
	window := advisor.PatternsWindow(now, company)
	window.Level = statelog.ReadStale
	work, err := s.Work.WorkPatterns(ctx, window)
	if err != nil {
		return nil, err
	}
	return work.Open.Holders, nil
}
