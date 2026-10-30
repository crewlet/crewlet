package auth

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// THE ASSISTANT GRANT: how the Dashboard Assistant acts as the person who
// asked it, and nothing else (ADR-0050).
//
// # A sibling of the view ticket, not a credential of its own
//
// The assistant has no identity. What it holds for a turn is a grant DERIVED
// from the request the guard resolved to a person acting through a browser
// session, and the grant carries the NAME of that session (`session:<lineage>`)
// rather than any authority: every request it rides is re-composed by
// [Lineages.Principal] from the identity rows, exactly as an artifact view
// ticket's document request is (ADR-0035). So a sign-out, a revocation, a
// suspension, a lost permission or a seat move stops the assistant at its next
// step, and nothing the grant says can widen what the person may do now.
//
// # Useless outside this process
//
// The grant is accepted only on a request carrying an UNEXPORTED context marker
// that [Dispatcher.Do] sets, and a request off the wire cannot carry a context
// value. Presented over a socket it is refused 401 and logged as
// `assist_over_wire`, whatever it says and whether or not it is still live. That
// is what makes a grant something no proxy log, no local process and no
// misconfigured bind can use: the in-process call never crosses a socket, and a
// copy that did would open nothing.
//
// # Every clause subtracts
//
//   - SCOPE. A read grant takes GET and HEAD only; an exec grant takes the ONE
//     request its approval bound, by the request's digest
//     ([opkey.RequestDigestOf]), once. The authority table's assistant column
//     (ADR-0052) is decided inside [authz.Decide] beside these, so a nested verb
//     a tool asks is held to it too.
//   - LIFETIME. A read grant lives [AssistReadLifetime], an exec grant
//     [AssistExecLifetime], and neither outlives the session's absolute or idle
//     deadline as the deriving request presented them.
//   - FRESHNESS. The re-composed principal's step-up instant is clamped to the
//     deriving request's ([AssistClaims.ReauthCeiling]), so a grant never proves
//     a fresher identity than the click that started the turn.
//   - REVOCATION. Every grant is in a NODE-LOCAL registry keyed by its id: the
//     turn and every call it makes run in this process and die with it, so
//     nothing else has to agree on it. A grant the registry does not hold live
//     is refused whatever its signature says.
//   - EDGES. A grant never reaches the surfaces that are not action-routed or
//     that would let the assistant act on itself: the credential surface, the
//     sockets, both MCP surfaces, telemetry, webhooks and the assistant's own
//     routes, so it can never approve its own proposal ([assistRefusedPrefixes]).
//
// # Who may derive one
//
// A PERSON, active, acting through a whole browser session: never a machine
// token (whose own owner is the person but whose presence is nobody's), never a
// Tier A token or a session exchanged from one, never a session that may only
// enrol a second factor, and never a request already riding a grant, so the
// assistant cannot mint itself a fresher one (decision D04).

// AssistDomain separates the grant's signing keys from every other runtoken's,
// so a token minted for the OTLP receiver, the MCP bridge or a view ticket never
// verifies as a grant, nor the other way round.
const AssistDomain = "crewlet.assistant.grant.v1"

// AssistReadLifetime is the longest a turn's read grant lives: the longest a
// turn may run (internal/config's AssistantTurnTimeoutCeiling), so a grant
// outlives no turn it could serve and no turn outlives its grant. A turn ends
// or parks its grant as it ends (internal/assistant), so a shorter turn holds
// it no longer than it runs.
const AssistReadLifetime = config.AssistantTurnTimeoutCeiling * time.Second

// AssistExecLifetime is the longest an exec grant lives: one in-process request
// with a slow write's p99 behind it, and nothing else.
const AssistExecLifetime = 60 * time.Second

// AssistantPrefix is the assistant's own surface. A grant never reaches it, so
// the assistant can neither recurse into a turn nor approve its own proposal.
// Named here, beside the refusal, for [PathAuthPrefix]'s reason: the surface
// that mounts it and the rule that refuses it must be one spelling.
const AssistantPrefix = "/assistant/"

// AssistMaxBody is the largest request body an exec grant may carry, read and
// digested by the guard before the route reads it again.
//
// THE LARGEST BODY ANY ROUTE AN APPROVED WRITE IS SENT TO ACCEPTS: the act
// route's, twice a page plus the call's own arguments (operator.MaxActBody,
// which this package cannot import because the operator surface imports it; a
// test holds the two together). A smaller bound would refuse an approved write
// the route itself accepts.
const AssistMaxBody = 2*(512<<10) + 64<<10

