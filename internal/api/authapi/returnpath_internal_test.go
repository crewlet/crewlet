package authapi

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
)

// cookieFloor is the size RFC 6265 §6.1 asks every browser to keep a cookie
// at — name, value and attributes — and the most it promises.
const cookieFloor = 4096

// A RETURN PATH IS BOUNDED BY THE COOKIE IT RIDES IN.
//
// The path is sealed into the flight cookie, and a browser DROPS a cookie past
// the floor without a word: the start answered, the person went round the
// provider, and the callback refused them for carrying no flight. So the
// longest path the start keeps is sealed at its WORST — every byte one the
// flight's JSON writes as six — beside the longest login, an invitation's id
// and its link's secret, and a keyring id of sixty-four characters, through
// the real seal and the real cookie, and it must fit; and a path one byte
// longer takes the default, as any path this deployment will not honour does.
//
// Mutation: drop the bound, or raise it past what fits, and one of the two
// cases fails.
func TestAReturnPathIsBoundedByTheCookieItRidesIn(t *testing.T) {
	t.Parallel()
	material, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	keyID := strings.Repeat("k", 64)
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: keyID, Keys: map[string][]byte{keyID: material},
	})
	if err != nil {
		t.Fatal(err)
	}
	b := config.DefaultBootstrap()
	b.API.ExternalURL = "https://crewlet.example.com"
	s := &Service{boot: &b}
	provider := oidc.Config{
		Issuer: "https://idp.example.com", ClientID: "crewlet",
		ClientSecret: "not-a-real-secret",
		RedirectURI:  "https://crewlet.example.com/auth/oidc/callback",
	}
	at := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	// EVERY BYTE ONE JSON WRITES AS SIX, and three kinds of it, so no one
	// escape's size is what the case happens to measure.
	worst := "/" + strings.Repeat("<&\xff", maxReturnPath)[:maxReturnPath-1]
	if kept := returnPath(worst); kept != worst {
		t.Fatalf("a path of %d bytes, the bound, was replaced by %q", len(worst), kept)
	}
	_, sealed, err := provider.Start(cipher, "https://idp.example.com/authorize",
		oidc.Flight{
			Return: worst, Invite: uuid.New().String(),
			// THE SECRET AT ITS REAL LENGTH, derived as every link's is.
			InviteSecret: inviteSecretOfLength(t),
			Login:        strings.Repeat("a", iam.MaxLogin-2) + ".b",
		}, at)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if cookie := s.flightCookie(sealed, at.Add(oidc.FlightTTL)).String(); len(cookie) > cookieFloor {
		t.Errorf("the longest flight's cookie is %d bytes, past the %d a browser "+
			"keeps: it is dropped, and the callback refuses the round trip",
			len(cookie), cookieFloor)
	}
	if kept := returnPath(worst + "x"); kept != "/dashboard" {
		t.Errorf("a path past the bound was kept (%d bytes), want the default",
			len(kept))
	}
}

