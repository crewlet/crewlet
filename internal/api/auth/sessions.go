package auth

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// HOW A SIGNED-IN PERSON BECOMES A PRINCIPAL.
//
// resolve.go is the other half of this file and says what a Tier A token
// becomes; this is what a browser's cookie becomes. The two meet at
// [Guard.Resolve] and nowhere else, so no route downstream has to know which
// credential shape arrived — which is the whole point of resolving both into
// one [iam.Principal].
//
// # Nothing here decides anything
//
// Every judgement is internal/iam/session's, taken from its two tables: which
// row a bearer landed on, and which seat a validated person acts as. This
// file is the ADAPTER — it reads the cookie off the request, states which
// column of the session table this request needs, and turns the three answers
// into the three [iam.Resolution] arms plus the one refusal that is neither.
// A condition written here would be a second policy the tables could not see.
//
// # Why the header wins when both arrive
//
// A browser sends its cookie on every request whether or not the caller meant
// to present it; an `Authorization` header is only ever there because somebody
// put it there. So an explicit credential is resolved first, and a request
// that presents a WRONG one stays anonymous rather than being silently
// upgraded by an ambient cookie that happens to be in the jar — `curl -H
// "Authorization: Bearer $TOKEN"` from a signed-in browser's session must act
// as the token or as nobody, never as the person.
//
// # And why a seat refusal is not a 401
//
// A person bound to a seat the chart no longer holds is not somebody whose
// session ended: they are exactly who they say they are, and signing in again
// changes nothing. 401 would send a browser to discard its cookie and loop
// through the sign-in page for ever. It is a 403 NAMING THE SEAT, which is
// [session.Binding]'s own rule — and it is written by the guard rather than
// carried on the principal, because an empty handle reaching a person-scoped
// read as "no rows" is the silent failure that rule exists to prevent.

// errIdentityUnavailable is the reason attached to a request whose cookie this
// node could not check.
//
// A SENTENCE AND NOT A BARE SENTINEL, because [iam.WithUnresolved] keeps the
// reason for the gate downstream to choose a status from, and internal/api's
// query registry already tells an estate that could not be read (retryable)
// from a route nobody wired through the guard (a fault that never clears).
// This is the first.
var errIdentityUnavailable = errors.New("auth: this node could not read the " +
	"identity estate, so it cannot say whether this session is live")

// ActsAsThemselves reports a route whose subject is the PERSON rather than
// the seat they hold.
//
// # Why the seat refusal has an exemption at all, and why it is exactly this
//
// A seat refusal exists because a person the chart cannot answer for, given a
// silent empty handle, would write audit rows under nobody's name and read
// every person-scoped query as empty. Every route that does either of those
// acts AS THE SEAT.
//
// The `/auth` surface does neither. Ending a session, re-proving identity,
// enrolling a second factor and regenerating a recovery set are gestures
// about the CREDENTIAL, keyed on the session's own lineage and the person's
// own row — the chart is not consulted by any of them, and none writes a row
// an author column reads. So refusing them buys nothing and costs the one
// thing a leaver must still be able to do: end the live bearer they are
// holding. Without this, an offboarded person keeps a working cookie with no
// way to sign out, and every screen they open loops through a refusal.
//
// A PREFIX AND NOT A LIST, because the whole surface shares the property: a
// route added under /auth is a route about somebody's own credential, which
// is what that path means. Anything else is guarded normally.
func ActsAsThemselves(path string) bool {
	return strings.HasPrefix(path, PathAuthPrefix)
}

// Refusal is an answer the guard writes itself, rather than one a route
// derives from the resolution it was handed.
//
// IT EXISTS FOR EXACTLY ONE CASE and is not a fourth resolution arm: a
// credential that validated and whose SEAT did not — a person whose session is
// live, or a Tier A token the directory binds, bound to a seat the chart will
// not let them act as. Every other outcome is one of [iam.Resolution]'s three,
// which is what every surface downstream already reads.
type Refusal struct {
	// Status is the HTTP status, Code the machine-readable reason, and
	// Detail the sentence that names the seat.
	Status int
	Code   httpjson.Code
	Detail string
}

// seatRefusal is the answer to a credential resolved to a seat the chart will
// not let it act as: 403 NAMING THE SEAT, which is [session.Binding]'s rule.
//
// ONE CONSTRUCTOR FOR BOTH CREDENTIAL ARMS, because a signed-in person and a
// Tier A token bound to one removed seat must be refused in one set of bytes —
// a second spelling is where a code or a status comes to differ, and the
// dashboard branches on the code.
func seatRefusal(binding session.Binding) *Refusal {
	return &Refusal{
		Status: http.StatusForbidden,
		Code:   httpjson.CodeSeatUnavailable,
		Detail: binding.Detail,
	}
}

