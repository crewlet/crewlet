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
// replicated row and a map lookup on the running organisation, and the store is
// never on a network path from the request. A cache here would add a second
// idea of who is signed in, with its own staleness, in front of a read that is
// already local — and the thing it would be caching is exactly the thing a
// revocation has to invalidate within an applier's lag.
//
// # 503 AND NEVER 401 ON A NODE THAT IS BEHIND
//
// The rule that shapes both tables in this package. A browser reads 401 as
// "sign in again" and discards the cookie, so one stalled applier answering
// 401 would log every person on that node out and send them all back to the
// sign-in form at once. A node that cannot tell whether a
// session is valid says so — 503 — and a node that KNOWS it is not says 401.
// The grace exists only for the arm a lagging node can honestly serve, reads
// of a session it has not yet seen, and it ends at [statelog.StallGrace],
// which is the same sixty seconds the alarm table already calls a stall: a
// node serving stale identity is by definition a node already alarmed.
//
// # The keyring is required, and a per-process key is refused
//
// A key of this process's own would be catastrophic here: every ingress node
// would derive a different key and reject every peer's cookies, so a browser
// would be signed in on whichever node its request happened to reach. So [New]
// REFUSES a keyring that cannot sign for the fleet, by name, rather than
// minting cookies nobody else can read — as [runtoken.New] does for a per-run
// token, which once took a per-process key and no longer does. Tier A refuses
// a file without a usable keyring and the engine refuses to start without one,
// so what reaches here unusable is a caller's mistake rather than a posture.
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
const Version = "v3"

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
// A CONSTANT AND NOT A CONFIGURED FIELD, unlike the absolute lifetime, because
// it RIDES IN EVERY BEARER: a node moves the deadline
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
//
// EVERY RE-ISSUE MOVES THE COOKIE'S BYTES, so nothing keys a cache on them: what
// a browser's cache keeps is scoped to who it is signed in as by [Begin] and
// [End], and a re-issue ([Continue]) leaves it alone.
const ReissueAfter = 5 * time.Minute

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
	if HostPrefixed(externalURL) {
		return HostCookieName
	}
	return CookieBaseName
}

// HostPrefixed reports whether a deployment reachable at externalURL names its
// cookies with the `__Host-` prefix — everything but plain http, for
// [CookieName]'s reasons — which is also whether they must be Secure.
func HostPrefixed(externalURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(externalURL))
	return err != nil || parsed.Scheme != "http"
}

// Begin hands a browser a session that may be somebody else's than the one it
// held — a sign-in, by any way in: a password, an invitation's redemption, a
// Tier A token's exchange — as the bearer's cookie, and empties the browser's
// HTTP cache for this origin.
//
// # What a browser's cache keeps is who it is signed in as
//
// An answer read by object id is kept by the browser that asked for it, for up
// to a year (internal/api/httpjson's Immutable), under a cache key that holds
// the `Authorization` header and NOT this cookie. The cookie cannot be in it:
// its bytes move at every re-issue ([ReissueAfter], [Continue]), so a key
// holding them stopped matching five minutes after an answer was kept, and the
// browser asked the node again for every page a person went back to — the
// read, on the repository's home git node, that the cache exists to save. So
// what a browser keeps is scoped to who it is signed in as from the other
// side: every response that may change who that is — this one, and every one
// that ends a session ([End]) — carries `Clear-Site-Data: "cache"`, and the
// next person to sign in on a browser is never answered from what the last one
// was sent. A response that carries the same person's session on ([Continue])
// leaves the cache alone.
//
// IT COSTS the browser everything else it kept from this origin — the
// dashboard's own bundle among it, fetched again once — at each sign-in and
// sign-out, and Chrome's own tracker records that emptying a large cache this
// way can hold the response for seconds (crbug.com/40233601). Both are paid
// once a session, at its two ends, where keying the cache on the cookie paid,
// after every re-issue, a read on the node for each kept answer asked again.
//
// A BROWSER HONOURS IT ONLY IN A SECURE CONTEXT — https, or a loopback address
// — and only where it implements the directive: Chrome from 65, Safari from
// 17, and Firefox from 63 to 93 and again from 138. Chrome's tracker also
// records that a tab open before it may still be answered from what it kept
// until the tab is reloaded (crbug.com/364634040). Wherever it is not
// honoured, what a person was answered stays in the browser after their
// session, until the browser evicts it, as anything a browser keeps does. That
// is why an answer may be kept only where it is the same for every caller who
// may ask it (internal/api/queries' Immutable): the next person to sign in
// there is served nothing they would not be answered themselves.
func Begin(w http.ResponseWriter, externalURL, bearer string, expires time.Time) {
	w.Header().Set(clearSiteData, clearCache)
	http.SetCookie(w, bearerCookie(externalURL, bearer, expires))
}

