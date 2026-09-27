package session_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/iam/session"
)

// A BEARER AUTHENTICATES ONLY UNDER THE NAME THIS DEPLOYMENT ISSUES, AND A
// SIGN-OUT REACHES BOTH.
//
// On https the bearer is `__Host-crewlet_session`, which a browser sets only
// from this exact host; `crewlet_session` any sibling host can write with a
// Domain covering this one. Read under either name, a sibling that planted its
// own session signed in a visitor who held none as that session's person — so
// [session.Presented] reads the issued name alone, on https and on plain http
// alike. [session.Held] is the sign-out's reading: every bearer the browser
// holds, the issued one first, so a browser still holding the name an http
// deployment issued before it moved to https has that session ended too.
//
// Mutation: read the other name in Presented and the planted cookie is
// presented; drop it from Held and the old session outlives its sign-out.
func TestABearerAuthenticatesOnlyUnderTheNameIssued(t *testing.T) {
	t.Parallel()
	const https, plain = "https://crewlet.example.com", "http://127.0.0.1:8080"
	for _, tc := range []struct {
		name     string
		external string
		cookies  map[string]string
		want     string
		held     []string
	}{
		{"https, the prefixed name", https,
			map[string]string{session.HostCookieName: "host"}, "host", []string{"host"}},
		{"https, a bare name a sibling planted", https,
			map[string]string{session.CookieBaseName: "planted"}, "", []string{"planted"}},
		{"https, both", https, map[string]string{
			session.HostCookieName: "host", session.CookieBaseName: "bare"},
			"host", []string{"host", "bare"}},
		{"http, the bare name", plain,
			map[string]string{session.CookieBaseName: "bare"}, "bare", []string{"bare"}},
		{"http, a prefixed name it cannot have issued", plain,
			map[string]string{session.HostCookieName: "host"}, "", []string{"host"}},
		{"neither", https, map[string]string{"another_app": "x"}, "", nil},
	} {
		r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		for name, value := range tc.cookies {
			r.AddCookie(&http.Cookie{Name: name, Value: value})
		}
		if got := session.Presented(r, tc.external); got != tc.want {
			t.Errorf("%s: presented %q, want %q", tc.name, got, tc.want)
		}
		if got := session.Held(r, tc.external); !slices.Equal(got, tc.held) {
			t.Errorf("%s: held %q, want %q", tc.name, got, tc.held)
		}
	}
}

// A SIGN-OUT CLEARS EVERY NAME A BEARER CAN BE HELD UNDER.
//
// A deletion matches on name, path and domain, so each has to agree with the
// cookie it deletes on those three — and the prefixed one has to be Secure,
// or a browser refuses the Set-Cookie and keeps the original. Clearing only
// the name this deployment issues left a browser holding the other one signed
// in after it signed out.
func TestClearsEndABearerUnderEveryName(t *testing.T) {
	t.Parallel()
	for _, external := range []string{
		"https://crewlet.example.com", "http://localhost:8000",
	} {
		clears := session.Clears(external)
		seen := map[string]*http.Cookie{}
		for _, c := range clears {
			seen[c.Name] = c
		}
		// THE TWO NAMES SPELLED OUT, not read back from CookieNames, so a
		// list that lost one cannot certify a clear that lost it too.
		for _, name := range []string{session.HostCookieName, session.CookieBaseName} {
			c, ok := seen[name]
			if !ok {
				t.Errorf("%s: nothing clears %q, so a browser holding it stays "+
					"signed in", external, name)
				continue
			}
			if c.MaxAge >= 0 || c.Value != "" || c.Path != "/" || c.Domain != "" {
				t.Errorf("%s: the clear for %q is %+v, which does not delete a "+
					"cookie set with Path / and no Domain", external, name, c)
			}
			if name == session.HostCookieName && !c.Secure {
				t.Errorf("%s: the prefixed clear is not Secure, which a browser "+
					"refuses for a __Host- cookie", external)
			}
		}
		// THE ISSUED NAME'S CLEAR IS [session.Clear] ITSELF, first, so the
		// attributes of the one this deployment sets never drift from it.
		want, got := session.Clear(external), clears[0]
		if got.Name != want.Name || got.Secure != want.Secure ||
			got.HttpOnly != want.HttpOnly || got.SameSite != want.SameSite ||
			got.Path != want.Path || got.MaxAge != want.MaxAge {
			t.Errorf("%s: the first clear is %+v, want session.Clear's %+v",
				external, got, want)
		}
	}
}
