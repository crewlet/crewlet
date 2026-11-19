package queries

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/duty"
	"github.com/crewlet/crewlet/internal/advisordomain"
)

// THE ADVISOR'S WEEKLY REPORTS, as the duty wrote them on the Monday after
// each week (internal/advisor's Report): kept as written, so each says what
// was true of its week however the list has moved since.

// AdvisorPeriod is the span a report covers, from its Monday to the next.
type AdvisorPeriod struct {
	From time.Time `json:"from"`
	To   time.Time `json:"to"`
}

// AdvisorReportSummary is one week's report as a list of them shows it.
type AdvisorReportSummary struct {
	Week      string        `json:"week"`
	Period    AdvisorPeriod `json:"period"`
	WrittenAt time.Time     `json:"written_at"`
	Opened    int           `json:"opened"`
	Resolved  int           `json:"resolved"`
	Applied   int           `json:"applied"`
	Dismissed int           `json:"dismissed"`
}

// AdvisorReportList is a page of reports, the newest week first.
type AdvisorReportList struct {
	Reports    []AdvisorReportSummary `json:"reports"`
	NextCursor *string                `json:"next_cursor"`
}

// AdvisorReport is one week's report.
type AdvisorReport struct {
	AdvisorReportSummary
	Metrics []advisor.Metric        `json:"metrics"`
	Unread  []string                `json:"unread"`
	Top     []advisor.ReportFinding `json:"top"`
	Summary *string                 `json:"summary"`
}

// The bounds of a page of reports.
const (
	maxReportsPage     = 52
	defaultReportsPage = 12
)

// advisorReports answers `advisor_reports`: a page of `limit=` weeks at
// `cursor=`, the newest first.
func (s Sources) advisorReports(ctx context.Context, p Params) (any, error) {
	limit := defaultReportsPage
	if raw := p.String("limit"); raw != "" {
		var err error
		if limit, err = strconv.Atoi(raw); err != nil || limit < 1 || limit > maxReportsPage {
			return nil, badParams("limit", raw, []string{"1 to " + strconv.Itoa(maxReportsPage)})
		}
	}
	rows, next, err := s.Advisor.Reports(ctx, p.String("cursor"), limit, advisorFresh)
	switch {
	case errors.Is(err, advisordomain.ErrInvalid):
		return nil, fmt.Errorf("%w: %s", ErrBadParams, advisordomain.Sentence(err))
	case err != nil:
		return nil, advisorUnreadable(err)
	}
	out := AdvisorReportList{Reports: []AdvisorReportSummary{}}
	for _, row := range rows {
		r, err := reportOf(row)
		if err != nil {
			return nil, err
		}
		out.Reports = append(out.Reports, r.AdvisorReportSummary)
	}
	if next != "" {
		out.NextCursor = &next
	}
	return out, nil
}

// advisorReport answers `advisor_report`: the report of `week=`, an ISO week
// such as 2026-W47.
func (s Sources) advisorReport(ctx context.Context, p Params) (any, error) {
	week := strings.TrimSpace(p.String("week"))
	if !advisordomain.IsWeek(week) {
		return nil, badParams("week", week, []string{"an ISO week such as 2026-W47"})
	}
	row, written, err := s.Advisor.Report(ctx, week, advisorFresh)
	switch {
	case err != nil:
		return nil, advisorUnreadable(err)
	case !written:
		return nil, &NotFound{Reason: "advisor_report", Detail: "The Advisor wrote no report for " + week + "."}
	}
	return reportOf(row)
}

// reportOf is a report row as a person reads it.
func reportOf(row advisordomain.ReportRow) (AdvisorReport, error) {
	r, err := duty.ReportOf(row)
	if err != nil {
		return AdvisorReport{}, err
	}
	return AdvisorReport{
		AdvisorReportSummary: AdvisorReportSummary{Week: row.Week, Period: AdvisorPeriod{From: row.From, To: row.To},
			WrittenAt: row.WrittenAt, Opened: r.Opened, Resolved: r.Resolved, Applied: r.Applied, Dismissed: r.Dismissed},
		Metrics: nonNil(r.Metrics), Unread: nonNil(r.Unread), Top: nonNil(r.Top), Summary: r.Summary,
	}, nil
}

// Week reads the week [from, to) a report is written for, from the records the
// detectors read over it: the turn ledger, the runs and the budget refusals.
// A source that fails is named, never counted as nothing.
func (o advisorObserver) Week(ctx context.Context, _, to time.Time) ([]advisor.Metric, []string, error) {
	until := to.Add(-time.Nanosecond)
	ev := advisor.Evidence{Until: until}
	var err error
	if ev.Ledger, err = o.s.advisorLedger(ctx, until); err != nil {
		log.WarnContext(ctx, "advisor_report_ledger_unreadable", "error", err.Error())
		ev.LedgerUnread = true
	}
	if ev.Runs, err = o.s.endedRuns(ctx, until); err != nil {
		log.WarnContext(ctx, "advisor_report_runs_unreadable", "error", err.Error())
		ev.RunsUnread = true
	}
	if ev.Stops, err = o.s.budgetStops(ctx, until); err != nil {
		log.WarnContext(ctx, "advisor_report_budget_stops_unreadable", "error", err.Error())
		ev.StopsUnread = true
	}
	metrics, unread := advisor.WeekMetrics(ev)
	return metrics, unread, nil
}
