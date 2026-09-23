package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
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

// A FRAME CARRIES THE REFUSAL OF THE SCOPE THAT MADE IT, IN THE WINDOW THAT
// MADE IT.
//
// "Refusing charges" is what the dashboard's attention rows and the budget
// badges key on, and a refused charge increments nothing, so a counter never
// reads as full. A frame that dropped the stamp left every one of those
// surfaces unreachable while the gate was turning turns away — and a frame
// that paired a refusing window's stamp with another window's figures would
// show a seat at a third of its cap and refusing.
func TestABudgetFrameCarriesEachScopesRefusal(t *testing.T) {
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
		// The MONTH refused Lead, and a coding run collected since took
		// its DAY closer to its ceiling than the month: the refusing
		// window is still the one the frame names, since it is the one
		// the seat is waiting on.
		row(scopeOf(t, c, lead), map[period.Period]int{period.Day: 390, period.Week: 700, period.Month: 8950},
			leadRefused, period.Month),
		row(scopeOf(t, c, dev), map[period.Period]int{period.Day: 50, period.Week: 50, period.Month: 50}, time.Time{}),
		row(scopeOf(t, c, ops), map[period.Period]int{period.Day: 70}, orgRefused, period.Day),
	})
	if !metered {
		t.Fatal("a company with caps produced no frame")
	}
	if report.OrgUsedTokens != 990 || report.OrgMaxTokens != 1000 {
		t.Errorf("org meter = %d of %d, want the refusing week's 990 of 1000",
			report.OrgUsedTokens, report.OrgMaxTokens)
	}
	if want := "2026-06-14T12:00:00Z"; report.OrgRefusedAt != want {
		t.Errorf("org refused_at = %q, want %q", report.OrgRefusedAt, want)
	}
	got := map[string]struct {
		used, max int
		refused   string
	}{}
	for _, m := range report.Agents {
		got[m.Role] = struct {
			used, max int
			refused   string
		}{m.UsedTokens, m.MaxTokens, m.RefusedAt}
	}
	if len(got) != 2 {
		t.Fatalf("metered seats = %v, want Lead and Dev only: Ops has no cap", got)
	}
	// In UTC whatever zone the store handed back, so two nodes' frames
	// for one refusal are the same string.
	if m := got["Lead"]; m.used != 8950 || m.max != 9000 || m.refused != "2026-06-14T10:00:05.25Z" {
		t.Errorf("Lead meter = %+v, want the refusing month's 8950 of 9000 refused at "+
			"2026-06-14T10:00:05.25Z", m)
	}
	if m := got["Dev"]; m.used != 50 || m.max != 400 || m.refused != "" {
		t.Errorf("Dev meter = %+v, want its week's 50 of 400 and not refusing", m)
	}
}

// WITH NOTHING REFUSING, THE FRAME NAMES THE WINDOW WITH THE LEAST ROOM LEFT —
// the one the next refusal will come from — rather than the smallest ceiling.
func TestABudgetFrameNamesTheWindowClosestToItsCeiling(t *testing.T) {
	t.Parallel()
	c := meteredCompany(config.TokenBudget{Day: ceiling(3000), Month: ceiling(40000)})
	report, _ := budgetSnapshot(c, snapshotWindows, []coord.Usage{
		row(coord.OrgScope, map[period.Period]int{period.Day: 100, period.Month: 39500}, time.Time{}),
	})
	if report.OrgUsedTokens != 39500 || report.OrgMaxTokens != 40000 {
		t.Errorf("org meter = %d of %d, want the month's 39500 of 40000: the day's "+
			"smaller ceiling has far more room", report.OrgUsedTokens, report.OrgMaxTokens)
	}
}

// A SCOPE THAT IS NOT REFUSING CARRIES AN EMPTY STAMP, never the zero instant
// spelled out: the dashboard tests the field for presence, and a formatted
// zero time would list every capped seat as refusing charges.
func TestAScopeNotRefusingCarriesNoStamp(t *testing.T) {
	t.Parallel()
	lead := &org.Role{Name: "Lead", TokenBudget: org.TokenCeilings{period.Day: 400}}
	c := meteredCompany(config.TokenBudget{Month: ceiling(1000)}, lead)
	// Lead has never been charged at all, so it has no usage row.
	report, metered := budgetSnapshot(c, snapshotWindows, []coord.Usage{
		row(coord.OrgScope, map[period.Period]int{period.Month: 10}, time.Time{}),
	})
	if !metered {
		t.Fatal("a company with caps produced no frame")
	}
	if report.OrgRefusedAt != "" {
		t.Errorf("org refused_at = %q, want empty", report.OrgRefusedAt)
	}
	if len(report.Agents) != 1 || report.Agents[0].UsedTokens != 0 ||
		report.Agents[0].MaxTokens != 400 || report.Agents[0].RefusedAt != "" {
		t.Errorf("agents = %+v, want Lead at zero of its 400 and not refusing", report.Agents)
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
