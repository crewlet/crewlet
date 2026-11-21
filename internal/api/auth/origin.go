package auth

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// THE CROSS-SITE REQUEST CHECK, a separate gate from CORS: CORS stops a page
// READING a cross-origin answer, not the request being SENT, and a form post
// or credentialed `fetch` arrives with whatever cookie the browser attaches.
// A bearer is attached by script, so only the session cookie is forgeable.
// For every non-read method:
//   - A present `Origin` that does not match is refused (a browser always
//     sends one on a non-GET).
//   - An absent `Origin` is allowed (curl, the CLI, CI send none) unless the
//     request authenticated by cookie, which is refused: a browser always sends
//     the header, so it did not come from the browser the cookie was issued to.
// A match is `api.external_url`'s origin or an `api.auth.allowed_origins` entry.

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
	if own := Origin(b.API.ExternalBase()); own != "" {
		out = append(out, own)
	}
	for _, allowed := range b.API.Auth.AllowedOrigins {
		if origin := Origin(allowed); origin != "" && !slices.Contains(out, origin) {
			out = append(out, origin)
		}
	}
	return &CSRF{origins: out}
}

// Origin is the `scheme://host[:port]` a URL is at, spelled as a browser's
// `Origin` header spells it, or "" for a value that names no origin.
//
// A path is dropped, not refused: `api.external_url` may carry one (a proxy),
// the browser's `Origin` has none. The rest takes the browser's serialized
// spelling: lower-case scheme and host, canonical IP (`[::1]`), the port
// omitted when it is the scheme's default. Both sides of a comparison go
// through here, so two spellings of one address compare equal.
//
// What cannot be spelled so the configuration refuses (config.checkBrowserHost):
// non-ASCII hosts, IPv4 not in four decimal octets, IPv6 zones. Exported so
// the CLI states its origin by the same rule.
func Origin(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Hostname() == "" {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	host := strings.ToLower(parsed.Hostname())
	if addr, err := netip.ParseAddr(host); err == nil {
		host = addr.String()
	}
	port := parsed.Port()
	if port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return ""
		}
		port = strconv.FormatUint(n, 10)
	}
	if port == defaultPorts[scheme] {
		port = ""
	}
	if port != "" {
		return scheme + "://" + net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return scheme + "://[" + host + "]"
	}
	return scheme + "://" + host
}

// defaultPorts are the ports a browser leaves out of an origin it serializes.
var defaultPorts = map[string]string{"http": "80", "https": "443"}

// Permits reports whether an origin is one this deployment is reached at,
// compared in the browser's spelling ([Origin]) on both sides.
func (c *CSRF) Permits(origin string) bool {
	canonical := Origin(origin)
	return canonical != "" && slices.Contains(c.origins, canonical)
}

// The paths this check does not judge are [Public]'s, exactly: the ones a
// SERVER reaches (webhook deliveries and the per-run token edges a box calls).
// Each verifies a credential no browser attaches, and refusing them for the
// `Origin` a server never sends would take every integration off the air. One
// list, so the skipped edges and the public listener's routes cannot drift.
//
// NOT [Unguarded]: its sign-in, invitation and password-reset routes are
// BROWSER routes that change state, and exempting them would leave login CSRF
// open (a foreign form posting an attacker's credentials to `/auth/login`).
// The throttle AND this check stand in for the guard there; they are exempt
// from the guard, never from the cross-site rule.

// Middleware refuses a cross-site state change.
//
// IT WRAPS INSIDE THE GUARD, unlike CORS, and the order is what makes the
// cookie arm reachable: the credential SHAPE decides whether a missing
// `Origin` is a refusal, and nothing before the guard knows which shape
// arrived.
func (c *CSRF) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if IsRead(r.Method) || Public(r.URL.Path) {
			// A READ CHANGES NOTHING, and a public route is reached by
			// a server verifying a credential of its own — see above
			// for why that list is not [Unguarded].
			next.ServeHTTP(w, r)
			return
		}
		if c.RefuseCrossSite(w, r) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// RefuseCrossSite judges one request's `Origin` by the rule above, whatever
// its method, answers the refusal itself and reports whether it refused.
//
// Exported for the live socket, the one GET that must be judged: a handshake
// is a GET, so [CSRF.Middleware] passes it as a read, yet it streams the
// company's state under a cookie the browser attaches cross-site. The
// WebSocket library compares `Origin` with `Host`, refusing allowed_origins
// entries and proxies rewriting `Host`; one rule keeps socket and writes agreed.
// Ask it only of a real handshake: the dashboard's same-origin GET of the path
// carries the cookie but no `Origin`.
func (c *CSRF) RefuseCrossSite(w http.ResponseWriter, r *http.Request) bool {
	switch origin := r.Header.Get("Origin"); {
	case origin == "":
		if c.cookieAuthenticated(r) {
			c.refuse(w, r, "The request had no Origin header.")
			return true
		}
	case !c.Permits(origin):
		c.refuse(w, r, origin+" isn't an address for this deployment.")
		return true
	}
	return false
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
			"hint": "Set this deployment's address in `api.external_url`, " +
				"and any other hostname in `api.auth.allowed_origins`.",
		})
}
