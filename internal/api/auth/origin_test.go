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
