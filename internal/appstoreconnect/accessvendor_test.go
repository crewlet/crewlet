package appstoreconnect_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// reachCompany is a company connecting one team with keys, naming apps (none
// for nil), whose one agent seat is on at operate with seatApps.
func reachCompany(keys config.AppStoreConnectKeys, apps []string, seatApps ...string) *config.Company {
	on := true
	return &config.Company{
		Integrations: config.Integrations{AppStoreConnect: &config.AppStoreConnect{Enabled: true, Apps: apps,
			Connections: map[string]config.AppStoreConnectTeam{"main": {
				IssuerID: "57246542-96fe-1a63-e053-0824d011072a", Keys: keys}}}},
		Roles: []config.Role{{Name: "Growth", Handle: "growth", Integrations: config.RoleIntegrations{
			AppStoreConnect: &config.RoleAppStoreConnect{Enabled: &on, Level: "operate", Apps: seatApps}}}},
	}
}

// adminOnly is the keys of a team that connected one Admin key.
func adminOnly() config.AppStoreConnectKeys {
	return config.AppStoreConnectKeys{Admin: &config.AppStoreConnectKey{KeyID: "A1B2C3D4E5", PrivateKey: "${K}"}}
}

// marketing is the keys of a team that connected a Marketing key.
func marketing() config.AppStoreConnectKeys {
	return config.AppStoreConnectKeys{Marketing: &config.AppStoreConnectKey{KeyID: "2X9R4HXF34", PrivateKey: "${K}"}}
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

// A COMPANY THAT NAMES NO APPS REACHES EVERY APP ITS KEYS REACH, so turning an
// agent on with any app converges, as `crewlet validate` and the access routes
// already allow; a company that names apps limits its agents to them.
//
// Mutation: drop the document's reach from seataccess.ReadOptions, and the
// sync refuses every app of a company that names none.
func TestAnAgentMayNameAnyAppWhereTheCompanyNamesNone(t *testing.T) {
	t.Parallel()
	if said := syncOutside(t, reachCompany(adminOnly(), nil, "6763222842")); said != "" {
		t.Fatalf("a company naming no apps refused the agent's app: %s", said)
	}
	said := syncOutside(t, reachCompany(adminOnly(), []string{"111"}, "6763222842"))
	if !strings.Contains(said, "6763222842") {
		t.Fatalf("an app outside the company's list was synced: %q", said)
	}
	if said := syncOutside(t, reachCompany(adminOnly(), []string{"111"}, "111")); said != "" {
		t.Fatalf("an app on the company's list was refused: %s", said)
	}
}
