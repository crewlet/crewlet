// Package auth is where a request acquires an identity.
//
// EVERY REQUEST LEAVES THE MIDDLEWARE CARRYING ONE, and that is the property
// the rest of the API is built on. A credential that matched resolves to a
// principal; one that did not is [iam.Anonymous]; a node that could not tell is
// [iam.Unknown]. Downstream nothing asks "is there a token" — it asks what the
// principal may do, with a grant, through [iam.From] or the [Caller] shape that
// has no second value to drop.
//
// # What is guarded is everything, and the list is of what is not
//
// It used to be the other way round: a list of prefixes that always needed a
// token, with everything else following `allow_anonymous_read`, which defaulted
// to open and could not be closed durably because it was an `omitempty` bool
// whose safe value was its zero. Under it /events, /agents/{id}/memory and
// /ws/stream served full LLM transcripts — prompts, tool arguments, diary
// entries — to anyone who could reach the port. There is no such posture now:
// [Unguarded] is the whole of the exemption, every other route needs a
// credential whatever its method, and a deliberately public read surface is a
// named token entry holding read grants and nothing else.
//
// That is also why there is no longer a list of ALWAYS-guarded prefixes. With
// every route guarded, a list saying which ones especially are would have no
// caller and no meaning, and each of those surfaces now states its own
// authority where it is enforced: a grant on the route, and internal/authz
// deciding whether THIS caller may do THAT.
//
// # Where a principal comes from
//
// One resolver, in resolve.go, over the credential shapes Tier A can express.
// The guard is mounted UNCONDITIONALLY: mounting it only when Tier A carries
// tokens would be two independent conditions deciding one security property,
// coinciding only because every real caller happens to supply both. Tier A
// supplies the POSTURE — which credentials exist and what ceiling their grants
// are cut to — never the existence of a check.
//
// # And two gates beside it
//
// [CSRF] refuses a state change a cross-site page could have caused, and
// [CORS] decides who may read an answer. They are separate because they answer
// different questions: same-origin policy stops an attacker's page READING a
// response, which on a write is the part they do not need.
package auth

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
)

var log = logging.Get("api.auth")

// unguardedExact and unguardedPrefixes are the routes served without a bearer
// token, because they authenticate by other means or because a client must
// reach them to obtain a token:
//
//   - /health, /ready: probes. An orchestrator has no token, and a liveness
//     check that 401s is a liveness check that fails.
//   - /webhooks/: every one verifies a provider signature before doing
//     anything, which is a stronger check than a shared bearer token. Includes
//     the Slack OAuth landing page, which a browser reaches mid-install with no
//     token in hand.
//   - /otlp/ and /mcp/: the per-run signed token in the path IS the
//     credential. Both are reached from INSIDE a sandbox, which is the one
//     place the API's own token must never go — it reads the whole company,
//     and the box is running generated code. See internal/runtoken.
//   - the dashboard shell and its assets: the page that prompts for the token
//     cannot itself require one. It ships no data — every byte it renders comes
//     from an authenticated fetch.
//   - FIVE ROUTES UNDER /auth/, and only five: a login cannot require a login.
//     The posture read, the sign-in, the first-operator bootstrap and the two
//     OIDC legs are how somebody OBTAINS a credential, so requiring one is a
//     deployment nobody can enter. What stands in for the guard on each is the
//     per-source throttle and the origin check, which they are NOT exempt
//     from. Plus /auth/invite/, whose own id is the credential.
//
// NOT A /auth/ PREFIX, and that is the whole care in this entry. The same
// surface serves the logout routes, the second-factor enrolment and the
// session read, and a prefix would exempt every one of them — a credential
// surface behind no credential, which is the exact shape /operator/ was
// deliberately kept out of /mcp/ to avoid.
//
// The split is deliberate. A PREFIX exempts everything beneath it, so only the
// ones that genuinely have sub-paths get one, and each ends in a slash, which
// is what stops it exempting a sibling. /health and /ready are single
// endpoints, so they are exact: as prefixes they would silently have exempted
// any future route merely starting with those letters — a /health-admin, a
// /readyz-reset — on the day it was added.
var unguardedExact = map[string]struct{}{
	"/": {}, "/dashboard": {}, "/favicon.ico": {}, "/health": {}, "/ready": {},
	PathAuthConfig: {}, PathAuthLogin: {}, PathAuthBootstrap: {},
	PathAuthOIDCStart: {}, PathAuthOIDCCallback: {},
}

