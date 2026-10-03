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
// One resolver, [Guard.Resolve], over THREE credential shapes: a Tier A token
// the configuration names (resolve.go), a machine token the identity directory
// minted — a person's own access token or a service account's (tokens.go) —
// and a session cookie (sessions.go). The first two arrive as a bearer and are
// told apart by the value's shape; the third arrives as a cookie and yields to
// any bearer presented beside it.
// The guard is mounted UNCONDITIONALLY: mounting it only when Tier A carries
// tokens would be two independent conditions deciding one security property,
// coinciding only because every real caller happens to supply both. Tier A
// supplies the POSTURE — which credentials exist and what ceiling their grants
// are cut to — never the existence of a check.
//
// # A bearer is compared as it arrives, and its value is what protects it
//
// No curve stands in front of the comparison. A bearer names nobody until it is
// compared, so the only key a curve there could have is the SOURCE, and a
// refusal decided on an address is one anybody sharing it holds shut for
// everybody else — every pipeline and the break-glass token at an office, a
// VPN's egress, or the whole internet behind a proxy this deployment does not
// trust. What makes guessing hopeless is the value — crypto/rand bytes, a
// keyring HMAC, a Tier A value config refuses short — and what a guess costs is
// a line in the audit trail's failure tally. bearers.go carries the argument,
// including the curve that was tried and what it cost.
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
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
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
//   - FOUR ROUTES UNDER /auth/, and only four. Two because a login cannot
//     require a login: the posture read and the sign-in are how somebody
//     OBTAINS a credential, so requiring one is a deployment nobody can
//     enter. What stands in for the guard on each is the throttle and the
//     origin check, which they are NOT exempt from. Plus /auth/invite/,
//     whose link is the credential — the id in the path and the secret
//     beside it, never in a URL. And the SIGN-OUT
//     OF THIS SESSION, because a sign-out must clear the cookie whatever this
//     node can read: guarded, a node that could not read its identity estate
//     answered it `503 identity_unavailable` before it ran, so on exactly the
//     node that could vouch for nobody nobody could sign out, and a person
//     left a shared machine looking at a signed-in page. It needs no guard:
//     it verifies every bearer the browser holds itself, under the signature
//     and this node's rows, and ends only a session it finds — and the origin
//     check still judges it, as it judges every state change. Signing out of
//     EVERY session, or of one named, stays guarded: those act on a caller
//     the guard has to have resolved.
//
// NOT A /auth/ PREFIX, and that is the whole care in this entry. The same
// surface ends every session a person holds, ends other sessions by name,
// enrols second factors and reads who the caller is, and a prefix would exempt
// every one of them — a credential surface behind no credential, which is the
// exact shape /operator/ was deliberately kept out of /mcp/ to avoid.
//
// The split is deliberate. A PREFIX exempts everything beneath it, so only the
// ones that genuinely have sub-paths get one, and each ends in a slash, which
// is what stops it exempting a sibling. /health and /ready are single
// endpoints, so they are exact: as prefixes they would silently have exempted
// any future route merely starting with those letters — a /health-admin, a
// /readyz-reset — on the day it was added.
var unguardedExact = map[string]struct{}{
	"/": {}, PathDashboard: {}, "/favicon.ico": {}, "/health": {}, "/ready": {},
	PathAuthConfig: {}, PathAuthLogin: {}, PathAuthLogout: {},
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
	// PathAuthConfig is the posture read: which backend, the password
	// floor, whether a second factor is required. No user list and no
	// count of people — see authapi.
	PathAuthConfig = "/auth/config"

	// PathAuthLogin is the sign-in itself.
	PathAuthLogin = "/auth/login"

	// PathAuthLogout ends THIS session. It clears the cookie whatever this
	// node can read, which is why it is here rather than behind the guard.
	PathAuthLogout = "/auth/logout"

	// AuthInvitePrefix is the invitation pair, and a PREFIX because the
	// id is a path segment. Holding the link is the credential — its
	// secret, which travels beside the id in a header, a body or a form
	// and never in a path.
	AuthInvitePrefix = "/auth/invite/"

	// PathDashboard is the dashboard's shell: the page every screen is a
	// fragment route of, and so the page a link a person follows points
	// at — an invitation's is `PathDashboard + "#/invite/<id>.<secret>"`.
	// Exempt for the shell's reason above, and named here because the
	// exemption, the mount and every link to it must be one spelling.
	PathDashboard = "/dashboard"

	// PathAuthPrefix is the whole sign-in surface.
	//
	// NOT AN EXEMPTION — most of what it covers is guarded, and the list
	// above is the whole of what is not. It names the surface whose
	// subject is a person's own CREDENTIAL, which is what
	// [ActsAsThemselves] is about.
	PathAuthPrefix = "/auth/"
)