// Continue hands a browser the bearer that carries on the session it holds
// FOR THE SAME PERSON — a re-issue's, the idle deadline moved
// ([Validation.Reissue]), and the session a step-up, an enrolment or a
// password change opens in place of the one that person presented — and
// leaves the browser's cache as it was.
//
// IT EMPTIES NOTHING, which is the whole difference from [Begin]: who the
// browser is signed in as has not changed, so nothing it kept is somebody
// else's answer. A re-issue comes every [ReissueAfter] of use, so emptying the
// cache on one would drop every answer the browser kept five minutes after
// keeping it — the cost keeping the cookie out of the cache key exists to
// remove — and a confirmation would cost the person every answer they kept,
// and a wait while Chrome empties the cache, to prove who they already were.
func Continue(w http.ResponseWriter, externalURL, bearer string, expires time.Time) {
	http.SetCookie(w, bearerCookie(externalURL, bearer, expires))
}

// End ends the session in a browser under every name a bearer can be held
// under — a sign-out, a session the guard refused, a gesture that found its
// person removed — and empties the browser's HTTP cache as [Begin] does, so
// nothing the ended session was answered is served from the browser after it.
func End(w http.ResponseWriter, externalURL string) {
	w.Header().Set(clearSiteData, clearCache)
	for _, deleted := range deletions(externalURL) {
		http.SetCookie(w, deleted)
	}
}

// clearSiteData and clearCache are how [Begin] and [End] empty a browser's
// HTTP cache — `"cache"` and nothing else, the one store of the browser's that
// holds what a node answered. Never `"cookies"`, which would race the cookie
// [Begin] sets beside it and reach every other host under the registrable
// domain; never `"storage"` or `"*"`, which would take what the dashboard
// keeps in the browser's own storage — its preferences, and each reader's
// under a key of their own — none of it an answer a node sent.
const (
	clearSiteData = "Clear-Site-Data"
	clearCache    = `"cache"`
)

