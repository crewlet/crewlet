package engine

import (
	"bytes"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
)

// meteredCompany is an epoch with an org budget and the given seats, which is
// all a budget frame reads. Built through the config's own transform, so the
// org model carries exactly what an authored `token_budget:` would give it.
func meteredCompany(orgBudget config.TokenBudget, seats ...*org.Role) *Company {
	return &Company{
		Config: &config.Company{TokenBudget: orgBudget},
		Org:    &org.Organization{Name: "Acme", Roles: seats, TokenBudget: orgBudget.Ceilings()},
	}
}

// ceiling is one authored window's ceiling.
func ceiling(n int) *int { return &n }

func scopeOf(t *testing.T, c *Company, seat *org.Role) string {
	t.Helper()
	id, ok := c.Org.AgentIDFor(seat)
	if !ok {
		t.Fatalf("%s has no agent id", seat.Name)
	}
	return coord.AgentScope(id.String())
}

// snapshotWindows is the instant every frame here is read at.
var snapshotWindows = coord.WindowsAt(time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC), time.UTC)

// row is one scope's counter at snapshotWindows: spend per period, and the
// periods refusing since the given instant.
func row(scope string, used map[period.Period]int, refused time.Time, refusing ...period.Period) coord.Usage {
	u := coord.Unspent(scope, snapshotWindows)
	for i, p := range period.Periods {
		u.Windows[i].Used = used[p]
	}
	for _, p := range refusing {
		for i, q := range period.Periods {
			if p == q {
				u.Windows[i].RefusedAt = refused
			}
		}
	}
	return u
}

// windowsOf keys a scope's windows by period, for assertions.
func windowsOf(ws []types.BudgetWindow) map[string]types.BudgetWindow {
	out := make(map[string]types.BudgetWindow, len(ws))
	for _, w := range ws {
		out[w.Period] = w
	}
	return out
}

// limitOf is a window's ceiling, and -1 where it states none.
func limitOf(w types.BudgetWindow) int {
	if w.Limit == nil {
		return -1
	}
	return *w.Limit
}

// A FRAME CARRIES EVERY CAPPED WINDOW'S REFUSAL, ON THE WINDOW THAT MADE IT.
//
// "Refusing charges" is what the dashboard's attention rows and the budget
// badges key on, and a refused charge increments nothing, so a counter never
// reads as full. The live projection used to drop the stamp on its way to the
// push, which left every one of those surfaces unreachable while the gate was
// turning turns away; and a frame that stated one window per scope showed a
// seat capped by the day and the month as one bar jumping between them.
func TestABudgetFrameCarriesEveryCappedWindowAndItsRefusal(t *testing.T) {
	t.Parallel()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 400, period.Month: 9000}}
	dev := &org.Role{Name: "Dev", TokenBudget: org.TokenCeilings{period.Week: 400}}
	ops := &org.Role{Name: "Ops"}
	c := meteredCompany(config.TokenBudget{Week: ceiling(1000), Month: ceiling(30000)}, lead, dev, ops)
	orgRefused := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	leadRefused := time.Date(2026, 6, 14, 12, 0, 5, 250_000_000, time.FixedZone("CEST", 2*3600))

	report, metered := budgetSnapshot(c, snapshotWindows, []coord.Usage{
		row(coord.OrgScope, map[period.Period]int{period.Day: 120, period.Week: 990, period.Month: 2000},
			orgRefused, period.Week),
		row(scopeOf(t, c, lead), map[period.Period]int{period.Day: 390, period.Week: 700, period.Month: 8950},
			leadRefused, period.Month),
		row(scopeOf(t, c, dev), map[period.Period]int{period.Day: 50, period.Week: 50, period.Month: 50}, time.Time{}),
		// Ops caps nothing, so a stamp on its counter is no meter.
		row(scopeOf(t, c, ops), map[period.Period]int{period.Day: 70}, orgRefused, period.Day),
	})
	if !metered {
		t.Fatal("a company with caps produced no frame")
	}
	if report.Timezone != "UTC" {
		t.Errorf("timezone = %q, want UTC for a company that names no clock", report.Timezone)
	}
	orgWindows := windowsOf(report.Org.Windows)
	if len(report.Org.Windows) != 2 || report.Org.Windows[0].Period != "week" || report.Org.Windows[1].Period != "month" {
		t.Fatalf("org windows = %+v, want the week then the month: the day caps nothing", report.Org.Windows)
	}
	if w := orgWindows["week"]; w.Used != 990 || limitOf(w) != 1000 || w.RefusedAt != "2026-06-14T12:00:00Z" ||
		w.State != types.BudgetRefusing || w.Window != "2026-W24" ||
		w.StartsAt != "2026-06-08T00:00:00Z" || w.ResetsAt != "2026-06-15T00:00:00Z" {
		t.Errorf("org week = %+v, want 990 of 1000, refusing since 12:00Z, W24 from the 8th to the 15th", w)
	}
	if w := orgWindows["month"]; w.Used != 2000 || limitOf(w) != 30000 || w.RefusedAt != "" || w.State != types.BudgetOK {
		t.Errorf("org month = %+v, want 2000 of 30000 and ok", w)
	}

	if len(report.Seats) != 2 {
		t.Fatalf("metered seats = %+v, want Lead and Dev only: Ops has no cap", report.Seats)
	}
	seats := map[string]types.BudgetSeatMeter{}
	for _, m := range report.Seats {
		seats[m.Role] = m
	}
	leadWindows := windowsOf(seats["Lead"].Windows)
	if seats["Lead"].Handle != lead.Handle() {
		t.Errorf("Lead's handle = %q, want %q", seats["Lead"].Handle, lead.Handle())
	}
	// In UTC whatever zone the store handed back, so two nodes' frames
	// for one refusal are the same string.
	if w := leadWindows["month"]; w.Used != 8950 || limitOf(w) != 9000 ||
		w.RefusedAt != "2026-06-14T10:00:05.25Z" || w.State != types.BudgetRefusing {
		t.Errorf("Lead's month = %+v, want 8950 of 9000 refusing since 2026-06-14T10:00:05.25Z", w)
	}
	if w := leadWindows["day"]; w.Used != 390 || limitOf(w) != 400 || w.RefusedAt != "" || w.State != types.BudgetNear {
		t.Errorf("Lead's day = %+v, want 390 of 400, near and not refusing", w)
	}
	if _, listed := leadWindows["week"]; listed {
		t.Errorf("Lead's week is listed, and nothing caps it: %+v", leadWindows["week"])
	}
	if w := windowsOf(seats["Dev"].Windows)["week"]; len(seats["Dev"].Windows) != 1 ||
		w.Used != 50 || limitOf(w) != 400 || w.State != types.BudgetOK {
		t.Errorf("Dev = %+v, want its week alone at 50 of 400", seats["Dev"].Windows)
	}
}

