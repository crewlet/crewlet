package auth

import (
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// THE CROSS-SITE REQUEST CHECK, and why it is a separate gate from CORS.
//
// # What CORS does not do
//
// A browser's same-origin policy stops a page READING a cross-origin response.
// It does not stop the request being SENT, and it never has: a form post and a
// `fetch` with `credentials: include` both leave the browser and both arrive
// here with whatever the browser attaches automatically. CORS decides whether
// the attacker's page may read the ANSWER, which on a state-changing request
// is the part they do not need.
//
// # Why it landed before the first cookie did
//
// What a browser attaches automatically is a COOKIE — the session cookie a
// sign-in sets, and the flight cookie a provider round trip carries. A bearer
// is attached by script, which a cross-site page cannot do without already
// holding the token, so while bearers were the only credential this check
// refused nothing, and that was exactly when to add it: landing the cookie
// first and the check second would have left a window in which every write on
// this surface was forgeable by any page an operator happened to visit.
//
// # The rule, and why each half of it is shaped the way it is
//
//   - A PRESENT `Origin` THAT DOES NOT MATCH IS REFUSED, on every method that
//     is not a read. This is the whole of the browser case: a browser sends
//     `Origin` on every non-GET it makes, cross-site or not, and cannot be
//     talked out of it.
//   - AN ABSENT `Origin` IS ALLOWED unless the request authenticated by
//     cookie. Every non-browser client sends none — curl, the operator CLI,
//     a CI pipeline — and refusing them would be refusing the callers this
//     API mostly has, to close a hole none of them can be used for: a bearer
//     is not attached by the browser, so a cross-site page cannot make one
//     travel.
//   - A COOKIE WITH NO `Origin` IS REFUSED, which is the one arm the rule
//     above must not swallow. A browser always sends the header on a non-GET,
//     so a cookie-authenticated request without one did not come from the
//     browser the cookie was issued to.
//
// # What counts as a match
//
// `api.external_url`'s own origin, and any `api.auth.allowed_origins` entry.
// The second is the same list CORS reads and it means the same thing here:
// another address this deployment is genuinely reached on. A deployment
// behind two hostnames has to name both, or the second one's writes are
// refused with a message that says exactly that.

// CSRF refuses a state-changing request that a cross-site page could have
// caused.
type CSRF struct {
	// origins are the addresses a browser may legitimately be at: the
	// deployment's own external URL, plus every configured allowance.
	origins []string
}

// NewCSRF reads the permitted origins out of Tier A.
//
// A NIL BOOTSTRAP PERMITS NOTHING, which is the fail-closed direction and the
// same answer [NewCORS] gives: nobody has said where this deployment is
// reached, so no cross-origin claim can be believed. Every request with no
// `Origin` still passes, which is what keeps a guard built with no config from
// refusing the CLI.
func NewCSRF(b *config.Bootstrap) *CSRF {
	if b == nil {
		return &CSRF{}
	}
	out := make([]string, 0, len(b.API.Auth.AllowedOrigins)+1)
	if own := originOf(b.API.ExternalBase()); own != "" {
		out = append(out, own)
	}
	for _, allowed := range b.API.Auth.AllowedOrigins {
		if origin := originOf(allowed); origin != "" && !slices.Contains(out, origin) {
			out = append(out, origin)
		}
	}
	return &CSRF{origins: out}
}

// originOf is the `scheme://host[:port]` a URL is at, which is the only shape
// a browser's `Origin` header ever takes.
//
// A PATH IS DROPPED RATHER THAN REFUSED. `api.external_url` legitimately
// carries one — a path-routing proxy needs it — and a browser reaching this
// deployment at `https://ops.example.com/crewlet` still sends
// `Origin: https://ops.example.com`, so comparing the whole value would refuse
// every write from exactly the deployment shape the path is there for.
func originOf(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	return parsed.Scheme + "://" + parsed.Host
}

// Permits reports whether an origin is one this deployment is reached at.
func (c *CSRF) Permits(origin string) bool {
	return slices.Contains(c.origins, origin)
}

// serverEdges are the paths this check does not judge, and they are exactly
// the ones a SERVER reaches rather than a browser: a vendor's webhook delivery
// and the two per-run token paths a sandbox box calls. Each verifies a
// credential of its own that no browser attaches — a signature over the body,
// a signed token in the path — so a page on another site cannot make one
// travel, and refusing a delivery for the `Origin` a server never sends would
// take every integration off the air.
//
// NOT [Unguarded], which it used to be, and the difference is the sign-in
// surface. The routes that are unguarded because a login cannot require a
// login — the sign-in, the first operator's bootstrap, an invitation's
// redemption — are BROWSER routes that change state, and exempting them here
// left login CSRF open: a form on somebody else's page could post an
// attacker's credentials to `/auth/login`, or redeem an attacker's invitation,
// and leave the victim's browser signed in as the attacker, doing their work in
// an account the attacker can read. What stands in for the guard on those
// routes is the throttle AND this check; the guard is what they are exempt
// from, never the cross-site rule.
var serverEdges = []string{WebhookPrefix, OTLPPrefix, mcpbridge.PathPrefix}

// serverEdge reports whether a path is one this check does not judge — see
// [serverEdges].
func serverEdge(path string) bool {
	for _, prefix := range serverEdges {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	return false
}

// Middleware refuses a cross-site state change.
//
// IT WRAPS INSIDE THE GUARD, unlike CORS, and the order is what makes the
// cookie arm reachable: the credential SHAPE decides whether a missing
// `Origin` is a refusal, and nothing before the guard knows which shape
// arrived.
func (c *CSRF) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsRead(r.Method) || serverEdge(r.URL.Path) {
			// A READ CHANGES NOTHING, and a server edge is reached by
			// a server verifying a credential of its own — see
			// [serverEdges] for why that list is not [Unguarded].
			next.ServeHTTP(w, r)
			return
		}
		switch origin := r.Header.Get("Origin"); {
		case origin == "":
			if c.cookieAuthenticated(r) {
				c.refuse(w, r, "a cookie-authenticated request carried no Origin")
				return
			}
		case !c.Permits(origin):
			c.refuse(w, r, "Origin "+origin+" is not an address this deployment is reached at")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cookieAuthenticated reports whether this request's credential is one the
// BROWSER attached rather than one a client chose to send.
//
// THE DISTINCTION IS THE WHOLE RULE. A bearer header is attached by script,
// and a cross-site page holding no token cannot make one travel; a cookie is
// attached by the browser to every request to this origin, whatever page
// caused it.
func (c *CSRF) cookieAuthenticated(r *http.Request) bool {
	// BOTH SPELLINGS, because which one a deployment issues follows its
	// external URL's scheme: `__Host-` requires Secure, so a loopback http
	// deployment carries the bare name. Checking one would leave the other
	// half of the deployments unprotected, and it is the http one — the
	// half with no TLS under it — that would be missed.
	for _, name := range session.CookieNames {
		if _, err := r.Cookie(name); err == nil {
			return true
		}
	}
	return false
}

// refuse answers a cross-site state change, naming what to change.
func (c *CSRF) refuse(w http.ResponseWriter, r *http.Request, why string) {
	log.Warn("api_csrf_refused", "route", r.URL.Path, "reason", why,
		"remote", remoteHost(r))
	httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeCSRFOrigin,
		map[string]string{
			"detail": why,
			"hint": "a browser sends Origin on every state-changing request; " +
				"name this deployment's address in api.external_url, and any " +
				"other hostname it is reached at in api.auth.allowed_origins",
		})
}