// RetryIdentitySeconds is the `Retry-After` on an identity 503 — the guard's,
// a route refusing a caller it could not resolve, and every 503 the sign-in
// surface answers.
//
// TWO SECONDS, which is an apply loop's own scale rather than a round number:
// what a caller is waiting for is this node's identity applier to commit one
// more batch, or a coordination or store blip on the order of one lease
// renewal. A longer hint would park a signed-in browser on an error screen
// long after the answer changed; a shorter one turns every blip into a retry
// storm from every open tab, against the estate that is already struggling.
// It is a hint and never a promise — [statelog.StallGrace] is what says a node
// is behind ENOUGH to be alarmed about, and nothing here is a second opinion
// about that.
//
// ONE NUMBER, declared once: internal/api/auth and internal/api/authapi each
// carried a private copy beside this one, which is how three spellings of one
// hint come to disagree.
const RetryIdentitySeconds = 2

// SessionCatchUp is how long a write presenting a session this node has not
// yet applied waits for it, before it is answered 503.
//
// THE PUBLISHER'S OWN RESOLVE BUDGET ([statelog.DefaultResolveBudget]),
// because it is the same wait moved to the other end of one sign-in. A write
// through the identity domain waits that long for this node's applier to reach
// the record the broker acknowledged; the sign-in that minted this bearer
// SKIPPED that wait ([statelog.Request.NoWait]) on the understanding that
// whoever reads the bearer next honours the position it carries — so the
// request that reads it next is given the budget the sign-in did not spend. A
// node still short of the position after it is genuinely behind, which is what
// the 503 and its [RetryIdentitySeconds] hint are for.
const SessionCatchUp = statelog.DefaultResolveBudget

// Applier is this node's identity applier, as the session arm waits on it.
//
// CONSUMER-DEFINED and one method wide: the arm never reads a position, it
// only waits for the one a bearer states — see [Sessions.awaitStart].
type Applier interface {
	// AwaitApplied blocks until this node's identity applier has committed
	// through the packed log position, and answers nil only once it has.
	// Anything else — the context ending, an applier that is gone — is an
	// error, and the caller answers as though it had not waited.
	AwaitApplied(ctx context.Context, position uint64) error
}

// Sessions is the guard's session arm: everything needed to turn a cookie into
// a person.
//
// A VALUE BUILT ONCE AT WIRING, held by the guard and read on every request.
// It carries no cache and no mutable state, for the reason
// internal/iam/session states at length: a cache here would be a second idea
// of who is signed in, with its own staleness, in front of a lookup that is
// already a local read.
type Sessions struct {
	signer    *session.Signer
	directory session.Directory
	chart     session.Chart

	// applier is what a write presenting a session this node has not yet
	// applied waits on — see [Sessions.awaitStart].
	applier Applier

	// external is `api.external_url`, which decides the cookie's NAME. It
	// must be the configured value and never the request's, for the reason
	// [session.CookieName] gives: the engine is behind a TLS-terminating
	// proxy and reads no r.TLS, so asking the request would drop the
	// `__Host-` prefix on exactly the deployments that need it.
	external string

	// audit is where a refused cookie is counted and the two session
	// facts only this arm can see are recorded: a replay, and a deadline
	// passing. See audit.go.
	audit Audit

	// onReuse is called when a bearer's rotation index proves a cookie was
	// replayed past the overlap, with the person and the EPOCH the bearer
	// was minted at. It ends every session the person opened at or below
	// that epoch.
	//
	// ONCE PER LINEAGE, on the same decision that records the replay: a
	// replayed cookie is refused, and whoever holds it can present it
	// again — before this, every presentation published another
	// revocation, which is a write to the identity log paced by the holder
	// of a cookie this node had already refused.
	//
	// AND CONDITIONAL ON THE EPOCH, which is what the correctness rests on
	// rather than the dedupe: the once-per-lineage decision is one NODE's
	// and it is bounded, so another ingress node seeing the same replay —
	// or this one after the lineage was forgotten — asks again, and a
	// revocation that moves the epoch only while it is still at the
	// bearer's is one that lands once however often it is asked for.
	//
	// A SEAM RATHER THAN A WRITE FROM HERE, which is internal/iam/session's
	// own rule arriving one layer out: validation runs on every ingress
	// node on every request, and a validator that could append to the log
	// is one an unauthenticated caller can make write. Nil logs and does
	// not write, which is the honest posture for a node with no publisher.
	onReuse func(ctx context.Context, person string, epoch uint64)

	// now is the clock, injectable so a case can pin what a principal's
	// freshness is measured against.
	now func() time.Time
}

