package advisor_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/usage"
)

// A REPORT COVERS THE LAST WHOLE WEEK ON THE COMPANY'S CLOCK, Monday to
// Monday where the company is, and is due on the Monday it ends on at the
// report's hour. Mutation: take the week in UTC, and a company west of it
// reports a week that ends on Sunday evening.
func TestAReportCoversTheLastWholeWeekOnTheCompanysClock(t *testing.T) {
	t.Parallel()
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("no time zone data: %v", err)
	}
	monday := time.Date(2030, 1, 14, 0, 0, 0, 0, zone)
	for _, c := range []struct {
		at   time.Time
		week string
	}{
		{monday.Add(3 * time.Hour), "2030-W02"},
		{monday.AddDate(0, 0, 2).Add(10 * time.Hour), "2030-W02"},
		{monday.Add(-time.Minute), "2030-W01"},
	} {
		week, from, to, due := advisor.ReportWeek(c.at.UTC(), zone)
		if week != c.week || to.After(c.at) || to.Weekday() != time.Monday || to.Hour() != 0 ||
			!from.Equal(to.AddDate(0, 0, -7)) || !due.Equal(to.Add(advisor.ReportHour*time.Hour)) {
			t.Errorf("at %s: %s from %s to %s due %s", c.at, week, from, to, due)
		}
	}
}

// A WEEK'S NUMBERS COME FROM THE RECORDS THE DETECTORS READ, and a source
// that could not be read is named rather than counted as nothing. Mutation:
// count an absorbed wake as a turn, or report an unread source's zero, and a
// number fails.
func TestAWeeksNumbersNameWhatCouldNotBeRead(t *testing.T) {
	t.Parallel()
	alarm := started("dev", "alarm:fired", "nothing", 3)
	alarm.Input = 3_000
	ev := advisor.Evidence{
		Ledger: []usage.TurnLedgerRow{alarm, started("dev", "chat:message", "nothing", 2),
			started("qa", "chat:message", "moved", 5), absorbed("qa", 9)},
		Runs: []advisor.EndedRun{capped("dev", "t1", 1, 30), {Seat: "qa", Kind: types.ExecutorErrorOutOfMemory}},
		Stops: []advisor.BudgetStop{{Seat: "dev"}, {Seat: "qa"}}, StopsUnread: true,
	}
	metrics, unread := advisor.WeekMetrics(ev)
	got := map[string]int64{}
	for _, m := range metrics {
		got[m.Key] = m.Value
		if m.Label == "" || !m.Unit.Valid() || m.Was != nil {
			t.Errorf("metric %+v has no words or a week before it was not given", m)
		}
	}
	want := map[string]int64{"turns": 10, "turns_nothing": 5, "tokens": 3_000, "alarm_turns_nothing": 3,
		"alarm_tokens_nothing": 3_000, "round_caps": 1}
	if len(got) != len(want) {
		t.Errorf("metrics %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %d, want %d", k, got[k], v)
		}
	}
	if !slices.Equal(unread, []string{advisor.UnreadStops}) {
		t.Errorf("unread %v, want the budget stops alone", unread)
	}
	last := &advisor.Report{Metrics: []advisor.Metric{{Key: "turns", Value: 7}}}
	for _, m := range advisor.Against(metrics, last) {
		if (m.Key == "turns") != (m.Was != nil) || (m.Was != nil && *m.Was != 7) {
			t.Errorf("%s beside last week: %v", m.Key, m.Was)
		}
	}
}
