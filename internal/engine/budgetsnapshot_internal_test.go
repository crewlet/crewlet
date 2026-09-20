package engine

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/org"
)

// meteredCompany is an epoch with an org cap and the given seats, which is all
// a budget frame reads.
func meteredCompany(orgCap int, seats ...*org.Role) *Company {
	return &Company{
		Config: &config.Company{TokenBudget: orgCap},
		Org:    &org.Organization{Name: "Acme", Roles: seats},
	}
}

func scopeOf(t *testing.T, c *Company, seat *org.Role) string {
	t.Helper()
	id, ok := c.Org.AgentIDFor(seat)
	if !ok {
		t.Fatalf("%s has no agent id", seat.Name)
	}
	return coord.AgentScope(id.String())
}

// A FRAME CARRIES THE REFUSAL OF THE SCOPE THAT MADE IT.
//
// "Refusing charges" is what the dashboard's attention rows and the budget
// badges key on, and a refused charge increments nothing, so a counter never
// reads as full. A frame that dropped the stamp left every one of those
// surfaces unreachable while the gate was turning turns away.
func TestABudgetFrameCarriesEachScopesRefusal(t *testing.T) {
	t.Parallel()
	lead := &org.Role{Name: "Lead", TokenBudget: 400}
	dev := &org.Role{Name: "Dev", TokenBudget: 400}
	ops := &org.Role{Name: "Ops"}
	c := meteredCompany(1000, lead, dev, ops)
	orgRefused := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)
	leadRefused := time.Date(2026, 6, 14, 12, 0, 5, 250_000_000, time.FixedZone("CEST", 2*3600))

	report, metered := budgetSnapshot(c, []coord.Usage{
		{Scope: coord.OrgScope, Used: 990, RefusedAt: orgRefused},
		{Scope: scopeOf(t, c, lead), Used: 399, RefusedAt: leadRefused},
		{Scope: scopeOf(t, c, dev), Used: 50},
		{Scope: scopeOf(t, c, ops), Used: 70, RefusedAt: orgRefused},
	})
	if !metered {
		t.Fatal("a company with caps produced no frame")
	}
	if report.OrgUsedTokens != 990 || report.OrgMaxTokens != 1000 {
		t.Errorf("org meter = %d of %d, want 990 of 1000", report.OrgUsedTokens, report.OrgMaxTokens)
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
	if m := got["Lead"]; m.used != 399 || m.max != 400 || m.refused != "2026-06-14T10:00:05.25Z" {
		t.Errorf("Lead meter = %+v, want 399 of 400 refused at 2026-06-14T10:00:05.25Z", m)
	}
	if m := got["Dev"]; m.used != 50 || m.refused != "" {
		t.Errorf("Dev meter = %+v, want 50 used and not refusing", m)
	}
}

// A SCOPE THAT IS NOT REFUSING CARRIES AN EMPTY STAMP, never the zero instant
// spelled out: the dashboard tests the field for presence, and a formatted
// zero time would list every capped seat as refusing charges.
func TestAScopeNotRefusingCarriesNoStamp(t *testing.T) {
	t.Parallel()
	lead := &org.Role{Name: "Lead", TokenBudget: 400}
	c := meteredCompany(1000, lead)
	// Lead has never been charged at all, so it has no usage row.
	report, metered := budgetSnapshot(c, []coord.Usage{{Scope: coord.OrgScope, Used: 10}})
	if !metered {
		t.Fatal("a company with caps produced no frame")
	}
	if report.OrgRefusedAt != "" {
		t.Errorf("org refused_at = %q, want empty", report.OrgRefusedAt)
	}
	if len(report.Agents) != 1 || report.Agents[0].UsedTokens != 0 || report.Agents[0].RefusedAt != "" {
		t.Errorf("agents = %+v, want Lead at zero and not refusing", report.Agents)
	}
}

// NOTHING CAPPED IS NO FRAME: a header bar over an unlimited budget is a
// claim nobody measured. A human seat is never metered, whatever it declares.
func TestAnUncappedCompanyPublishesNoFrame(t *testing.T) {
	t.Parallel()
	c := meteredCompany(0, &org.Role{Name: "Lead"}, &org.Role{Name: "Founder", Kind: org.KindHuman, TokenBudget: 500})
	if report, metered := budgetSnapshot(c, []coord.Usage{{Scope: coord.OrgScope, Used: 10}}); metered {
		t.Errorf("an uncapped company produced a frame: %+v", report)
	}
}