// SessionsDeps is what the session arm is built from.
type SessionsDeps struct {
	// Signer verifies a bearer's signature. REQUIRED: without it there is
	// nothing to validate against, and a nil one would make every cookie
	// unreadable rather than refused.
	Signer *session.Signer

	// Directory resolves the session and person rows. REQUIRED for the
	// same reason.
	Directory session.Directory

	// Applier is this node's identity applier, waited on by a write that
	// presents a session the node has not applied yet. REQUIRED: a sign-in
	// answers before this node applies the session it opened, so without
	// it every write a client makes straight after signing in — `crewlet
	// iam token -login`'s mint is exactly that — is a 503 on every node.
	Applier Applier

	// Chart resolves a bound person's seat. REQUIRED, and the zero value
	// of the engine's adapter is what a node with no chart domain passes:
	// it answers UNKNOWN to every seat question, which is 503 — never the
	// seatless arm, which would hand somebody bound to a lead's seat an
	// empty handle and serve the request.
	Chart session.Chart

	// External is `api.external_url`.
	External string

	// OnReuse ends every session of a person whose cookie was replayed,
	// up to the epoch the replayed bearer carries. Optional; see
	// [Sessions.onReuse].
	OnReuse func(ctx context.Context, person string, epoch uint64)

	// Audit records what this arm sees. REQUIRED, and not only for the
	// rows: the revocation a replay triggers is taken on the same
	// once-per-lineage decision that records it, so an arm with no trail
	// would have nothing to stop a refused cookie writing a revocation
	// every time it was presented.
	Audit Audit

	// Now is the clock. Nil takes UTC wall time.
	Now func() time.Time
}

// NewSessions builds the arm, or says which half is missing.
func NewSessions(deps SessionsDeps) (*Sessions, error) {
	switch {
	case deps.Signer == nil:
		return nil, errors.New("auth: the session arm needs a signer; " +
			"without one every cookie is unverifiable, which is not the " +
			"same answer as refused")
	case deps.Directory == nil:
		return nil, errors.New("auth: the session arm needs the identity " +
			"directory — a bearer's signature says it was minted here and " +
			"the rows say whether it is still live")
	case deps.Applier == nil:
		return nil, errors.New("auth: the session arm needs the identity " +
			"applier to wait on — a sign-in answers before this node applies " +
			"the session it opened, so without it every write made straight " +
			"after signing in is refused 503")
	case deps.Chart == nil:
		return nil, errors.New("auth: the session arm needs a chart seam; " +
			"a nil one would resolve every bound person as seatless, which " +
			"is the one fall-through internal/iam/session forbids")
	case deps.Audit == nil:
		return nil, errors.New("auth: the session arm needs an audit trail; " +
			"a replayed cookie's revocation is taken once per lineage on the " +
			"trail's own decision, so without one every presentation of a " +
			"refused cookie would write another")
	}
	s := &Sessions{
		signer: deps.Signer, directory: deps.Directory, chart: deps.Chart,
		applier:  deps.Applier,
		external: deps.External, onReuse: deps.OnReuse, audit: deps.Audit,
		now: deps.Now,
	}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	return s, nil
}

// WithSessions installs the session arm and returns the guard for chaining.
//
// CALLED ONCE, AT WIRING TIME, for [Guard.BindSeats]' reason: it is read on
// every request, and a seam that moved under one in flight would resolve one
// request two ways. Nil is an ordinary wiring — a node whose keyring cannot
// sign for the fleet serves no sign-in surface and therefore has no cookie to
// resolve — and leaves the Tier A arm as the whole of authentication.
func (g *Guard) WithSessions(s *Sessions) *Guard {
	g.sessions = s
	return g
}

// cookieOf is the bearer a request presents, or empty.
//
// BOTH NAMES ARE READ, because a deployment's scheme decides which one a
// browser holds and a node whose `api.external_url` was just corrected from
// http to https would otherwise refuse every cookie already in every jar
// until each person signed in again. The rule is [session.Presented]'s, so the
// sign-out that ends a bearer reads exactly the ones this resolves.
func cookieOf(r *http.Request) string { return session.Presented(r) }

