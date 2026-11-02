package accessmigrate_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/accessmigrate"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// run migrates root with vs, vault and ledger, at a fixed instant.
func run(t *testing.T, root map[string]any, vs *vendors, vault *fakeSecrets,
	ledger *fakeLedger) accessmigrate.Result {

	t.Helper()
	doc := &fakeDocument{root: root, revision: "rev-1", sealed: vault, ledger: ledger}
	result, err := accessmigrate.Run(t.Context(), accessmigrate.Deps{
		Document: doc, Secrets: vault, Records: &fakeRecords{ledger: ledger},
		Open: vs.open, Env: config.NewResolver(),
		Now: func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return result
}

// AN ACCOUNT KEEPS THE ROLE IT HOLDS AT THE VENDOR (D14), and only a seat with
// no account yet takes the company role.
//
// The SRE's Datadog account holds the Standard Role while the company role is
// Admin, and QA's GitLab account holds Reporter while the company's override
// says Maintainer: both keep what they hold. The backend engineer, whose
// GitLab account the engine has not made, takes the company default. An
// account holding several Datadog roles is not guessed at: Datadog waits,
// naming the agent. Mutation: write the company role over an account, or keep
// one of several roles, and a case here fails.
func TestAnAccountKeepsTheRoleItHolds(t *testing.T) {
	t.Parallel()
	vs := gophraseVendors()
	vs.accounts[integration.KindDatadog]["sre"] = map[seataccess.Lane]seataccess.Observed{
		seataccess.LaneDatadog: {Exists: true, Enabled: true, AccountID: "dd-user-sre",
			Level: "role-std"}}
	vs.accounts[integration.KindGitLab]["qa"] = map[seataccess.Lane]seataccess.Observed{
		seataccess.LaneGitLab: {Exists: true, Enabled: true, AccountID: "42", Level: "20"}}
	delete(vs.accounts[integration.KindGitLab], "backend-engineer")
	m := plan(t, gophrase(t), vs)
	for _, tc := range []struct {
		handle string
		lane   seataccess.Lane
		want   string
	}{
		{"sre", seataccess.LaneDatadog, "role-std"},
		{"qa", seataccess.LaneGitLab, "20"},
		{"backend-engineer", seataccess.LaneGitLab, "30"},
	} {
		if got := m.Seats[tc.handle][tc.lane]; !got.Enabled || got.Level != tc.want {
			t.Errorf("%s on %s = %+v, want on at %s", tc.handle, tc.lane, got, tc.want)
		}
	}

	several := gophraseVendors()
	several.accounts[integration.KindDatadog]["sre"] = map[seataccess.Lane]seataccess.Observed{
		seataccess.LaneDatadog: {Exists: true, Enabled: true, AccountID: "dd-user-sre",
			Level: "role-std role-admin"}}
	m = plan(t, gophrase(t), several)
	why := m.Pending[integration.KindDatadog]
	if why == nil || !errors.Is(why, seataccess.ErrSeveralRoles) || !strings.Contains(why.Error(), "SRE") ||
		slices.Contains(m.Migrated, integration.KindDatadog) {
		t.Errorf("several roles: migrated %v, pending %v; want Datadog waiting, naming the SRE",
			m.Migrated, m.Pending)
	}
}

// ONLY A SEAT THE OLD PLAN INCLUDED IS SEEDED.
//
// GitLab shows an account by the engine's naming for the designer, whose seat
// never named a GitLab token: the old engine did not make it, so the ledger is
// not told it did. Mutation: seed every account found by naming, and the
// designer is recorded as the engine's.
func TestOnlyASeatInTheOldPlanIsSeeded(t *testing.T) {
	t.Parallel()
	vs := gophraseVendors()
	vs.accounts[integration.KindGitLab]["designer"] = map[seataccess.Lane]seataccess.Observed{
		seataccess.LaneGitLab: {Exists: true, Enabled: true, AccountID: "43",
			Account: "crewlet-designer", Level: "30"}}
	m := plan(t, gophrase(t), vs)
	var seeded []string
	for _, s := range m.Seeds {
		if s.Kind == integration.KindGitLab {
			seeded = append(seeded, s.Handle)
		}
	}
	slices.Sort(seeded)
	if !slices.Equal(seeded, []string{"backend-engineer", "qa"}) {
		t.Errorf("GitLab seeds = %v, want the backend engineer and QA alone", seeded)
	}
}

// A UNIT'S SHARED KEY WAS ITS AGENTS' OWN TO THE OLD ENGINE, and is migrated.
//
// The passes read a seat's `mcp_env` laid over its unit's, so a Datadog key the
// unit shares put every agent of the unit on Datadog. Each is on; the shared
// variable moves nowhere and is recorded for every one of them (D12); and the
// unit's key is dropped, since the engine binds each agent's own credential
// to the Datadog server itself. Mutation: read the seat's own env alone, and
// the agents come out off with the unit still feeding them the shared key.
func TestAUnitsSharedKeyIsMigratedForEveryAgent(t *testing.T) {
	t.Parallel()
	root := gophrase(t)
	unit := root["units"].([]any)[0].(map[string]any)
	unit["mcp_env"] = map[string]any{"datadog": map[string]any{
		"DD_APP_KEY": "${DATADOG_TEAM_KEY}"}}
	delete(seat(t, root, "sre"), "mcp_env")
	m := plan(t, root, gophraseVendors())
	for _, handle := range []string{"sre", "designer", "pm"} {
		if got := m.Seats[handle][seataccess.LaneDatadog]; !got.Enabled {
			t.Errorf("%s on Datadog = %+v, want on through its unit's key", handle, got)
		}
	}
	if slices.ContainsFunc(m.Moves, func(mv accessmigrate.Move) bool {
		return mv.From == "DATADOG_TEAM_KEY"
	}) {
		t.Error("the unit's shared key was moved into one agent's own variable")
	}
	i := slices.IndexFunc(m.Notices, func(n accessmigrate.Notice) bool {
		return n.Variable == "DATADOG_TEAM_KEY"
	})
	if i < 0 || !slices.Contains(m.Notices[i].Seats, "sre") ||
		!slices.Contains(m.Notices[i].Seats, "designer") {
		t.Fatalf("notices = %+v, want the unit's key recorded for its agents", m.Notices)
	}
	if _, err := accessmigrate.Apply(root, m); err != nil {
		t.Fatal(err)
	}
	if doc := mustJSON(t, root); strings.Contains(doc, "DATADOG_TEAM_KEY") {
		t.Errorf("the migrated document still names the unit's shared key: %s", doc)
	}
}

// A MIGRATION NEVER SEALS OVER A CREDENTIAL A SEAT ALREADY HOLDS, and records
// the one it finds under the engine's name.
//
// Three seats, three cases: the backend engineer's ledger already records a
// credential of its own (a migration reverted after its sync minted one), so
// its old token is not sealed over it; QA's token already sits under the
// engine's name with a value, so it moves nothing and is recorded as QA's
// own; and the product manager's GitHub token, which the engine issues at
// use, is never sealed or recorded at all. Mutation: seal unconditionally, or
// drop the record of a credential already in place, and a case here fails.
func TestAMigrationNeverSealsOverAHeldCredential(t *testing.T) {
	t.Parallel()
	root := gophrase(t)
	qa := seat(t, root, "qa")
	qa["mcp_env"].(map[string]any)["gitlab"] = map[string]any{
		"Authorization": "Bearer ${GITLAB_TOKEN_QA}"}
	vault := &fakeSecrets{values: map[string]string{
		"BACKEND_GITLAB_PAT": "fake-old-token", "GITLAB_TOKEN_QA": "fake-qa-token",
		"GITHUB_APP_TOKEN_PM": "fake-hourly-token",
	}}
	ledger := &fakeLedger{records: map[integration.Kind]map[string]seataccess.Record{
		integration.KindGitLab: {"backend-engineer": {Kind: integration.KindGitLab,
			Handle: "backend-engineer", Credential: &seataccess.Credential{
				Variable: "GITLAB_TOKEN_BACKEND_ENGINEER", Source: seataccess.CredentialMinted}}},
	}}
	result := run(t, root, gophraseVendors(), vault, ledger)
	if _, sealed := vault.sealed["GITLAB_TOKEN_BACKEND_ENGINEER"]; sealed || result.Moved != 0 {
		t.Errorf("the old token was sealed over the backend engineer's own (moved %d)",
			result.Moved)
	}
	if cred := ledger.records[integration.KindGitLab]["qa"].Credential; cred == nil ||
		cred.Variable != "GITLAB_TOKEN_QA" || cred.Source != seataccess.CredentialMinted {
		t.Errorf("QA's credential under the engine's name is recorded as %+v", cred)
	}
	for name := range vault.sealed {
		if strings.HasPrefix(name, "GITHUB_APP_TOKEN") {
			t.Errorf("GitHub's issued token was sealed as %s", name)
		}
	}
	if cred := ledger.records[integration.KindGitHub]["pm"].Credential; cred != nil &&
		strings.HasPrefix(cred.Variable, "GITHUB_APP_TOKEN") {
		t.Errorf("GitHub's issued token was recorded as the PM's credential: %+v", cred)
	}
}
