package queries

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/usage"
)

// dayUsage is a turn ledger that answers each company day with its own rows.
type dayUsage struct {
	UsageReads
	byDay map[string][]usage.TurnLedgerRow
	err   error
	asked *[]usage.SpendQuery
}

func (u dayUsage) TurnLedger(_ context.Context, q usage.SpendQuery) ([]usage.TurnLedgerRow, error) {
	*u.asked = append(*u.asked, q)
	return u.byDay[q.From], u.err
}

// A WEEK'S DAYS ARE READ DAY BY DAY from the turn ledger, one company day a
// read, Monday first, each counted as the week is; a ledger that cannot be
// read is a failure, never seven empty days. Mutation: read the week as one
// range, or answer an unread ledger with zeros, and a check fails.
func TestAWeeksDaysAreReadDayByDay(t *testing.T) {
	t.Parallel()
	monday := time.Date(2030, 1, 14, 0, 0, 0, 0, time.UTC)
	from := monday.AddDate(0, 0, -7)
	nothing := usage.TurnLedgerRow{Disposition: "started", Outcome: "nothing", Turns: 2, Input: 300}
	moved := usage.TurnLedgerRow{Disposition: "started", Outcome: "moved", Turns: 5, Input: 1_000}
	var asked []usage.SpendQuery
	o := advisorObserver{s: Sources{Usage: dayUsage{asked: &asked, byDay: map[string][]usage.TurnLedgerRow{
		"2030-01-07": {nothing, moved}, "2030-01-13": {moved},
	}}}}
	days, err := o.Days(t.Context(), from, monday)
	if err != nil {
		t.Fatal(err)
	}
	if len(days) != 7 || len(asked) != 7 || asked[0].From != "2030-01-07" || asked[0].To != "2030-01-07" ||
		asked[6].From != "2030-01-13" {
		t.Fatalf("read %+v, answered %+v; want seven reads of one day each, Monday first", asked, days)
	}
	if days[0] != (advisor.Day{Day: "2030-01-07", Turns: 7, TurnsNothing: 2, Tokens: 1_300}) ||
		days[1] != (advisor.Day{Day: "2030-01-08"}) || days[6].Turns != 5 {
		t.Errorf("days = %+v", days)
	}
	asked = nil
	down := advisorObserver{s: Sources{Usage: dayUsage{asked: &asked, err: errors.New("estate closed")}}}
	if days, err := down.Days(t.Context(), from, monday); err == nil {
		t.Errorf("an unread ledger answered %+v, want a failure", days)
	}
}

// A LIST OF REPORTS SAYS EACH WEEK'S HEADLINE, and a report its days: both
// sent as lists, so a report written before either was kept reads as one
// with none. Mutation: send a missing list as null, and a check fails.
func TestAListOfReportsSaysEachWeeksHeadlineAndAReportItsDays(t *testing.T) {
	t.Parallel()
	monday := time.Date(2030, 1, 14, 0, 0, 0, 0, time.UTC)
	was := int64(25)
	body := func(r advisor.Report) json.RawMessage {
		raw, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	l := reportLog{reports: []advisordomain.ReportRow{
		{Week: "2030-W02", From: monday.AddDate(0, 0, -7), To: monday, Body: body(advisor.Report{
			Metrics: []advisor.Metric{{Key: "budget_stops", Value: 1}, {Key: "tokens", Value: 9_000},
				{Key: "turns", Value: 40, Was: &was}},
			Days: []advisor.Day{{Day: "2030-01-07", Turns: 40, Tokens: 9_000}}})},
		// A REPORT KEPT BEFORE THE DAYS WERE: its body says nothing of them.
		{Week: "2030-W01", From: monday.AddDate(0, 0, -14), To: monday.AddDate(0, 0, -7),
			Body: json.RawMessage(`{"metrics":[],"unread":[],"top":[],"summary":null}`)},
	}}
	s := Sources{Advisor: l}
	got, err := s.advisorReports(t.Context(), FromMap(map[string]any{}))
	if err != nil {
		t.Fatal(err)
	}
	page := got.(AdvisorReportList)
	keys := func(ms []advisor.Metric) []string {
		out := []string{}
		for _, m := range ms {
			out = append(out, m.Key)
		}
		return out
	}
	if len(page.Reports) != 2 || !slices.Equal(keys(page.Reports[0].Headline), []string{"turns", "tokens"}) ||
		page.Reports[0].Headline[0].Was == nil || page.Reports[1].Headline == nil {
		t.Fatalf("the headlines are %+v", page.Reports)
	}
	one, err := s.advisorReport(t.Context(), FromMap(map[string]any{"week": "2030-W02"}))
	if err != nil || len(one.(AdvisorReport).Days) != 1 {
		t.Fatalf("the report is %+v (%v), want its one day", one, err)
	}
	old, err := s.advisorReport(t.Context(), FromMap(map[string]any{"week": "2030-W01"}))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]any
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if days, ok := wire["days"].([]any); !ok || len(days) != 0 {
		t.Errorf("a report kept before its days sends days %v, want []", wire["days"])
	}
	if headline, ok := wire["headline"].([]any); !ok || len(headline) != 0 {
		t.Errorf("a report with no numbers sends headline %v, want []", wire["headline"])
	}
}
