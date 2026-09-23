package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/session"
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

// RetryIdentitySeconds is the `Retry-After` on an identity 503.
//
// TWO SECONDS, which is an apply loop's own scale rather than a round number:
// what a caller is waiting for is this node's identity applier to commit one
// more batch, and a longer hint would park a signed-in browser on an error
// screen long after the answer changed. It is a hint and never a promise —
// [statelog.StallGrace] is what says a node is behind ENOUGH to be alarmed
// about, and nothing here is a second opinion about that.
const RetryIdentitySeconds = 2

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
	// replayed past the overlap. It bumps the person's revocation epoch,
	// which ends every session they hold.
	//
	// ONCE PER LINEAGE, on the same decision that records the replay: a
	// replayed cookie is refused, and whoever holds it can present it
	// again — before this, every presentation published another
	// revocation, which is a write to the identity log paced by the holder
	// of a cookie this node had already refused.
	//
	// A SEAM RATHER THAN A WRITE FROM HERE, which is internal/iam/session's
	// own rule arriving one layer out: validation runs on every ingress
	// node on every request, and a validator that could append to the log
	// is one an unauthenticated caller can make write. Nil logs and does
	// not write, which is the honest posture for a node with no publisher.
	onReuse func(ctx context.Context, person string)

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

	// Chart resolves a bound person's seat. REQUIRED, and the zero value
	// of the engine's adapter is what a node with no chart domain passes:
	// it answers UNKNOWN to every seat question, which is 503 — never the
	// seatless arm, which would hand somebody bound to a lead's seat an
	// empty handle and serve the request.
	Chart session.Chart

	// External is `api.external_url`.
	External string

	// OnReuse ends every session of a person whose cookie was replayed.
	// Optional; see [Sessions.onReuse].
	OnReuse func(ctx context.Context, person string)

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

// resolve turns a cookie into an answer, or reports that this request carries
// none.
//
// The bool is "this arm applies" rather than "it succeeded": a request with no
// cookie falls through to the Tier A arm, and one with a cookie gets this
// arm's answer whatever it is.
//
// client is the GUARD's resolver of the caller's own address, handed in
// rather than held, because the guard is what reads `api.trusted_proxies`
// and a second reading of it here would be a second answer to "is this peer
// the proxy". It is called only on the paths that record something.
func (s *Sessions) resolve(w http.ResponseWriter, r *http.Request,
	ceiling []iam.Grant, client func(*http.Request) string) (
	iam.Principal, iam.Resolution, *Refusal, bool) {

	cookie := cookieOf(r)
	if cookie == "" {
		return iam.Principal{}, iam.Anonymous, nil, false
	}
	v := s.signer.Validate(r.Context(), s.directory, cookie)
	if v.Reuse {
		s.reuse(r, v, client(r))
	}
	switch v.Answer(needOf(r.Method)) {
	case session.AnswerUnavailable:
		log.WarnContext(r.Context(), "api_session_unavailable",
			"row", string(v.Row), "detail", v.Detail, "error", errText(v.Err))
		return iam.Principal{}, iam.Unknown, nil, true
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
		s.refused(r, v, cookie, client)
		for _, clear := range session.Clears(s.external) {
			http.SetCookie(w, clear)
		}
		return iam.Principal{}, iam.Anonymous, nil, true
	}

	binding := session.ResolveSeat(r.Context(), s.chart, v.Person)
	switch binding.Answer() {
	case session.AnswerUnavailable:
		log.WarnContext(r.Context(), "api_session_seat_unavailable",
			"person", v.Bearer.Person, "seat", v.Person.Seat,
			"detail", binding.Detail, "error", errText(binding.Err))
		return iam.Principal{}, iam.Unknown, nil, true
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
		return s.principal(v, binding, ceiling), iam.Resolved,
			seatRefusal(binding), true
	}

	if v.Reissue != "" {
		http.SetCookie(w, session.Cookie(s.external, v.Reissue,
			v.Bearer.AbsoluteExpiresAt))
	}
	return s.principal(v, binding, ceiling), iam.Resolved, nil, true
}

// principal composes who the holder of a validated bearer is.
//
// THE CEILING IS APPLIED HERE, exactly as it is for a Tier A token and for the
// same reason: `api.auth.max_grants` is a decision-time bound, so a node whose
// ceiling was lowered enforces it on its next request rather than on a row
// somebody has to rewrite. A mixed fleet mid-rollout is a LEGAL state, which
// is why each node publishes a hash of its own ceiling.
func (s *Sessions) principal(v session.Validation, binding session.Binding,
	ceiling []iam.Grant) iam.Principal {

	person := v.Person
	return iam.Principal{
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
		Seat:      binding.Handle(),
		SeatAt:    person.SeatAt,
		Position:  binding.Seat.Unit,
		Grants:    intersect(person.Grants, ceiling),
		Colleague: person.Colleague,
		Stage:     person.Stage,
		// WHEN THIS SESSION LAST PROVED IDENTITY, which is what the
		// step-up column compares against. It is the session's own
		// fact rather than a window this package opens, so a sensitive
		// gesture an hour into a session asks for a password again.
		ReauthAt: person.ReauthAt,
	}
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
	first := s.audit.EmitOnce(ctx, "session_reuse:"+lineage, window,
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
	s.onReuse(ctx, v.Bearer.Person)
}

// refused records what a refusing row means for the audit trail — which, for
// most rows, is nothing.
//
// A MALFORMED VALUE is a credential presented and refused: a forged cookie,
// one signed under a key this deployment does not hold. It is a failed attempt
// of method `bearer`, counted and never a row of its own.
//
// A DEADLINE is the one way a session ends that no record states — the idle
// deadline lives in the bearer and nowhere else — so this is the only frame
// that can ever say a session ended that way, and it says so ONCE PER LINEAGE
// PER NODE: the cookie is cleared by this very answer, and a script that goes
// on replaying it is not a second ending. Every OTHER ended row — the row
// says so, the epoch or the generation moved, the person was suspended — was
// ended by a record, and whoever wrote the record already said so; a second
// announcement from every node a stale cookie reaches would name the wrong
// cause.
func (s *Sessions) refused(r *http.Request, v session.Validation, cookie string,
	client func(*http.Request) string) {

	switch {
	case v.Row == session.RowMalformed:
		s.audit.Failed(r.Context(), authevents.Failure{
			Client: client(r), Method: types.FailBearer, Subject: cookie,
		})
	case v.Row == session.RowEnded && v.Deadline != "":
		reason := types.EndIdle
		if v.Deadline == session.DeadlineAbsolute {
			reason = types.EndAbsolute
		}
		lineage := v.Bearer.Lineage.String()
		s.audit.EmitOnce(r.Context(), "session_ended:"+lineage, 0,
			types.IAMSessionEnded{
				Person: v.Bearer.Person, Lineage: lineage, Reason: reason,
			})
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
