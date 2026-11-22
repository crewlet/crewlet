package advisor_test

import (
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/worklet"
)

// teamShape is a company whose Eng unit holds two seats doing one job, each
// one turn at a time, and a reviewer doing another.
const teamShape = `name: Acme
providers:
  llm:
    p: {model: m}
  sandbox: {fake: true}
roles:
  - name: Owner
    handle: owner
    kind: human
    contact: {slack_user_id: U0OWNER}
units:
  - name: Eng
    id: eng
    roles:
      - name: Dev A
        handle: dev-a
        llm: p
        sandbox: {max_concurrent_runs: 1}
        goal: Ship the backlog.
        responsibilities: [code]
      - name: Dev B
        handle: dev-b
        llm: p
        sandbox: {max_concurrent_runs: 1}
        goal: Ship the backlog.
        responsibilities: [code]
      - name: Reviewer
        handle: rev
        llm: p
        goal: Review the changes.
`

// staffing is a window ending at monday of days, its seats' time as given, the
// document unchanged over it.
func staffing(t *testing.T, days int, seats map[string]advisor.SeatTime, due map[string]time.Duration,
	items map[string]int) *advisor.Staffing {
	t.Helper()
	s := &advisor.Staffing{From: monday.AddDate(0, 0, -days), To: monday, Seats: seats,
		Documents: []*config.Company{company(t, teamShape)}, Items: items}
	for seat, d := range due {
		s.Flow = append(s.Flow, worklet.FlowHours{Seat: seat, State: worklet.FlowDue, Stood: d})
	}
	for range days {
		day := map[string]time.Duration{}
		for seat, d := range due {
			day[seat] = d / time.Duration(days)
		}
		s.DueByDay = append(s.DueByDay, day)
	}
	return s
}

// seatTime is a seat measured busy for busy over a window with nothing stopped.
func seatTime(busy time.Duration) advisor.SeatTime {
	return advisor.SeatTime{Busy: busy, Measured: true}
}

// staffed is every finding of class over teamShape with ev's staffing.
func staffed(t *testing.T, doc string, ev advisor.Evidence, class advisor.Class) []advisor.Finding {
	t.Helper()
	ev.Until = monday
	var out []advisor.Finding
	in := advisor.Inputs{Company: company(t, doc), Revision: "r1", Integrations: []integration.State{}}
	for _, f := range advisor.ReadWithEvidence(in, ev).Findings {
		if f.Class == class {
			out = append(out, f)
		}
	}
	return out
}

