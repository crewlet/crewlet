// Package session mints and validates the signed cookie a browser holds, and
// resolves it to the person and the seat behind it.
//
// # What a bearer is, and what it is not
//
// It is SIGNATURE-STATELESS AND ROW-STATEFUL, and neither half works alone. A
// purely stateless cookie cannot be revoked before it expires, so off-boarding
// somebody would be impossible. A purely stateful one — an opaque id into a
// table — costs a database read for every forged cookie an attacker sends and
// has nothing at all to say until the row it names has been applied on the
// node the request reached. Signed plus row-backed gets both: the signature,
// the deadlines and the person's revocation epoch are checked with no I/O, and
// the row supplies revocation and the "sign me out everywhere" listing from
// THIS NODE'S OWN replicated copy, with no network hop on the request path.
//
// # There is no validation cache, anywhere
//
// Not an omission — there is nothing to cache. The lookup is a local read of a
// replicated row and a map lookup on a pinned chart view, and the store is
// never on a network path from the request. A cache here would add a second
// idea of who is signed in, with its own staleness, in front of a read that is
// already local — and the thing it would be caching is exactly the thing a
// revocation has to invalidate within an applier's lag.
//
// # 503 AND NEVER 401 ON A NODE THAT IS BEHIND
//
// The rule that shapes both tables in this package. A browser reads 401 as
// "sign in again" and discards the cookie, so one stalled applier answering
// 401 would log every person on that node out and stampede the identity
// provider with the re-authentications. A node that cannot tell whether a
// session is valid says so — 503 — and a node that KNOWS it is not says 401.
// The grace exists only for the arm a lagging node can honestly serve, reads
// of a session it has not yet seen, and it ends at [statelog.StallGrace],
// which is the same sixty seconds the alarm table already calls a stall: a
// node serving stale identity is by definition a node already alarmed.
//
// # The keyring is required, and the per-process fallback is refused
//
// [runtoken] falls back to a per-process random key when a deployment has no
// keyring, which is correct for an endpoint only that process ever verifies.
// It is catastrophic here: every ingress node would derive a different key and
// reject every peer's cookies, so a browser would be signed in on whichever
// node its request happened to reach. So [New] REFUSES a keyring that cannot
// sign for the fleet, by name, rather than minting cookies nobody else can
// read.
//
// What it does share with [runtoken] is the KEY TAG: a bearer names the key
// that signed it, a verifier looks that tag up, and a tag it does not hold is
// refused rather than retried against the active key. That is what makes a
// keyring rotation zero-downtime — add a key, restart, flip active, restart,
// drop the old one once the absolute session lifetime has passed — and
// dropping a key early is what ends live sessions.
package session

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// Version prefixes every bearer, so a later format is told from this one
// rather than failing as a bad signature.
const Version = "v2"

// SigningDomain separates a session bearer from every other thing this fleet's
// keyring signs.
//
// WITHOUT IT a cookie would validate at the telemetry receiver and a per-run
// token would validate here: all of them are HMACs over the same keyring, and
// a subject is just a string. It is the same reason a signing key is never
// reused across protocols, and [runtoken.KeyTag] binds it into the tag as well
// as into the key, so a tag minted for one domain does not even resolve in
// another.
const SigningDomain = "crewlet/iam/session/v2"

// Idle is how long a session survives with nothing happening on it.
//
// A CONSTANT AND NOT A CONFIGURED FIELD, unlike the absolute lifetime and the
// rotation period, because it RIDES IN EVERY BEARER: a node moves the deadline
// out by this much on a re-issue, and two nodes disagreeing about it would
// hand one browser two different expiries depending on which node it reached.
// A configured value would also mean a deployment that lowered it did not
// shorten a single outstanding cookie, because the deadline in the signature
// was already written.
//
// TWELVE HOURS, which is one working day plus the evening: long enough that
// nobody is signed out mid-task or over lunch, short enough that a laptop left
// open overnight is signed out by morning. The absolute deadline is what
// bounds a session that IS being used, and it is configured, because how long
// a company lets one sign-in last is a policy rather than a mechanism.
const Idle = 12 * time.Hour

// ReissueAfter is how stale a cookie may be before a request gets a fresh one.
//
// FIVE MINUTES, and what it trades is precision in the idle deadline against
// a Set-Cookie header on every single response. At five minutes a session's
// real idle window is [Idle, Idle+5m] rather than exactly Idle, which nobody
// can perceive, and a page making twenty requests re-issues once rather than
// twenty times. It costs no store write at all — the deadline is in the
// signature — so the only cost being managed here is the header.
const ReissueAfter = 5 * time.Minute