// assistOperatorMCP is the operator's MCP surface (operator.MCPPath), named
// here for [AssistMaxBody]'s reason and held to it by a test.
const assistOperatorMCP = "/operator/mcp"

// assistRefusedPrefixes are the paths no grant reaches, refused before routing:
//
//   - the credential surface (`/auth/`), where somebody signs in, steps up,
//     enrols a factor or ends sessions, and the assistant must never;
//   - the socket (`/ws/`), whose connection outlives any one request;
//   - both MCP surfaces, the operator's and the sandbox bridge's, which are
//     transports into the tools rather than routes with a row of their own;
//   - telemetry and webhooks, which authenticate by other means;
//   - the assistant's own routes, so it cannot recurse or approve itself.
var assistRefusedPrefixes = []string{
	PathAuthPrefix, "/ws/", assistOperatorMCP, mcpbridge.PathPrefix, OTLPPrefix,
	WebhookPrefix, AssistantPrefix,
}

// AssistRefusedPath reports whether a grant is refused path before routing.
//
// A prefix ending in a slash also refuses the path without it (`/assistant`),
// so no spelling of a refused surface's root is a way past the rule.
func AssistRefusedPath(path string) bool {
	for _, prefix := range assistRefusedPrefixes {
		if strings.HasPrefix(path, prefix) || path == strings.TrimSuffix(prefix, "/") {
			return true
		}
	}
	return false
}

// ErrAssistRefused reports a request a grant is not derived for. Every refusal
// [Delegations.Derive] answers wraps it with the reason.
var ErrAssistRefused = errors.New("auth: an assistant grant is not derived for this request")

// AssistRequest is what a grant is derived for: one turn of one conversation,
// and for an exec grant the one approved request it may carry.
type AssistRequest struct {
	Conversation, Turn uuid.UUID
	Scope              iam.AssistScope

	// Proposal is the approved proposal, exec only.
	Proposal uuid.UUID
	// Action is the verb the approved request takes, exec only, for the
	// audit reference. It is NEVER an authorization input: the route's own
	// row decides, and the request binding is what limits the grant.
	Action authz.Action
	// Request is the ONE request an exec grant may carry, exec only.
	Request *exec.Request
}

// AssistClaims is what a grant carries: a signed subject, base64url JSON, as
// the view ticket's is. Nothing in it confers anything; the principal is
// re-composed from the rows on every use, and each field only binds or bounds.
type AssistClaims struct {
	// Grant is the grant's id, a UUIDv7 and the registry key.
	Grant uuid.UUID `json:"g"`
	// Principal is the person it was derived for; the re-composed
	// principal's id must equal it.
	Principal uuid.UUID `json:"p"`
	// Credential is the session it was derived from, `session:<lineage>`.
	Credential   string    `json:"k"`
	Conversation uuid.UUID `json:"c"`
	Turn         uuid.UUID `json:"t"`
	// Proposal is the approved proposal, exec only.
	Proposal uuid.UUID       `json:"x,omitempty"`
	Scope    iam.AssistScope `json:"s"`
	// Action is the approved request's verb, exec only, for the audit ref.
	Action authz.Action `json:"a,omitempty"`
	// RequestDigest is the approved request's [opkey.RequestDigestOf], 64
	// hex digits, exec only.
	RequestDigest string `json:"d,omitempty"`
	// ReauthCeiling is the deriving request's step-up instant; the
	// re-composed principal's is never later.
	ReauthCeiling time.Time `json:"r"`
	// Expires is when the grant stops being accepted.
	Expires time.Time `json:"e"`
}

// check refuses claims this build never mints.
func (c AssistClaims) check() error {
	switch {
	case c.Grant.Version() != 7:
		return errors.New("the grant id is not a UUIDv7")
	case c.Principal == uuid.Nil:
		return errors.New("it names no person")
	case !iam.ValidSessionName(c.Credential):
		return fmt.Errorf("%q names no browser session", c.Credential)
	case c.Conversation == uuid.Nil || c.Turn == uuid.Nil:
		return errors.New("it names no conversation or no turn")
	case c.Expires.IsZero():
		return errors.New("it carries no expiry")
	case !c.Scope.Valid():
		return fmt.Errorf("scope %q is not one this build mints", c.Scope)
	}
	isExec := c.Scope == iam.AssistScopeExec
	switch {
	case isExec && (c.Proposal == uuid.Nil || c.Action == "" || !digestShaped(c.RequestDigest)):
		return errors.New("an exec grant binds no proposal, action or request")
	case !isExec && (c.Proposal != uuid.Nil || c.Action != "" || c.RequestDigest != ""):
		return errors.New("a read grant binds a proposal, an action or a request")
	}
	return nil
}

