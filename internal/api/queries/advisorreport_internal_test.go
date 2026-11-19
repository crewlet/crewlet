package queries

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// reportLog is an advisor log holding the reports it was given, the newest first.
type reportLog struct {
	noticeLog
	reports []advisordomain.ReportRow
}

func (l reportLog) Reports(_ context.Context, cursor string, limit int, _ statelog.Freshness) ([]advisordomain.ReportRow, string, error) {
	if cursor != "" && !advisordomain.IsWeek(cursor) {
		return nil, "", advisordomain.ErrInvalid
	}
	rows := l.reports
	for len(rows) > 0 && cursor != "" && rows[0].Week >= cursor {
		rows = rows[1:]
	}
	if len(rows) > limit {
		return rows[:limit], rows[limit-1].Week, nil
	}
	return rows, "", nil
}

func (l reportLog) Report(_ context.Context, week string, _ statelog.Freshness) (advisordomain.ReportRow, bool, error) {
	for _, r := range l.reports {
		if r.Week == week {
			return r, true, nil
		}
	}
	return advisordomain.ReportRow{}, false, nil
}

// A WEEK'S REPORT IS READ AS IT WAS WRITTEN, a page of weeks the newest first,
// and every field is sent: a metric with no week before it says null, as does
// a report the model did not summarise. Mutation: drop the cursor, or send a
// missing list as null, and a check fails.
func TestAWeeksReportIsReadAsItWasWritten(t *testing.T) {
	t.Parallel()
	monday := time.Date(2030, 1, 14, 0, 0, 0, 0, time.UTC)
	was := int64(25)
	row := func(week string, to time.Time, r advisor.Report) advisordomain.ReportRow {
		body, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return advisordomain.ReportRow{Week: week, From: to.AddDate(0, 0, -7), To: to, WrittenAt: to.Add(6 * time.Hour), Body: body}
	}
	l := reportLog{reports: []advisordomain.ReportRow{
		row("2030-W02", monday, advisor.Report{Tally: advisor.Tally{Opened: 4, Resolved: 2, Applied: 1, Dismissed: 1},
			Metrics: []advisor.Metric{{Key: "turns_nothing", Label: "Turns that committed nothing", Value: 40,
				Unit: advisor.UnitTurns, Was: &was}, {Key: "tokens", Label: "Tokens", Value: 9, Unit: advisor.UnitTokens}},
			Top: []advisor.ReportFinding{{ID: "f-1", Class: advisor.ClassRoundCap, Severity: advisor.SeverityCosts}}}),
		row("2030-W01", monday.AddDate(0, 0, -7), advisor.Report{}),
	}}
	s := Sources{Advisor: l}
	got, err := s.advisorReports(t.Context(), FromMap(map[string]any{"limit": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	page := got.(AdvisorReportList)
	if len(page.Reports) != 1 || page.Reports[0].Week != "2030-W02" || page.Reports[0].Opened != 4 ||
		page.NextCursor == nil || *page.NextCursor != "2030-W02" {
		t.Fatalf("the first page is %+v", page)
	}
	rest, err := s.advisorReports(t.Context(), FromMap(map[string]any{"cursor": *page.NextCursor}))
	if err != nil || len(rest.(AdvisorReportList).Reports) != 1 || rest.(AdvisorReportList).NextCursor != nil {
		t.Fatalf("the next page is %+v (%v), want the week before alone", rest, err)
	}
	one, err := s.advisorReport(t.Context(), FromMap(map[string]any{"week": "2030-W02"}))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(one)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"week", "period", "written_at", "opened", "resolved", "applied", "dismissed",
		"metrics", "unread", "top", "summary"} {
		if _, sent := wire[field]; !sent {
			t.Errorf("the report does not send %q: %s", field, raw)
		}
	}
	metrics := wire["metrics"].([]any)
	if metrics[0].(map[string]any)["was"] != float64(25) || metrics[1].(map[string]any)["was"] != nil {
		t.Errorf("the metrics are %v, want 25 beside the first and null beside the second", metrics)
	}
	empty, err := s.advisorReport(t.Context(), FromMap(map[string]any{"week": "2030-W01"}))
	if err != nil {
		t.Fatal(err)
	}
	if r := empty.(AdvisorReport); r.Metrics == nil || r.Unread == nil || r.Top == nil || r.Summary != nil {
		t.Errorf("an empty week sends %+v, want empty lists and a null summary", r)
	}
	var missing *NotFound
	if _, err := s.advisorReport(t.Context(), FromMap(map[string]any{"week": "2029-W50"})); !errors.As(err, &missing) {
		t.Errorf("an unwritten week answered %v, want not found", err)
	}
	if _, err := s.advisorReport(t.Context(), FromMap(map[string]any{"week": "last"})); !errors.Is(err, ErrBadParams) {
		t.Errorf("a week that is none answered %v, want bad params", err)
	}
}