// THE WINDOWS ARE CUT ON THE COMPANY'S CLOCK and stated in UTC: the frame
// names the zone, and a window's span is the company's midnight, not the
// browser's and not UTC's.
func TestABudgetFrameStatesTheCompanysClock(t *testing.T) {
	t.Parallel()
	c := meteredCompany(config.TokenBudget{Day: ceiling(1000)})
	c.Config.Timezone = "Asia/Tokyo"
	tokyo, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatalf("load Asia/Tokyo: %v", err)
	}
	windows := coord.WindowsAt(time.Date(2026, 6, 14, 20, 0, 0, 0, time.UTC), tokyo)
	report, _ := budgetSnapshot(c, windows, nil)
	if report.Timezone != "Asia/Tokyo" {
		t.Errorf("timezone = %q, want Asia/Tokyo", report.Timezone)
	}
	if w := report.Org.Windows; len(w) != 1 || w[0].Window != "2026-06-15" ||
		w[0].StartsAt != "2026-06-14T15:00:00Z" || w[0].ResetsAt != "2026-06-15T15:00:00Z" {
		t.Errorf("org day = %+v, want Tokyo's 15 June, from 15:00Z to 15:00Z", w)
	}
}

// ONE THRESHOLD, THE ENGINE'S. A window is near at nine tenths of its ceiling
// and not a token before; it is refusing when the gate has said so or when it
// has no room for a single token — the predicate the budget park waits on,
// so a parked seat's meter never reads as merely near. A window nothing caps
// is ok whatever its counter says.
func TestTheBudgetStateIsTheEnginesAndItsNearFractionIsNineTenths(t *testing.T) {
	t.Parallel()
	stamped := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		used    int
		refused time.Time
		ceiling int
		capped  bool
		want    types.BudgetState
	}{
		{"89.9% is ok", 899, time.Time{}, 1000, true, types.BudgetOK},
		{"90% is near", 900, time.Time{}, 1000, true, types.BudgetNear},
		{"a token of room is near", 999, time.Time{}, 1000, true, types.BudgetNear},
		{"no room for a token refuses", 1000, time.Time{}, 1000, true, types.BudgetRefusing},
		{"past the ceiling refuses", 1200, time.Time{}, 1000, true, types.BudgetRefusing},
		{"a stamp refuses below the ceiling", 10, stamped, 1000, true, types.BudgetRefusing},
		{"nothing capped is ok", 5000, stamped, 0, false, types.BudgetOK},
	} {
		slot := coord.WindowUsage{Used: tc.used, RefusedAt: tc.refused}
		if got := budgetState(slot, tc.ceiling, tc.capped); got != tc.want {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.want)
		}
		if tc.capped && windowRefuses(slot, tc.ceiling) != (tc.want == types.BudgetRefusing) {
			t.Errorf("%s: the park and the meter disagree about whether it refuses", tc.name)
		}
	}
	if BudgetNearFraction != 0.9 {
		t.Errorf("BudgetNearFraction = %v, want 0.9", BudgetNearFraction)
	}
}

