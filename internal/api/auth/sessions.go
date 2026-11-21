package auth

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// HOW A SIGNED-IN PERSON BECOMES A PRINCIPAL (resolve.go does a Tier A token;
// both meet at [Guard.Resolve]). Nothing here decides anything: every judgement
// is internal/iam/session's, and a condition here would be a second policy.
//
// The header wins when both arrive: a browser sends its cookie on every request
// but `Authorization` is only there on purpose, so a WRONG explicit credential
// stays anonymous rather than being upgraded by an ambient cookie.
//
// A seat refusal is not a 401, which would loop a browser through sign-in: the
// person is who they say they are. It is a 403 NAMING THE SEAT
// ([session.Binding]'s rule), written by the guard because an empty handle read
// as "no rows" is the silent failure that rule prevents.

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
// A seat refusal exists because an empty handle would write audit rows under
// nobody's name and read person-scoped queries as empty: routes that do either
// act AS THE SEAT. `/auth` (ending a session, re-proving identity, enrolling a
// factor, recovery codes) is about the CREDENTIAL, never consults the chart and
// writes no author column. Refusing it would stop a leaver ending their live
// bearer, leaving an offboarded person a cookie with no way to sign out.
//
// A prefix and not a list: a route added under /auth is about somebody's own
// credential. Anything else is guarded normally.
func ActsAsThemselves(path string) bool {
	return strings.HasPrefix(path, PathAuthPrefix)
}

// Refusal is an answer the guard writes itself, rather than one a route
// derives from the resolution it was handed.
//
// IT EXISTS FOR EXACTLY TWO CASES and is not a fourth resolution arm, because
// in both the caller is somebody this node knows perfectly well and what is
// refused is what they may reach: a credential that validated and whose SEAT
// did not — a person whose session is live, or a Tier A token the directory
// binds, bound to a seat the chart will not let them act as — and a session
// that may only ENROL A SECOND FACTOR. Every other outcome is one of
// [iam.Resolution]'s three, which is what every surface downstream already
// reads.
type Refusal struct {
	// Status is the HTTP status, Code the machine-readable reason, and
	// Detail the sentence that says what was refused and why.
	Status int
	Code   httpjson.Code
	Detail string

	// spares names the routes this refusal does not reach — see
	// [Refusal.Applies].
	spares func(r *http.Request) bool

	// fields is the machine-readable detail beside the sentence, or nil:
	// an assistant grant's refusal carries the authority table's reason
	// under its own keys ([assistRefusal]).
	fields httpjson.Detail
}

// write answers the refusal in the engine's envelope: the sentence as
// `detail`, and any machine-readable fields beside it.
//
// THE ONE WRITER, so a refusal the guard writes looks the same whichever frame
// decided it.
func (f *Refusal) write(w http.ResponseWriter) {
	detail := httpjson.Detail{"detail": f.Detail}
	maps.Copy(detail, f.fields)
	httpjson.FailWithFields(w, f.Status, f.Code, detail)
}

// Applies reports whether this refusal answers r, or whether r is one of the
// routes it leaves the caller.
//
// EACH REFUSAL CARRIES ITS OWN, because the two leave different things: a seat
// refusal leaves the whole `/auth/` surface, whose subject is the person's own
// credential rather than the seat ([ActsAsThemselves]), and an enrolment
// refusal leaves exactly the three guarded routes that let a person in
// ([EnrolmentAdmits]). One rule in the middleware for both was either an
// enrolment-only session reaching every `/auth/` route — recovery codes and
// signing everybody out included — or an offboarded person unable to sign out.
func (f *Refusal) Applies(r *http.Request) bool {
	return f.spares == nil || !f.spares(r)
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
		spares: func(r *http.Request) bool { return ActsAsThemselves(r.URL.Path) },
	}
}