// bearerCookie is the bearer as a browser must be given it.
//
// ONE PLACE THAT DECIDES THE ATTRIBUTES, because there are five of them and
// every one is load-bearing: `HttpOnly` keeps it out of reach of any script
// on the page, `Secure` is what the `__Host-` prefix requires, `Path=/` is
// what it requires as well AND what makes one cookie cover the whole API,
// `SameSite=Lax` is what stops a cross-site form posting as somebody, and NO
// `Domain` attribute is the third thing the prefix requires. Written at each
// call site they drift, and every one of them drifts SILENTLY — the sign-in
// still works. UNEXPORTED, with [deletion] and [deletions], so a cookie is set
// only through [Begin], [Continue] and [End], each of which also decides what
// the browser's cache keeps.
//
// THE EXPIRY IS THE ABSOLUTE DEADLINE, never the idle one. A browser that
// dropped the cookie at the idle deadline would sign somebody out at exactly
// the moment a re-issue was about to move it; one with no expiry at all holds
// a cookie that can never work again, and re-presents it on every request for
// ever. The absolute deadline is the one instant after which the value is
// certainly dead.
func bearerCookie(externalURL, value string, expires time.Time) *http.Cookie {
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

// deletion is the cookie that ends a session in the browser.
//
// THE SAME ATTRIBUTES AS [bearerCookie], which is why it is here rather than
// at the sign-out route: a browser matches a deletion on name, path and
// domain, so one that differs in any of them leaves the original in place and
// the person signed in — visibly signed out, still holding a live bearer.
func deletion(externalURL string) *http.Cookie {
	out := bearerCookie(externalURL, "", time.Unix(0, 0))
	out.MaxAge = -1
	return out
}

// CookieNames are the two names a browser can be holding a bearer under, the
// prefixed one first.
//
// BOTH, AND IN ONE PLACE. The scheme of `api.external_url` decides which name
// a node ISSUES, but a browser keeps whatever it was issued: a deployment that
// corrected its URL from http to https has every signed-in browser still
// holding the bare name. Everything that ENDS a bearer therefore reaches both
// — [Held] and [End] — while only the issued name AUTHENTICATES, which is
// [Presented]'s rule.
var CookieNames = []string{HostCookieName, CookieBaseName}

// Presented is the bearer a request carries under the name a deployment
// reachable at externalURL issues — [CookieName] — or empty.
//
// ONE NAME, READ AS ISSUED, because the prefix is the whole of what keeps a
// sibling host's cookie out: a browser sets `__Host-crewlet_session` only from
// this exact host, while `crewlet_session` any host under the same registrable
// domain can write, with a Domain covering this one. Read under either name —
// as it was, so that a deployment moving from http to https kept its browsers
// signed in — a sibling that planted ITS OWN valid session under the bare name
// signed a visitor who held none in as that session's person: every change
// they then made landed in an account the sibling's author can read, which is
// the fixation the prefix exists to close. So a move to https signs each
// browser in again once, and [Held] is what lets the sign-out still end what
// the old name held.
func Presented(r *http.Request, externalURL string) string {
	if c, err := r.Cookie(CookieName(externalURL)); err == nil {
		return c.Value
	}
	return ""
}

// Held is every bearer a request carries under either name, the one a
// deployment reachable at externalURL issues first.
//
// FOR ENDING AND NEVER FOR AUTHENTICATING: a sign-out closes each one that
// verifies and is still live, so a browser holding the name an http deployment
// issued before it moved to https — a name [Presented] no longer reads — has
// that session closed rather than merely its cookie forgotten. Reaching a
// planted cookie here ends its planter's own session, which is theirs to lose.
func Held(r *http.Request, externalURL string) []string {
	issued := CookieName(externalURL)
	var out []string
	if c, err := r.Cookie(issued); err == nil && c.Value != "" {
		out = append(out, c.Value)
	}
	for _, name := range CookieNames {
		if name == issued {
			continue
		}
		if c, err := r.Cookie(name); err == nil && c.Value != "" {
			out = append(out, c.Value)
		}
	}
	return out
}

// deletions are the cookies that end a session in a browser under EVERY name
// one can be held under — [deletion] for the name this deployment issues, and
// a deletion for the other.
//
// A DELETION MATCHES ON NAME, PATH AND DOMAIN, never on the security flags,
// so the second only has to agree with [bearerCookie] on those three — which
// it does, carrying Path `/` and no Domain. It is Secure exactly when its name
// requires it: a `__Host-` Set-Cookie without Secure is refused outright, so
// the prefixed deletion always carries it (and a plain-http deployment,
// which can hold no prefixed cookie, merely has it ignored), while the bare
// deletion is plain, which a secure page may set and an insecure one must.
func deletions(externalURL string) []*http.Cookie {
	issued := deletion(externalURL)
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
// honest thing is to refuse rather than to mint cookies each node rejects on
// the next request that lands somewhere else. A validated Tier A never
// produces it, so a caller treats it as the fault it is.
var ErrNoKeyring = errors.New("session: no fleet keyring")

// Options configure [New].
type Options struct {
	// Material is the fleet's Tier A keyring. One that cannot sign for the
	// fleet is REFUSED — see the package doc.
	Material runtoken.Material

	// Now is the clock.
	//
	// WALL CLOCK, NOT MONOTONIC, and there is no choice about it: every
	// instant a bearer carries was written by another process on another
	// node, and a monotonic reading's epoch is per-boot — meaningless
	// anywhere but where it was taken. What that costs is a deadline read
	// on a node whose clock is off by the difference, and no more: nothing
	// here turns a disagreement between two clocks into a revocation.
	Now func() time.Time
}

// Signer mints and validates bearers.
//
// SAFE FOR CONCURRENT USE and holding no mutable state: a bearer is a function
// of the keyring, the session's own facts and the clock. There is nothing here
// to invalidate, which is the same sentence as "there is no validation cache".
type Signer struct {
	activeTag string
	keys      map[string][]byte
	now       func() time.Time
}

// New builds a signer, or refuses the deployment.
func New(opts Options) (*Signer, error) {
	if !opts.Material.Usable() {
		return nil, fmt.Errorf("%w: `secrets.keys` has no usable key for "+
			"session cookies. Add one and set "+
			"`secrets.active_key_id` to it: %w",
			ErrNoKeyring, errNoActiveKey(opts.Material))
	}
	s := &Signer{keys: map[string][]byte{}, now: opts.Now}
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
	return fmt.Errorf("`secrets.active_key_id` names %q and the keyring holds %v",
		m.ActiveID, held)
}