// Overlap is how long a cookie from the window either side of a rotation
// boundary is still served.
//
// TWO MINUTES, and it exists for requests IN FLIGHT ACROSS THE BOUNDARY: a
// page that loaded at 10:59:58 and fires four parallel requests at 11:00:01
// must not have three of them refused because the window turned over between
// the first and the rest. It also absorbs the ordinary skew between two
// ingress nodes' clocks, which is what makes the forward arm safe.
const Overlap = 2 * time.Minute

// DefaultRotateAfter is how long one rotation window lasts when nothing says
// otherwise.
//
// ONE HOUR. What it bounds is how long a CAPTURED cookie goes on looking
// current beside the one its owner keeps being re-issued — see rotate.go for
// what the index can and cannot prove. It is a default rather than the value:
// the configured field lands with the rest of the session block.
const DefaultRotateAfter = time.Hour

// CookieBaseName is the bearer's name without the `__Host-` prefix.
const CookieBaseName = "crewlet_session"

// HostCookieName is the name a bearer takes on an https deployment.
//
// THE `__Host-` PREFIX IS WHAT MAKES THE COOKIE UNFORGEABLE BY A SIBLING: a
// browser refuses to set one unless it is Secure, path `/` and carries no
// Domain attribute, so `evil.example.com` cannot write a session cookie that
// `crewlet.example.com` would then read. It is also why the name has to change
// on a plain-http deployment rather than the prefix simply being ignored — a
// browser rejects the Set-Cookie outright, and the sign-in would appear to
// succeed and then not stick.
const HostCookieName = "__Host-" + CookieBaseName

// CookieName is what the bearer is set under for a deployment reachable at
// externalURL.
//
// THE SCHEME DECIDES, and it has to be the CONFIGURED url rather than the
// request: the engine sits behind a TLS-terminating proxy and reads no
// `r.TLS`, so asking the request would make every deployment look like plain
// http and drop the prefix on exactly the deployments that need it.
//
// ANYTHING THAT IS NOT PLAIN http TAKES THE PREFIX, including an unparseable
// or empty value. The failure directions are not symmetric: a prefixed cookie
// on a loopback http deployment does not stick and somebody notices during
// setup, while an unprefixed cookie on a public https deployment works
// perfectly and silently accepts one a sibling subdomain wrote.
func CookieName(externalURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(externalURL))
	if err == nil && parsed.Scheme == "http" {
		return CookieBaseName
	}
	return HostCookieName
}

// Cookie is the bearer as a browser must be given it.
//
// ONE PLACE THAT DECIDES THE ATTRIBUTES, because there are five of them and
// every one is load-bearing: `HttpOnly` keeps it out of reach of any script
// on the page, `Secure` is what the `__Host-` prefix requires, `Path=/` is
// what it requires as well AND what makes one cookie cover the whole API,
// `SameSite=Lax` is what stops a cross-site form posting as somebody, and NO
// `Domain` attribute is the third thing the prefix requires. Written at each
// call site they drift, and every one of them drifts SILENTLY — the sign-in
// still works.
//
// THE EXPIRY IS THE ABSOLUTE DEADLINE, never the idle one. A browser that
// dropped the cookie at the idle deadline would sign somebody out at exactly
// the moment a re-issue was about to move it; one with no expiry at all holds
// a cookie that can never work again, and re-presents it on every request for
// ever. The absolute deadline is the one instant after which the value is
// certainly dead.
func Cookie(externalURL, value string, expires time.Time) *http.Cookie {
	return &http.Cookie{
		Name:     CookieName(externalURL),
		Value:    value,
		Path:     "/",
		Expires:  expires.UTC(),
		HttpOnly: true,
		Secure:   CookieName(externalURL) == HostCookieName,
		SameSite: http.SameSiteLaxMode,
	}
}

// Clear is the cookie that ends a session in the browser.
//
// THE SAME ATTRIBUTES AS [Cookie], which is why it is here rather than at the
// sign-out route: a browser matches a deletion on name, path and domain, so a
// clear that differs in any of them leaves the original in place and the
// person signed in — visibly signed out, still holding a live bearer.
func Clear(externalURL string) *http.Cookie {
	out := Cookie(externalURL, "", time.Unix(0, 0))
	out.MaxAge = -1
	return out
}

// CookieNames are the two names a browser can be holding a bearer under, in
// the order a request's are read.
//
// BOTH, AND IN ONE PLACE. The scheme of `api.external_url` decides which name
// a node ISSUES, but a browser keeps whatever it was issued: a deployment that
// corrected its URL from http to https has every signed-in browser still
// presenting the bare name. Everything that READS a bearer therefore reads
// both, and everything that ENDS one clears both — and the sign-out read only
// the configured name and cleared only it, so after such a correction a
// person who signed out stayed signed in, their session never closed. The
// prefixed name first, because it is the one this deployment issues when it
// can and the only one a sibling host cannot plant.
var CookieNames = []string{HostCookieName, CookieBaseName}

