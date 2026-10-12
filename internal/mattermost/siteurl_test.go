package mattermost_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mattermost"
)

// inCluster is the address an engine on Kubernetes dials: the server's own
// Service, which nobody browses.
const inCluster = "http://mattermost.chat.svc.cluster.local:8065"

// THE ADDRESS THE ENGINE DIALS IS NOT THE ADDRESS PEOPLE BROWSE, AND THAT IS
// NOT A FAULT. It was: a warning that browsers would fail fired on every
// operator-managed deployment, whose engine dials the in-cluster Service
// while people browse the public Site URL — and the browsers worked.
func TestADifferentRouteToTheServerIsNotAFault(t *testing.T) {
	for _, site := range []string{
		"https://chat.example.com",
		"https://chat.example.com/",
		"http://localhost:8065",
		"https://Chat.Example.com",
	} {
		if fault, ok := mattermost.JudgeSiteURL(inCluster, site); ok {
			t.Errorf("JudgeSiteURL(%q, %q) = %s: %s", inCluster, site, fault.Event, fault.Detail)
		}
	}
}

// A SITE URL IS JUDGED BY THE SERVER'S OWN RULE: it is reported as refusing
// browsers exactly when Mattermost's origin check ([mattermost.AdmitsOrigin])
// refuses a browser at the address people type — and that is a fault whatever
// address the engine dials, which is why the engine's address appears in no
// case's verdict but the path's.
func TestASiteURLIsReportedExactlyWhenTheServerRefusesItsBrowsers(t *testing.T) {
	for _, tc := range []struct {
		site, browsedAt string
	}{
		{"https://chat.example.com", "https://chat.example.com"},
		{"https://Chat.Example.com", "https://chat.example.com"},
		{"http://localhost:8065", "http://localhost:8065"},
		{"https://chat.example.com/team", "https://chat.example.com/team"},
		{"http://[::1]:8065", "http://[::1]:8065"},
		// A browser never sends a default port, and the server
		// compares hosts as written.
		{"https://chat.example.com:443", "https://chat.example.com"},
		{"http://chat.example.com:80", "http://chat.example.com"},
		// Values the server's own validation accepts
		// (url.ParseRequestURI) and no browser can match.
		{"localhost:8065", "http://localhost:8065"},
		{"chat.example.com:8065", "http://chat.example.com:8065"},
		{"/mattermost", "https://chat.example.com/mattermost"},
		{"http://", "http://localhost"},
		{"ftp://chat.example.com", "https://chat.example.com"},
	} {
		browser, err := url.Parse(tc.browsedAt)
		if err != nil {
			t.Fatal(err)
		}
		admitted := mattermost.AdmitsOrigin(mattermost.BrowserOrigin(tc.browsedAt),
			browser.Host, tc.site, "")
		// The engine's path is the Site URL's, so the only question left
		// is what the server does with its browsers.
		configured := inCluster + browser.Path
		fault, ok := mattermost.JudgeSiteURL(configured, tc.site)
		refused := ok && fault.Event == mattermost.EventSiteURLRefusesBrowsers
		if refused == admitted {
			t.Errorf("Site URL %q, browsed at %s: the server admits the browser = %v, "+
				"but JudgeSiteURL = %+v (reported %v)", tc.site, tc.browsedAt, admitted, fault, ok)
		}
		if ok && !refused {
			t.Errorf("Site URL %q: an unexpected fault %s: %s", tc.site, fault.Event, fault.Detail)
		}
	}
}

// AN UNSET SITE URL IS A FAULT, BUT NOT THE LIVE FEED'S: with none, the server
// admits an Origin naming the host the upgrade was sent to, and what breaks is
// what Mattermost's own documentation lists — email links, email batching,
// OAuth sign-in.
func TestAnUnsetSiteURLIsReportedForWhatItBreaks(t *testing.T) {
	if !mattermost.AdmitsOrigin("https://chat.example.com", "chat.example.com", "", "") {
		t.Fatal("the transcription refuses a same-origin browser with no Site URL")
	}
	fault, ok := mattermost.JudgeSiteURL(inCluster, "  ")
	if !ok || fault.Event != mattermost.EventSiteURLUnset {
		t.Fatalf("JudgeSiteURL(unset) = %+v, %v", fault, ok)
	}
	for _, want := range []string{"email", "OAuth", "keep their live feed", "System Console"} {
		if !strings.Contains(fault.Detail, want) {
			t.Errorf("the detail does not say %q: %s", want, fault.Detail)
		}
	}
}

// THE PATH IS THE ENGINE'S OWN PROBLEM: the server routes under its Site URL's
// path on every host, so an address under another path misses its API while
// the host can be anything that reaches it.
func TestAnAddressUnderAnotherPathIsReported(t *testing.T) {
	for _, tc := range []struct {
		configured, site string
		want             bool
	}{
		{inCluster, "https://chat.example.com/team", true},
		{inCluster + "/team", "https://chat.example.com", true},
		{inCluster + "/team", "https://chat.example.com/team/", false},
		{inCluster + "/", "https://chat.example.com", false},
	} {
		fault, ok := mattermost.JudgeSiteURL(tc.configured, tc.site)
		got := ok && fault.Event == mattermost.EventURLPathMismatch
		if got != tc.want {
			t.Errorf("JudgeSiteURL(%q, %q) = %+v, %v; want a path fault = %v",
				tc.configured, tc.site, fault, ok, tc.want)
		}
		if got && !strings.Contains(fault.Detail, "integrations.mattermost.url") {
			t.Errorf("the detail does not name the field to change: %s", fault.Detail)
		}
	}
}