var unguardedPrefixes = []string{
	WebhookPrefix, OTLPPrefix, mcpbridge.PathPrefix, "/static/", AuthInvitePrefix,
}

// The sign-in routes served without a credential, named HERE rather than in
// the surface that serves them.
//
// THE EXEMPTION AND THE REGISTRATION MUST BE ONE SPELLING, and this is the
// package that owns the exemption — internal/api/authapi registers on these
// constants, so a route it moved without moving the exemption would not
// compile rather than quietly landing behind a credential nobody can obtain.
// The dependency runs that way round because authapi already imports this
// package for the guard, and the reverse would be a cycle.
const (
	// PathAuthConfig is the posture read: which backend, whether the
	// first-operator route is still open, the password floor. No user
	// list and no count of people — see authapi.
	PathAuthConfig = "/auth/config"

	// PathAuthLogin is the sign-in itself.
	PathAuthLogin = "/auth/login"

	// PathAuthBootstrap creates the first person from a one-time code
	// written to a file on the host.
	PathAuthBootstrap = "/auth/bootstrap"

	// PathAuthOIDCStart and PathAuthOIDCCallback are the provider round
	// trip. Both are reached by a BROWSER following a redirect, which
	// carries nothing this engine issued.
	PathAuthOIDCStart    = "/auth/oidc/start"
	PathAuthOIDCCallback = "/auth/oidc/callback"

	// AuthInvitePrefix is the invitation pair, and a PREFIX because the
	// id is a path segment. Holding the link is the credential.
	AuthInvitePrefix = "/auth/invite/"

	// PathAuthPrefix is the whole sign-in surface.
	//
	// NOT AN EXEMPTION — most of what it covers is guarded, and the list
	// above is the whole of what is not. It names the surface whose
	// subject is a person's own CREDENTIAL, which is what
	// [ActsAsThemselves] is about.
	PathAuthPrefix = "/auth/"
)

// WebhookPrefix and OTLPPrefix are the two exempt edges a second rule also
// reads. Named here, beside the exemption, for the reason [SocketPath] is: the
// API's drain gate refuses the first and serves the second, and a prefix it
// spelled for itself could stop matching the one this guard exempts without
// either rule looking wrong on its own.
const (
	WebhookPrefix = "/webhooks/"
	OTLPPrefix    = "/otlp/"
)

// readMethods are the methods that change nothing and start nothing.
var readMethods = map[string]struct{}{
	http.MethodGet: {}, http.MethodHead: {}, http.MethodOptions: {},
}

// IsRead reports whether a method is a read: GET, HEAD or OPTIONS.
//
// THE GUARD NO LONGER BRANCHES ON IT — every guarded route needs a credential
// whatever the method — and it stays exported because a read is a read to more
// than the guard: the DRAIN gate goes on serving reads while it refuses
// everything that would start work, and the authorization layer above still
// classifies a route by verb. A second list of which methods those are would
// be a second answer to one question.
func IsRead(method string) bool {
	_, ok := readMethods[strings.ToUpper(method)]
	return ok
}

// loopbackHosts are bind addresses no other machine can reach.
//
// WHAT READS THIS IS NARROWER THAN IT WAS. It used to decide whether an open
// read posture on this bind deserved a warning; that posture is gone. What is
// left is the development principal, which is refused outright off loopback —
// and note that `api.auth.local`'s own insecure rule deliberately judges the
// EXTERNAL URL instead, because a hardened node binds loopback behind its
// proxy and a bind check would permit the insecure posture in exactly the
// deployment that must refuse it.
var loopbackHosts = map[string]struct{}{
	"127.0.0.1": {}, "::1": {}, "localhost": {}, "localhost6": {},
}

// BindIsLoopback reports whether api.host binds an address only this machine
// can reach.
func BindIsLoopback(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	_, ok := loopbackHosts[h]
	return ok
}