// enrolmentRefusal is the answer to a session that may only enrol a second
// factor, on every guarded route but the three [EnrolmentAdmits] names: 403
// `second_factor_enrolment_required`.
//
// A session and not a sign-in refusal: a person with no second factor (newly
// invited, or administrator-reset) has nothing else to present, so refusing the
// sign-in would lock them out of the gesture that satisfies the rule. The
// sign-in succeeds into a session that can only enrol, and enrolling replaces
// it with a whole one.
//
// The person is still resolved, as for a seat refusal: the three routes need
// to know who they are, and the cookie is not cleared.
func enrolmentRefusal() *Refusal {
	return &Refusal{
		Status: http.StatusForbidden,
		Code:   httpjson.CodeSecondFactorEnrolmentRequired,
		Detail: "This company requires two-step verification. Set it up " +
			"(POST " + PathAuthTOTP + ") before you do anything else.",
		spares: EnrolmentAdmits,
	}
}

// EnrolmentAdmits reports whether r is one of the three GUARDED routes an
// enrolment-only session may reach: `GET /auth/session` (to render the
// enrolment), `POST /auth/totp` (the enrolment, whose completion replaces this
// session) and `POST /auth/step-up` (enrolling asks a proof inside the step-up
// window; a slow person must re-confirm without signing out). Sign-out is
// [Unguarded].
//
// An exact list, never a prefix: the surface also regenerates recovery codes
// (which wait for the whole session: a set minted on a password alone would
// satisfy the rule without an authenticator), signs out everywhere and ends
// others' sessions.
func EnrolmentAdmits(r *http.Request) bool {
	switch r.URL.Path {
	case PathAuthSession:
		return r.Method == http.MethodGet || r.Method == http.MethodHead
	case PathAuthTOTP, PathAuthStepUp:
		return r.Method == http.MethodPost
	}
	return false
}

// RetryIdentitySeconds is the `Retry-After` on an identity 503: the guard's,
// and every sign-in surface's.
//
// Two seconds is an apply loop's scale: the wait is for the identity applier
// to commit one more batch, or a blip on the order of one lease renewal.
// Longer parks a browser on an error screen after the answer changed; shorter
// turns every blip into a retry storm from every open tab. A hint, never a
// promise: [statelog.StallGrace] says when a node is behind enough to alarm.
// One number, declared once, so copies cannot disagree.
//
// A 503 whose CAUSE is in hand uses [RetryIdentity], since the cause may be a
// refusal no wait clears.
const RetryIdentitySeconds = 2

// RetryIdentity is the `Retry-After` on an identity 503, in seconds, given
// what caused it: [RetryIdentitySeconds] unless the state log's own rule —
// [statelog.RetryAfter], every surface's — says otherwise. ZERO, which writes
// no header at all, for a refusal waiting cannot clear (an oversized record, a
// full log, an evicted node, an undecodable record). A read refusal's own
// derived hint where it carries one; a nil or non-state-log cause is the
// constant. It takes the cause so a `record_too_large` write is not told to
// retry in two seconds: a gate in /iam and /auth refuses the bare constant at
// every 503, and a site with nothing in hand passes nil.
func RetryIdentity(cause error) int {
	return httpjson.RetrySeconds(statelog.RetryAfter(cause,
		RetryIdentitySeconds*time.Second))
}

// UnavailableIdentity answers a request the identity estate could not serve:
// `503 identity_unavailable` under the hint [RetryIdentity] gives cause, and,
// where cause is a refusal the state log made, its sentence for a person
// ([statelog.ConditionSentence]) in `detail`, whose advice agrees with that
// hint. nil where nothing caused it, which answers the code's own words.
//
// THE REFUSAL'S OWN WORDS, NOT THE CODE'S, because the code's ("can't check
// who you are right now, try again in a moment") asked for a wait under no
// Retry-After where none clears the refusal — a full log, an evicted node — and
// on a directory listing was not about who the caller is at all. One writer
// for every site on /iam and /auth, so a read and a write of one surface tell
// one refusal in one sentence.
func UnavailableIdentity(w http.ResponseWriter, cause error) {
	var detail httpjson.Detail
	if said, ok := statelog.ConditionSentence(cause); ok {
		detail = httpjson.Detail{"detail": said}
	}
	httpjson.UnavailableWith(w, httpjson.CodeIdentityUnavailable,
		RetryIdentity(cause), detail)
}