// digestShaped reports a full SHA-256 in lowercase hex.
func digestShaped(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

// Delegations mints, revokes and checks assistant grants.
//
// ONE PER NODE, built by the process that serves the API and bound to that API's
// guard ([Guard.BindDelegations]), because a grant is re-composed through the
// guard's own arms and its registry is this node's alone.
type Delegations struct {
	signer    *runtoken.Signer
	standings SessionStandings
	now       func() time.Time

	mu       sync.Mutex
	lineages *Lineages
	live     map[uuid.UUID]liveGrant
}

// liveGrant is a grant the registry holds.
type liveGrant struct {
	turn    uuid.UUID
	scope   iam.AssistScope
	expires time.Time
	// spent marks an exec grant whose one request has been admitted.
	spent bool
}

// NewDelegations builds the minter over the fleet keyring and the read that
// says whose a session is.
//
// THE SESSION READ RATHER THAN A RESOLVER: a grant is re-composed through the
// API's own guard, which is built inside the API, so the resolver is made when
// the guard binds this ([Guard.BindDelegations]) and not handed in.
func NewDelegations(m runtoken.Material, standings SessionStandings,
	now func() time.Time) (*Delegations, error) {

	if standings == nil {
		return nil, errors.New("auth: assistant grants need to read whose a session " +
			"is; without it no grant could be re-composed")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	signer, err := runtoken.New(runtoken.Options{Domain: AssistDomain, Material: m, Now: now})
	if err != nil {
		return nil, fmt.Errorf("auth: assistant grants: %w", err)
	}
	return &Delegations{signer: signer, standings: standings, now: now,
		live: map[uuid.UUID]liveGrant{}}, nil
}

// BindDelegations installs the assistant arm: grants d mints are accepted by
// this guard, and re-composed through its own arms.
//
// CALLED ONCE, AT WIRING TIME, for [Guard.BindSeats]' reason. A Delegations is
// bound to exactly one guard, because the principal a grant stands for must be
// composed by the guard that serves its requests. Nil installs nothing, and
// every grant is then refused as an unknown credential.
func (g *Guard) BindDelegations(d *Delegations) error {
	if d == nil {
		return nil
	}
	l, err := NewLineages(g, d.standings)
	if err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.lineages != nil {
		return errors.New("auth: assistant grants are already bound to a guard")
	}
	d.lineages = l
	g.delegations = d
	return nil
}

// Derive mints a grant from the request the guard resolved: the principal and
// the session's deadlines are read from ctx, never passed in.
//
// Refused ([ErrAssistRefused], wrapped with the reason) unless the request is a
// PERSON, active, acting through a whole browser session that is not itself a
// grant; and unless req is well formed for its scope. The grant expires at the
// earliest of its scope's lifetime and the session's absolute and idle
// deadlines.
func (d *Delegations) Derive(ctx context.Context, req AssistRequest) (exec.Grant, error) {
	refuse := func(reason string) (exec.Grant, error) {
		return nil, fmt.Errorf("%w: %s", ErrAssistRefused, reason)
	}
	p, how := iam.From(ctx)
	if how != iam.Resolved {
		return refuse("no person was resolved for the request")
	}
	if reason := assistable(p); reason != "" {
		return refuse(reason)
	}
	presented, held := presentedSessionOf(ctx)
	absolute, bounded := Lifetime(ctx)
	switch {
	case !held || !bounded:
		return refuse("the request presented no browser session")
	case presented.enrolmentOnly:
		return refuse("the session may only enrol a second factor")
	}
	if err := req.check(); err != nil {
		return refuse(err.Error())
	}
	d.mu.Lock()
	bound := d.lineages != nil
	d.mu.Unlock()
	if !bound {
		return refuse("no guard accepts these grants")
	}

	now := d.now()
	lifetime := AssistReadLifetime
	if req.Scope == iam.AssistScopeExec {
		lifetime = AssistExecLifetime
	}
	expires := earliest(now.Add(lifetime), absolute, presented.idle).UTC()
	if !expires.After(now) {
		return refuse("the session ends before a grant could be used")
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, fmt.Errorf("auth: mint an assistant grant's id: %w", err)
	}
	claims := AssistClaims{
		Grant: id, Principal: p.ID, Credential: p.Via,
		Conversation: req.Conversation, Turn: req.Turn, Scope: req.Scope,
		ReauthCeiling: p.ReauthAt.UTC(), Expires: expires,
	}
	if req.Scope == iam.AssistScopeExec {
		u, urlErr := req.Request.URL()
		if urlErr != nil {
			return refuse(urlErr.Error())
		}
		digest, digestErr := opkey.RequestDigestOf(req.Request.Method, u, req.Request.Body)
		if digestErr != nil {
			return refuse(digestErr.Error())
		}
		claims.Proposal, claims.Action, claims.RequestDigest = req.Proposal, req.Action, digest
	}
	if invalid := claims.check(); invalid != nil {
		return refuse(invalid.Error())
	}
	subject, err := encodeClaims(claims)
	if err != nil {
		return nil, err
	}
	token := iam.AssistBearerPrefix + d.signer.Mint(subject, expires.Sub(now))

	d.mu.Lock()
	defer d.mu.Unlock()
	d.prune(now)
	d.live[id] = liveGrant{turn: req.Turn, scope: req.Scope, expires: expires}
	return &grant{id: id, expires: expires, token: token, issuer: d}, nil
}

// check refuses a request no grant is derived for.
func (r AssistRequest) check() error {
	switch {
	case r.Conversation == uuid.Nil || r.Turn == uuid.Nil:
		return errors.New("it names no conversation or no turn")
	case !r.Scope.Valid():
		return fmt.Errorf("scope %q is not one this build derives", r.Scope)
	}
	if r.Scope == iam.AssistScopeRead {
		if r.Proposal != uuid.Nil || r.Action != "" || r.Request != nil {
			return errors.New("a read grant binds no proposal, action or request")
		}
		return nil
	}
	switch {
	case r.Proposal == uuid.Nil || r.Action == "" || r.Request == nil:
		return errors.New("an exec grant needs its proposal, its action and its request")
	case len(r.Request.Body) > AssistMaxBody:
		return fmt.Errorf("the request's body is %d bytes, over the %d an approved "+
			"write may carry", len(r.Request.Body), AssistMaxBody)
	}
	u, err := r.Request.URL()
	if err != nil {
		return err
	}
	if AssistRefusedPath(u.Path) {
		return fmt.Errorf("%s is a surface the assistant never reaches", u.Path)
	}
	return nil
}

// assistable is why p may not ride an assistant grant, or "": [iam.Assistable],
// asked both of the principal a grant is derived for and of the principal it
// is re-composed into, so a grant derived for somebody who may hold one is
// never served to somebody who may not.
func assistable(p iam.Principal) string { return iam.Assistable(p) }

// Revoke ends one grant: a turn stopped, ended or parked, a conversation sent
// to the trash. Revoking a grant this node does not hold is nothing.
func (d *Delegations) Revoke(id uuid.UUID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.live, id)
}

// RevokeTurn ends every grant derived for one turn.
func (d *Delegations) RevokeTurn(turn uuid.UUID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id, g := range d.live {
		if g.turn == turn {
			delete(d.live, id)
		}
	}
}

// prune drops every grant past its expiry, under d.mu. A grant lives minutes at
// most, so the registry holds no more than the turns running now.
func (d *Delegations) prune(now time.Time) {
	for id, g := range d.live {
		if !now.Before(g.expires) {
			delete(d.live, id)
		}
	}
}

// open is the claims of a live grant presented as token, or why it is not one:
// its signature, its expiry and the registry are all required.
func (d *Delegations) open(token string) (AssistClaims, error) {
	raw, ok := strings.CutPrefix(token, iam.AssistBearerPrefix)
	if !ok {
		return AssistClaims{}, errors.New("the bearer is not an assistant grant")
	}
	subject := d.signer.Validate(raw)
	if subject == "" {
		return AssistClaims{}, errors.New("the grant is forged, malformed, signed under " +
			"a key this node does not hold, or expired")
	}
	claims, err := decodeClaims(subject)
	if err != nil {
		return AssistClaims{}, err
	}
	now := d.now()
	if !now.Before(claims.Expires) {
		return AssistClaims{}, errors.New("the grant has expired")
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	held, live := d.live[claims.Grant]
	switch {
	case !live || !now.Before(held.expires):
		return AssistClaims{}, errors.New("the grant was revoked, or never minted on this node")
	case held.scope != claims.Scope || held.turn != claims.Turn:
		return AssistClaims{}, errors.New("the grant's claims are not the ones this node minted")
	case held.spent:
		return AssistClaims{}, errors.New("the exec grant's one request was already sent")
	}
	return claims, nil
}

// consume admits an exec grant's one request, and reports false for a grant
// already spent, revoked or expired: a compare-and-set under the registry's
// lock, so two requests racing on one grant admit exactly one.
func (d *Delegations) consume(id uuid.UUID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	held, live := d.live[id]
	if !live || held.spent || !d.now().Before(held.expires) {
		return false
	}
	held.spent = true
	d.live[id] = held
	return true
}

// encodeClaims is the grant's signed subject: base64url JSON, which carries no
// `.` and so is one runtoken field.
func encodeClaims(c AssistClaims) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("auth: encode an assistant grant: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decodeClaims reads a subject back, refusing a field this build never writes
// and claims it never mints.
func decodeClaims(subject string) (AssistClaims, error) {
	raw, err := base64.RawURLEncoding.DecodeString(subject)
	if err != nil {
		return AssistClaims{}, fmt.Errorf("the grant's claims are not base64url: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var c AssistClaims
	if err := dec.Decode(&c); err != nil {
		return AssistClaims{}, fmt.Errorf("the grant's claims do not decode: %w", err)
	}
	if err := c.check(); err != nil {
		return AssistClaims{}, fmt.Errorf("the grant's claims are not ones this build mints: %w", err)
	}
	return c, nil
}

// earliest is the earliest of the non-zero instants given.
func earliest(first time.Time, rest ...time.Time) time.Time {
	out := first
	for _, t := range rest {
		if !t.IsZero() && t.Before(out) {
			out = t
		}
	}
	return out
}

// grant is the one [exec.Grant] implementation: the token stays here, and the
// orchestrator holds only what it can name and time.
type grant struct {
	id      uuid.UUID
	expires time.Time
	token   string
	issuer  *Delegations
}

func (g *grant) ID() uuid.UUID      { return g.id }
func (g *grant) Expires() time.Time { return g.expires }

// String never prints the token, so a grant logged by mistake discloses
// nothing a request could carry.
func (g *grant) String() string { return iam.AssistName(g.id) }

var _ exec.Grant = (*grant)(nil)

// inProcessKey marks a request [Dispatcher.Do] built. UNEXPORTED, and that is
// the whole guarantee: nothing outside this package can set it, and nothing off
// the wire can set a context value at all.
type inProcessKey struct{}

func withInProcess(ctx context.Context) context.Context {
	return context.WithValue(ctx, inProcessKey{}, true)
}

func inProcess(ctx context.Context) bool {
	marked, _ := ctx.Value(inProcessKey{}).(bool)
	return marked
}

// assistKey carries the claims of the grant a request came through.
type assistKey struct{}

// AssistOf is the claims of the grant the resolved request came through: for
// the act route's audit reference and the approval path. False for any other
// request.
func AssistOf(ctx context.Context) (AssistClaims, bool) {
	c, ok := ctx.Value(assistKey{}).(AssistClaims)
	return c, ok
}

// presentedSession is what the session a request presented says about when it
// ends, beside its absolute deadline ([Lifetime]): the idle deadline its bearer
// carried, and whether it may only enrol a second factor.
type presentedSession struct {
	idle          time.Time
	enrolmentOnly bool
}

type presentedSessionKey struct{}

func withPresentedSession(ctx context.Context, s presentedSession) context.Context {
	return context.WithValue(ctx, presentedSessionKey{}, s)
}

func presentedSessionOf(ctx context.Context) (presentedSession, bool) {
	s, ok := ctx.Value(presentedSessionKey{}).(presentedSession)
	return s, ok
}

// IsAssistBearer reports a bearer shaped as an assistant grant. Shape only:
// nothing is admitted on it.
func IsAssistBearer(candidate string) bool {
	return strings.HasPrefix(candidate, iam.AssistBearerPrefix)
}

// assistEdge is the guard's first answer to a request presenting a grant,
// before routing and before the exemption list: refused over the wire, and
// refused on every path no grant reaches. True when it has answered; false
// lets the request on to [Guard.Resolve], whose arm checks the grant itself.
func (g *Guard) assistEdge(w http.ResponseWriter, r *http.Request) bool {
	if !inProcess(r.Context()) {
		// THE VALUE IS NEVER LOGGED: a grant presented over the wire is
		// a leak, and a log is a second place it would live.
		log.WarnContext(r.Context(), "api_auth_failed", "route", r.URL.Path,
			"reason", "assist_over_wire", "remote", g.Client(r))
		g.countFailed(r)
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return true
	}
	if AssistRefusedPath(r.URL.Path) {
		log.InfoContext(r.Context(), "api_assist_path_refused", "route", r.URL.Path)
		assistRefusal(authz.ReasonAssistRefused, "Ask Crewlet can't reach "+
			r.URL.Path+".").write(w)
		return true
	}
	return false
}

// assist resolves a request presenting a grant: the assistant arm of
// [Guard.bearer].
//
// IN ORDER, each a refusal of its own: the in-process marker, the grant
// itself (signature, expiry, registry), the session it names re-composed from
// the rows and held to the person it was derived for, the scope, and for an
// exec grant its one request's digest and then its single use.
func (g *Guard) assist(r *http.Request, candidate string) (*http.Request, *Refusal) {
	ctx := r.Context()
	refused := func() (*http.Request, *Refusal) {
		return r.WithContext(refusedCredential(iam.WithAnonymous(ctx))), nil
	}
	d := g.delegations
	if !inProcess(ctx) || d == nil {
		return refused()
	}
	claims, err := d.open(candidate)
	if err != nil {
		log.InfoContext(ctx, "api_assist_grant_refused", "route", r.URL.Path,
			"detail", err.Error())
		return refused()
	}
	p, how := d.lineages.PrincipalFor(ctx, claims.Credential, needOf(r.Method))
	switch how {
	case iam.Unknown:
		return r.WithContext(iam.WithUnresolved(ctx, errIdentityUnavailable)), nil
	case iam.Resolved:
	default:
		log.InfoContext(ctx, "api_assist_session_over", "grant", claims.Grant.String())
		return refused()
	}
	if p.ID != claims.Principal || assistable(p) != "" {
		log.InfoContext(ctx, "api_assist_principal_moved", "grant", claims.Grant.String())
		return refused()
	}
	p.Via, p.AssistScope = iam.AssistName(claims.Grant), claims.Scope
	if claims.ReauthCeiling.Before(p.ReauthAt) {
		p.ReauthAt = claims.ReauthCeiling
	}

	switch claims.Scope {
	case iam.AssistScopeRead:
		// A WRITE'S METHOD ONLY AS A CHECK (ADR-0055): `dry_run=true`, which
		// the route's guard then holds to a route that declares it reads
		// the flag ([authz.IsCheck]), so nothing a read grant sends stores
		// anything.
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !authz.AsksCheck(r) {
			return assisted(r, p, claims), assistRefusal(authz.ReasonAssistScope,
				"Ask Crewlet's read access can't send a "+r.Method+" request.")
		}
	case iam.AssistScopeExec:
		body, err := io.ReadAll(io.LimitReader(r.Body, AssistMaxBody+1))
		if err != nil {
			return refused()
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		digest, err := opkey.RequestDigestOf(r.Method, r.URL, body)
		if err != nil || len(body) > AssistMaxBody ||
			subtle.ConstantTimeCompare([]byte(digest), []byte(claims.RequestDigest)) != 1 {
			log.WarnContext(ctx, "api_assist_request_unbound", "grant", claims.Grant.String(),
				"route", r.URL.Path)
			return assisted(r, p, claims), assistRefusal(authz.ReasonAssistScope,
				"This request isn't the one that was approved.")
		}
		if !d.consume(claims.Grant) {
			log.InfoContext(ctx, "api_assist_grant_spent", "grant", claims.Grant.String())
			return refused()
		}
	}
	return assisted(r, p, claims), nil
}

// assisted is r carrying the re-composed principal, the grant's claims and the
// instant the grant ends on its own.
func assisted(r *http.Request, p iam.Principal, claims AssistClaims) *http.Request {
	ctx := iam.WithPrincipal(r.Context(), p)
	ctx = context.WithValue(ctx, assistKey{}, claims)
	return r.WithContext(withLifetime(ctx, claims.Expires))
}

// assistRefusal is the guard's 403 for a grant that does not reach a request:
// `unauthorized`, carrying the authority table's reason under its keys, as
// the table's one mapping writes it ([authz.RefusalFields]).
func assistRefusal(reason authz.Reason, detail string) *Refusal {
	status, code, fields := authz.RefusalFields(authz.Decision{Reason: reason})
	return &Refusal{Status: status, Code: code, Detail: detail, fields: fields}
}
