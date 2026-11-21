package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// googleAnalyticsDoc is a company on Google Cloud's identity with Google
// Analytics at company, and the seat `growth-lead` written as seat.
func googleAnalyticsDoc(company, seat string) string {
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
  google_analytics:
` + company + `
roles:
  - name: Growth lead
    handle: growth-lead
    llm: fast
` + seat
}

// A GOOGLE ANALYTICS BLOCK SIGNS IN THROUGH THE GOOGLE CLOUD IDENTITY: an
// enabled block validates beside a provisioned Google Cloud block, a seat
// names its level and properties, and its level reaches the role the chart
// decides growth tools by.
func TestAGoogleAnalyticsBlockAndSeatValidate(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(googleAnalyticsDoc("    enabled: true\n    properties: [\"123456789\"]",
		"    integrations:\n      google_analytics: {enabled: true, level: operate, properties: [\"123456789\"]}\n")))
	if err != nil {
		t.Fatalf("a complete Google Analytics company was refused: %v", err)
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	seat := o.AgentSeatByHandle("growth-lead")
	if seat == nil || seat.GrowthLevels["google_analytics"] != "operate" {
		t.Fatalf("the seat's growth levels are %v, want google_analytics at operate", seat)
	}
}

// AN ENABLED BLOCK NEEDS THE GOOGLE IDENTITY, and every property is an ID once,
// a seat's within the company's narrowing; each refusal names the field.
func TestGoogleAnalyticsRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, company, seat, path string
	}{
		{"no Google identity", "    enabled: true", "", "integrations.google_analytics.enabled"},
		{"a property that is no ID", "    enabled: true\n    properties: [\"G-ABC123\"]", "",
			"integrations.google_analytics.properties[0]"},
		{"a property listed twice", "    enabled: true\n    properties: [\"1\", \"1\"]", "",
			"integrations.google_analytics.properties[1]"},
		{"a seat's property outside the company's", "    enabled: true\n    properties: [\"1\"]",
			"    integrations:\n      google_analytics: {enabled: true, properties: [\"2\"]}\n",
			"roles[0].integrations.google_analytics.properties[0]"},
		{"an unknown level", "    enabled: true",
			"    integrations:\n      google_analytics: {enabled: true, level: owner, properties: [\"1\"]}\n",
			"roles[0].integrations.google_analytics.level"},
		{"a seat with no property", "    enabled: true",
			"    integrations:\n      google_analytics: {enabled: true}\n",
			"roles[0].integrations.google_analytics.properties"},
		{"a seat on where the company is off", "    enabled: false",
			"    integrations:\n      google_analytics: {enabled: true, properties: [\"1\"]}\n",
			"roles[0].integrations.google_analytics.enabled"},
		{"a person's seat", "    enabled: true",
			"    kind: human\n    integrations:\n      google_analytics: {enabled: false}\n",
			"roles[0].integrations.google_analytics"},
	} {
		doc := googleAnalyticsDoc(tc.company, tc.seat)
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

// A DOCUMENT STORED BEFORE GOOGLE ANALYTICS VALIDATES UNCHANGED: every new key
// is optional, and a company on Google Cloud alone, with no growth block,
// holds no growth level on any seat.
func TestADocumentStoredBeforeGoogleAnalyticsValidatesUnchanged(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(strings.Replace(googleAnalyticsDoc("", ""),
		"  google_analytics:\n\n", "", 1)))
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