// UnreadLevel is the level an identity read a request could not make is
// logged at: WARN, because a node that cannot read its identity estate is
// one an operator should hear about — unless the request itself has gone
// away, whose read failing IS the cancellation and says nothing about the
// estate, so DEBUG.
//
// A browser abandons requests routinely — a page reloading its data after a
// password change cancels its own reads in flight — and each one was logged
// `api_session_unavailable row=stalled ... context canceled` beside
// `api_auth_unavailable`, which an operator reads as a stalled identity
// estate.
func UnreadLevel(ctx context.Context) slog.Level {
	if ctx.Err() != nil {
		return slog.LevelDebug
	}
	return slog.LevelWarn
}

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

// SessionClientWait is how long a client waits for `GET /auth/session` before
// it says the engine has not answered: twice [SessionCatchUp], the longest the
// guard holds a request presenting a session this node has not applied, which
// leaves as long again for the round trip. The dashboard's frame waits exactly
// this (`SESSION_READ_TIMEOUT_MS`, held by a test), and shows a retry after it.
const SessionClientWait = 2 * SessionCatchUp

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

	// Chart resolves a bound person's seat. REQUIRED, and the engine's
	// adapter on a node running no company yet answers UNKNOWN to every
	// seat question, which is 503 — never the
	// seatless arm, which would hand somebody bound to a lead's seat an
	// empty handle and serve the request.
	Chart session.Chart

	// External is `api.external_url`.
	External string

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
			"directory: a bearer's signature says it was minted here and " +
			"the rows say whether it is still live")
	case deps.Applier == nil:
		return nil, errors.New("auth: the session arm needs the identity " +
			"applier to wait on: a sign-in answers before this node applies " +
			"the session it opened, so without it every write made straight " +
			"after signing in is refused 503")
	case deps.Chart == nil:
		return nil, errors.New("auth: the session arm needs a chart seam; " +
			"a nil one would resolve every bound person as seatless, which " +
			"is the one fall-through internal/iam/session forbids")
	}
	s := &Sessions{
		signer: deps.Signer, directory: deps.Directory, chart: deps.Chart,
		applier:  deps.Applier,
		external: deps.External,
		now:      deps.Now,
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

// cookieOf is the bearer a request presents under the name this deployment
// issues, or empty.
//
// ONE NAME, never either: the bare name is one a sibling host can plant on an
// https deployment, and a planted session read as the visitor's is a sign-in
// they never made — see [session.Presented], whose rule this is, so the
// sign-in surface reads exactly the bearer this resolves.
func (s *Sessions) cookieOf(r *http.Request) string {
	return session.Presented(r, s.external)
}

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

	// malformed is a cookie that is not a bearer of this format at all —
	// forged, truncated, or signed under a key this deployment does not
	// hold — which is a credential presented and refused. The guard MARKS
	// it and a guarded route's refusal counts it; see audit.go for why the
	// count is not taken here.
	malformed bool

	// lifetime is the served session's absolute deadline, which no
	// re-issue moves — see [Lifetime].
	lifetime time.Time

	// session is what the presented bearer says about when it ends besides
	// its absolute deadline, for an assistant grant derived from it.
	session presentedSession
}