// needOf is which column of the session table this request reads.
//
// THE METHOD AND NOT THE ROUTE, because the distinction the table draws is
// between a request that only READS this node's rows and one that changes
// something: a node that is behind may serve the first from what it has and
// must not accept the second. A route table would be a second answer to a
// question `r.Method` already settles, and the two would disagree the first
// time somebody mounted a GET that writes.
func needOf(method string) session.Need {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return session.NeedRead
	}
	return session.NeedWrite
}

// sessionAnswer is what the session arm made of one request.
type sessionAnswer struct {
	principal iam.Principal
	how       iam.Resolution
	refusal   *Refusal

	// presented is "this arm applies" rather than "it succeeded": a request
	// with no cookie falls through to the Tier A arm, and one with a cookie
	// gets this arm's answer whatever it is.
	presented bool

	// tierA is the entry a session exchanged from a Tier A token stands
	// for, set on a served session whose subject is a token rather than a
	// person. The GUARD composes that principal, through the one function
	// the token's own bearer is composed through — see [Guard.exchanged] —
	// and via is the session it arrived through, which the guard records
	// beside it as a person's session is recorded beside them.
	tierA *config.APIToken
	via   string

	// malformed is the cookie value when it is not a bearer of this format
	// at all — forged, truncated, or signed under a key this deployment
	// does not hold — which is a credential presented and refused. The
	// guard MARKS it and a guarded route's refusal counts it; see
	// audit.go for why the count is not taken here.
	malformed string
}

// resolve turns a cookie into an answer, or reports that this request carries
// none.
//
// client is the GUARD's resolver of the caller's own address, handed in
// rather than held, because the guard is what reads `api.trusted_proxies`
// and a second reading of it here would be a second answer to "is this peer
// the proxy". It is called only on the paths that record something.
//
// ceiling and proof are the guard's own `api.auth.max_grants` and step-up
// window, applied to a person as they are to a token.
//
// tokens is the guard's Tier A entries by login, for a session exchanged from
// one: see [tierASubjects].
func (s *Sessions) resolve(w http.ResponseWriter, r *http.Request,
	ceiling []iam.Grant, proof proofWindows, client func(*http.Request) string,
	tokens func(login string) (config.APIToken, bool)) sessionAnswer {

	cookie := cookieOf(r)
	if cookie == "" {
		return sessionAnswer{how: iam.Anonymous}
	}
	subjects := tierASubjects{directory: s.directory, tokens: tokens}
	need := needOf(r.Method)
	v := s.signer.Validate(r.Context(), subjects, cookie)
	if v.Row == session.RowBehind && v.Answer(need) == session.AnswerUnavailable {
		v = s.awaitStart(r.Context(), subjects, cookie, v)
	}
	if v.Reuse {
		s.reuse(r, v, client(r))
	}
	switch v.Answer(need) {
	case session.AnswerUnavailable:
		log.WarnContext(r.Context(), "api_session_unavailable",
			"row", string(v.Row), "detail", v.Detail, "error", errText(v.Err))
		return sessionAnswer{how: iam.Unknown, presented: true}
	case session.AnswerServe:
	default:
		// EVERY REFUSING ROW IS ANONYMOUS, and the cookie is CLEARED so
		// a browser stops re-presenting a value that can never work
		// again. The log line carries which row decided; the caller
		// gets one code for all of them, because telling somebody
		// whether their session was revoked, expired, swept or never
		// existed tells an attacker holding it the same.
		log.InfoContext(r.Context(), "api_session_refused",
			"row", string(v.Row), "detail", v.Detail)
		s.ended(r, v, subjects)
		for _, clear := range session.Clears(s.external) {
			http.SetCookie(w, clear)
		}
		answer := sessionAnswer{how: iam.Anonymous, presented: true}
		if v.Row == session.RowMalformed {
			answer.malformed = cookie
		}
		return answer
	}

	if tokens != nil {
		if entry, isToken := tokens(v.Bearer.Person); isToken {
			// A TOKEN'S SESSION, served: the row, the deadlines and the
			// generation all checked out, and the entry is still held.
			// Who it is belongs to the guard, which composes it from
			// the entry exactly as it composes the token's bearer.
			s.reissue(w, v)
			return sessionAnswer{how: iam.Resolved, presented: true, tierA: &entry,
				via: iam.SessionName(v.Bearer.Lineage.String())}
		}
	}

	binding := session.ResolveSeat(r.Context(), s.chart, v.Person)
	switch binding.Answer() {
	case session.AnswerUnavailable:
		log.WarnContext(r.Context(), "api_session_seat_unavailable",
			"person", v.Bearer.Person, "seat", v.Person.Seat,
			"detail", binding.Detail, "error", errText(binding.Err))
		return sessionAnswer{how: iam.Unknown, presented: true}
	case session.AnswerServe:
	default:
		// THEY ARE STILL RESOLVED, and that is the whole difference
		// between this refusal and an ended session: this node knows
		// exactly who they are and cannot say what they act AS. The
		// refusal beside the principal is what stops them reaching
		// anything that would use a seat; [ActsAsThemselves] is the
		// one surface it does not cover, and nothing there reads one.
		//
		// [iam.Principal.SeatAt] IS KEPT while Seat is empty, which is
		// the pair that says "a binding was decided and this node will
		// not honour it" — a principal with neither is a genuinely
		// seatless person, and the two must never read alike.
		//
		// THE COOKIE IS NOT CLEARED, unlike a refused session: the
		// bearer is live and works the moment somebody rebinds them,
		// and discarding it would sign out a person whose only problem
		// is a chart edit.
		return sessionAnswer{
			principal: s.principal(v, binding, ceiling, proof), how: iam.Resolved,
			refusal: seatRefusal(binding), presented: true,
		}
	}

	s.reissue(w, v)
	return sessionAnswer{
		principal: s.principal(v, binding, ceiling, proof), how: iam.Resolved,
		presented: true,
	}
}

