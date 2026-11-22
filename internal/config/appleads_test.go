package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// appleAdsDoc is a company with an Apple Ads block and one seat on its lane.
const appleAdsDoc = `name: Acme
roles:
  - name: Founder
    handle: founder
    kind: human
  - name: Growth
    handle: growth
    integrations:
      apple_ads: {enabled: true, level: operate, ad_accounts: ["123456789"]}
integrations:
  apple_ads:
    enabled: true
    approvers: [founder]
    ad_accounts: ["123456789"]
    connections:
      main:
        org_id: "987654321"
        client_id: SEARCHADS.aeb3ef5f-0c5a-4f2a-99c8-fca83f25a9e1
        team_id: SEARCHADS.hgw3ef3p-0w7a-8a2n-77c8-scv83f25a7e1
        key_id: a273d0d3-4d9e-458c-a173-0db8619ca7d7
        private_key: ${APPLE_ADS_MAIN_KEY}
    spend:
      accounts:
        - {account: "123456789", currency: USD, per_change: "50.00", daily: "200.00"}
`

// EVERY APPLE ADS KEY IS OPTIONAL: the shipped example company, which names
// none, still validates, and a block holding every key validates as written,
// its seat at its level and its caps read as micros.
//
// Mutation: make the block or its spend required, and the example fails.
func TestAnAppleAdsBlockIsOptionalAndReadsWhole(t *testing.T) {
	t.Parallel()
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "nimbus.company.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.ParseCompany(example); err != nil {
		t.Fatalf("the example company no longer validates: %v", err)
	}
	c, err := config.ParseCompany([]byte(appleAdsDoc))
	if err != nil {
		t.Fatal(err)
	}
	seat := c.AppleAdsSeat("growth")
	if seat == nil || seat.GrowthSeatLevel() != "operate" || c.AppleAdsSeat("founder") != nil {
		t.Fatalf("the seat reads as %+v", seat)
	}
	caps, ok := c.Integrations.GrowthCaps("apple_ads", "123456789")
	if !ok || caps.Currency != "USD" || caps.PerChangeMicros == nil || *caps.PerChangeMicros != 50_000_000 ||
		caps.DailyMicros == nil || *caps.DailyMicros != 200_000_000 {
		t.Errorf("the caps read as %+v (%v)", caps, ok)
	}
	off := strings.Replace(appleAdsDoc, "apple_ads: {enabled: true, level", "apple_ads: {enabled: false, level", 1)
	if c, err := config.ParseCompany([]byte(off)); err != nil || c.AppleAdsSeat("growth") != nil {
		t.Errorf("a seat turned off is on (%v)", err)
	}
}

// Each refusal names the field to change: a connection missing its IDs, a
// private key written in place, an ID of the wrong shape, two connections of
// one API user, a seat with no ad account or one outside the block's, and caps
// on an account that is not digits or outside the block's.
//
// Mutation: drop any one check, and its case validates.
func TestAnAppleAdsBlockRefusesWhatCannotSignIn(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ from, to, path string }{
		"an organization not digits": {`org_id: "987654321"`, `org_id: "acme"`, "connections.main.org_id"},
		"a key in place":             {"${APPLE_ADS_MAIN_KEY}", "-----BEGIN EC PRIVATE KEY-----", "connections.main.private_key"},
		"a key ID of two words":      {"key_id: a273d0d3-4d9e-458c-a173-0db8619ca7d7", `key_id: "a273d0d3 4d9e"`, "connections.main.key_id"},
		"a client ID unprefixed":     {"client_id: SEARCHADS.aeb3ef5f", "client_id: aeb3ef5f", "connections.main.client_id"},
		"no team":                    {"        team_id: SEARCHADS.hgw3ef3p-0w7a-8a2n-77c8-scv83f25a7e1\n", "", "connections.main.team_id"},
		"a seat with no account":     {`ad_accounts: ["123456789"]}`, "ad_accounts: []}", "roles[1].integrations.apple_ads"},
		"a seat outside the block": {`level: operate, ad_accounts: ["123456789"]`, `level: operate, ad_accounts: ["555"]`,
			"roles[1].integrations.apple_ads.ad_accounts[0]"},
		"a seat at no level":     {"level: operate", "level: spend", "roles[1].integrations.apple_ads.level"},
		"caps on a name":         {`{account: "123456789", currency`, `{account: "brand", currency`, "spend.accounts[0].account"},
		"caps outside the block": {`{account: "123456789", currency`, `{account: "555", currency`, "spend.accounts[0].account"},
	}
	for name, tc := range cases {
		doc := strings.Replace(appleAdsDoc, tc.from, tc.to, 1)
		if doc == appleAdsDoc {
			t.Fatalf("%s: the case changes nothing", name)
		}
		_, err := config.ParseCompany([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s answered %v, want a refusal at %s", name, err, tc.path)
		}
	}
	start, end := strings.Index(appleAdsDoc, "    connections:"), strings.Index(appleAdsDoc, "    spend:")
	if _, err := config.ParseCompany([]byte(appleAdsDoc[:start] + appleAdsDoc[end:])); err == nil ||
		!strings.Contains(err.Error(), "apple_ads.connections") {
		t.Errorf("a block on with no connection answered %v", err)
	}
	two := strings.Replace(appleAdsDoc, "    spend:", "      eu:\n"+
		"        org_id: \"987654322\"\n        client_id: SEARCHADS.aeb3ef5f-0c5a-4f2a-99c8-fca83f25a9e1\n"+
		"        team_id: SEARCHADS.other\n        key_id: b273d0d3-4d9e-458c-a173-0db8619ca7d7\n"+
		"        private_key: ${APPLE_ADS_EU_KEY}\n    spend:", 1)
	if _, err := config.ParseCompany([]byte(two)); err == nil || !strings.Contains(err.Error(), "connections.main.client_id") {
		t.Errorf("two connections of one API user answered %v", err)
	}
}