// Unguarded reports whether a path is served without a bearer token.
//
// A single trailing slash is normalised away before the exact set is consulted,
// because the guard runs BEFORE routing: the mux would redirect /health/ to
// /health, but only if the request survives long enough to be routed. Without
// this, a load balancer configured to probe /health/ gets a 401 in the closed
// posture and takes the node out of rotation — an outage caused by a slash.
//
// Only the exact set is normalised, and only by one slash. The /config guard
// reads the raw path (/config/ starts with /config either way) and the prefix
// exemptions already end in a slash, so nothing here can widen what is exempt
// beyond the trailing-slash spelling of a path that was exempt already.
func Unguarded(path string) bool {
	if _, ok := unguardedExact[path]; ok {
		return true
	}
	for _, prefix := range unguardedPrefixes {
		if strings.HasPrefix(path, prefix) {
			return true
		}
	}
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		_, ok := unguardedExact[path[:len(path)-1]]
		return ok
	}
	return false
}

// Guard holds the loaded posture and answers every auth question about it.
//
// EVERY GUARDED ROUTE NEEDS A CREDENTIAL, reads included. There is no longer a
// posture in which a GET serves without one: `allow_anonymous_read` defaulted
// to open over a read surface carrying LLM transcripts, diary entries and the
// whole event stream, and it was a bool whose safe value was its zero and
// which was `omitempty`, so closing it did not survive an export round trip. A
// deliberately public reader is a named token entry holding read grants and
// nothing else. What is still served without a credential is [Unguarded], and
// only that.
type Guard struct {
	// tokens maps operator id to that token's whole Tier A entry — its
	// value, the grants it may use and the colleague level it reaches
	// the company's work at. Empty is a real posture: no candidate can
	// match, so every guarded route is refused outright. Config refuses
	// it once the API is served, which is where a `crewlet validate` on
	// a laptop catches it.
	//
	// THE WHOLE ENTRY RATHER THAN THE VALUE, because what a surface
	// needs is a PRINCIPAL rather than an id: a credential's blast
	// radius is stated where it is pinned, and the guard is the last
	// frame that holds both the request and the entry it matched.
	tokens map[string]config.APIToken

	// ceiling is this node's own `api.auth.max_grants`, intersected into
	// every principal at the moment it is resolved. See
	// [Guard.principalFor].
	ceiling []iam.Grant

	// stepUp is `api.auth.session.step_up`, which is how long presenting
	// a credential authorises an administrative gesture.
	stepUp time.Duration

	// now is the clock, injectable so a case can pin what a principal's
	// freshness is measured against.
	now func() time.Time

	// bindings is where a Tier A token's seat binding is read and
	// resolved. See [SeatBindings] and resolve.go.
	//
	// THE COMPANY'S HALF OF A PRINCIPAL, handed in rather than read here,
	// because it comes from the identity estate and the chart while this
	// package resolves a CREDENTIAL. Carrying it is what makes a LEAD
	// relation reachable for a token at all: without the seat handle every
	// authority rule asking "do you lead this" falls through to the admin
	// grant, and a founder is indistinguishable from a CI pipeline in every
	// audit row.
	bindings SeatBindings

	// dev is the development principal an unauthenticated request resolves
	// to, or nil. Installed by [Guard.WithDevPrincipal], refused at
	// construction on anything but a loopback bind of an unreleased
	// binary — see devprincipal.go.
	dev *DevPrincipal

	// clients resolves a caller's own address through the CIDR blocks
	// whose forwarded headers this deployment believes. See client.go.
	clients *Clients

	// sessions turns a browser's cookie into the person holding it, or
	// nil on a node that mints none. See sessions.go.
	sessions *Sessions
}

// BindSeats installs the seams that let a bound credential act as its seat, and
// returns the guard for chaining.
//
// CALLED ONCE, AT WIRING TIME, before this guard serves anything: the seams
// are read on every request and a guard whose seam moved under a request in
// flight would attribute one write two ways.
func (g *Guard) BindSeats(bindings SeatBindings) *Guard {
	g.bindings = bindings
	return g
}

