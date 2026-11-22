package searchconsole

import "testing"

// A PAGE BELONGS TO A SITE ONLY AS SEARCH CONSOLE COUNTS IT: a URL prefix
// holds the pages it starts, never a longer host that shares its letters, and
// a domain holds its host and every host below it, never one that merely ends
// in its letters.
//
// A page a server could resolve to another path than it spells (a dot
// segment, raw or percent-encoded, or a backslash) belongs to no site.
//
// Mutation: compare a URL prefix as a raw string, or a domain's host by
// suffix alone, and a page of another site is in scope.
func TestAPageBelongsToASiteOnlyAsSearchConsoleCountsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		site, page string
		want       bool
	}{
		{"https://www.example.com/", "https://www.example.com/jobs/1", true},
		{"https://www.example.com/", "https://www.example.com.evil.test/jobs/1", false},
		{"https://www.example.com/", "http://www.example.com/jobs/1", false},
		{"https://www.example.com/blog/", "https://www.example.com/jobs/1", false},
		{"https://www.example.com/", "https://www.example.com@evil.test/", false},
		{"sc-domain:example.com", "https://example.com/", true},
		{"sc-domain:example.com", "http://jobs.EXAMPLE.com/1", true},
		{"sc-domain:example.com", "https://notexample.com/", false},
		{"sc-domain:example.com", "https://example.com.evil.test/", false},
		{"sc-domain:example.com", "ftp://example.com/", false},
		{"sc-domain:example.com", "not a url", false},
		{"https://www.example.com/blog/", "https://www.example.com/blog/../careers/job-1", false},
		{"https://www.example.com/blog/", "https://www.example.com/blog/%2e%2e/careers/job-1", false},
		{"https://www.example.com/blog/", "https://www.example.com/blog/%252E%252E/careers/job-1", false},
		{"https://www.example.com/blog/", "https://www.example.com/blog/..\\careers/job-1", false},
		{"https://www.example.com/blog/", "https://www.example.com/blog/%5c..%5ccareers/job-1", false},
		{"https://www.example.com/blog/", "https://www.example.com/blog/./post", false},
		{"https://www.example.com/blog/", "https://WWW.EXAMPLE.COM/blog/post", true},
		{"https://www.example.com/blog/", "https://www.example.com/blog/a..b/post", true},
		{"sc-domain:example.com", "https://jobs.example.com/x/../../admin", false},
	} {
		if got := covers(tc.site, tc.page); got != tc.want {
			t.Errorf("covers(%q, %q) = %v, want %v", tc.site, tc.page, got, tc.want)
		}
	}
	sites := []string{"sc-domain:example.com", "https://www.example.com/", "https://www.example.com/blog/"}
	if got, _ := coverOf(sites, "https://www.example.com/blog/post"); got != "https://www.example.com/blog/" {
		t.Errorf("a blog post belongs to %q, want its narrowest site", got)
	}
	if got, _ := coverOf(sites, "https://shop.example.com/"); got != "sc-domain:example.com" {
		t.Errorf("a shop page belongs to %q, want the domain", got)
	}
	if _, ok := coverOf(sites, "https://example.org/"); ok {
		t.Error("a page of no site belongs to one")
	}
}

// A SITE VERIFICATION RESOURCE IS THE PROPERTY IT VERIFIES, however Google
// writes its ID: a domain's dns:// ID is its sc-domain property, a site's URL
// its URL prefix with or without the trailing slash, percent-encoded or not;
// an Android app is no property.
func TestASiteVerificationResourceIsThePropertyItVerifies(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]string{
		"dns://example.com":                 "sc-domain:example.com",
		"dns%3A%2F%2Fexample.com":           "sc-domain:example.com",
		"http://www.example.com/":           "http://www.example.com/",
		"http%3A%2F%2Fwww.example.com%2F":   "http://www.example.com/",
		"https://www.example.com":           "https://www.example.com/",
		"android-app://com.example.app":     "",
		"dns://EXAMPLE.com":                 "",
		"https://www.example.com/blog/x?q=": "",
	} {
		got, ok := resourceSite(id)
		if ok != (want != "") || (ok && got != want) {
			t.Errorf("resourceSite(%q) = %q, %v, want %q", id, got, ok, want)
		}
	}
	for _, site := range []string{"sc-domain:example.com", "https://www.example.com/"} {
		kind, identifier, id := resourceOf(site)
		if back, ok := verifiedSite(kind, identifier); !ok || back != site {
			t.Errorf("%s's resource %s %s reads back as %q", site, kind, identifier, back)
		}
		if back, ok := resourceSite(id); !ok || back != site {
			t.Errorf("%s's resource ID %s reads back as %q", site, id, back)
		}
	}
	if methodFits(verifyDomain, "META") || methodFits(verifySite, "DNS_TXT") || !methodFits(verifyDomain, "DNS_TXT") {
		t.Error("a verification method is fitted to the wrong kind of property")
	}
}
