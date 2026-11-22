package config_test

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/authz/orgchart"
	"github.com/crewlet/crewlet/internal/config"
	_ "github.com/crewlet/crewlet/internal/googleplay"
	"github.com/crewlet/crewlet/internal/growth"
)

// googlePlayDoc is a company on Google Cloud's identity with Google Play at
// company, and the seat `mobile-lead` written as seat.
func googlePlayDoc(company, seat string) string {
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
  google_play:
` + company + `
roles:
  - name: Mobile lead
    handle: mobile-lead
    llm: fast
` + seat
}

// A GOOGLE PLAY BLOCK SIGNS IN THROUGH THE GOOGLE CLOUD IDENTITY: an enabled
// block with its apps, developer accounts and reports buckets validates, a
// seat names its level and apps, and its level reaches the role the chart
// decides growth tools by: at operate, the read and operate grants and never
// administer.
func TestAGooglePlayBlockAndSeatValidate(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(googlePlayDoc("    enabled: true\n    apps: [\"com.example.app\"]\n"+
		"    developer_accounts: [\"1234567890123456789\"]\n    reports_buckets: [\"pubsite_prod_rev_0123\"]",
		"    integrations:\n      google_play: {enabled: true, level: operate, apps: [\"com.example.app\"]}\n")))
	if err != nil {
		t.Fatalf("a complete Google Play company was refused: %v", err)
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	seat := o.AgentSeatByHandle("mobile-lead")
	if seat == nil || seat.GrowthLevels["google_play"] != "operate" {
		t.Fatalf("the seat's growth levels are %v, want google_play at operate", seat)
	}
	for _, l := range growth.Levels() {
		carried := slices.Contains(orgchart.GrantedSeats(o, authz.GrowthSeatGrant("google_play", l)), "mobile-lead")
		if want := l != growth.LevelAdminister; carried != want {
			t.Errorf("the seat carries google_play_%s: %v, want %v", l, carried, want)
		}
	}
}

// AN ENABLED BLOCK NEEDS THE GOOGLE IDENTITY, and every app, account and
// bucket has its shape, once, a seat's apps within the company's; each
// refusal names the field.
func TestGooglePlayRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, company, seat, path string
	}{
		{"no Google identity", "    enabled: true", "", "integrations.google_play.enabled"},
		{"an app that is no package", "    enabled: true\n    apps: [\"Acme App\"]", "",
			"integrations.google_play.apps[0]"},
		{"an app listed twice", "    enabled: true\n    apps: [\"com.a.b\", \"com.a.b\"]", "",
			"integrations.google_play.apps[1]"},
		{"a developer account that is no ID", "    enabled: true\n    developer_accounts: [\"acme\"]", "",
			"integrations.google_play.developer_accounts[0]"},
		{"a bucket with its scheme", "    enabled: true\n    reports_buckets: [\"gs://pubsite_prod_rev_1\"]", "",
			"integrations.google_play.reports_buckets[0]"},
		{"a seat's app outside the company's", "    enabled: true\n    apps: [\"com.a.b\"]",
			"    integrations:\n      google_play: {enabled: true, apps: [\"com.a.c\"]}\n",
			"roles[0].integrations.google_play.apps[0]"},
		{"an unknown level", "    enabled: true",
			"    integrations:\n      google_play: {enabled: true, level: owner, apps: [\"com.a.b\"]}\n",
			"roles[0].integrations.google_play.level"},
		{"a seat with no app", "    enabled: true",
			"    integrations:\n      google_play: {enabled: true}\n",
			"roles[0].integrations.google_play.apps"},
		{"a seat on where the company is off", "    enabled: false",
			"    integrations:\n      google_play: {enabled: true, apps: [\"com.a.b\"]}\n",
			"roles[0].integrations.google_play.enabled"},
		{"a person's seat", "    enabled: true",
			"    kind: human\n    integrations:\n      google_play: {enabled: false}\n",
			"roles[0].integrations.google_play"},
	} {
		doc := googlePlayDoc(tc.company, tc.seat)
		if tc.name == "no Google identity" {
			doc = strings.Replace(doc, "  google_cloud:\n"+completeGoogleCloud+"\n", "", 1)
		}
		if tc.name == "a person's seat" {
			doc = strings.Replace(doc, "    llm: fast\n", "", 1)
		}
		_, err := config.ParseCompany([]byte(doc))
		if err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s: refused with %v, want the field %s named", tc.name, err, tc.path)
		}
	}
}

// A DOCUMENT STORED BEFORE GOOGLE PLAY VALIDATES UNCHANGED: every new key is
// optional, so the shipped example and a company on Google Cloud alone, with
// no growth block, validate and hold no growth level on any seat.
//
// Mutation: make reports_buckets required, and both are refused.
func TestADocumentStoredBeforeGooglePlayValidatesUnchanged(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(strings.Replace(googlePlayDoc("", ""), "  google_play:\n\n", "", 1)))
	if err != nil {
		t.Fatalf("a company document stored today was refused: %v", err)
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	if seat := o.AgentSeatByHandle("mobile-lead"); seat == nil || seat.GrowthLevels != nil {
		t.Errorf("a seat with no growth block holds growth levels %v", seat)
	}
	example, err := os.ReadFile("../../examples/nimbus.company.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.ParseCompany(example); err != nil {
		t.Errorf("the shipped example is refused: %v", err)
	}
}
