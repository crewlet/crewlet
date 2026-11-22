package advisor_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/usage"
	"github.com/crewlet/crewlet/internal/worklet"
)

func statOf(s advisor.Snapshot, key string) (advisor.Stat, bool) {
	at := slices.IndexFunc(s.Stats, func(st advisor.Stat) bool { return st.Key == key })
	if at < 0 {
		return advisor.Stat{}, false
	}
	return s.Stats[at], true
}

func chartOf(s advisor.Snapshot, id string) (advisor.Chart, bool) {
	at := slices.IndexFunc(s.Charts, func(c advisor.Chart) bool { return c.ID == id })
	if at < 0 {
		return advisor.Chart{}, false
	}
	return s.Charts[at], true
}

func value(v *float64) float64 {
	if v == nil {
		return -1
	}
	return *v
}

// A SNAPSHOT'S NUMBERS ARE THE RECORDS IT READS: turns, the share that changed
// nothing and tokens per turn from the turn ledger, each beside the range
// before; the hours work stood in each flow state and the median pickup wait
// from the flow record; open items from who holds them; and each chart's
// series the same numbers split by seat or wake. Mutation: count an absorbed
// wake as a turn, or drop the range before, and a number fails.
func TestASnapshotsNumbersAreTheRecordsItReads(t *testing.T) {
	t.Parallel()
	ops, dev := started("dev", "lead_fallback", "nothing", 30), started("dev", "chat:message", "moved", 10)
	ops.Input, dev.Input = 3_000_000, 1_000_000
	in := advisor.SnapshotInputs{
		Ledger:       []usage.TurnLedgerRow{ops, dev, started("qa", "lead_fallback", "moved", 10), absorbed("qa", 99)},
		LedgerBefore: []usage.TurnLedgerRow{started("dev", "chat:message", "nothing", 10)},
		FlowKept:     true,
		Flow: []worklet.FlowHours{{Seat: "dev", State: worklet.FlowDue, Stood: 3 * time.Hour},
			{Seat: "dev", State: worklet.FlowHeld, Cause: worklet.FlowCauseLimit, Stood: 5 * time.Hour},
			{Seat: "qa", State: worklet.FlowMoving, Stood: 2 * time.Hour}},
		FlowBefore: []worklet.FlowHours{{Seat: "dev", State: worklet.FlowDue, Stood: time.Hour}},
		Pickups:    []time.Duration{10 * time.Minute, 30 * time.Minute, 50 * time.Minute},
		Open:       []worklet.HeldOpen{{Handle: "qa", Open: 2}, {Handle: "dev", Open: 5, Pressing: 2}},
		Names:      map[string]string{"dev": "Developer", "qa": "QA"},
	}
	s := advisor.ComputeSnapshot(in, monday)
	want := map[string]float64{"turns": 50, "wasted_turns_percent": 60, "tokens_per_turn": 80_000,
		"wait_hours": 3, "held_hours": 5, "blocked_hours": 0, "pickup_wait_minutes": 30, "open_items": 7}
	for key, v := range want {
		if st, ok := statOf(s, key); !ok || st.Value != v || st.Label == "" || !st.Unit.Valid() {
			t.Errorf("%s = %+v (%v), want %v", key, st, ok, v)
		}
	}
	if st, _ := statOf(s, "turns"); value(st.Was) != 10 {
		t.Errorf("turns was %v, want the range before's 10", st.Was)
	}
	if st, _ := statOf(s, "wait_hours"); value(st.Was) != 1 || st.Better == nil || *st.Better != "down" {
		t.Errorf("wait hours = %+v, want 1 the range before and better down", st)
	}
	wakes, ok := chartOf(s, "wasted_wakes")
	if !ok || wakes.X.Keys[0] != "lead_fallback" || wakes.X.Labels[0] != "Lead fallback" ||
		value(wakes.Series[0].Values[0]) != 30 || wakes.Note == nil ||
		*wakes.Note != "Lead fallback wakes changed nothing in 30 of 40 turns (75%)." {
		t.Errorf("wasted wakes = %+v", wakes)
	}
	open, ok := chartOf(s, "open_work")
	if !ok || !slices.Equal(open.X.Keys, []string{"dev", "qa"}) || open.X.Labels[0] != "Developer" ||
		value(open.Series[0].Values[0]) != 2 || value(open.Series[1].Values[0]) != 3 {
		t.Errorf("open work = %+v", open)
	}
	flow, ok := chartOf(s, "flow_by_seat")
	if !ok || flow.X.Keys[0] != "dev" || flow.Kind != advisor.ChartStackedColumns {
		t.Errorf("flow by seat = %+v", flow)
	}
	cost, ok := chartOf(s, "tokens_per_turn")
	if !ok || cost.X.Keys[0] != "dev" || value(cost.Series[0].Values[0]) != 100_000 || cost.Note == nil {
		t.Errorf("tokens per turn = %+v", cost)
	}
	if len(s.Unread) != 0 {
		t.Errorf("unread %v, want none", s.Unread)
	}
}