// resolve turns a cookie into an answer, or reports that this request carries
// none.
//
// admin and proof are the guard's own `api.auth.admin` posture and step-up
// window, applied to a person as they are to a token.
//
// tokens is the guard's Tier A entries by login, for a session exchanged from
// one: see [tierASubjects].
//
// open is whether the cookie is the one an OPEN CONNECTION was opened with —
// see [Guard.ResolveOpen] — which sets its idle deadline aside.
func (s *Sessions) resolve(w http.ResponseWriter, r *http.Request,
	admin iam.AdminPosture, proof proofWindow,
	tokens func(login string) (config.APIToken, bool), open bool) sessionAnswer {

	cookie := s.cookieOf(r)
	if cookie == "" {
		return sessionAnswer{how: iam.Anonymous}
	}
	subjects := tierASubjects{directory: s.directory, tokens: tokens}
	need := needOf(r.Method)
	// AN OPEN CONNECTION IS ACTIVITY, so its idle deadline is not what
	// ends it: the bearer it holds was presented at its handshake and can
	// never be re-issued over a socket. The session table says so
	// ([session.Signer.ValidateOpen]); this only says which question is
	// being asked.
	validate := s.signer.Validate
	if open {
		validate = s.signer.ValidateOpen
	}
	v := validate(r.Context(), subjects, cookie)
	if v.Row == session.RowBehind && v.Answer(need) == session.AnswerUnavailable {
		v = s.awaitStart(r.Context(), subjects, cookie, v, validate)
	}
	switch v.Answer(need) {
	case session.AnswerUnavailable:
		log.Log(r.Context(), UnreadLevel(r.Context()), "api_session_unavailable",
			"row", string(v.Row), "detail", v.Detail, "error", errText(v.Err))
		return sessionAnswer{how: iam.Unknown, presented: true}
	case session.AnswerServe:
	default:
		// EVERY REFUSING ROW IS ANONYMOUS and the cookie is CLEARED. The
		// log line carries which row decided; the caller gets one code
		// for all, since revoked, expired and never-existed would tell an
		// attacker holding it the same.
		//
		// NOTHING IS ANNOUNCED: a record that ended a session was
		// announced by its writer, and an expiry nobody authored would
		// need a remembered set of every session ever noticed.
		//
		// THE COOKIE ALONE ([session.End]), never the browser's cache:
		// this runs on the dashboard's scripts too, and Chrome holds a
		// response that empties its cache until it has scanned all of it.
		log.InfoContext(r.Context(), "api_session_refused",
			"row", string(v.Row), "detail", v.Detail)
		session.End(w, s.external)
		answer := sessionAnswer{how: iam.Anonymous, presented: true}
		answer.malformed = v.Row == session.RowMalformed
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
				via:      iam.SessionName(v.Bearer.Lineage.String()),
				lifetime: v.Ends.Absolute, session: presentedOf(v)}
		}
	}

	binding := session.ResolveSeat(r.Context(), s.chart, v.Person)
	var refusal *Refusal
	reissue := true
	switch binding.Answer() {
	case session.AnswerUnavailable:
		log.Log(r.Context(), UnreadLevel(r.Context()), "api_session_seat_unavailable",
			"person", v.Bearer.Person, "seat", v.Person.Seat,
			"detail", binding.Detail, "error", errText(binding.Err))
		return sessionAnswer{how: iam.Unknown, presented: true}
	case session.AnswerServe:
	default:
		// THEY ARE STILL RESOLVED, unlike an ended session: this node
		// knows who they are but not what they act AS. The refusal
		// beside the principal stops anything that would use a seat
		// ([ActsAsThemselves] is the one surface it spares). It is also
		// what says a binding was decided and not honoured: the seat is
		// empty either way, and a genuinely seatless principal (a service
		// account) carries no refusal.
		//
		// THE COOKIE IS NOT CLEARED: the bearer is live and works once
		// somebody rebinds them; discarding it would sign out a person
		// whose only problem is a chart edit.
		refusal, reissue = seatRefusal(binding), false
	}
	if v.EnrolmentOnly() {
		// AN ENROLMENT-ONLY SESSION is refused as that whatever its seat:
		// the seat refusal spares all of /auth/ and this one only three
		// routes, so the narrower answer holds. The cookie is kept and
		// re-issued as any live session's.
		//
		// READ OFF THE BEARER AS WELL AS THE ROW: a node that has not
		// applied the session's start serves a read on the bearer alone
		// ([session.RowBehind]), and every sign-in answers before any node
		// applies it. Asked of the row alone, a password reached every
		// read, the socket's snapshot included, for the apply latency.
		refusal, reissue = enrolmentRefusal(), true
	}
	if reissue {
		s.reissue(w, v)
	}
	return sessionAnswer{
		principal: s.principal(v, binding, admin, proof), how: iam.Resolved,
		refusal: refusal, presented: true, lifetime: v.Ends.Absolute,
		session: presentedOf(v),
	}
}

// presentedOf is what a validated bearer says about when its session ends,
// beside its absolute deadline.
//
// THE IDLE DEADLINE AS PRESENTED, never the re-issue's: a re-issue only moves it
// later, so a grant bounded by the presented one never outlives the cookie the
// browser actually held.
func presentedOf(v session.Validation) presentedSession {
	return presentedSession{idle: v.Ends.Idle, enrolmentOnly: v.EnrolmentOnly()}
}

