package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	_ "github.com/crewlet/crewlet/internal/adsense"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/growth"
)

// adSenseClient is a complete OAuth client block.
const adSenseClient = `    oauth:
      client_id: "123456789012-abc123def456.apps.googleusercontent.com"
      client_secret: ${GOOGLE_OAUTH_CLIENT_SECRET}
`

// adSenseDoc is a company with AdSense at company, and the seat `finance` written
// as seat.
func adSenseDoc(company, seat string) string {
	return `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    fast:
      model: claude-golden
integrations:
  adsense:
` + company + `
roles:
  - name: Finance
    handle: finance
    llm: fast
` + seat
}

// AN ADSENSE BLOCK SIGNS IN AS THE COMPANY'S OWN CLIENT, NEEDING NO GOOGLE
// CLOUD: an enabled block with its OAuth client validates alone, a seat names
// its level and accounts, and its level reaches the role the chart decides
// growth tools by: at administer, every grant.
func TestAnAdSenseBlockAndSeatValidate(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(adSenseDoc("    enabled: true\n"+adSenseClient+
		"    accounts: [\"pub-1234567890123456\"]",
		"    integrations:\n      adsense: {enabled: true, level: administer, accounts: [\"pub-1234567890123456\"]}\n")))
	if err != nil {
		t.Fatalf("a complete AdSense company was refused: %v", err)
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	seat := o.AgentSeatByHandle("finance")
	if seat == nil || seat.GrowthLevels["adsense"] != "administer" {
		t.Fatalf("the seat's growth levels are %v, want adsense at administer", seat)
	}
	for _, l := range growth.Levels() {
		if !slices.Contains(orgchart.GrantedSeats(o, authz.GrowthSeatGrant("adsense", l)), "finance") {
			t.Errorf("the seat does not carry adsense_%s", l)
		}
	}
}

// A BLOCK TURNED OFF NEEDS NOTHING: only an enabled block signs in, so one
// written off, with no client yet, validates.
//
// Mutation: require oauth whatever enabled says, and this test goes red.
func TestAnAdSenseBlockTurnedOffNeedsNothing(t *testing.T) {
	t.Parallel()
	if _, err := config.ParseCompany([]byte(adSenseDoc("    enabled: false", ""))); err != nil {
		t.Errorf("an AdSense block turned off was refused: %v", err)
	}
}

// AN ENABLED BLOCK NEEDS ITS CLIENT, and every account is a publisher ID once,
// a seat's within the company's narrowing; each refusal names the field.
func TestAdSenseRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, company, seat, path string
	}{
		{"no OAuth client", "    enabled: true", "", "integrations.adsense.oauth"},
		{"a client ID of another shape", "    enabled: true\n    oauth:\n      client_id: \"abc\"\n" +
			"      client_secret: ${S}", "", "integrations.adsense.oauth.client_id"},
		{"no client secret", "    enabled: true\n    oauth:\n      client_id: " +
			"\"1-a.apps.googleusercontent.com\"", "", "integrations.adsense.oauth.client_secret"},
		{"an account that is no publisher ID", "    enabled: true\n" + adSenseClient + "    accounts: [\"ca-pub-1\"]",
			"", "integrations.adsense.accounts[0]"},
		{"an account listed twice", "    enabled: true\n" + adSenseClient +
			"    accounts: [\"pub-1234567890123456\", \"pub-1234567890123456\"]", "", "integrations.adsense.accounts[1]"},
		{"a seat's account outside the company's", "    enabled: true\n" + adSenseClient +
			"    accounts: [\"pub-1234567890123456\"]",
			"    integrations:\n      adsense: {enabled: true, accounts: [\"pub-6543210987654321\"]}\n",
			"roles[0].integrations.adsense.accounts[0]"},
		{"a seat's account that is no publisher ID", "    enabled: true\n" + adSenseClient,
			"    integrations:\n      adsense: {enabled: true, accounts: [\"ca-pub-1234567890123456\"]}\n",
			"roles[0].integrations.adsense.accounts[0]"},
		{"an unknown level", "    enabled: true\n" + adSenseClient,
			"    integrations:\n      adsense: {enabled: true, level: owner, accounts: [\"pub-1234567890123456\"]}\n",
			"roles[0].integrations.adsense.level"},
		{"a seat with no account", "    enabled: true\n" + adSenseClient,
			"    integrations:\n      adsense: {enabled: true}\n", "roles[0].integrations.adsense.accounts"},
		{"a seat on where the company is off", "    enabled: false",
			"    integrations:\n      adsense: {enabled: true, accounts: [\"pub-1234567890123456\"]}\n",
			"roles[0].integrations.adsense.enabled"},
	} {
		_, err := config.ParseCompany([]byte(adSenseDoc(tc.company, tc.seat)))
		if err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: refused with %v, want the field %s named", tc.name, err, tc.path)
		}
	}
}

// THE CLIENT'S SECRET IS ADSENSE'S CREDENTIAL ALONE: named at a box's
// environment it would reach Google around the engine's tools, so the
// document is refused there.
//
// Mutation: drop the client secret's secret tag, and the second document
// validates.
func TestTheAdSenseClientSecretIsNamedAtItsOwnFieldAlone(t *testing.T) {
	t.Parallel()
	doc := adSenseDoc("    enabled: true\n"+adSenseClient,
		"    sandbox:\n      env:\n        LEAK: ${GOOGLE_OAUTH_CLIENT_SECRET}\n")
	_, err := config.ParseCompany([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "integrations.adsense.oauth.client_secret") {
		t.Fatalf("a box naming AdSense's client secret was refused with %v, want the growth credential named", err)
	}
}

// A DOCUMENT STORED BEFORE ADSENSE VALIDATES UNCHANGED: every new key is
// optional, so each example company and a company with no AdSense block parse
// as they did, and no seat holds an AdSense level.
//
// Mutation: make integrations.adsense required, or default a seat's adsense block
// on, and this test goes red.
func TestADocumentStoredBeforeAdSenseValidatesUnchanged(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(strings.Replace(adSenseDoc("", ""), "  adsense:\n\n", "", 1)))
	if err != nil {
		t.Fatalf("a company document stored today was refused: %v", err)
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	if seat := o.AgentSeatByHandle("finance"); seat == nil || seat.GrowthLevels != nil {
		t.Errorf("a seat with no growth block holds growth levels %v", seat)
	}
	examples, err := filepath.Glob("../../examples/*.company.yaml")
	if err != nil || len(examples) == 0 {
		t.Fatalf("no example company found: %v", err)
	}
	for _, path := range examples {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := config.ParseCompany(raw); err != nil {
			t.Errorf("%s no longer validates: %v", path, err)
		}
	}
}