// New builds the guard from Tier A.
//
// It does not fail. Every posture that would leave this surface unreachable —
// no credential once a port is set, no ceiling, no external address, no
// keyring — and the reserved token id are refused by config validation, which
// is where a `crewlet validate` on a laptop can catch them rather than a
// process discovering them at bind time.
func New(b *config.Bootstrap) *Guard {
	if b == nil {
		// No Tier A at all: nobody has said who may act, so nothing
		// authenticates and every guarded route is refused. That is
		// the honest reading, and it is the same answer as a config
		// listing no tokens.
		//
		// AND NO CEILING, which grants nothing rather than everything
		// — see [intersect] for why that direction is the only safe
		// one.
		return &Guard{now: time.Now}
	}
	auth := b.API.Auth
	tokens := make(map[string]config.APIToken, len(auth.Tokens))
	for _, entry := range auth.Tokens {
		tokens[entry.ID] = entry
	}
	if len(tokens) > 0 {
		log.Info("api_auth_tokens_loaded", "count", len(tokens),
			"grant_ceiling", auth.CeilingHash())
	}
	return &Guard{
		tokens: tokens, ceiling: auth.MaxGrants,
		stepUp: auth.Session.StepUp(), now: time.Now,
		clients: NewClients(b),
	}
}

// Mux is what a surface mounts routes on.
//
// DEFINED HERE, in the package that owns the exemption list, because the two
// packages that need it both already import this one and the alternative is a
// cycle. It is narrower than [http.ServeMux] — which satisfies it — for one
// reason: the standard mux reports NOTHING about what was registered on it, so
// a gate holding the exemption list against the registration could not read
// one half of what it is about, and the failure when those two drift is a
// credential surface behind no credential.
type Mux interface {
	HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request))
}

// Client is the address a per-source rule keys on, resolved through this
// deployment's trusted proxies. See client.go.
//
// ON THE GUARD rather than a free function, because the trusted blocks are
// Tier A's and every caller that needs a client address is already holding a
// guard — a second parse somewhere else is a second answer to "is this peer
// the proxy", and the two would drift the day somebody edited one.
func (g *Guard) Client(r *http.Request) string { return g.clients.Of(r) }

// Tokens reports how many credentials are loaded, for the same startup line.
func (g *Guard) Tokens() int { return len(g.tokens) }

// Operator returns the operator id a bare token authenticates as.
//
// THE TOKEN COMPARISON, IN ONE PLACE. The HTTP middleware and the socket
// handshake reach it through [Guard.Presented], which peels the credential
// off the request first, and the dashboard's WebSocket query channel calls it
// directly, because an operator-only query arrives as a field on a socket
// frame rather than as a header. All three therefore accept exactly the same
// tokens, honour disabled identically, and compare in constant time.
func (g *Guard) Operator(candidate string) (string, bool) {
	// An empty candidate never authenticates, and the check is not
	// redundant with the compare below: config refuses an empty token
	// value, but Bootstrap is an exported struct an embedder can build
	// directly, and a token configured as "" would otherwise match a
	// request that presented no credential at all. A total bypass, from
	// one unset environment variable.
	if candidate == "" {
		return "", false
	}
	// Every token is compared, and the loop does not stop at the first
	// match: an early exit makes the time taken depend on WHICH id
	// matched, which is exactly the leak the constant-time compare below
	// exists to close.
	matched, _ := g.entry(candidate)
	return matched.ID, matched.ID != ""
}

// entry is the Tier A token a candidate matches, compared in constant time.
//
// EVERY TOKEN IS COMPARED, and the loop does not stop at the first match: an
// early exit makes the time taken depend on WHICH id matched, which is exactly
// the leak the constant-time compare exists to close.
func (g *Guard) entry(candidate string) (config.APIToken, bool) {
	if candidate == "" {
		return config.APIToken{}, false
	}
	var matched config.APIToken
	for _, held := range g.tokens {
		if subtle.ConstantTimeCompare([]byte(candidate), []byte(held.Token)) == 1 {
			matched = held
		}
	}
	return matched, matched.ID != ""
}

// SocketPath is the dashboard's live socket, the one route whose credential
// may arrive on the query string. It is named here, beside the rule that
// reads it, so the mux that mounts it and the handler that serves it cannot
// spell it differently from the guard that admits it.
const SocketPath = "/ws/stream"