// awaitStart waits for this node to apply the start of the session a write
// presents, then validates again, or answers the row it already had.
//
// A sign-in answers BEFORE any applier reaches its session
// ([statelog.Request.NoWait]); a read is served on the signature and epoch
// ([session.RowBehind]). A write acts on the rows, yet a 503 would fail the
// first request of every client that signs in and acts at once (`crewlet iam
// token -login`), so it waits for the one position its bearer states.
//
// [SessionCatchUp] bounds it: a node that misses it is really behind, and the
// request gets 503 with its retry hint. The bearer is SIGNED, so a caller
// cannot name a position to park a request on.
func (s *Sessions) awaitStart(ctx context.Context, directory session.Directory,
	cookie string, behind session.Validation,
	validate func(context.Context, session.Directory, string) session.Validation,
) session.Validation {

	wait, cancel := context.WithTimeout(ctx, SessionCatchUp)
	defer cancel()
	if err := s.applier.AwaitApplied(wait, behind.Bearer.StartPosition); err != nil {
		log.DebugContext(ctx, "api_session_start_not_applied",
			"lineage", behind.Bearer.Lineage.String(),
			"position", behind.Bearer.StartPosition, "error", err)
		return behind
	}
	return validate(ctx, directory, cookie)
}

// reissue sets the cookie a served validation re-issued, if it re-issued one.
func (s *Sessions) reissue(w http.ResponseWriter, v session.Validation) {
	if v.Reissue != "" {
		session.Issue(w, s.external, v.Reissue, v.Ends.Absolute)
	}
}

// SessionSubjects is the directory a session bearer is validated against by
// every frame that validates one: the identity estate's rows for a person's
// session, and a session exchanged from a Tier A token answered from the
// CONFIGURATION this node holds — see [tierASubjects].
//
// The guard composes it per request from its token table and
// internal/api/authapi builds it once from the same Tier A through the same
// table ([tokensOf]): one parser, not one instance, as [NewClients]. Validating
// against the bare estate would miss `token:<id>`, which no person row holds,
// so a sign-out would read the session as already over and never close it.
func SessionSubjects(b *config.Bootstrap, directory session.Directory) session.Directory {
	return tierASubjects{directory: directory, tokens: tokensOf(b).byLogin}
}

// tierATokens is Tier A's `api.auth.tokens`, keyed by each entry's id — the
// table a presented bearer is matched against and a session exchanged from
// one is answered from.
//
// A TYPE OF ITS OWN so the two readers build it one way: the guard's match and
// [SessionSubjects] each held a spelling of "which entry does this name", and
// a second spelling is how one of them comes to answer a renamed entry
// differently from the other.
type tierATokens map[string]config.APIToken

// tokensOf is the table Tier A declares, empty for no Tier A at all.
func tokensOf(b *config.Bootstrap) tierATokens {
	if b == nil {
		return tierATokens{}
	}
	tokens := make(tierATokens, len(b.API.Auth.Tokens))
	for _, entry := range b.API.Auth.Tokens {
		tokens[entry.ID] = entry
	}
	return tokens
}

// byLogin is the entry a token's login names, or false.
//
// BY THE ID IN THE LOGIN and never by value: what an exchanged session carries
// is the token's NAME, and the entry this node holds under that name now is
// what it answers to — so removing the entry, or renaming it, ends every
// session exchanged from it on this node's next request.
func (t tierATokens) byLogin(login string) (config.APIToken, bool) {
	id, isToken := strings.CutPrefix(login, iam.TokenLoginPrefix)
	if !isToken || id == "" {
		return config.APIToken{}, false
	}
	entry, held := t[id]
	return entry, held
}

