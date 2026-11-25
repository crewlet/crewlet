package appstoreconnect_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// reachCompany is a company connecting team main, which holds no key, whose
// one agent seat is on at operate with seatApps.
func reachCompany(seatApps ...string) *config.Company {
	on := true
	return &config.Company{
		Integrations: config.Integrations{AppStoreConnect: &config.AppStoreConnect{Enabled: true,
			Connections: map[string]config.AppStoreConnectTeam{"main": {
				IssuerID: "57246542-96fe-1a63-e053-0824d011072a"}}}},
		Roles: []config.Role{{Name: "Growth", Handle: "growth", Integrations: config.RoleIntegrations{
			AppStoreConnect: &config.RoleAppStoreConnect{Enabled: &on, Level: "operate", Apps: seatApps}}}},
	}
}

// syncOutside is what a real sync of company says of the seat's apps: the
// finding naming an app outside the connection's reach, or "".
func syncOutside(t *testing.T, company *config.Company) string {
	t.Helper()
	v, _ := seataccess.Lookup(integration.KindAppStoreConnect)
	roster, err := company.Organization()
	if err != nil {
		t.Fatal(err)
	}
	report, err := seataccess.Sync(t.Context(), seataccess.SyncInput{Vendor: v,
		Open:   seataccess.OpenInput{Config: company, Org: roster},
		Ledger: seataccesstest.NewLedger(), Sink: seataccesstest.NewSink(), Pass: "pass-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range report.Findings {
		if strings.Contains(f.Detail, "outside what") {
			return f.Detail
		}
	}
	lane := report.Records["growth"].Lanes[seataccess.LaneAppStoreConnect]
	if lane.Sync == nil || lane.Sync.Outcome != seataccess.OutcomeOK {
		t.Fatalf("the seat's sync ended %+v, findings %+v", lane.Sync, report.Findings)
	}
	return ""
}

// AN AGENT MAY NAME ANY APP OF THE CONNECTED TEAMS: the company keeps no list
// of its own, so turning an agent on with any app converges, as `crewlet
// validate` and the access routes already allow, before any key listed them.
//
// Mutation: give the vendor's Reach a scope, and the sync refuses an agent's
// app nobody listed yet.
func TestAnAgentMayNameAnyAppOfTheConnectedTeams(t *testing.T) {
	t.Parallel()
	if said := syncOutside(t, reachCompany("6763222842")); said != "" {
		t.Fatalf("an agent's app was refused: %s", said)
	}
}

// AN AGENT WITH NO KEY OF ITS OWN HOLDS NONE: its row asks for Set up
// identity, offers no company identity to fall back to, and never says it
// signs as its own while every call of its is refused.
//
// Mutation: answer a keyless own-only lane as holding its own, and the tab
// says the agent signs as itself while it can make no call.
func TestAnAgentWithNoKeyHoldsNone(t *testing.T) {
	t.Parallel()
	v, _ := seataccess.Lookup(integration.KindAppStoreConnect)
	company := reachCompany("6763222842")
	want := seataccess.Desired{Enabled: true, Level: "read", Scope: []string{"6763222842"}}
	id := seataccess.IdentityOf(v, company, seataccess.SeatRef{Handle: "growth", Name: "Growth"},
		seataccess.LaneAppStoreConnect, want, seataccess.Observed{}, nil, time.Now())
	if id == nil || id.Holds != seataccess.HoldsNone || !id.SetUp || id.UseCompany {
		t.Fatalf("a keyless agent's identity answers %+v", id)
	}
}