// TWO FRAMES OF AN UNCHANGED COMPANY ARE THE SAME BYTES, whatever order the
// counter listed its scopes in, so a consumer diffing them sees nothing move;
// and a window that is not refusing carries no `refused_at` key at all — the
// dashboard tests the field for presence, and a formatted zero time would list
// every capped seat as refusing charges.
func TestABudgetFrameIsByteStable(t *testing.T) {
	t.Parallel()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 400}}
	dev := &org.Role{Name: "Dev", TokenBudget: org.TokenCeilings{period.Week: 400}}
	c := meteredCompany(config.TokenBudget{Month: ceiling(1000)}, lead, dev)
	rows := []coord.Usage{
		row(coord.OrgScope, map[period.Period]int{period.Month: 10}, time.Time{}),
		row(scopeOf(t, c, lead), map[period.Period]int{period.Day: 20}, time.Time{}),
		row(scopeOf(t, c, dev), map[period.Period]int{period.Week: 30}, time.Time{}),
	}
	first, _ := budgetSnapshot(c, snapshotWindows, rows)
	reversed := slices.Clone(rows)
	slices.Reverse(reversed)
	second, _ := budgetSnapshot(c, snapshotWindows, reversed)
	a, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	b, err := json.Marshal(second)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Errorf("one company's frames differ by the counter's listing order:\n%s\n%s", a, b)
	}
	if bytes.Contains(a, []byte(`"refused_at"`)) {
		t.Errorf("a frame with nothing refusing carries a refusal key: %s", a)
	}
}

// A SCOPE NOTHING HAS CHARGED is metered at zero rather than dropped: Lead
// has no usage row at all, and still has a bar under its ceiling.
func TestAnUnchargedScopeIsMeteredAtZero(t *testing.T) {
	t.Parallel()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 400}}
	c := meteredCompany(config.TokenBudget{Month: ceiling(1000)}, lead)
	report, metered := budgetSnapshot(c, snapshotWindows, []coord.Usage{
		row(coord.OrgScope, map[period.Period]int{period.Month: 10}, time.Time{}),
	})
	if !metered {
		t.Fatal("a company with caps produced no frame")
	}
	if len(report.Seats) != 1 || len(report.Seats[0].Windows) != 1 {
		t.Fatalf("seats = %+v, want Lead's day alone", report.Seats)
	}
	if w := report.Seats[0].Windows[0]; w.Used != 0 || limitOf(w) != 400 || w.RefusedAt != "" || w.State != types.BudgetOK {
		t.Errorf("Lead's day = %+v, want zero of its 400, ok and not refusing", w)
	}
}

// A company whose seats cap nothing still frames the org with an EMPTY list
// of windows when it caps nothing either — which is no frame at all — while
// a seat-only company frames the org as `[]`, never null.
func TestASeatOnlyCompanyFramesTheOrgAsNoWindows(t *testing.T) {
	t.Parallel()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 400}}
	c := meteredCompany(config.TokenBudget{}, lead)
	report, metered := budgetSnapshot(c, snapshotWindows, nil)
	if !metered {
		t.Fatal("a capped seat produced no frame")
	}
	raw, err := json.Marshal(report.Org)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"windows":[]}` {
		t.Errorf("org = %s, want {\"windows\":[]}", raw)
	}
}

// NOTHING CAPPED IS NO FRAME: a header bar over an unlimited budget is a
// claim nobody measured. A human seat is never metered, whatever it declares.
func TestAnUncappedCompanyPublishesNoFrame(t *testing.T) {
	t.Parallel()
	c := meteredCompany(config.TokenBudget{}, &org.Role{Name: "Lead"},
		&org.Role{Name: "Founder", Kind: org.KindHuman, TokenBudget: org.TokenCeilings{period.Day: 500}})
	if report, metered := budgetSnapshot(c, snapshotWindows, []coord.Usage{
		row(coord.OrgScope, map[period.Period]int{period.Day: 10}, time.Time{}),
	}); metered {
		t.Errorf("an uncapped company produced a frame: %+v", report)
	}
}