// A GROUP WHOSE WORK WAITED WHILE EVERY SEAT OF IT WAS BUSY IS OFFERED ONE MORE
// TURN AT ONCE ON EACH SEAT, THEN A SEAT LIKE ITS BUSIEST, on exact numbers
// only: four item-hours a day waited, on four of the days, every seat busy six
// tenths and idle a tenth or less of its capacity, ten items worked, the group
// unchanged and every seat measured. The clone joins the unit with the first
// free name, its contact, secrets and schedules left out. Mutation: judge a
// seat whose busy time was not measured, or a group that changed in the week,
// and a row is raised; drop the raise, and the first option is the seat.
func TestAGroupWhoseWorkWaitedWhileItWasBusyIsOfferedASeat(t *testing.T) {
	t.Parallel()
	week := 7 * 24 * time.Hour
	busy := map[string]advisor.SeatTime{"dev-a": seatTime(week - 8*time.Hour), "dev-b": seatTime(week - 13*time.Hour)}
	due := map[string]time.Duration{"dev-a": 20 * time.Hour, "dev-b": 15 * time.Hour}
	items := map[string]int{"dev-a": 6, "dev-b": 5}
	found := staffed(t, teamShape, advisor.Evidence{StaffingWeek: staffing(t, 7, busy, due, items)}, advisor.ClassAddSeat)
	if len(found) != 1 || found[0].Subject.Kind != advisor.SubjectGroup || found[0].Subject.Unit != "eng" ||
		found[0].Subject.Seat != "dev-a" {
		t.Fatalf("raised %+v, want Eng's developers", found)
	}
	f := found[0]
	if f.Item != "35h item-hours waited for Dev A, Dev B, its seats busy 93% of their capacity, 2029-12-31 to 2030-01-07" {
		t.Errorf("item = %q", f.Item)
	}
	if f.Fix.Kind != advisor.FixConfigChange || len(f.Fix.Options) != 2 {
		t.Fatalf("fix = %+v, want one more turn at once, then one seat added", f.Fix)
	}
	if got := raisedRuns(t, f.Fix.Options[0].Change); got["dev-a"] != 2 || got["dev-b"] != 2 || len(got) != 2 {
		t.Errorf("the first option raises %v, want both developers to 2 turns at once", got)
	}
	ch := f.Fix.Options[1].Change
	roles, _ := ch.Set["roles"].([]any)
	clone, _ := roles[len(roles)-1].(map[string]any)
	if ch.Entity != advisor.EntityUnits || ch.ID != "eng" || len(roles) != 4 || clone["handle"] != "dev-a-2" ||
		clone["name"] != "Dev A 2" || clone["goal"] != "Ship the backlog." {
		t.Errorf("the change is %+v, want Eng with Dev A 2 cloned from Dev A", ch)
	}
	for name, s := range map[string]*advisor.Staffing{
		"a seat not measured": func() *advisor.Staffing {
			unmeasured := map[string]advisor.SeatTime{"dev-a": busy["dev-a"], "dev-b": {Busy: busy["dev-b"].Busy}}
			return staffing(t, 7, unmeasured, due, items)
		}(),
		"a group that changed in the week": func() *advisor.Staffing {
			s := staffing(t, 7, busy, due, items)
			s.Documents = append(s.Documents, company(t, replace(t, teamShape,
				"      - name: Dev B\n        handle: dev-b\n        llm: p\n        sandbox: {max_concurrent_runs: 1}\n"+
					"        goal: Ship the backlog.\n        responsibilities: [code]\n", "")))
			return s
		}(),
		"nine items":       staffing(t, 7, busy, due, map[string]int{"dev-a": 5, "dev-b": 4}),
		"an idle seat":     staffing(t, 7, map[string]advisor.SeatTime{"dev-a": busy["dev-a"], "dev-b": seatTime(week - 20*time.Hour)}, due, items),
		"less work waited": staffing(t, 7, busy, map[string]time.Duration{"dev-a": 10 * time.Hour, "dev-b": 10 * time.Hour}, items),
	} {
		if got := staffed(t, teamShape, advisor.Evidence{StaffingWeek: s}, advisor.ClassAddSeat); len(got) != 0 {
			t.Errorf("%s: raised %+v", name, got)
		}
	}
}

// raisedRuns is each seat's sandbox.max_concurrent_runs in a unit change's
// roles, by handle, for the seats that name one.
func raisedRuns(t *testing.T, ch advisor.Change) map[string]int {
	t.Helper()
	if ch.Entity != advisor.EntityUnits || ch.ID != "eng" {
		t.Fatalf("the change is %+v, want Eng's roles", ch)
	}
	out := map[string]int{}
	roles, _ := ch.Set["roles"].([]any)
	for _, r := range roles {
		m, _ := r.(map[string]any)
		sandbox, _ := m["sandbox"].(map[string]any)
		if runs, ok := sandbox["max_concurrent_runs"].(int); ok {
			out[m["handle"].(string)] = runs
		}
	}
	return out
}