// Credential returns the token a request presented, or "".
//
// THE ONE PLACE A REQUEST'S CREDENTIAL IS READ. The Authorization bearer
// header on every route; and on the socket path only, the token query
// parameter as well, because a browser cannot set a header on a WebSocket
// constructor and the dashboard has no other way to send one. The query is
// read nowhere else: a token in a URL appears in proxy logs and browser
// history, which is a price worth paying for exactly one route that has no
// alternative and for no route that has.
//
// The header wins when both are present, so a non-browser client that sent
// the right header is never judged by a stale query.
func (g *Guard) Credential(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const scheme = "bearer "
	if len(header) >= len(scheme) && strings.EqualFold(header[:len(scheme)], scheme) {
		return strings.TrimSpace(header[len(scheme):])
	}
	if r.URL.Path == SocketPath {
		return r.URL.Query().Get("token")
	}
	return ""
}

// Presented returns the operator id for the credential a request presented.
func (g *Guard) Presented(r *http.Request) (string, bool) {
	return g.Operator(g.Credential(r))
}

// operatorKey carries the authenticated operator id down the handler chain.
type operatorKey struct{}

// OperatorOf is the id a write on this request is ATTRIBUTED to, and it is
// total: a request nobody resolved answers [iam.AnonymousActor] rather than
// the empty string.
//
// # Why it has no second value, and why that is not the discarded bool
//
// It is not how a handler asks whether somebody is there — [Caller] is, and
// the difference is that Caller writes the refusal and has nothing to drop.
// This is what a handler calls AFTER that question has been answered, at the
// moment a row is written, on a surface where the guard has already refused
// everything but [iam.Resolved].
//
// So its total answer covers the case that is left: a handler reached by a
// path nobody wired through the guard. An empty `created_by` there reads as a
// write nobody made, and a reader filtering the audit trail would never find
// it; `anonymous` is a name config refuses to every real credential, so the
// row says plainly that this engine could not name its author.
func OperatorOf(ctx context.Context) string {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return iam.AnonymousActor
	}
	return OperatorID(principal)
}

// OperatorID is the id a principal is recorded under on the surfaces that key
// on one: the bare token id, without the `token:` class its login carries,
// because that is the string a stored revision's `created_by` and a secret's
// `set_by` already hold.
//
// A CONVERSION RATHER THAN A SECOND IDENTITY. What a row should carry in the
// long run is [iam.ActorFor]'s answer — a name AND a kind — and this exists
// for the columns that predate it, so the day those two tables take their
// author kind from that function nothing migrates.
func OperatorID(p iam.Principal) string {
	return strings.TrimPrefix(p.Login, TokenLoginPrefix)
}

// WithOperator attaches a principal carrying one operator id and NO GRANTS.
//
// ATTRIBUTION, NEVER AUTHORITY, and the empty grant set is the whole of what
// makes that safe: it names who a write is recorded as and opens nothing. The
// real resolution is [Guard.Resolve], which composes a principal from the Tier
// A entry it matched — its grants, its colleague level, its freshness — and
// every request through [Guard.Middleware] takes that path.
//
// What is left for this is a caller that already knows the id and needs the
// row it writes to say so: a test standing a handler up directly, and any
// surface that authenticated by some other means and has nothing to look the
// entry up with. A principal from here can be recorded and can do nothing.
func WithOperator(ctx context.Context, operatorID string) context.Context {
	// AN EMPTY ID IS NOBODY, and it attaches the FINDING rather than a
	// principal with no name. Attached as one, every reader asking "is
	// somebody there" is told yes and then writes a row whose author
	// column is the bare class prefix — which is the failure the whole
	// three-valued resolution exists to make impossible.
	if operatorID == "" {
		return iam.WithAnonymous(ctx)
	}
	return iam.WithPrincipal(ctx, iam.Principal{
		ID:    uuid.NewSHA1(TokenNamespace, []byte(operatorID)),
		Login: TokenLogin(operatorID),
		Kind:  iam.KindMachine,
		Stage: iam.StageActive,
	})
}

