package session_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/iam/session"
)

// A BEARER IS READ UNDER EITHER NAME, the prefixed one first.
//
// A deployment that corrected `api.external_url` from http to https has every
// signed-in browser still presenting the bare name, and whatever reads a
// bearer — the guard, the sign-out — has to read the same ones, or a browser
// is signed in by one and not signed out by the other.
func TestABearerIsPresentedUnderEitherName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cookies map[string]string
		want    string
	}{
		{"the prefixed name", map[string]string{session.HostCookieName: "host"}, "host"},
		{"the bare name", map[string]string{session.CookieBaseName: "bare"}, "bare"},
		{"both, and the prefixed one wins", map[string]string{
			session.HostCookieName: "host", session.CookieBaseName: "bare"}, "host"},
		{"neither", map[string]string{"another_app": "x"}, ""},
	} {
		r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
		for name, value := range tc.cookies {
			r.AddCookie(&http.Cookie{Name: name, Value: value})
		}
		if got := session.Presented(r); got != tc.want {
			t.Errorf("%s: presented %q, want %q", tc.name, got, tc.want)
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