// Presented is the bearer a request carries under either name, or empty.
//
// It reads nothing else and decides nothing: a value under either name still
// has to verify, so reading both discloses nothing and costs a map lookup.
func Presented(r *http.Request) string {
	for _, name := range CookieNames {
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			return c.Value
		}
	}
	return ""
}

// Clears are the cookies that end a session in a browser under EVERY name one
// can be held under — [Clear] for the name this deployment issues, and a
// deletion for the other.
//
// A DELETION MATCHES ON NAME, PATH AND DOMAIN, never on the security flags,
// so the second only has to agree with [Cookie] on those three — which it
// does, carrying Path `/` and no Domain. It is Secure exactly when its name
// requires it: a `__Host-` Set-Cookie without Secure is refused outright, so
// the prefixed deletion always carries it (and a plain-http deployment,
// which can hold no prefixed cookie, merely has it ignored), while the bare
// deletion is plain, which a secure page may set and an insecure one must.
func Clears(externalURL string) []*http.Cookie {
	issued := Clear(externalURL)
	out := []*http.Cookie{issued}
	for _, name := range CookieNames {
		if name == issued.Name {
			continue
		}
		out = append(out, &http.Cookie{
			Name: name, Value: "", Path: "/", Expires: time.Unix(0, 0).UTC(),
			MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode,
			Secure: name == HostCookieName,
		})
	}
	return out
}

// ErrNoKeyring reports a deployment whose Tier A keyring cannot sign for the
// fleet.
//
// ITS OWN SENTINEL because the caller's move is specific and nothing else
// produces it: an operator has to set `secrets.keys`, and until they do the
// honest thing is to refuse to serve sign-ins rather than to mint cookies each
// node rejects on the next request that lands somewhere else.
var ErrNoKeyring = errors.New("session: no fleet keyring")

// Options configure [New].
type Options struct {
	// Material is the fleet's Tier A keyring. One that cannot sign for the
	// fleet is REFUSED — see the package doc.
	Material runtoken.Material

	// RotateAfter is one rotation window. Zero takes
	// [DefaultRotateAfter].
	RotateAfter time.Duration

	// Now is the clock.
	//
	// WALL CLOCK, NOT MONOTONIC, and there is no choice about it: every
	// instant a bearer carries was written by another process on another
	// node, and a monotonic reading's epoch is per-boot — meaningless
	// anywhere but where it was taken. rotate.go states what that costs.
	Now func() time.Time
}

// Signer mints and validates bearers.
//
// SAFE FOR CONCURRENT USE and holding no mutable state: a bearer is a function
// of the keyring, the session's own facts and the clock. There is nothing here
// to invalidate, which is the same sentence as "there is no validation cache".
type Signer struct {
	activeTag   string
	keys        map[string][]byte
	rotateAfter time.Duration
	now         func() time.Time
}

// New builds a signer, or refuses the deployment.
func New(opts Options) (*Signer, error) {
	if !opts.Material.Usable() {
		return nil, fmt.Errorf("%w: `secrets.keys` names no key this node "+
			"holds, so every ingress node would sign cookies under a key of "+
			"its own and reject every other node's — a browser would be "+
			"signed in on whichever node its request happened to reach: %w",
			ErrNoKeyring, errNoActiveKey(opts.Material))
	}
	s := &Signer{
		keys:        map[string][]byte{},
		rotateAfter: opts.RotateAfter,
		now:         opts.Now,
	}
	if s.rotateAfter <= 0 {
		s.rotateAfter = DefaultRotateAfter
	}
	if s.now == nil {
		s.now = time.Now
	}
	for _, key := range opts.Material.Keys {
		tag := runtoken.KeyTag(SigningDomain, key.ID)
		s.keys[tag] = runtoken.DeriveKey(SigningDomain, key.ID, key.Material)
		if key.ID == opts.Material.ActiveID {
			s.activeTag = tag
		}
	}
	return s, nil
}

// errNoActiveKey says which half of the keyring is missing, because the two
// need different edits: an empty keyring is a missing `secrets.keys` block,
// and an active id naming nothing is a typo in one line of one that exists.
func errNoActiveKey(m runtoken.Material) error {
	if len(m.Keys) == 0 {
		return errors.New("the keyring is empty")
	}
	held := make([]string, 0, len(m.Keys))
	for _, key := range m.Keys {
		held = append(held, key.ID)
	}
	return fmt.Errorf("`secrets.active_key` names %q and the keyring holds %v",
		m.ActiveID, held)
}