// awaitStart waits for this node to apply the start of the session a write
// presents, and validates the bearer again once it has — or answers the row it
// already had.
//
// # Why a write waits and a read does not
//
// A sign-in answers BEFORE any node's applier reaches the session it opened
// ([statelog.Request.NoWait]): the broker has acknowledged the record, the
// bearer carries the position it landed at, and what the sign-in owes in
// return is that whoever reads the bearer next honours that position. A read
// always has — [session.RowBehind] serves it on the signature and the epoch. A
// write may not be served on those alone, because it acts on the rows, and it
// used to be refused 503 outright: on EVERY node for the few hundred
// milliseconds an apply takes, which is exactly when a client that signs in
// and then acts makes its first request. `crewlet iam token -login` signs in
// and mints at once, and was refused on every real run, with the node it asked
// a fraction of a second from being able to answer.
//
// So the write waits for the one position its bearer states — the wait the
// sign-in skipped — and is then decided on the rows like any other request.
//
// # Bounded, and a miss is the answer it already had
//
// [SessionCatchUp] bounds it, and a node that does not arrive inside it is
// behind for real: the row stays [session.RowBehind] and the request is
// answered 503 with its retry hint, as before. Nothing is waited for that the
// bearer did not state, and the bearer is SIGNED, so a caller cannot name a
// position of its choosing to park a request on.
func (s *Sessions) awaitStart(ctx context.Context, directory session.Directory,
	cookie string, behind session.Validation) session.Validation {

	wait, cancel := context.WithTimeout(ctx, SessionCatchUp)
	defer cancel()
	if err := s.applier.AwaitApplied(wait, behind.Bearer.StartPosition); err != nil {
		log.DebugContext(ctx, "api_session_start_not_applied",
			"lineage", behind.Bearer.Lineage.String(),
			"position", behind.Bearer.StartPosition, "error", err)
		return behind
	}
	return s.signer.Validate(ctx, directory, cookie)
}

// reissue sets the cookie a served validation re-issued, if it re-issued one.
func (s *Sessions) reissue(w http.ResponseWriter, v session.Validation) {
	if v.Reissue != "" {
		http.SetCookie(w, session.Cookie(s.external, v.Reissue,
			v.Bearer.AbsoluteExpiresAt))
	}
}

// tierASubjects is the directory a bearer is validated against, answering a
// Tier A token's own subject from the CONFIGURATION rather than from a row.
//
// # Why a session can stand for a token at all
//
// `POST /auth/token` exchanges a Tier A bearer for a session, so a browser can
// present the deployment's break-glass credential without holding its value.
// Such a session names the token's LOGIN (`token:<id>`) as its subject, and a
// token has no directory row: its authority is a line in a config file. So
// every fact validation reads about the SESSION — its row, whether it ended,
// the fleet's generation, this node's lag and deferrals — is the directory's,
// read as for anybody, and the one fact about its SUBJECT is whether this node
// still holds the entry.
//
// A GONE ENTRY IS A SUBJECT THAT MAY NOT ACT, answered as a retired principal:
// validation then ends the session wherever this node stands against its start
// record, and the cookie is cleared. Answered as ABSENT instead, a node below
// the start position would serve reads on a credential the operator withdrew
// — the absent-row grace exists for a record not yet applied, and a
// configuration entry is never late.
//
// A person's subject is a uuid and a token's carries the `token:` class, which
// no uuid can, so the two never share a subject; nil tokens answers everything
// from the directory.
type tierASubjects struct {
	directory session.Directory
	tokens    func(login string) (config.APIToken, bool)
}

