package advisor_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
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
		to   time.Time
	}{
		{monday.Add(3 * time.Hour), "2030-W02", monday},
		{monday.AddDate(0, 0, 2).Add(10 * time.Hour), "2030-W02", monday},
		{monday.Add(-time.Minute), "2030-W01", monday.AddDate(0, 0, -7)},
	} {
		week, from, to, due := advisor.ReportWeek(c.at.UTC(), zone)
		if week != c.week || !to.Equal(c.to) || !from.Equal(to.AddDate(0, 0, -7)) ||
			!due.Equal(to.Add(advisor.ReportHour*time.Hour)) {
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
		Runs:  []advisor.EndedRun{capped("dev", "t1", 1, 30), {Seat: "qa", Kind: types.ExecutorErrorOutOfMemory}},
		Stops: []advisor.BudgetStop{{Seat: "dev"}, {Seat: "qa"}}, StopsUnread: true,
	}
	metrics, unread := advisor.WeekMetrics(ev, nil)
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

// A WEEK'S FLOW IS ITS PICKUP WAIT, AND THE HOURS WORK WAITED, WAS HELD AND
// STOOD BLOCKED: the median and 90th percentile of how long each item picked
// up stood due, the due, held and blocked hours, and the held hours by why;
// a week nothing was picked up in says no pickup wait, and a company whose
// tracker keeps no flow record no flow numbers. Mutation: report a pickup
// wait of zero for a week with none, or the flow numbers of a tracker that
// keeps no flow record, and a check fails.
func TestAWeeksFlowIsItsPickupWaitAndTheHoursWorkWaitedWasHeldAndBlocked(t *testing.T) {
	t.Parallel()
	minutes := func(ms ...int) []time.Duration {
		out := make([]time.Duration, len(ms))
		for i, m := range ms {
			out[i] = time.Duration(m) * time.Minute
		}
		return out
	}
	flow := &advisor.WeekFlow{
		Hours: []worklet.FlowHours{
			{Seat: "dev", State: worklet.FlowDue, Stood: 3 * time.Hour},
			{Seat: "qa", State: worklet.FlowDue, Stood: 2 * time.Hour},
			{Seat: "dev", State: worklet.FlowHeld, Cause: worklet.FlowCauseLimit, Stood: 5 * time.Hour},
			{Seat: "dev", State: worklet.FlowHeld, Cause: worklet.FlowCausePaused, Stood: 2 * time.Hour},
			{Seat: "qa", State: worklet.FlowHeld, Cause: worklet.FlowCauseNobody, Stood: 20 * time.Minute},
			{Seat: "qa", State: worklet.FlowBlocked, Stood: 4 * time.Hour},
			{Seat: "qa", State: worklet.FlowMoving, Stood: 30 * time.Hour},
		},
		Pickups: minutes(5, 10, 20, 30, 40, 50, 60, 70, 80, 300),
	}
	metrics, unread := advisor.WeekMetrics(advisor.Evidence{}, flow)
	got := map[string]advisor.Metric{}
	var keys []string
	for _, m := range metrics {
		got[m.Key] = m
		keys = append(keys, m.Key)
	}
	want := map[string]int64{"pickup_p50": 40, "pickup_p90": 80, "wait_h": 5, "held_h": 7, "held_h.paused": 2,
		"held_h.limit": 5, "blocked_h": 4}
	for k, v := range want {
		if got[k].Value != v || got[k].Label == "" || !got[k].Unit.Valid() {
			t.Errorf("%s = %+v, want %d", k, got[k], v)
		}
	}
	if _, ok := got["held_h.nobody"]; ok {
		t.Error("twenty minutes held reads as a cause of the week's held hours")
	}
	if at := slices.Index(keys, "held_h"); at < 0 || keys[at+1] != "held_h.paused" || keys[at+2] != "held_h.limit" {
		t.Errorf("the held hours by why do not follow the held hours: %v", keys)
	}
	if len(unread) != 0 {
		t.Errorf("unread %v, want none", unread)
	}
	none, _ := advisor.WeekMetrics(advisor.Evidence{}, &advisor.WeekFlow{Hours: flow.Hours, Pickups: []time.Duration{}})
	for _, m := range none {
		if m.Key == "pickup_p50" || m.Key == "pickup_p90" {
			t.Errorf("a week nothing was picked up in says a pickup wait of %d", m.Value)
		}
	}
	kept, _ := advisor.WeekMetrics(advisor.Evidence{}, nil)
	for _, m := range kept {
		if m.Key == "wait_h" || m.Key == "pickup_p50" {
			t.Errorf("a tracker that keeps no flow record reports %s", m.Key)
		}
	}
	_, unread = advisor.WeekMetrics(advisor.Evidence{FlowUnread: true}, nil)
	if !slices.Contains(unread, advisor.UnreadFlow) {
		t.Errorf("an unread flow record is not named: %v", unread)
	}
}
