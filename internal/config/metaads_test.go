package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// metaAdsDoc is a company with a Meta Ads block and one seat on its lane.
const metaAdsDoc = `name: Acme
roles:
  - name: Founder
    handle: founder
    kind: human
  - name: Growth
    handle: growth
    integrations:
      meta_ads: {enabled: true, level: operate, ad_accounts: ["act_1112223333"]}
integrations:
  meta_ads:
    enabled: true
    approvers: [founder]
    app_id: "987654321012345"
    app_secret: ${META_ADS_APP_SECRET}
    read_token: ${META_ADS_READ_TOKEN}
    operate_token: ${META_ADS_OPERATE_TOKEN}
    business_id: "1234567890"
    ad_accounts: ["act_1112223333", "4445556666"]
    spend:
      accounts:
        - {account: "act_1112223333", currency: EUR, per_change: "50.00", daily: "200.00"}
`

// EVERY META ADS KEY IS OPTIONAL: the shipped example company, which names
// none, still validates, and a block holding the keys validates as written:
// its seat at its level, bare digits read as act_ and them, its caps as micros.
//
// Mutation: make the block, operate_token or spend required, and a case fails.
func TestAMetaAdsBlockIsOptionalAndReadsWhole(t *testing.T) {
	t.Parallel()
	example, err := os.ReadFile(filepath.Join("..", "..", "examples", "nimbus.company.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.ParseCompany(example); err != nil {
		t.Fatalf("the example company no longer validates: %v", err)
	}
	c, err := config.ParseCompany([]byte(metaAdsDoc))
	if err != nil {
		t.Fatal(err)
	}
	seat := c.MetaAdsSeat("growth")
	if seat == nil || seat.GrowthSeatLevel() != "operate" || c.MetaAdsSeat("founder") != nil || seat.BusinessResources {
		t.Fatalf("the seat reads as %+v", seat)
	}
	if got := c.Integrations.MetaAds.AdAccountIDs(); len(got) != 2 || got[1] != "act_4445556666" {
		t.Errorf("the block's ad accounts read as %v", got)
	}
	caps, ok := c.Integrations.GrowthCaps("meta_ads", "act_1112223333")
	if !ok || caps.Currency != "EUR" || caps.PerChangeMicros == nil || *caps.PerChangeMicros != 50_000_000 ||
		caps.DailyMicros == nil || *caps.DailyMicros != 200_000_000 {
		t.Errorf("the caps read as %+v (%v)", caps, ok)
	}
	bare := strings.Replace(metaAdsDoc, "    operate_token: ${META_ADS_OPERATE_TOKEN}\n", "", 1)
	bare = bare[:strings.Index(bare, "    business_id:")]
	if _, err := config.ParseCompany([]byte(bare)); err != nil {
		t.Errorf("a block with the three required keys alone is refused: %v", err)
	}
	off := strings.Replace(metaAdsDoc, "meta_ads: {enabled: true, level", "meta_ads: {enabled: false, level", 1)
	if c, err := config.ParseCompany([]byte(off)); err != nil || c.MetaAdsSeat("growth") != nil {
		t.Errorf("a seat turned off is on (%v)", err)
	}
	if config.MetaAdAccountID("act_12") != "act_12" || config.MetaAdAccountID("12") != "act_12" ||
		config.MetaAdAccountID("act_x") != "" {
		t.Error("an ad account ID is not read as Meta writes it")
	}
}

// Each refusal names the field to change: a required key missing, a token or
// the secret written in place, one token in two places, an ID of the wrong
// shape, a seat with no ad account or one outside the block's, and caps on an
// account not written as Meta writes it or outside the block's.
//
// Mutation: drop any one check, and its case validates.
func TestAMetaAdsBlockRefusesWhatCannotSignIn(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ from, to, path string }{
		"an app ID not digits":    {`app_id: "987654321012345"`, `app_id: "acme"`, "meta_ads.app_id"},
		"no app ID":               {"    app_id: \"987654321012345\"\n", "", "meta_ads.app_id"},
		"no app secret":           {"    app_secret: ${META_ADS_APP_SECRET}\n", "", "meta_ads.app_secret"},
		"no read token":           {"    read_token: ${META_ADS_READ_TOKEN}\n", "", "meta_ads.read_token"},
		"a token in place":        {"read_token: ${META_ADS_READ_TOKEN}", "read_token: EAAGm0PX4ZCpsBAx", "meta_ads.read_token"},
		"a secret in place":       {"app_secret: ${META_ADS_APP_SECRET}", "app_secret: 0123abcd", "meta_ads.app_secret"},
		"one token in two places": {"operate_token: ${META_ADS_OPERATE_TOKEN}", "operate_token: ${META_ADS_READ_TOKEN}", "meta_ads.operate_token"},
		"a business not digits":   {`business_id: "1234567890"`, `business_id: "acme"`, "meta_ads.business_id"},
		"an account by name":      {`ad_accounts: ["act_1112223333", "4445556666"]`, `ad_accounts: ["brand"]`, "meta_ads.ad_accounts[0]"},
		"a seat with no account":  {`ad_accounts: ["act_1112223333"]}`, "ad_accounts: []}", "roles[1].integrations.meta_ads"},
		"a seat outside the block": {`level: operate, ad_accounts: ["act_1112223333"]`, `level: operate, ad_accounts: ["act_555"]`,
			"roles[1].integrations.meta_ads.ad_accounts[0]"},
		"a seat at no level":     {"level: operate", "level: spend", "roles[1].integrations.meta_ads.level"},
		"caps on bare digits":    {`{account: "act_1112223333", currency`, `{account: "1112223333", currency`, "spend.accounts[0].account"},
		"caps outside the block": {`{account: "act_1112223333", currency`, `{account: "act_555", currency`, "spend.accounts[0].account"},
	}
	for name, tc := range cases {
		doc := strings.Replace(metaAdsDoc, tc.from, tc.to, 1)
		if doc == metaAdsDoc {
			t.Fatalf("%s: the case changes nothing", name)
		}
		_, err := config.ParseCompany([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s answered %v, want a refusal at %s", name, err, tc.path)
		}
	}
}

// A Meta Ads credential's variable is refused anywhere but its own field, and
// Meta's own Ads MCP server is refused while the block is on: either would
// reach the same ad accounts around the caps, the decisions and seat scope.
//
// Mutation: drop meta_ads from the refused servers, and the server validates.
func TestMetaAdsCredentialsAndMetasServerStayOutOfEveryOtherPlace(t *testing.T) {
	t.Parallel()
	server := strings.Replace(metaAdsDoc, "\nintegrations:\n", "\nmcp_servers:\n  - name: meta\n"+
		"    url: https://mcp.facebook.com/ads\n    transport: http\n"+
		"    headers: {Authorization: \"Bearer ${META_ADS_OPERATE_TOKEN}\"}\nintegrations:\n", 1)
	_, err := config.ParseCompany([]byte(server))
	if err == nil || !strings.Contains(err.Error(), "mcp_servers[0]") || !strings.Contains(err.Error(), "META_ADS_OPERATE_TOKEN") {
		t.Errorf("Meta's server with the operate token answered %v", err)
	}
	off := strings.Replace(server, "  meta_ads:\n    enabled: true\n", "  meta_ads:\n    enabled: false\n", 1)
	off = strings.Replace(off, "${META_ADS_OPERATE_TOKEN}\"}", "${META_TOKEN}\"}", 1)
	off = strings.Replace(off, "meta_ads: {enabled: true, level", "meta_ads: {enabled: false, level", 1)
	if _, err := config.ParseCompany([]byte(off)); err != nil {
		t.Errorf("Meta's server beside an integration that is off is refused: %v", err)
	}
}
