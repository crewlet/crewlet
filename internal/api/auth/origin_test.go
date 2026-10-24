package auth_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// csrfFixture is a CSRF gate over a deployment reached at one address, with
// one extra allowance.
func csrfFixture(t *testing.T) *auth.CSRF {
	t.Helper()
	b := config.DefaultBootstrap()
	b.API.Port = config.DefaultAPIPort
	b.API.ExternalURL = "https://crewlet.example.com/behind-a-proxy"
	b.API.Auth.AllowedOrigins = []string{"https://ops.example.com"}
	return auth.NewCSRF(&b)
}

// send runs one request through the gate and reports the status and whether
// the handler beneath it ran.
func send(t *testing.T, c *auth.CSRF, method, path string,
	headers map[string]string, cookies ...*http.Cookie,
) (int, bool) {
	t.Helper()
	ran := false
	handler := c.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	for _, cookie := range cookies {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec.Code, ran
}

// A CROSS-SITE ORIGIN IS REFUSED ON EVERY STATE CHANGE.
//
// This is the whole of the browser case: a browser sends `Origin` on every
// non-GET it makes, cross-site or not, and cannot be talked out of it. CORS
// does not cover it — same-origin policy stops an attacker's page READING the
// answer, and on a write that is the part they do not need.
func TestACrossSiteOriginIsRefused(t *testing.T) {
	t.Parallel()
	c := csrfFixture(t)
	status, ran := send(t, c, http.MethodPost, "/config",
		map[string]string{"Origin": "https://evil.example.com"})
	if status != http.StatusForbidden {
		t.Errorf("status = %d, want 403", status)
	}
	if ran {
		t.Error("the handler ran, so the write was carried out before the refusal")
	}
}

// AND THE DEPLOYMENT'S OWN ADDRESS IS NOT, which is the control: without it
// the case above would pass on a gate that refused everything.
//
// THE PATH IS DROPPED FROM THE COMPARISON, and that is what this pins:
// `api.external_url` legitimately carries one — a path-routing proxy needs it
// — while a browser at `https://crewlet.example.com/behind-a-proxy` still
// sends `Origin: https://crewlet.example.com`. Compared whole, every write
// from exactly the deployment shape the path exists for would be refused.
func TestTheDeploymentsOwnOriginIsPermitted(t *testing.T) {
	t.Parallel()
	c := csrfFixture(t)
	for _, origin := range []string{
		"https://crewlet.example.com", // the external URL, path dropped
		"https://ops.example.com",     // a configured allowance
	} {
		status, ran := send(t, c, http.MethodPost, "/config",
			map[string]string{"Origin": origin})
		if status != http.StatusOK || !ran {
			t.Errorf("%s = %d (ran %v), want the write to go through",
				origin, status, ran)
		}
	}
}

// AN ADDRESS IS COMPARED AS A BROWSER SPELLS IT, so a deployment configured
// with a host in upper case, or with its scheme's default port written out,
// still permits the origin every browser sends for it: the scheme and host in
// lower case, an IP address in its canonical form, and no default port. Kept
// as written, the first two refused every write from the deployment's own
// address as cross-site.
//
// Mutation: compare parsed.Scheme + "://" + parsed.Host again in Origin and
// every permitted case fails; drop the default-port rule and the two default
// ports (`:443`, `:80`) do; compare the port as text and the two with a
// leading zero do; stop putting the request's Origin in the same spelling and
// the IPv4-mapped one does, whose two sides name one address two ways.
func TestAnAddressIsComparedAsABrowserSpellsIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, external, origin string
		permitted              bool
	}{
		{"a host in upper case", "https://Crewlet.Example.com/app", "https://crewlet.example.com", true},
		{"the scheme's default port", "https://crewlet.example.com:443", "https://crewlet.example.com", true},
		{"plain http's default port", "http://LOCALHOST:80", "http://localhost", true},
		{"an IPv6 address written out", "http://[0:0:0:0:0:0:0:1]:8080", "http://[::1]:8080", true},
		{"the default port with a leading zero", "https://crewlet.example.com:0443", "https://crewlet.example.com", true},
		{"another port with a leading zero", "https://crewlet.example.com:08443", "https://crewlet.example.com:8443", true},
		// A browser writes an IPv4-mapped address in hex; netip in dotted form.
		{"an IPv4-mapped address", "http://[::ffff:127.0.0.1]:8080", "http://[::ffff:7f00:1]:8080", true},
		{"another port", "https://crewlet.example.com:8443", "https://crewlet.example.com", false},
		{"another scheme", "https://crewlet.example.com", "http://crewlet.example.com", false},
		{"another host", "https://Crewlet.Example.com", "https://crewlet.example.org", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b := config.DefaultBootstrap()
			b.API.Port = config.DefaultAPIPort
			b.API.ExternalURL = tc.external
			c := auth.NewCSRF(&b)
			status, ran := send(t, c, http.MethodPost, "/config", map[string]string{"Origin": tc.origin})
			if got := status == http.StatusOK && ran; got != tc.permitted {
				t.Errorf("external %s, Origin %s: status %d (ran %v), want permitted %v",
					tc.external, tc.origin, status, ran, tc.permitted)
			}
		})
	}
}

