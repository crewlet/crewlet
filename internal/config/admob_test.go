package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	_ "github.com/crewlet/crewlet/internal/admob"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/growth"
)

// adMobClient is a complete OAuth client block.
const adMobClient = `    oauth:
      client_id: "123456789012-abc123def456.apps.googleusercontent.com"
      client_secret: ${GOOGLE_OAUTH_CLIENT_SECRET}
`

// adMobDoc is a company with AdMob at company, and the seat `finance` written
// as seat.
func adMobDoc(company, seat string) string {
	return `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    fast:
      model: claude-golden
integrations:
  admob:
` + company + `
roles:
  - name: Finance
    handle: finance
    llm: fast
` + seat
}

// AN ADMOB BLOCK SIGNS IN AS THE COMPANY'S OWN CLIENT, NEEDING NO GOOGLE
// CLOUD: an enabled block with its OAuth client validates alone, a seat names
// its level and accounts, and its level reaches the role the chart decides
// growth tools by: at administer, every grant.
func TestAnAdMobBlockAndSeatValidate(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(adMobDoc("    enabled: true\n"+adMobClient+
		"    accounts: [\"pub-1234567890123456\"]",
		"    integrations:\n      admob: {enabled: true, level: administer, accounts: [\"pub-1234567890123456\"]}\n")))
	if err != nil {
		t.Fatalf("a complete AdMob company was refused: %v", err)
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	seat := o.AgentSeatByHandle("finance")
	if seat == nil || seat.GrowthLevels["admob"] != "administer" {
		t.Fatalf("the seat's growth levels are %v, want admob at administer", seat)
	}
	for _, l := range growth.Levels() {
		if !slices.Contains(orgchart.GrantedSeats(o, authz.GrowthSeatGrant("admob", l)), "finance") {
			t.Errorf("the seat does not carry admob_%s", l)
		}
	}
}

// A BLOCK TURNED OFF NEEDS NOTHING: only an enabled block signs in, so one
// written off, with no client yet, validates.
//
// Mutation: require oauth whatever enabled says, and this test goes red.
func TestAnAdMobBlockTurnedOffNeedsNothing(t *testing.T) {
	t.Parallel()
	if _, err := config.ParseCompany([]byte(adMobDoc("    enabled: false", ""))); err != nil {
		t.Errorf("an AdMob block turned off was refused: %v", err)
	}
}

// AN ENABLED BLOCK NEEDS ITS CLIENT, and every account is a publisher ID once,
// a seat's within the company's narrowing; each refusal names the field.
func TestAdMobRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, company, seat, path string
	}{
		{"no OAuth client", "    enabled: true", "", "integrations.admob.oauth"},
		{"a client ID of another shape", "    enabled: true\n    oauth:\n      client_id: \"abc\"\n" +
			"      client_secret: ${S}", "", "integrations.admob.oauth.client_id"},
		{"no client secret", "    enabled: true\n    oauth:\n      client_id: " +
			"\"1-a.apps.googleusercontent.com\"", "", "integrations.admob.oauth.client_secret"},
		// THE GRANT'S RECORD names the client id at a bound, refused here
		// naming the field rather than as the sign-in starts.
		{"a client ID past what a grant keeps", "    enabled: true\n    oauth:\n      client_id: \"1-" +
			strings.Repeat("a", coord.MaxGrantAddressBytes) + ".apps.googleusercontent.com\"\n" +
			"      client_secret: ${S}", "", "integrations.admob.oauth.client_id"},
		{"an account that is no publisher ID", "    enabled: true\n" + adMobClient + "    accounts: [\"ca-pub-1\"]",
			"", "integrations.admob.accounts[0]"},
		{"an account listed twice", "    enabled: true\n" + adMobClient +
			"    accounts: [\"pub-1234567890123456\", \"pub-1234567890123456\"]", "", "integrations.admob.accounts[1]"},
		{"a seat's account outside the company's", "    enabled: true\n" + adMobClient +
			"    accounts: [\"pub-1234567890123456\"]",
			"    integrations:\n      admob: {enabled: true, accounts: [\"pub-6543210987654321\"]}\n",
			"roles[0].integrations.admob.accounts[0]"},
		{"an unknown level", "    enabled: true\n" + adMobClient,
			"    integrations:\n      admob: {enabled: true, level: owner, accounts: [\"pub-1234567890123456\"]}\n",
			"roles[0].integrations.admob.level"},
		{"a seat with no account", "    enabled: true\n" + adMobClient,
			"    integrations:\n      admob: {enabled: true}\n", "roles[0].integrations.admob.accounts"},
		{"a seat on where the company is off", "    enabled: false",
			"    integrations:\n      admob: {enabled: true, accounts: [\"pub-1234567890123456\"]}\n",
			"roles[0].integrations.admob.enabled"},
	} {
		_, err := config.ParseCompany([]byte(adMobDoc(tc.company, tc.seat)))
		if err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: refused with %v, want the field %s named", tc.name, err, tc.path)
		}
	}
}

// THE CLIENT'S SECRET IS ADMOB'S CREDENTIAL ALONE: named at a box's
// environment it would reach Google around the engine's tools, so the
// document is refused there.
//
// Mutation: drop the client secret's secret tag, and the second document
// validates.
func TestTheAdMobClientSecretIsNamedAtItsOwnFieldAlone(t *testing.T) {
	t.Parallel()
	doc := adMobDoc("    enabled: true\n"+adMobClient,
		"    sandbox:\n      env:\n        LEAK: ${GOOGLE_OAUTH_CLIENT_SECRET}\n")
	_, err := config.ParseCompany([]byte(doc))
	if err == nil || !strings.Contains(err.Error(), "integrations.admob.oauth.client_secret") {
		t.Fatalf("a box naming AdMob's client secret was refused with %v, want the growth credential named", err)
	}
}

// A DOCUMENT STORED BEFORE ADMOB VALIDATES UNCHANGED: every new key is
// optional, so each example company and a company with no AdMob block parse
// as they did, and no seat holds an AdMob level.
//
// Mutation: make integrations.admob required, or default a seat's admob block
// on, and this test goes red.
func TestADocumentStoredBeforeAdMobValidatesUnchanged(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(strings.Replace(adMobDoc("", ""), "  admob:\n\n", "", 1)))
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
