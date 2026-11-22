package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// googleAdsDoc is a company on Google Cloud's identity with Google Ads at
// company, and the seat `growth-lead` written as seat.
func googleAdsDoc(company, seat string) string {
	return `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    fast:
      model: claude-golden
integrations:
  google_cloud:
` + completeGoogleCloud + `
  google_ads:
` + company + `
roles:
  - name: Growth lead
    handle: growth-lead
    llm: fast
` + seat
}

// A GOOGLE ADS BLOCK SIGNS IN THROUGH THE GOOGLE CLOUD IDENTITY: an enabled
// block with accounts, a manager and caps validates, a seat names its level
// and accounts, and its level reaches the chart.
func TestAGoogleAdsBlockAndSeatValidate(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(googleAdsDoc(`    enabled: true
    customers: [{id: "1234567890", manager: "9876543210"}]
    spend:
      accounts: [{account: "1234567890", currency: EUR, per_change: "50.00", daily: "200.00"}]`,
		"    integrations:\n      google_ads: {enabled: true, level: administer, customers: [\"1234567890\"]}\n")))
	if err != nil {
		t.Fatalf("a complete Google Ads company was refused: %v", err)
	}
	if got := c.Integrations.GoogleAds.ManagerOf("1234567890"); got != "9876543210" {
		t.Errorf("the account's manager reads %q", got)
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	if seat := o.AgentSeatByHandle("growth-lead"); seat == nil || seat.GrowthLevels["google_ads"] != "administer" {
		t.Fatalf("the seat's growth levels are %v, want google_ads at administer", seat)
	}
}

// AN ENABLED BLOCK NEEDS THE GOOGLE IDENTITY, every account is a customer ID
// once with another account as its manager, a cap names a listed account, and
// a seat's accounts are within the company's; each refusal names the field.
func TestGoogleAdsRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, company, seat, path string
	}{
		{"no Google identity", "    enabled: true", "", "integrations.google_ads.enabled"},
		{"an account with hyphens", "    enabled: true\n    customers: [{id: \"123-456-7890\"}]", "",
			"integrations.google_ads.customers[0].id"},
		{"an account listed twice", "    enabled: true\n    customers: [{id: \"1234567890\"}, {id: \"1234567890\"}]", "",
			"integrations.google_ads.customers[1].id"},
		{"an account its own manager",
			"    enabled: true\n    customers: [{id: \"1234567890\", manager: \"1234567890\"}]", "",
			"integrations.google_ads.customers[0].manager"},
		{"a cap on an account not listed", `    enabled: true
    customers: [{id: "1234567890"}]
    spend:
      accounts: [{account: "5555555555", currency: EUR, per_change: "1.00", daily: "2.00"}]`, "",
			"integrations.google_ads.spend.accounts[0].account"},
		{"a seat's account outside the company's", "    enabled: true\n    customers: [{id: \"1234567890\"}]",
			"    integrations:\n      google_ads: {enabled: true, customers: [\"5555555555\"]}\n",
			"roles[0].integrations.google_ads.customers[0]"},
		{"a seat with no account", "    enabled: true",
			"    integrations:\n      google_ads: {enabled: true}\n", "roles[0].integrations.google_ads.customers"},
	} {
		doc := googleAdsDoc(tc.company, tc.seat)
		if tc.name == "no Google identity" {
			doc = strings.Replace(doc, "  google_cloud:\n"+completeGoogleCloud+"\n", "", 1)
		}
		_, err := config.ParseCompany([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: refused with %v, want the field %s named", tc.name, err, tc.path)
		}
	}
}

// A DOCUMENT STORED BEFORE GOOGLE ADS VALIDATES UNCHANGED: every new key is
// optional, and a seat with no growth block holds no growth level.
func TestADocumentStoredBeforeGoogleAdsValidatesUnchanged(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(strings.Replace(googleAdsDoc("", ""), "  google_ads:\n\n", "", 1)))
	if err != nil {
		t.Fatalf("a company document stored today was refused: %v", err)
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	if seat := o.AgentSeatByHandle("growth-lead"); seat == nil || seat.GrowthLevels != nil {
		t.Errorf("a seat with no growth block holds growth levels %v", seat)
	}
}
