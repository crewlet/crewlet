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

// teamShape is a company whose Eng unit holds two seats doing one job, and a
// reviewer doing another.
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
        goal: Ship the backlog.
        responsibilities: [code]
      - name: Dev B
        handle: dev-b
        llm: p
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

// A GROUP WHOSE WORK WAITED WHILE EVERY SEAT OF IT WAS BUSY IS OFFERED A SEAT
// LIKE ITS BUSIEST, on exact numbers only: four item-hours a day waited, on
// four of the days, every seat busy six tenths and idle a tenth or less of its
// available time, ten items worked, the group unchanged and every seat
// measured. The clone joins the unit with the first free name, its contact,
// secrets and schedules left out. Mutation: judge a seat whose busy time was
// not measured, or a group that changed in the week, and a row is raised.
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
	if f.Item != "35h item-hours waited for Dev A, Dev B, its seats busy 93% of their available time, 2029-12-31 to 2030-01-07" {
		t.Errorf("item = %q", f.Item)
	}
	if f.Fix.Kind != advisor.FixConfigChange || len(f.Fix.Options) != 1 {
		t.Fatalf("fix = %+v, want one seat added", f.Fix)
	}
	ch := f.Fix.Options[0].Change
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
				"      - name: Dev B\n        handle: dev-b\n        llm: p\n        goal: Ship the backlog.\n        responsibilities: [code]\n", "")))
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
