package digitalocean

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// checkWith runs Check for one token against the fake.
func checkWith(t *testing.T, f *fakeDO, token, level, teamUUID string, heldBy func(string) string) (Checked, error) {
	t.Helper()
	c, err := NewClient(ClientOptions{Token: token, HTTP: f.server(t)})
	if err != nil {
		t.Fatal(err)
	}
	return Check(t.Context(), c, CheckInput{Token: token, Team: "Example Team", TeamUUID: teamUUID,
		Level: level, HeldBy: heldBy})
}

// A TOKEN HOLDING EXACTLY ITS LEVEL'S SCOPES PASSES, for every level, and the
// check sends nothing but to DigitalOcean.
func TestATokenHoldingItsLevelPasses(t *testing.T) {
	t.Parallel()
	for _, level := range Levels() {
		f := newFakeDO()
		token := fakeToken('a')
		f.add(token, scopesOf(level))
		checked, err := checkWith(t, f, token, level, "", nil)
		if err != nil {
			t.Fatalf("%s: %v", level, err)
		}
		if checked.Level != level || checked.Account.Team.Name != "Example Team" {
			t.Errorf("%s: checked %+v", level, checked)
		}
		f.onlyDigitalOcean(t)
	}
}

// EACH REFUSAL NAMES WHAT TO DO, in the order the contract gives, and never
// carries the token.
func TestAPasteIsRefusedWithItsRemedy(t *testing.T) {
	t.Parallel()
	read, operate, administer := config.DigitalOceanLevelRead, config.DigitalOceanLevelOperate,
		config.DigitalOceanLevelAdminister
	for _, tc := range []struct {
		name    string
		token   string
		level   string
		scopes  []string
		mutate  func(*profile)
		pin     string
		heldBy  func(string) string
		said    string
		remedy  string
		noToken bool
	}{
		{name: "an OAuth token", token: "doo_v1_" + strings.Repeat("b", 64), level: read,
			said: "This is an OAuth token", remedy: "personal access token", noToken: true},
		{name: "not a token", token: "hunter2", level: read,
			said: "not a DigitalOcean personal access token", remedy: "dop_v1_", noToken: true},
		{name: "another seat's token", token: fakeToken('c'), level: read, scopes: scopesOf(read),
			heldBy: func(string) string { return "qa" }, said: "already qa's", remedy: "its own token"},
		{name: "a refused token", token: fakeToken('d'), level: read, scopes: scopesOf(read),
			mutate: func(p *profile) { p.revoked = true }, said: "did not accept", remedy: "Create a new token"},
		{name: "no account:read", token: fakeToken('e'), level: read, scopes: Grants(read)[1:],
			said: "lacks account:read", remedy: "account:read ticked"},
		{name: "a locked account", token: fakeToken('f'), level: read, scopes: scopesOf(read),
			mutate: func(p *profile) { p.status = "locked" }, said: "locked", remedy: "Resolve it"},
		{name: "another team", token: fakeToken('g'), level: read, scopes: scopesOf(read),
			mutate: func(p *profile) { p.team = "Other" }, said: "made in team Other, and Crewlet is connected to team Example Team",
			remedy: "Switch to Example Team"},
		{name: "a same-named team", token: fakeToken('h'), level: read, scopes: scopesOf(read),
			pin: "99999999-2222-4333-a444-555555555555", said: "other agents' tokens are in Team ID", remedy: "Team ID"},
		{name: "billing", token: fakeToken('i'), level: operate, scopes: scopesOf(operate, "billing:read"),
			said: "Full Access or Read Only", remedy: "billing:read unticked"},
		{name: "a stored secret", token: fakeToken('j'), level: administer,
			scopes: scopesOf(administer, "security:read"), said: "(security:read). Agents never hold it",
			remedy: "security:read unticked"},
		{name: "a missing read", token: fakeToken('k'), level: read,
			scopes: removing(scopesOf(read), "droplet:read"), said: "lacks droplet:read, which Read needs",
			remedy: "every scope the list names"},
		{name: "a write on Read", token: fakeToken('l'), level: read, scopes: scopesOf(read, "tag:create"),
			said: "(tag:create), and you chose Read", remedy: "choose Operate here"},
		{name: "a write missing on Operate", token: fakeToken('m'), level: operate,
			scopes: removing(scopesOf(operate), "droplet:delete"), said: "lacks droplet:delete, which Operate needs",
			remedy: "every scope the list names"},
		{name: "an Administer scope on Operate", token: fakeToken('n'), level: operate,
			scopes: scopesOf(operate, "kubernetes:access_cluster"), said: "which only Administer grants",
			remedy: "choose Administer here"},
		{name: "an Administer scope missing", token: fakeToken('o'), level: administer,
			scopes: removing(scopesOf(administer), "app:access_console"),
			said:   "lacks app:access_console, which Administer needs", remedy: "every scope the list names"},
		{name: "a level not offered", token: fakeToken('p'), level: "full_access", scopes: scopesOf(administer),
			said: "not a DigitalOcean level", remedy: "Choose a level"},
	} {
		f := newFakeDO()
		if tc.scopes != nil {
			p := f.add(tc.token, tc.scopes)
			if tc.mutate != nil {
				tc.mutate(p)
			}
		}
		_, err := checkWith(t, f, tc.token, tc.level, tc.pin, tc.heldBy)
		r, ok := IsRefusal(err)
		if !ok {
			t.Errorf("%s: %v, want a refusal", tc.name, err)
			continue
		}
		if !strings.Contains(r.Detail, tc.said) || !strings.Contains(r.Hint, tc.remedy) {
			t.Errorf("%s: said %q (hint %q), want %q and %q", tc.name, r.Detail, r.Hint, tc.said, tc.remedy)
		}
		if strings.Contains(r.Detail+r.Hint+err.Error(), tc.token) && len(tc.token) > 8 {
			t.Errorf("%s: the refusal carries the token", tc.name)
		}
		if tc.noToken && len(f.requests) != 0 {
			t.Errorf("%s: DigitalOcean was asked %v about a value that is no token", tc.name, f.requests)
		}
	}
}

// A WRITE CHECK THAT SUCCEEDS IS A FAULT, NOT A PASS: no write check can
// succeed, so a 2xx means DigitalOcean did something Crewlet never meant.
func TestAWriteCheckThatSucceedsIsAFault(t *testing.T) {
	t.Parallel()
	f := newFakeDO()
	token := fakeToken('q')
	f.add(token, scopesOf(config.DigitalOceanLevelOperate))
	f.answer["tag:create"] = 201
	_, err := checkWith(t, f, token, config.DigitalOceanLevelOperate, "", nil)
	if _, refused := IsRefusal(err); err == nil || refused || !strings.Contains(err.Error(), "cannot succeed") {
		t.Errorf("a write check answering 201 = %v, want a fault naming it", err)
	}
}

// removing is scopes without drop.
func removing(scopes []string, drop string) []string {
	var out []string
	for _, s := range scopes {
		if s != drop {
			out = append(out, s)
		}
	}
	return out
}