// tierASubjects is the directory a bearer is validated against, answering a
// Tier A token's own subject from the CONFIGURATION rather than from a row.
// The subject is the token's LOGIN (`token:<id>`), with no row: facts about the
// SESSION are the directory's, the one about the SUBJECT is whether this node
// still holds the entry. By the id and never the value: a new value under one
// id leaves old sessions working until their hour ends, and an operator who
// must cut them off rotates to a new id. A MAC would cost a check per request.
//
// A gone entry is a retired principal, so the session ends and the cookie is
// cleared. As ABSENT, a node below the start position would serve reads on a
// withdrawn credential: that grace is for a record not yet applied, and a
// configuration entry is never late. nil tokens answers from the directory.
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
	// NO GRANTS AND NO SEAT on the row: who a token's session acts as is
	// composed from the entry by the guard, on every request.
	stage := iam.StageActive
	if _, held := d.tokens(person); !held {
		// THE ENTRY IS GONE — removed, or renamed out from under the
		// login — so the subject may not act, and the session is over.
		stage = iam.StageRetired
	}
	identity.Person = session.PersonRow{Found: true, Stage: stage,
		Login: person, Epoch: epoch}
	return identity, nil
}

// principal composes who the holder of a validated bearer is.
//
// The admin posture is applied here as for a Tier A token: `api.auth.admin` is
// a decision-time bound, so a node turned to `refuse` clamps admin on its next
// request. A mixed fleet mid-rollout is LEGAL, so each node publishes its own.
//
// So is the step-up window: the company's, within this node's limit, read per
// request; the row states only WHEN its holder proved themselves.
// [iam.Principal.ReauthAt] is the proof plus this node's window, so a shortened
// window applies at once and a session that proved nothing (a row not yet
// applied) is never fresh. A Tier A token's exchanged cookie never reaches
// here: the guard composes it from the entry.
func (s *Sessions) principal(v session.Validation, binding session.Binding,
	admin iam.AdminPosture, proof proofWindow) iam.Principal {

	person := v.Person
	p := iam.Principal{
		// THE PERSON'S OWN ID, parsed from the bearer. A bearer that
		// reached here verified, so the value is one this engine wrote.
		ID:    personID(v.Bearer.Person),
		Login: person.Login,
		Kind:  iam.KindPerson,
		// THE SEAT COMES FROM THE BINDING AND NEVER FROM THE ROW,
		// because the binding is the row's seat as the running
		// organisation holds it — present and human — and the row
		// alone says neither.
		Seat:     binding.Handle(),
		Position: binding.Seat.Unit,
		// WHAT THE PERSON WAS GIVEN, under this node's admin posture:
		// admin expanded into every app and then served or clamped
		// away, and a name this build does not know dropped. A session
		// carries no authority of its own: the grants are the person's
		// row's, so an administrator's edit reaches every session they
		// hold on its next request.
		Grants: admin.Effective(person.Grants),
		Stage:  person.Stage,
		// AND THROUGH WHAT: this session, by its lineage. The person is
		// the author of what they write, and the operator column beside
		// them says which sign-in it came through — it used to repeat
		// their login, which the author already names, so two browsers
		// or a tab left open on a shared machine were one name in every
		// trail. The lineage is verified: the bearer reached here signed.
		Via: iam.SessionName(v.Bearer.Lineage.String()),
	}
	// WHEN THIS SESSION'S PROOF STOPS COUNTING. It used to be read off a
	// person field nothing ever set, so every session was stale from its
	// first request and no person could reach a step-up surface at all —
	// enrolling a second factor included.
	proof.stamp(&p, v.Session.ProvedAt)
	return p
}

// proofWindow is the step-up window in force, read per request: the company's
// `sign_in.step_up_seconds` held within this node's limit, so a change takes
// effect on the next request with nothing rewritten (ADR-0152).
type proofWindow func() time.Duration

// windowOf is the step-up window of a sign-in policy read per request.
func windowOf(policy func() config.SignInPolicy) proofWindow {
	return func() time.Duration { return policy().StepUp }
}

// stamp gives p the deadline a proof taken at provedAt earns: the proof plus
// the window.
//
// A ZERO provedAt STAMPS NOTHING, which [iam.Principal.Proved] reads as stale:
// a session that proved nothing — a row this node has not applied yet — must
// never read as fresh, and a proof of the zero instant plus an hour would read
// as long stale only by luck.
func (w proofWindow) stamp(p *iam.Principal, provedAt time.Time) {
	if provedAt.IsZero() {
		p.ReauthAt = time.Time{}
		return
	}
	p.ReauthAt = provedAt.Add(w())
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

// errText is an error's message, or empty — so a log line carries the field
// only when there is one.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