// Resolve answers one bearer's facts.
func (d tierASubjects) Resolve(ctx context.Context, lineage, person string) (
	session.Identity, error) {

	identity, err := d.directory.Resolve(ctx, lineage, person)
	if err != nil || d.tokens == nil ||
		!strings.HasPrefix(person, iam.TokenLoginPrefix) {
		return identity, err
	}
	// THE EPOCH IS THE DIRECTORY'S, kept: "sign out everywhere" from a
	// token's session bumps it under the token's login, which is what ends
	// every session exchanged from that token.
	epoch := identity.Person.Epoch
	if _, held := d.tokens(person); !held {
		identity.Person = session.PersonRow{Found: true, Stage: iam.StageRetired,
			Login: person, Epoch: epoch}
		return identity, nil
	}
	// NO GRANTS AND NO SEAT on the row: who a token's session acts as is
	// composed from the entry by the guard, on every request.
	identity.Person = session.PersonRow{Found: true, Stage: iam.StageActive,
		Login: person, Epoch: epoch}
	return identity, nil
}

// principal composes who the holder of a validated bearer is.
//
// THE CEILING IS APPLIED HERE, exactly as it is for a Tier A token and for the
// same reason: `api.auth.max_grants` is a decision-time bound, so a node whose
// ceiling was lowered enforces it on its next request rather than on a row
// somebody has to rewrite. A mixed fleet mid-rollout is a LEGAL state, which
// is why each node publishes a hash of its own ceiling.
//
// AND SO IS THE STEP-UP WINDOW, for the same reason: `api.auth.session.step_up`
// is this node's own setting, and the session row states only WHEN its holder
// proved who they are. The principal's [iam.Principal.ReauthAt] is the instant
// that proof stops counting — the proof plus this node's window — so a
// shortened window takes effect on the next request, and a session that
// proved nothing (a row this node has not applied) is never fresh. A Tier A
// token's exchanged cookie never reaches here: the guard composes it from the
// entry, exactly as it composes the token's bearer.
func (s *Sessions) principal(v session.Validation, binding session.Binding,
	ceiling []iam.Grant, proof proofWindows) iam.Principal {

	person := v.Person
	p := iam.Principal{
		// THE PERSON'S OWN ID, parsed from the bearer. A bearer that
		// reached here verified, so the value is one this engine wrote.
		ID:    personID(v.Bearer.Person),
		Login: person.Login,
		Kind:  iam.KindPerson,
		// THE SEAT COMES FROM THE BINDING AND NEVER FROM THE ROW,
		// because the row's handle is what was written down and the
		// binding's is what the chart answers to NOW — a renamed seat
		// resolves through the chart's own former-handle trail, and
		// taking the row would write audit rows under a handle nothing
		// answers to.
		Seat:     binding.Handle(),
		SeatAt:   person.SeatAt,
		Position: binding.Seat.Unit,
		// WHAT THE PERSON WAS GIVEN HERE AND WHAT THIS SESSION CARRIES,
		// clamped to this node's ceiling. The carried half is the
		// identity provider's group mapping, recorded on the session
		// that presented it: it used to be merged into the sign-in's
		// sighting and then dropped, so no group mapping ever conferred
		// anything. The zero session — a row this node has not applied —
		// carries nothing, which only ever narrows.
		Grants:    intersect(union(person.Grants, v.Session.GroupGrants), ceiling),
		Colleague: person.Colleague,
		Stage:     person.Stage,
		// AND THROUGH WHAT: this session, by its lineage. The person is
		// the author of what they write, and the operator column beside
		// them says which sign-in it came through — it used to repeat
		// their login, which the author already names, so two browsers
		// or a tab left open on a shared machine were one name in every
		// trail. The lineage is verified: the bearer reached here signed.
		Via: iam.SessionName(v.Bearer.Lineage.String()),
	}
	// WHEN THIS SESSION'S PROOF STOPS COUNTING, for each window. It used
	// to be read off a person field nothing ever set, so every session
	// was stale from its first request and no person could reach a
	// step-up surface at all — enrolling a second factor included.
	proof.stamp(&p, v.Session.ProvedAt)
	return p
}