// A SOURCE A SNAPSHOT COULD NOT READ IS NAMED, AND ITS NUMBERS LEFT OUT, never
// drawn as nothing; a tracker that keeps no flow record draws no flow numbers
// and names nothing unread. Mutation: draw an unread ledger's zero turns, and
// the strip reads as a quiet week.
func TestASourceASnapshotCouldNotReadIsNamedAndLeftOut(t *testing.T) {
	t.Parallel()
	s := advisor.ComputeSnapshot(advisor.SnapshotInputs{LedgerUnread: true, FlowUnread: true, WorkUnread: true}, monday)
	if len(s.Stats) != 0 || len(s.Charts) != 0 ||
		!slices.Equal(s.Unread, []string{advisor.UnreadWork, advisor.UnreadFlow, advisor.UnreadLedger}) {
		t.Errorf("an unread company drew %d stats and %d charts, unread %v", len(s.Stats), len(s.Charts), s.Unread)
	}
	s = advisor.ComputeSnapshot(advisor.SnapshotInputs{FlowKept: false}, monday)
	if _, ok := statOf(s, "wait_hours"); ok || slices.Contains(s.Unread, advisor.UnreadFlow) {
		t.Errorf("a tracker keeping no flow record drew %+v, unread %v", s.Stats, s.Unread)
	}
}

// EVERY CLASS SAYS WHAT IT CONCLUDED: a rule run names every class it runs as
// found, clear, unread with its source, or silent with why it could not judge,
// so a class that judged nothing never reads as one that found nothing.
// Mutation: leave a class without an outcome, or call a silent one clear, and
// the run hides what it skipped.
func TestEveryClassSaysWhatItConcluded(t *testing.T) {
	t.Parallel()
	found := []advisor.Finding{{Class: advisor.ClassRoundCap, Source: advisor.SourceEvidence}}
	silent := []advisor.Silence{{Class: advisor.ClassAddSeat, Reason: "Some seat groups could not be judged."}}
	out := advisor.Outcomes(found, []string{advisor.UnreadWork}, silent)
	by := map[advisor.Class]advisor.ClassOutcome{}
	for _, o := range out {
		by[o.Class] = o
	}
	for _, class := range append(advisor.EvidenceClasses(), advisor.ClassAddSeat, advisor.ClassRemoveSeat) {
		if _, ok := by[class]; !ok && !advisor.Shared(class) {
			t.Errorf("%s has no outcome", class)
		} else if ok && advisor.Shared(class) {
			t.Errorf("%s is never listed, yet the run says it concluded %s", class, by[class].Outcome)
		}
	}
	for _, c := range []struct {
		class   advisor.Class
		outcome advisor.Outcome
	}{
		{advisor.ClassRoundCap, advisor.OutcomeFound}, {advisor.ClassAddSeat, advisor.OutcomeSilent},
		{advisor.WorkClasses()[0], advisor.OutcomeUnread}, {advisor.ClassRemoveSeat, advisor.OutcomeClear},
	} {
		explained := c.outcome == advisor.OutcomeSilent || c.outcome == advisor.OutcomeUnread
		if got := by[c.class]; got.Outcome != c.outcome || explained != (got.Reason != "") {
			t.Errorf("%s = %+v, want %s, with a reason only where it could not judge", c.class, got, c.outcome)
		}
	}
}

// THE STAFFING CLASSES SAY WHY THEY COULD NOT JUDGE: a group whose seats' time
// was not measured every day, or whose seats changed over the window, is named
// in the class's reason rather than passed over in silence. Mutation: drop
// the reason, and a company that moved its seats mid-week reads as well staffed.
func TestTheStaffingClassesSayWhyTheyCouldNotJudge(t *testing.T) {
	t.Parallel()
	week := 7 * 24 * time.Hour
	seats := map[string]advisor.SeatTime{"dev-a": {Busy: week / 2}, "dev-b": seatTime(week / 2)}
	ev := advisor.Evidence{StaffingWeek: staffing(t, 7, seats, nil, nil), Until: monday}
	in := advisor.Inputs{Company: company(t, teamShape), Revision: "r1", Integrations: []integration.State{}}
	answer := advisor.ReadWithEvidence(in, ev)
	at := slices.IndexFunc(answer.Silent, func(s advisor.Silence) bool { return s.Class == advisor.ClassAddSeat })
	if at < 0 || !strings.Contains(answer.Silent[at].Reason, "dev-a") ||
		!strings.Contains(answer.Silent[at].Reason, "not measured every day") {
		t.Errorf("silent = %+v, want add_seat naming dev-a's unmeasured time", answer.Silent)
	}
	answer = advisor.ReadWithEvidence(in, advisor.Evidence{Until: monday})
	if at := slices.IndexFunc(answer.Silent, func(s advisor.Silence) bool { return s.Class == advisor.ClassRemoveSeat }); at < 0 ||
		!strings.Contains(answer.Silent[at].Reason, "no flow record") {
		t.Errorf("silent = %+v, want remove_seat saying the tracker keeps no flow record", answer.Silent)
	}
}