// TURNS THAT OVERLAP NEVER READ ABOVE THEIR SEATS' CAPACITY. A seat running
// two turns at once (sandbox.max_concurrent_runs: 2) adds both turns'
// durations, so its busy time is more than its available time; measured
// against its capacity, its available time for each turn it may run, the
// share stays at or below 100%. A seat with a slot free all week is not busy.
// Mutation: measure busy against available time alone, and the overlapping
// group reads 186% and the half-used one is offered a seat.
func TestOverlappingTurnsNeverReadAboveTheirSeatsCapacity(t *testing.T) {
	t.Parallel()
	week := 7 * 24 * time.Hour
	capped := func(doc, handle string) string {
		at := "        handle: " + handle + "\n        llm: p\n"
		return replace(t, doc, at+"        sandbox: {max_concurrent_runs: 1}\n",
			at+"        sandbox: {max_concurrent_runs: 2}\n")
	}
	shape := capped(capped(teamShape, "dev-a"), "dev-b")
	due := map[string]time.Duration{"dev-a": 20 * time.Hour, "dev-b": 15 * time.Hour}
	items := map[string]int{"dev-a": 6, "dev-b": 5}
	window := func(seats map[string]advisor.SeatTime) *advisor.Staffing {
		s := staffing(t, 7, seats, due, items)
		s.Documents = []*config.Company{company(t, shape)}
		return s
	}
	both := map[string]advisor.SeatTime{"dev-a": seatTime(2*week - 16*time.Hour), "dev-b": seatTime(2*week - 26*time.Hour)}
	found := staffed(t, shape, advisor.Evidence{StaffingWeek: window(both)}, advisor.ClassAddSeat)
	if len(found) != 1 {
		t.Fatalf("raised %+v, want Eng's developers, busy on both their turns", found)
	}
	var busy int64
	for _, fact := range found[0].Evidence {
		if fact.Key == "busy_percent" {
			busy = fact.Value
		}
	}
	if busy > 100 || busy != 93 {
		t.Errorf("busy = %d%%, want 93%%: overlapping turns are measured against two turns' capacity", busy)
	}
	if got := raisedRuns(t, found[0].Fix.Options[0].Change); got["dev-a"] != 3 || got["dev-b"] != 3 {
		t.Errorf("the first option raises %v, want each developer one above its 2", got)
	}
	half := map[string]advisor.SeatTime{"dev-a": seatTime(week - 8*time.Hour), "dev-b": seatTime(week - 13*time.Hour)}
	if got := staffed(t, shape, advisor.Evidence{StaffingWeek: window(half)}, advisor.ClassAddSeat); len(got) != 0 {
		t.Errorf("seats with a turn's slot free all week were offered a seat: %+v", got)
	}
}

// A GROUP OF SEATS IDLE MOST OF TWO WEEKS IS TOLD IT COULD LOSE ONE, the least
// busy, when nothing waited for them; a seat stopped is not idle. Mutation:
// count stopped time as idle, and the stopped group is told to shrink.
func TestAGroupIdleMostOfTwoWeeksCouldLoseASeat(t *testing.T) {
	t.Parallel()
	fortnight := 14 * 24 * time.Hour
	idle := map[string]advisor.SeatTime{"dev-a": seatTime(fortnight / 5), "dev-b": seatTime(fortnight / 10)}
	found := staffed(t, teamShape, advisor.Evidence{StaffingFortnight: staffing(t, 14, idle, nil, nil)}, advisor.ClassRemoveSeat)
	if len(found) != 1 || found[0].Severity != advisor.SeverityNote || found[0].Fix.Kind != advisor.FixConfigChange {
		t.Fatalf("raised %+v, want a note with the unit write", found)
	}
	roles, _ := found[0].Fix.Options[0].Change.Set["roles"].([]any)
	var handles []string
	for _, r := range roles {
		handles = append(handles, r.(map[string]any)["handle"].(string))
	}
	if !slices.Equal(handles, []string{"dev-a", "rev"}) {
		t.Errorf("the unit keeps %v, want Dev B (the least busy) removed", handles)
	}
	stopped := map[string]advisor.SeatTime{"dev-a": {Busy: fortnight / 10, Stopped: fortnight * 8 / 10, Measured: true},
		"dev-b": seatTime(fortnight / 10)}
	if got := staffed(t, teamShape, advisor.Evidence{StaffingFortnight: staffing(t, 14, stopped, nil, nil)},
		advisor.ClassRemoveSeat); len(got) != 0 {
		t.Errorf("a group with a seat stopped most of the time raised %+v", got)
	}
	waited := staffing(t, 14, idle, map[string]time.Duration{"dev-a": 3 * time.Hour}, nil)
	if got := staffed(t, teamShape, advisor.Evidence{StaffingFortnight: waited}, advisor.ClassRemoveSeat); len(got) != 0 {
		t.Errorf("a group work waited three hours for raised %+v", got)
	}
}
