package config_test

import (
	"regexp"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// A TYPED SITE THE PICKER OFFERS IS ONE THE DOCUMENT TAKES: every site the
// shared pattern matches passes SearchConsoleSite, and every site Search
// Console writes matches it, so the picker never offers what the save refuses
// nor refuses what it takes.
//
// Mutation: drop the trailing slash from the pattern's URL prefix, and the
// picker offers a site the document refuses.
func TestATypedSiteThePickerOffersIsOneTheDocumentTakes(t *testing.T) {
	t.Parallel()
	shape := regexp.MustCompile(config.SearchConsoleSitePattern)
	for _, site := range []string{"sc-domain:example.com", "https://www.example.com/", "http://example.co.uk/blog/",
		"https://shop.example.com:8443/"} {
		if !shape.MatchString(site) || !config.SearchConsoleSite(site) {
			t.Errorf("%q: pattern %v, document %v; both should take it", site, shape.MatchString(site),
				config.SearchConsoleSite(site))
		}
	}
	for _, typed := range []string{"Example Shop", "example.com", "https://example.com", "sc-domain:Example.com",
		"https://example.com/?q=1", "ftp://example.com/"} {
		if shape.MatchString(typed) {
			t.Errorf("the pattern offers %q, which is no site", typed)
		}
	}
}

// OPERATE AS MARKETER IS A SEAT THE DOCUMENT TAKES, read back through the one
// growth seat path with its role.
//
// Mutation: leave Role out of the seat's access, and the Agents tab reads the
// seat at Editor's operate.
func TestOperateAsMarketerIsASeatTheDocumentTakes(t *testing.T) {
	t.Parallel()
	c, err := config.ParseCompany([]byte(googleAnalyticsDoc("    enabled: true",
		"    integrations:\n      google_analytics: {enabled: true, level: operate, role: marketer, properties: [\"1\"]}\n")))
	if err != nil {
		t.Fatal(err)
	}
	a, ok := c.Roles[0].GrowthSeatAccess(config.GoogleAnalyticsKind)
	if !ok || a.Level != "operate" || a.Role != config.GoogleAnalyticsMarketer {
		t.Errorf("read back as %+v", a)
	}
}