// A CLIENT THAT IS NOT A BROWSER SENDS NO ORIGIN, and refusing it would refuse
// the callers this API mostly has: curl, the operator CLI, a CI pipeline.
//
// It is safe precisely because of what those callers present. A bearer is
// attached by SCRIPT, and a cross-site page holding no token cannot make one
// travel — so there is no cross-site request here for the check to refuse.
func TestAnAbsentOriginIsAllowedForABearerClient(t *testing.T) {
	t.Parallel()
	c := csrfFixture(t)
	status, ran := send(t, c, http.MethodPost, "/config",
		map[string]string{"Authorization": "Bearer something"})
	if status != http.StatusOK || !ran {
		t.Errorf("a bearer client with no Origin = %d (ran %v), want it served",
			status, ran)
	}
}

// BUT A COOKIE WITH NO ORIGIN IS REFUSED, which is the one arm the rule above
// must not swallow.
//
// A cookie is attached by the BROWSER to every request to this origin,
// whatever page caused it — and a browser always sends `Origin` on a non-GET.
// So a cookie-authenticated request carrying none did not come from the
// browser the cookie was issued to.
func TestACookieWithNoOriginIsRefused(t *testing.T) {
	t.Parallel()
	c := csrfFixture(t)
	// BOTH SPELLINGS, because which one a deployment issues follows its
	// external URL's scheme, and checking one would leave the http half —
	// the half with no TLS under it — unprotected.
	for _, name := range []string{session.HostCookieName, session.CookieBaseName} {
		status, ran := send(t, c, http.MethodPost, "/config", nil,
			&http.Cookie{Name: name, Value: "a-session"})
		if status != http.StatusForbidden || ran {
			t.Errorf("%s with no Origin = %d (ran %v), want 403", name, status, ran)
		}
	}
}

// THE HANDSHAKE RULE JUDGES A GET, which the middleware never does: the live
// socket's handshake is a GET that opens a stream of the company's state, and a
// browser attaches the session cookie to a cross-site page's handshake. Each of
// the three arms, with the permitted address as the control.
func TestRefuseCrossSiteJudgesAHandshakeWhateverItsMethod(t *testing.T) {
	t.Parallel()
	c := csrfFixture(t)
	for _, tc := range []struct {
		name   string
		origin string
		cookie bool
		refuse bool
	}{
		{"another site", "https://evil.example.com", false, true},
		{"a cookie and no Origin", "", true, true},
		{"a bearer client and no Origin", "", false, false},
		{"the deployment's own address, with a cookie", "https://crewlet.example.com", true, false},
		{"an allowance", "https://ops.example.com", false, false},
	} {
		req := httptest.NewRequest(http.MethodGet, "/ws/stream", nil)
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		if tc.cookie {
			req.AddCookie(&http.Cookie{Name: session.HostCookieName, Value: "a-session"})
		}
		rec := httptest.NewRecorder()
		refused := c.RefuseCrossSite(rec, req)
		if refused != tc.refuse {
			t.Errorf("%s: refused = %v, want %v", tc.name, refused, tc.refuse)
		}
		if refused && rec.Code != http.StatusForbidden {
			t.Errorf("%s: a refusal answered %d, want 403", tc.name, rec.Code)
		}
		if !refused && rec.Code != http.StatusOK {
			t.Errorf("%s: an admitted handshake had an answer written (%d)", tc.name, rec.Code)
		}
	}
}

// A READ CHANGES NOTHING, so it is never refused for its origin. Refusing one
// would break every cross-origin dashboard the CORS allowance exists to serve,
// to prevent a request that alters nothing.
func TestAReadIsNeverRefusedForItsOrigin(t *testing.T) {
	t.Parallel()
	c := csrfFixture(t)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		status, ran := send(t, c, method, "/events",
			map[string]string{"Origin": "https://evil.example.com"})
		if status != http.StatusOK || !ran {
			t.Errorf("%s = %d (ran %v), want a read served", method, status, ran)
		}
	}
}

