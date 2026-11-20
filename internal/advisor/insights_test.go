package advisor_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A KIND OF WAKE THAT MOSTLY CHANGED NOTHING IS ONE FINDING, the seats it woke
// inside with their wasted turns, its headline the share; a schedule's turns
// are the schedule classes' and a kind under the bar raises nothing. Mutation:
// raise it per seat, and the cause is split across rows again.
func TestAKindOfWakeThatMostlyChangedNothingIsOneFinding(t *testing.T) {
	t.Parallel()
	ledger := []usage.TurnLedgerRow{
		started("dev-a", "lead_fallback", "nothing", 40), started("dev-b", "lead_fallback", "nothing", 20),
		started("dev-a", "lead_fallback", "moved", 30), started("rev", "chat:message", "nothing", 49),
		started("rev", "schedule:check", "nothing", 90),
	}
	found := staffed(t, teamShape, advisor.Evidence{Ledger: ledger}, advisor.ClassWakeReasonWaste)
	if len(found) != 1 || found[0].Subject.Kind != advisor.SubjectWake || found[0].Subject.Name != "lead_fallback" {
		t.Fatalf("raised %+v, want one finding about lead-fallback wakes", found)
	}
	f := found[0]
	if f.Headline != "Lead fallback wakes changed nothing in 60 of 90 turns (67%) this week." {
		t.Errorf("headline = %q", f.Headline)
	}
	if len(f.Seats) != 2 || f.Seats[0].Name != "Dev A" || f.Seats[0].Value != 40 || f.Fix.Kind != advisor.FixAskCrewlet {
		t.Errorf("seats %+v, fix %+v", f.Seats, f.Fix)
	}
}

// SEATS WHOSE TURNS COST SEVERAL TIMES THE COMPANY'S ARE ONE FINDING, the
// costliest in its headline and every outlier inside it; a company too small
// to have a median raises nothing. Mutation: compare against the mean, and
// the outlier drags the bar up past itself.
func TestSeatsWhoseTurnsCostSeveralTimesTheCompanysAreOneFinding(t *testing.T) {
	t.Parallel()
	spend := func(seat string, turns, tokens int64) usage.TurnLedgerRow {
		r := started(seat, "chat:message", "moved", turns)
		r.Input = tokens
		return r
	}
	ledger := []usage.TurnLedgerRow{spend("dev-a", 10, 10_000_000), spend("dev-b", 10, 1_000_000),
		spend("rev", 10, 1_200_000), spend("ops", 10, 900_000)}
	found := staffed(t, teamShape, advisor.Evidence{Ledger: ledger}, advisor.ClassTurnCostOutlier)
	if len(found) != 1 || len(found[0].Seats) != 1 || found[0].Seats[0].Seat != "dev-a" {
		t.Fatalf("raised %+v, want one finding naming dev-a", found)
	}
	if h := found[0].Headline; h != "Dev A spends 1,000,000 tokens a turn, 8 times the company's median of 120,000." {
		t.Errorf("headline = %q", h)
	}
	if found := staffed(t, teamShape, advisor.Evidence{Ledger: ledger[:2]}, advisor.ClassTurnCostOutlier); len(found) != 0 {
		t.Errorf("two seats raised %+v, want no median to judge by", found)
	}
}

// A SEAT WHOSE SCHEDULES KEEP IT BUSY WHILE ITS WORK WAITS IS A FINDING: its
// schedules firing many times a day and its items waiting to start for hours,
// both in the headline. Mutation: drop the wait, and a seat whose checks run
// often but whose work moves is flagged.
func TestASeatWhoseSchedulesKeepItBusyWhileItsWorkWaitsIsAFinding(t *testing.T) {
	t.Parallel()
	var fires []advisor.ScheduleFire
	for i := range 7 * 48 {
		fires = append(fires, advisor.ScheduleFire{Seat: "rev", Name: []string{"health", "window", "post"}[i%3]})
	}
	flow := []worklet.FlowHours{{Seat: "rev", State: worklet.FlowDue, Stood: 60 * time.Hour, Items: 14}}
	found := staffed(t, teamShape, advisor.Evidence{Fires: fires, Flow: flow}, advisor.ClassScheduleLoad)
	if len(found) != 1 || found[0].Subject.Seat != "rev" ||
		found[0].Headline != "Reviewer's 3 schedules fired 48 times a day while 14 items of its work waited 60h to start." {
		t.Fatalf("raised %+v", found)
	}
	flow[0].Stood = 2 * time.Hour
	if found := staffed(t, teamShape, advisor.Evidence{Fires: fires, Flow: flow}, advisor.ClassScheduleLoad); len(found) != 0 {
		t.Errorf("a seat whose work moved raised %+v", found)
	}
}