// The three GUARDED routes an enrolment-only session may reach — see
// [EnrolmentAdmits] — named here for the exemption list's reason: the refusal
// and the registration are one spelling, so a route authapi moved without
// moving this would not compile rather than quietly lock a new person out of
// the one route that lets them in.
const (
	// PathAuthSession is who the caller is — and, for an enrolment-only
	// session, that it is one.
	PathAuthSession = "/auth/session"

	// PathAuthTOTP enrols a second factor: the one gesture an
	// enrolment-only session exists to make.
	PathAuthTOTP = "/auth/totp"

	// PathAuthStepUp re-confirms the password on a session, which an
	// enrolment-only session needs when enrolling asks for a proof fresher
	// than the one it was opened with.
	PathAuthStepUp = "/auth/step-up"
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
	// value and the grants it may use. Empty is a real posture: no candidate can
	// match, so every guarded route is refused outright. Config refuses
	// it once the API is served, which is where a `crewlet validate` on
	// a laptop catches it.
	//
	// THE WHOLE ENTRY RATHER THAN THE VALUE, because what a surface
	// needs is a PRINCIPAL rather than an id: a credential's blast
	// radius is stated where it is pinned, and the guard is the last
	// frame that holds both the request and the entry it matched.
	tokens tierATokens

	// ceiling is this node's own `api.auth.max_grants`, intersected into
	// every principal at the moment it is resolved. See
	// [Guard.principalFor].
	ceiling []iam.Grant

	// proof is `api.auth.session.step_up` and `step_up_sensitive`: how long
	// a proof of identity authorises an administrative gesture and a
	// sensitive one. See [proofWindows].
	proof proofWindows

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

	// machine turns a machine token — a personal access token or a service
	// account's — into its owner, or nil where nothing installed the arm
	// (a suite). See tokens.go.
	machine *Tokens

	// audit is where a refused credential is counted and a Tier A
	// token's use and overreach are recorded. See audit.go.
	audit Audit
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
	tokens := tokensOf(b)
	if len(tokens) > 0 {
		log.Info("api_auth_tokens_loaded", "count", len(tokens),
			"grant_ceiling", auth.CeilingHash())
	}
	return &Guard{
		tokens: tokens, ceiling: auth.MaxGrants,
		proof: windowsOf(auth.Session), now: time.Now,
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

// TokenIDs names the Tier A credentials this guard accepts, sorted — their
// LABELS and never their values, for `GET /iam/node-tokens`.
//
// READ OFF THE GUARD rather than off Tier A, because the guard is what decides
// which bearer authenticates on this node: an answer built from a document the
// guard was not built from would name credentials that authenticate nobody
// here. Per node because Tier A is — two nodes mid-rollout may accept
// different tokens, and each answers for its own.
func (g *Guard) TokenIDs() []string {
	return slices.Sorted(maps.Keys(g.tokens))
}

// entry is the Tier A token a candidate matches, compared in constant time —
// THE TOKEN COMPARISON, IN ONE PLACE: every bearer the guard resolves, on a
// header or on the socket's query, is decided here.
//
// EVERY TOKEN IS COMPARED, and the loop does not stop at the first match: an
// early exit makes the time taken depend on WHICH id matched, which is exactly
// the leak the constant-time compare exists to close.
func (g *Guard) entry(candidate string) (config.APIToken, bool) {
	// AN EMPTY CANDIDATE NEVER MATCHES, and the check is not redundant with
	// the compare below: config refuses an empty token value, but Bootstrap
	// is an exported struct an embedder can build directly, and a token
	// configured as "" would otherwise match a request that presented no
	// credential at all. A total bypass, from one unset environment
	// variable.
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

// SocketPath is the dashboard's live socket. It is named here, beside the
// guard that admits it, so the mux that mounts it, the handler that serves it
// and the rules that single it out (the enrolment refusal among them) cannot
// spell it differently.
const SocketPath = "/ws/stream"

// Credential returns the bearer a request presented, or "".
//
// THE ONE PLACE A REQUEST'S BEARER IS READ, and it is the Authorization
// header on every route, the socket included. There is deliberately NO
// query-string credential: a token in a URL is written into every proxy's
// access log and the browser's history, and the one route that ever took one
// — this socket, because a browser cannot set a header on a WebSocket
// constructor — no longer needs it. A browser's handshake carries its session
// cookie like any other same-origin request, and a script that dials the
// socket sets the header, which every WebSocket client outside a browser can.
func (g *Guard) Credential(r *http.Request) string {
	header := r.Header.Get("Authorization")
	const scheme = "bearer "
	if len(header) >= len(scheme) && strings.EqualFold(header[:len(scheme)], scheme) {
		return strings.TrimSpace(header[len(scheme):])
	}
	return ""
}

// AttributionOf is who a write on this request is ATTRIBUTED to, and it is
// total: a request nobody resolved answers [iam.AnonymousActor] rather than an
// empty name.
//
// # The author AND the credential
//
// [iam.ActorFor]'s whole answer: the name the write is recorded under, its
// kind, and the credential it was made through. The trails that key on this —
// a configuration revision, a stored secret — used to record ONE name, and it
// was the credential: `pat:<id>` for a person writing through their own
// machine token, a name the identity sweep can no longer resolve to anybody a
// week after the token lapses, on a revision kept for ever. They record both
// now, as every other trail in the engine does.
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
func AttributionOf(ctx context.Context) iam.Actor {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return iam.Actor{Name: iam.AnonymousActor, Kind: iam.ActorOperator}
	}
	return iam.ActorFor(principal)
}

// markNoStore marks every answer to a request under [PathAuthPrefix]
// `Cache-Control: no-store`.
//
// # Why every answer there
//
// What that surface answers is the one class of body no cache may hold: a
// second-factor seed and the `otpauth://` URI that carries it, ten recovery
// codes shown exactly once, who somebody is and what they may do, the address
// an invitation was sent to, and every `Set-Cookie` a sign-in writes. A
// response with no Cache-Control is one a browser keeps in its disk cache and
// a shared proxy may keep for everybody behind it — where it outlives the tab,
// the session and the step-up that was needed to read it, and where "shown
// once" stops being true. Refusals are included: they say who the caller is,
// what their session may reach and which window their proof falls outside of.
//
// # Why here, and not beside the routes
//
// It was a wrapper around the surface's own mux, which covered what the
// routes wrote and nothing written before them: this guard's own refusals —
// `401 invalid_token`, `403 second_factor_enrolment_required` (on an answer
// that re-issues the session's cookie), `503 identity_unavailable` — the origin
// check's `403`, and the mux's own `404` and `405` all went out cacheable. This
// is the one frame every request under /auth passes through, guarded or not,
// before anything beneath it can write; so it is set here, by path, and a
// route added under /auth is covered the moment it is mounted. `no-store`
// rather than `private` or `no-cache`: private still lets the browser's own
// cache keep it, and no-cache only forces a revalidation of what was stored.
func markNoStore(w http.ResponseWriter, path string) {
	if strings.HasPrefix(path, PathAuthPrefix) {
		w.Header().Set("Cache-Control", "no-store")
	}
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
//
// EVERY ANSWER UNDER /auth LEAVES HERE `no-store` — see [markNoStore] — set
// before anything is resolved, so the refusals this writes, the origin check's
// beneath it and every route's own all carry it.
func (g *Guard) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		markNoStore(w, path)

		// EVERY REQUEST LEAVES HERE CARRYING AN ANSWER, which is what
		// stops [iam.From] reading a handler nobody wired through this
		// as [iam.Unknown] — silence is not anonymity, and that
		// distinction is only worth anything if the resolver actually
		// runs everywhere. An unguarded route's answer is
		// [Guard.resolveUnguarded]'s, which never compares a bearer.
		if Unguarded(path) {
			next.ServeHTTP(w, g.resolveUnguarded(w, r))
			return
		}
		r, refusal := g.Resolve(w, r)
		// THE RESOLVED REQUEST'S CONTEXT IS THIS HANDLER'S OWN, carrying
		// the answer: Resolve returns r.WithContext of a context derived
		// from r.Context(). contextcheck follows a request only when it is
		// the handler's parameter, never one a call returned, so it reads
		// both uses below as new contexts.
		//nolint:contextcheck // derived from r.Context(); see the paragraph above
		principal, how := iam.From(r.Context())
		if refusal != nil && refusal.Applies(r) {
			// RESOLVED AND STILL REFUSED, on a route the refusal does
			// not leave them — see [Refusal.Applies]. Logged at info
			// rather than warn: a seat removed under somebody who is
			// still signed in is an ordinary consequence of an
			// offboarding, and a person who has not enrolled their
			// second factor yet is an ordinary first day.
			log.Info("api_auth_refused_resolved",
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
			//
			// COUNTED HERE AND ONLY HERE: this is the one arm where a
			// refused credential decided something. See audit.go.
			g.refused(r)
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
		actor := iam.ActorFor(principal)
		log.Debug("api_auth_ok", "actor", actor.Name, "operator_id",
			actor.OperatorID, "route", path)
		//nolint:contextcheck // the resolved request's; see where Resolve is called
		entry, tierA := TierA(r.Context())
		if !tierA {
			next.ServeHTTP(w, r)
			return
		}
		// A TIER A TOKEN IS RECORDED AT THE REQUEST, and only here: the
		// use as it arrives, and an overreach once the route has said
		// no. See audit.go for why the resolution itself — which an
		// open socket re-runs once a minute — records neither.
		g.used(r, entry)
		recorded := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(recorded, r)
		if refusalStatus(recorded.status) {
			g.overreached(r, entry, recorded.status)
		}
	})
}

// resolveUnguarded answers who a request to an [Unguarded] route is — without
// comparing a presented bearer.
//
// # A session is resolved there, and a bearer is only marked
//
// One unguarded route reads a resolution at all: the sign-out, which records
// who signed out when this node can say — so a cookie is resolved here exactly
// as on a guarded route, and an UNKNOWN answer is handed through rather than
// refused. A BEARER is not. Nothing unguarded acts on the principal a bearer
// resolves to — the webhooks verify their own signatures, the per-run edges
// carry their own token and a sign-out ends only the sessions its cookies
// name — and comparing one anyway was an oracle: a matching Tier A value reads
// the identity directory for the token's seat binding before it answers, and
// a refused one returns after a map compare, so `Authorization: Bearer
// <guess>` against /health, /favicon.ico or /static answered a right value and
// a wrong one at different speeds, as fast as they were sent, on routes whose
// refusals the audit trail's failure tally does not count. So the value is
// never looked at here: the request is anonymous.
//
// THE REFUSAL [Guard.Resolve] RETURNS IS DISCARDED ON PURPOSE. It is only ever
// a credential whose SEAT is gone, or a session that may only enrol a second
// factor, and the routes that are unguarded are how somebody signs in, how
// the sign-in screen renders and how somebody signs out — locking either
// person out of those would leave them holding a live cookie with no way on,
// or no way off.
func (g *Guard) resolveUnguarded(w http.ResponseWriter, r *http.Request) *http.Request {
	if g.Credential(r) != "" {
		return r.WithContext(iam.WithAnonymous(r.Context()))
	}
	resolved, _ := g.Resolve(w, r)
	return resolved
}

// remoteHost is the caller's address without its port, for the failure log.
func remoteHost(r *http.Request) string {
	addr := r.RemoteAddr
	if i := strings.LastIndex(addr, ":"); i > 0 {
		return addr[:i]
	}
	return addr
}