// Middleware wraps a handler with the guard.
//
// A WebSocket upgrade is an HTTP request and passes through here like any
// other, which is why [Guard.Credential] reads the socket's query token: this
// used to read the header alone and leave the query to the stream handler,
// and under a closed posture the socket then never reached that handler at
// all. The middleware answered 401 first, and the dashboard could not
// connect with a valid token, on the one posture whose point is that the
// token is required.
func (g *Guard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		// ATTRIBUTION AND AUTHORIZATION ARE DIFFERENT QUESTIONS, and
		// the credential is resolved for both. A route that does not
		// REQUIRE one can still be told who presented one — the
		// webhook edge attributing a delivery, a probe answered
		// differently for an operator — and skipping the resolution
		// there made that unreachable: the request arrived with a
		// valid token, no principal attached, and came back
		// unauthorized to a caller holding the right credential.
		//
		// EVERY REQUEST LEAVES HERE CARRYING AN ANSWER, which is what
		// stops [iam.From] reading a handler nobody wired through this
		// as [iam.Unknown] — silence is not anonymity, and that
		// distinction is only worth anything if the resolver actually
		// runs everywhere.
		r, refusal := g.Resolve(w, r)
		principal, how := iam.From(r.Context())
		if Unguarded(path) {
			// THE REFUSAL IS DISCARDED HERE ON PURPOSE. It is only
			// ever a credential whose SEAT is gone — a signed-in
			// person or a bound Tier A token — and the routes that
			// are unguarded are how somebody signs out and how the
			// sign-in screen renders — locking a leaver out of those
			// would leave them holding a live cookie with no way to
			// end it.
			next.ServeHTTP(w, r)
			return
		}
		if refusal != nil && !ActsAsThemselves(path) {
			// RESOLVED AND STILL REFUSED. Logged at info rather than
			// warn: a seat removed under somebody who is still signed
			// in is an ordinary consequence of an offboarding, and
			// the remedy is a rebind rather than an investigation.
			log.Info("api_auth_seat_refused",
				"route", path, "code", refusal.Code,
				"detail", refusal.Detail, "remote", g.Client(r))
			httpjson.FailWith(w, refusal.Status, refusal.Code,
				map[string]string{"detail": refusal.Detail})
			return
		}
		if how == iam.Unknown {
			// 503 AND NEVER 401 ON A NODE THAT CANNOT TELL. A
			// browser reads 401 as "sign in again" and discards the
			// cookie, so one stalled applier answering 401 signs
			// everybody on this node out and stampedes the identity
			// provider — which is the failure internal/iam/session's
			// whole three-valued shape exists to prevent, arriving
			// at the one frame that could still undo it.
			log.Warn("api_auth_unavailable",
				"route", path, "remote", g.Client(r))
			httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable,
				RetryIdentitySeconds)
			return
		}
		if how != iam.Resolved {
			// A LOG LINE, AND DELIBERATELY NOT AN EVENT. Whoever can
			// reach this listener authors the rate of these, with no
			// credential and no identity — so no per-attempt event
			// type may carry one into the node estate, which is the
			// admission rule internal/events states and walks. A
			// durable record of failed authentication is a COALESCED
			// count, per source per minute, paced by the engine's own
			// loop rather than by the caller.
			log.Warn("api_auth_failed",
				"route", path,
				"reason", "missing_or_invalid_bearer",
				// The candidate value is NEVER logged: a rejected token
				// is still a credential, and a log is a place it would
				// outlive the request.
				// THE RESOLVED CLIENT, not the peer. Behind a
				// proxy every line would otherwise name the
				// proxy, which is the one address that tells an
				// operator nothing about who is guessing.
				"remote", g.Client(r))
			// THE SAME REFUSAL ENVELOPE EVERY OTHER SURFACE
			// ANSWERS WITH. This was a hand-written JSON literal
			// and a hand-set header pair — the shape that drifts,
			// because nothing about it says which vocabulary the
			// code belongs to or where the sentence beside it
			// comes from. A caller refused here and refused by a
			// route reads one body.
			httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
			return
		}
		log.Debug("api_auth_ok", "operator_id", OperatorID(principal), "route", path)
		next.ServeHTTP(w, r)
	})
}

// remoteHost is the caller's address without its port, for the failure log.
func remoteHost(r *http.Request) string {
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}