// union is every grant in either set, once each, in the order they were first
// named.
//
// A UNION AND NEVER A REPLACEMENT, because the two answer different
// questions: a person's declared grants are what this company gave them, and
// the carried ones are what their directory membership said when they signed
// in. Replacing either with the other would make a group removal at the
// provider silently revoke something an administrator granted here, or an
// administrator's edit silently drop what the provider asserted.
func union(declared, carried []iam.Grant) []iam.Grant {
	out := slices.Clone(declared)
	for _, g := range carried {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out
}

// proofWindows is this node's two step-up windows: `api.auth.session.step_up`
// and `step_up_sensitive`.
//
// THE NODE'S OWN, applied when a principal is composed, for the ceiling's
// reason: a session row states only WHEN its holder proved who they are, so a
// shortened window takes effect on this node's next request with nothing
// rewritten, and a fleet mid-rollout legally disagrees about it.
type proofWindows struct {
	stepUp, sensitive time.Duration
}

// windowsOf reads the two windows off the session settings, defaults applied.
func windowsOf(s config.APISession) proofWindows {
	return proofWindows{stepUp: s.StepUp(), sensitive: s.StepUpSensitive()}
}

// stamp gives p the two deadlines a proof taken at provedAt earns: the proof
// plus each window.
//
// A ZERO provedAt STAMPS NOTHING, which [iam.Principal.Proved] reads as stale
// in both windows: a session that proved nothing — one exchanged from no
// person, or a row this node has not applied yet — must never read as fresh,
// and a proof of the zero instant plus an hour would read as long stale only
// by luck.
func (w proofWindows) stamp(p *iam.Principal, provedAt time.Time) {
	if provedAt.IsZero() {
		p.ReauthAt, p.SensitiveReauthAt = time.Time{}, time.Time{}
		return
	}
	p.ReauthAt = provedAt.Add(w.stepUp)
	p.SensitiveReauthAt = provedAt.Add(w.sensitive)
}

// stampOrdinary gives p the ORDINARY window's deadline for a proof taken at
// provedAt, and none for the sensitive one — which [iam.Principal.Proved] reads
// as stale there, for ever.
//
// WHAT A MACHINE TOKEN EARNS. It has nobody at a keyboard and nothing else to
// present, so presenting it is its whole proof for the gestures an automation
// makes — a configuration, a chart, a credential or a directory write. The
// sensitive gestures are the ones that need a PERSON present — a secret's
// value, somebody's authority, how somebody proves who they are, every session
// at once — and a token proves nobody is. Stamped fresh in both windows, a
// token reached a sensitive gesture wherever a verb admitted its owner as
// THEMSELVES rather than on a grant: the safety argument was that no token
// carries a grant the sensitive rows ask for, and a self arm asks none. The
// break-glass credential keeps both windows ([Guard.principalFor]), because it
// has to reach a sensitive gesture on the day the identity provider is down.
func (w proofWindows) stampOrdinary(p *iam.Principal, provedAt time.Time) {
	w.stamp(p, provedAt)
	p.SensitiveReauthAt = time.Time{}
}

// personID parses the id a bearer carries.
//
// AN UNPARSEABLE ONE IS THE ZERO UUID rather than a refusal, and it cannot be
// reached from a verified bearer: the value was written by [Signer.Mint] from
// a uuid this engine minted. What makes it a conversion rather than a check is
// that the row is what authorises — [iam.Principal.ID] is an audit key — so a
// refusal here would turn a formatting surprise into an outage for one person
// while the row that governs them is perfectly readable.
func personID(value string) uuid.UUID {
	parsed, err := uuid.Parse(value)
	if err != nil {
		return uuid.UUID{}
	}
	return parsed
}

// reuse records a replayed cookie and asks for the person's epoch to be
// bumped — ONCE per lineage, for as long as the bearer could still be
// presented.
//
// THE ROW AND THE REVOCATION ARE ONE DECISION. The replayed bearer is refused
// and nothing stops whoever holds it presenting it again, and the rotation
// check that recognises a replay runs before any row is read, so every
// presentation reaches here. Taken per presentation, the revocation was a
// write to the identity log paced by the holder of a cookie this node had
// already turned away; taken once, it has already ended every session the
// person held, which is all a second one could do.
//
// THE WINDOW IS THE BEARER'S OWN REMAINING LIFETIME: past its absolute
// deadline the deadline check refuses it before the rotation is looked at,
// so it can never reach here again.
func (s *Sessions) reuse(r *http.Request, v session.Validation, remote string) {
	ctx := r.Context()
	lineage := v.Bearer.Lineage.String()
	window := v.Bearer.AbsoluteExpiresAt.Sub(s.now())
	first := s.audit.EmitOnce(ctx, authevents.OnceSessionReuse, lineage, window,
		types.IAMSessionReuseDetected{
			Person: v.Bearer.Person, Lineage: lineage,
			Rotation: v.Bearer.Rotation, Remote: remote,
		})
	if !first {
		log.DebugContext(ctx, "iam_session_reuse_repeated",
			"lineage", lineage, "detail", "already recorded and acted on")
		return
	}
	// AT WARN whether or not a writer is wired: the log line is the
	// evidence an investigation looks for, and a node with no publisher
	// must not make the event invisible as well as unactionable.
	log.WarnContext(ctx, "iam_session_reuse_detected",
		"person", v.Bearer.Person, "lineage", lineage, "detail", v.Detail)
	if s.onReuse == nil {
		return
	}
	s.onReuse(ctx, v.Bearer.Person, v.Bearer.Epoch)
}

// ended records what a refusing row means for the audit trail — which, for
// most rows, is nothing.
//
// A MALFORMED VALUE is a credential presented and refused, and it is not
// recorded here: [Sessions.resolve] hands it back for the guard to mark,
// because whether it was somebody's failed attempt depends on whether the
// route needed it — see audit.go.
//
// A DEADLINE is the one way a session ends that no record states — the idle
// deadline lives in the bearer and nowhere else — so this is the only frame
// that can ever say a session ended that way; see [Sessions.deadline]. Every
// OTHER ended row — the row says so, the epoch or the generation moved, the
// person was suspended — was ended by a record, and whoever wrote the record
// already said so.
func (s *Sessions) ended(r *http.Request, v session.Validation,
	directory session.Directory) {

	if v.Row == session.RowEnded && v.Deadline != "" {
		s.deadline(r, v, directory)
	}
}

// deadline announces a session its own deadline ended — ONCE, and only when a
// deadline is what ended it.
//
// # Once per lineage per node
//
// The cookie is cleared by the refusal this rides on, and a script that goes
// on replaying it is not a second ending, so the lineage is CLAIMED before
// anything is read: a replay costs a map lookup and never a read of the
// estate.
//
// # And only when no record got there first
//
// The deadlines are decided before any row is read, so a session a RECORD
// ended — revoked, signed out everywhere, its person removed or suspended, the
// company's generation bumped — is refused on its deadline too once its cookie
// outlives it: a person revoked on Monday whose other browser presents the
// cookie on Tuesday. Whoever wrote that record already announced the ending,
// and a second row naming `idle` or `absolute` would name the wrong cause for
// a session that was over a day earlier. So the rows are asked
// ([session.Standing]) — the same directory validation read through, a token's
// exchanged session included — and the ending is announced only when they
// say the session was live. A node that cannot say HANDS THE CLAIM BACK and
// announces nothing: a fact nobody could confirm is not one to announce, and
// the next presentation asks again.
func (s *Sessions) deadline(r *http.Request, v session.Validation,
	directory session.Directory) {

	ctx := r.Context()
	lineage := v.Bearer.Lineage.String()
	release, claimed := s.audit.Claim(ctx, authevents.OnceSessionEnded, lineage, 0)
	if !claimed {
		return
	}
	standing := session.Standing(ctx, directory, v.Bearer)
	switch standing.Row {
	case session.RowValid:
		reason := types.EndIdle
		if v.Deadline == session.DeadlineAbsolute {
			reason = types.EndAbsolute
		}
		s.audit.Emit(ctx, types.IAMSessionEnded{
			Person: v.Bearer.Person, Lineage: lineage, Reason: reason,
		})
	case session.RowBehind, session.RowStalled:
		log.DebugContext(ctx, "iam_session_deadline_unconfirmed",
			"lineage", lineage, "row", string(standing.Row),
			"detail", standing.Detail, "error", errText(standing.Err))
		release()
	default:
		// ENDED BY A RECORD, or collected by the sweep — said already, by
		// whatever did it. The claim stays: nothing a later presentation
		// could read would make this ending the deadline's.
	}
}

// errText is an error's message, or empty — so a log line carries the field
// only when there is one.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