// THE EXEMPT EDGES ARE NOT BROWSERS. A vendor's webhook delivery is a POST
// from a server, carrying no `Origin` and verifying a signature of its own;
// refused here, every integration would go off the air to close a hole a
// server-to-server call cannot be used for.
func TestTheWebhookEdgeIsNotRefusedForItsOrigin(t *testing.T) {
	t.Parallel()
	c := csrfFixture(t)
	for _, path := range []string{"/webhooks/github", "/otlp/tok/v1/traces", "/mcp/tok"} {
		status, ran := send(t, c, http.MethodPost, path, nil,
			&http.Cookie{Name: session.CookieBaseName, Value: "a-session"})
		if status != http.StatusOK || !ran {
			t.Errorf("%s = %d (ran %v), want the delivery served", path, status, ran)
		}
	}
}

// A SIGN-IN ROUTE IS A BROWSER ROUTE, AND IT IS JUDGED LIKE ONE.
//
// The routes a credential cannot guard — a login cannot require a login — are
// exempt from the GUARD, and they used to be exempt from this check too, which
// left login CSRF open: a form on somebody else's page could post an
// attacker's credentials to the sign-in or redeem an attacker's invitation,
// and leave the victim's browser signed in as somebody the attacker controls.
// Walked over every one of them, with the controls beside each: the
// deployment's own origin is served, and so is a client that sends no Origin
// and holds no cookie — the operator CLI signing in — while a cookie with no
// Origin is refused as it is everywhere.
//
// Mutation: exempt [auth.Unguarded] again and every cross-site case goes
// through.
func TestASignInRouteIsRefusedForACrossSiteOrigin(t *testing.T) {
	t.Parallel()
	c := csrfFixture(t)
	for _, path := range []string{
		auth.PathAuthLogin, auth.PathAuthLogout,
		auth.AuthInvitePrefix + "an-invitation",
	} {
		status, ran := send(t, c, http.MethodPost, path,
			map[string]string{"Origin": "https://evil.example.com"})
		if status != http.StatusForbidden || ran {
			t.Errorf("%s from another site = %d (ran %v), want 403 before "+
				"the handler", path, status, ran)
		}
		if status, ran := send(t, c, http.MethodPost, path,
			map[string]string{"Origin": "https://crewlet.example.com"}); status != http.StatusOK || !ran {
			t.Errorf("%s from the deployment's own origin = %d (ran %v), "+
				"want it served", path, status, ran)
		}
		if status, ran := send(t, c, http.MethodPost, path, nil); status != http.StatusOK || !ran {
			t.Errorf("%s from a client sending no Origin and no cookie = %d "+
				"(ran %v), want it served — that is the CLI signing in",
				path, status, ran)
		}
		if status, ran := send(t, c, http.MethodPost, path, nil,
			&http.Cookie{Name: session.CookieBaseName, Value: "a-session"}); status != http.StatusForbidden || ran {
			t.Errorf("%s carrying a cookie and no Origin = %d (ran %v), want 403",
				path, status, ran)
		}
	}
}

// A DEPLOYMENT THAT NAMES NO ADDRESS PERMITS NO ORIGIN, which is the
// fail-closed direction: nobody has said where this deployment is reached, so
// no cross-origin claim can be believed. A client sending none still passes,
// which is what keeps a guard built with no config from refusing the CLI.
func TestANilBootstrapPermitsNoOrigin(t *testing.T) {
	t.Parallel()
	c := auth.NewCSRF(nil)
	if status, _ := send(t, c, http.MethodPost, "/config",
		map[string]string{"Origin": "https://crewlet.example.com"}); status != http.StatusForbidden {
		t.Errorf("status = %d, want 403 from a deployment that names no address", status)
	}
	if status, ran := send(t, c, http.MethodPost, "/config", nil); status != http.StatusOK || !ran {
		t.Errorf("a client sending no Origin = %d (ran %v), want it served", status, ran)
	}
}

// THE REFUSAL NAMES WHAT TO CHANGE. "403" alone sends an operator whose
// deployment is reached at a second hostname looking at their credentials,
// which is the one thing that is not wrong.
func TestTheCsrfRefusalNamesTheSettings(t *testing.T) {
	t.Parallel()
	c := csrfFixture(t)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/config", nil)
	req.Header.Set("Origin", "https://second-name.example.com")
	c.Middleware(http.NotFoundHandler()).ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"csrf_origin", "api.external_url", "allowed_origins"} {
		if !strings.Contains(body, want) {
			t.Errorf("the refusal does not name %s: %s", want, body)
		}
	}
}
