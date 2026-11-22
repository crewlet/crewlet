package config_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// searchConsoleDoc is a company on Google Cloud's identity with Search
// Console at company, and the seat `growth-lead` written as seat.
func searchConsoleDoc(company, seat string) string {
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
  search_console:
` + company + `
roles:
  - name: Growth lead
    handle: growth-lead
    llm: fast
` + seat
}

// A SEARCH CONSOLE BLOCK SIGNS IN THROUGH THE GOOGLE CLOUD IDENTITY: an
// enabled block validates beside a provisioned Google Cloud block, a seat
// names its level and sites in both of Search Console's property forms, and
// its level reaches the role the chart decides growth tools by.
func TestASearchConsoleBlockAndSeatValidate(t *testing.T) {
	t.Parallel()
	sites := `["https://www.example.com/", "sc-domain:example.org"]`
	c, err := config.ParseCompany([]byte(searchConsoleDoc("    enabled: true\n    sites: "+sites,
		"    integrations:\n      search_console: {enabled: true, level: operate, sites: "+sites+"}\n")))
	if err != nil {
		t.Fatalf("a complete Search Console company was refused: %v", err)
	}
	o, err := c.Organization()
	if err != nil {
		t.Fatal(err)
	}
	seat := o.AgentSeatByHandle("growth-lead")
	if seat == nil || seat.GrowthLevels["search_console"] != "operate" {
		t.Fatalf("the seat's growth levels are %v, want search_console at operate", seat)
	}
}

// AN ENABLED BLOCK NEEDS THE GOOGLE IDENTITY, and every site is a property as
// Search Console writes it, once, a seat's within the company's narrowing;
// each refusal names the field.
//
// Mutation: accept a URL prefix without its trailing slash, and Google would
// be asked about a property that cannot exist.
func TestSearchConsoleRefusesWhatCannotWork(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, company, seat, path string
	}{
		{"no Google identity", "    enabled: true", "", "integrations.search_console.enabled"},
		{"a URL prefix without its trailing slash", "    enabled: true\n    sites: [\"https://www.example.com\"]",
			"", "integrations.search_console.sites[0]"},
		{"a domain without sc-domain", "    enabled: true\n    sites: [\"example.com\"]", "",
			"integrations.search_console.sites[0]"},
		{"a URL prefix with a query", "    enabled: true\n    sites: [\"https://example.com/?a=1\"]", "",
			"integrations.search_console.sites[0]"},
		{"an uppercase host", "    enabled: true\n    sites: [\"https://Example.com/\"]", "",
			"integrations.search_console.sites[0]"},
		{"a site listed twice", "    enabled: true\n    sites: [\"sc-domain:example.com\", \"sc-domain:example.com\"]",
			"", "integrations.search_console.sites[1]"},
		{"a seat's site outside the company's", "    enabled: true\n    sites: [\"sc-domain:example.com\"]",
			"    integrations:\n      search_console: {enabled: true, sites: [\"sc-domain:example.org\"]}\n",
			"roles[0].integrations.search_console.sites[0]"},
		{"an unknown level", "    enabled: true",
			"    integrations:\n      search_console: {enabled: true, level: owner, sites: [\"sc-domain:example.com\"]}\n",
			"roles[0].integrations.search_console.level"},
		{"a seat with no site", "    enabled: true",
			"    integrations:\n      search_console: {enabled: true}\n",
			"roles[0].integrations.search_console.sites"},
		{"a seat on where the company is off", "    enabled: false",
			"    integrations:\n      search_console: {enabled: true, sites: [\"sc-domain:example.com\"]}\n",
			"roles[0].integrations.search_console.enabled"},
		{"a person's seat", "    enabled: true",
			"    kind: human\n    integrations:\n      search_console: {enabled: false}\n",
			"roles[0].integrations.search_console"},
	} {
		doc := searchConsoleDoc(tc.company, tc.seat)
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

// A PROPERTY IS ACCEPTED IN EXACTLY THE FORMS SEARCH CONSOLE WRITES: a URL
// prefix with its scheme, a lowercase host, an optional port and path, and
// its trailing slash; or sc-domain and a DNS name.
func TestASearchConsoleSiteIsWrittenAsGoogleWritesIt(t *testing.T) {
	t.Parallel()
	for site, want := range map[string]bool{
		"https://www.example.com/":        true,
		"http://example.com/blog/":        true,
		"https://example.com:8443/":       true,
		"sc-domain:example.com":           true,
		"sc-domain:xn--bcher-kva.example": true,
		"https://www.example.com":         false,
		"https://www.example.com/#top":    false,
		"ftp://example.com/":              false,
		"https://user@example.com/":       false,
		"sc-domain:Example.com":           false,
		"sc-domain:localhost":             false,
		"sc-domain:":                      false,
		"https:///":                       false,
		"":                                false,
	} {
		if got := config.SearchConsoleSite(site); got != want {
			t.Errorf("SearchConsoleSite(%q) = %v, want %v", site, got, want)
		}
	}
}

// A DOCUMENT STORED BEFORE SEARCH CONSOLE VALIDATES UNCHANGED: every new key
// is optional, and a company on Google Cloud alone, with no growth block,
// holds no growth level on any seat.
func TestADocumentStoredBeforeSearchConsoleValidatesUnchanged(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(strings.Replace(searchConsoleDoc("", ""),
		"  search_console:\n\n", "", 1)))
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