// inviteSecretOfLength is an invitation link's secret as the estate derives
// one, so the bound is measured against the value a real flight carries.
func inviteSecretOfLength(t *testing.T) string {
	t.Helper()
	blinder, err := iamdomain.NewBlinder([]byte(strings.Repeat("k", 32)))
	if err != nil {
		t.Fatal(err)
	}
	secret, err := blinder.InvitationSecret(uuid.New().String())
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// A RETURN PATH IS JUDGED AS A BROWSER WILL READ IT, AND NEVER LEAVES THE SITE.
//
// The value becomes a `Location`, and what decides where a person lands is how
// their BROWSER resolves that header — not how Go's parser reads it. The two
// disagree: a browser reads a backslash as a slash in an http(s) address and
// strips every tab and newline before it parses one, so `/\evil.example.com`
// was a path to the check this replaced and `https://evil.example.com` to the
// browser, and `http.Redirect` emitted it verbatim. So every case is sent
// through the real `http.Redirect` and resolved by [browserResolve], which
// applies those two rules, against the deployment's own address: whatever the
// caller sent, the person lands on this host.
//
// Mutation: drop the backslash refusal and `/\evil.example.com` lands off-site;
// judge only the raw value and the percent-encoded ones are kept. The tab and
// newline cases are held twice — by the control-character refusal and by Go's
// own parser, which refuses a control byte too — so they stay here as the
// shape a browser strips, whichever layer refuses them.
func TestAReturnPathNeverLeavesTheSite(t *testing.T) {
	t.Parallel()
	const site = "https://crewlet.example.com"
	base, err := url.Parse(site + "/auth/oidc/callback")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, raw string
		kept      bool
	}{
		{"a screen", "/work", true},
		{"a dashboard fragment route", "/dashboard#/work/items?view=mine", true},
		{"a query naming another host is only a query", "/work?next=//evil.example.com", true},
		{"a colon in a segment", "/work/a:b", true},
		{"the root", "/", true},

		{"a protocol-relative address", "//evil.example.com", false},
		{"a backslash for the second slash", `/\evil.example.com`, false},
		{"a backslash then a slash", `/\/evil.example.com`, false},
		{"two backslashes", `\\evil.example.com`, false},
		{"a backslash deeper in", `/work\..\\evil.example.com`, false},
		{"a tab a browser strips", "/\t/evil.example.com", false},
		{"a newline a browser strips", "/\n/evil.example.com", false},
		{"a carriage return a browser strips", "/\r/evil.example.com", false},
		{"surrounding spaces", "  //evil.example.com  ", false},
		{"an encoded backslash", "/%5Cevil.example.com", false},
		{"an encoded slash", "/%2F/evil.example.com", false},
		{"two encoded slashes", "/%2f%2fevil.example.com", false},
		{"an absolute address", "https://evil.example.com/", false},
		{"a scheme with no slashes", "https:evil.example.com", false},
		{"a script", "javascript:alert(1)", false},
		{"a relative path", "work", false},
		{"nothing", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := returnPath(tc.raw)
			switch {
			case tc.kept && got != strings.TrimSpace(tc.raw):
				t.Errorf("returnPath(%q) = %q, want it kept", tc.raw, got)
			case !tc.kept && got != "/dashboard":
				t.Errorf("returnPath(%q) = %q, want the default", tc.raw, got)
			}

			// AS THE BROWSER LANDS: through the real redirect, resolved by
			// the browser's own rules against where it is.
			rec := httptest.NewRecorder()
			http.Redirect(rec, httptest.NewRequest(http.MethodGet, base.String(), nil),
				got, http.StatusFound)
			landed := browserResolve(t, base, rec.Header().Get("Location"))
			if landed.Scheme != base.Scheme || landed.Host != base.Host {
				t.Errorf("return_to %q redirects with Location %q, which a "+
					"browser follows to %s — off this deployment", tc.raw,
					rec.Header().Get("Location"), landed)
			}
		})
	}
}

// browserResolve resolves a Location the way a browser does for an http(s)
// page, as far as an open redirect is concerned: the WHATWG URL standard strips
// leading and trailing C0 controls and spaces, removes every tab and newline,
// and — for a special scheme — reads a backslash before the query as a slash.
// What is left is resolved against the page like any reference.
func browserResolve(t *testing.T, base *url.URL, location string) *url.URL {
	t.Helper()
	location = strings.TrimFunc(location, func(r rune) bool { return r <= 0x20 })
	location = strings.NewReplacer("\t", "", "\n", "", "\r", "").Replace(location)
	head, tail := location, ""
	if i := strings.IndexAny(location, "?#"); i >= 0 {
		head, tail = location[:i], location[i:]
	}
	ref, err := url.Parse(strings.ReplaceAll(head, `\`, "/") + tail)
	if err != nil {
		t.Fatalf("a browser could not parse Location %q: %v", location, err)
	}
	return base.ResolveReference(ref)
}
