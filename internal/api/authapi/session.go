package authapi

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
)

// providerLabel is the host a provider is reached at, for a button's label.
func providerLabel(issuer string) string {
	parsed, err := url.Parse(strings.TrimSpace(issuer))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}

// directoryFor adapts this surface's own seam to the one
// [session.Signer.Validate] takes.
//
// TWO SEAMS OVER ONE READER, and that is deliberate rather than duplication:
// this package's [Directory] is what a SIGN-IN may reach and carries no
// per-bearer resolve, while session's is what a VALIDATION needs and carries
// nothing else. Widening either to be the other would give a route access to
// the half it has no business with.
//
// # And the guard's reading of the subject, never a second one
//
// A session exchanged from a Tier A token names the token's login, which no
// person row holds; the guard answers that subject from the entry this node
// holds ([auth.SessionSubjects]), and so does this. It used to validate
// against the bare estate, so a token's session read as one whose person was
// gone: [Service.Logout] cleared the cookie and closed nothing, while the guard
// went on serving the same cookie — and a captured copy with it — until its
// hour ran out.
func (s *Service) directoryFor() session.Directory { return s.sessions }

// configResponse is what a client reads before anybody has signed in.
//
// # What is in it, and what is deliberately not
//
// Enough to render the right form and nothing that says who works here. A
// backend name, whether a provider button is offered and what it is called,
// whether the first-operator route is still open, and the password floor so a
// form can refuse twelve characters before a round trip.
//
// THERE IS NO USER LIST, no count of people, and no hint of whether any
// particular login exists. This route is unguarded, so everything on it is
// public — and the one question an attacker most wants answered here is who
// they could be.
type configResponse struct {
	// Backend is how this deployment signs people in: local, oidc or
	// none.
	Backend config.AuthBackend `json:"backend"`

	// Provider is the identity provider's host, for a button to be
	// labelled with. The HOST and never the whole issuer URL: a browser
	// is redirected to that host the moment somebody clicks, so it is not
	// a secret — while the path, which may name a tenant or a realm, is
	// deployment topology nothing on a sign-in page needs.
	Provider string `json:"provider,omitempty"`

	// Bootstrap reports whether the first-operator route is still
	// available. It closes for good the moment anybody is enrolled.
	//
	// IT IS SAFE TO SAY, and saying it is what a client needs to render
	// the difference between a sign-in form and a set-up form. What it
	// discloses is whether this company has started, which whoever can
	// reach an unstarted one is about to find out anyway.
	Bootstrap bool `json:"bootstrap"`

	// MinPasswordLength is the floor a form enforces before it posts.
	MinPasswordLength int `json:"min_password_length,omitempty"`

	// SecondFactor reports whether this deployment requires one.
	SecondFactor string `json:"second_factor,omitempty"`
}

// Config answers what a client needs before anybody has signed in.
//
// UNGUARDED, necessarily: it is what a sign-in page reads to know what kind of
// sign-in page to be. See [configResponse] for what that costs and what it is
// therefore not allowed to carry.
func (s *Service) Config(w http.ResponseWriter, r *http.Request) {
	out := configResponse{Backend: s.backend()}
	if s.backend() == config.AuthBackendLocal {
		out.MinPasswordLength = s.passwordFloor()
		out.SecondFactor = string(s.boot.API.Auth.Local.TOTP)
	}
	if s.provider != nil {
		out.Provider = providerLabel(s.boot.API.Auth.OIDC.Issuer)
	}
	// THE ROUTE'S OWN GATE, so this flag and the route can never
	// disagree: `api.auth.bootstrap` says whether the route MAY run and
	// the estate says whether it still can — a company with people in it
	// closes it for good whatever the file says, because the route creates
	// an operator carrying the whole ceiling.
	closed, err := s.bootstrapClosed(r.Context())
	if err != nil {
		// THE UNKNOWN ARM IS REPORTED AS CLOSED, which is the fail-safe
		// direction: a client told the route is open and refused is
		// confusing, and a client told it is closed when the estate
		// could not be read merely waits.
		log.WarnContext(r.Context(), "api_auth_config_estate_unreadable",
			"error", err)
	} else {
		out.Bootstrap = closed == ""
	}
	httpjson.Write(w, http.StatusOK, out)
}

// sessionResponse is who the caller is.
type sessionResponse struct {
	Person    string        `json:"person"`
	Login     string        `json:"login"`
	Seat      string        `json:"seat,omitempty"`
	Kind      iam.Kind      `json:"kind"`
	Stage     iam.Stage     `json:"stage"`
	Grants    []iam.Grant   `json:"grants"`
	Colleague iam.Colleague `json:"colleague"`

	// ExpiresAt is the absolute deadline of the session this request
	// carried, which no re-issue moves. ABSENT for a caller presenting a
	// bearer rather than a cookie — a Tier A token or a personal access
	// token has its own lifetime and no session to end.
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	// ReauthAt and SensitiveReauthAt are when this caller's proof of who
	// they are stops counting for an ordinary step-up gesture
	// (`api.auth.session.step_up`) and for a sensitive one
	// (`step_up_sensitive`) — the two instants the authority table judges
	// against, so a screen counting down to either counts to the refusal
	// itself. Absent when nothing was proved.
	ReauthAt          time.Time `json:"reauth_at,omitzero"`
	SensitiveReauthAt time.Time `json:"sensitive_reauth_at,omitzero"`

	// StepUpDue and SensitiveStepUpDue report whether the next gesture of
	// each kind will ask this caller to confirm who they are, so a client
	// can say so before they start rather than after.
	StepUpDue          bool `json:"step_up_due"`
	SensitiveStepUpDue bool `json:"sensitive_step_up_due"`

	// Status is what the session this request carried may do: `signed_in`,
	// or `second_factor_enrolment_required` for one that may only enrol a
	// second factor — which is how a client that holds such a session,
	// and is refused everything else, learns to render the enrolment. A
	// request whose credential was a header carries no session and is
	// `signed_in`.
	Status sessionStatus `json:"status"`
}

// Session answers who the caller is.
//
// GUARDED, unlike everything else in this file — the guard resolves the cookie
// and this reads what it resolved, rather than validating a second time. Two
// readings of one request eventually disagree, and the one that would be
// wrong here is the one that decides whether somebody is signed in.
func (s *Service) Session(w http.ResponseWriter, r *http.Request) {
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		// THE THIRD ANSWER. This node could not tell, which is 503 and
		// never 401: a browser reads 401 as "sign in again" and
		// discards the cookie, so answering it during an identity
		// outage signs the whole company out and stampedes the provider
		// with re-authentications.
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(iam.Reason(r.Context())))
		return
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	// THE DEADLINE IS THE BEARER'S OWN, read off its signature: it was
	// declared and never filled, so every answer said the session ended in
	// the year 1. A request whose credential was a header carries no
	// session, and the zero bearer omits it.
	var expires time.Time
	status := statusSignedIn
	if r.Header.Get("Authorization") == "" {
		presented := s.presentedSession(r)
		expires = presented.Bearer.AbsoluteExpiresAt
		// THE BEARER'S MARK AND THE ROW'S TOGETHER, as the guard reads
		// it: on a node that has not applied the session's start there
		// is no row, and the session is still the restricted one.
		if presented.EnrolmentOnly() {
			status = statusEnrolmentRequired
		}
	}
	httpjson.Write(w, http.StatusOK, sessionResponse{
		Person: principal.ID.String(), Login: principal.Login,
		Seat:               principal.Seat,
		Kind:               principal.Kind,
		Stage:              principal.Stage,
		Grants:             principal.Grants,
		Colleague:          principal.Colleague,
		ExpiresAt:          expires,
		ReauthAt:           principal.ReauthAt,
		SensitiveReauthAt:  principal.SensitiveReauthAt,
		StepUpDue:          s.stepUpDue(principal),
		SensitiveStepUpDue: !principal.Proved(iam.RecencySensitive, s.now()),
		Status:             status,
	})
}

// stepUpDue reports whether this caller's proof of identity is old enough that
// the next sensitive action will ask them to confirm it.
//
// THE PRINCIPAL'S OWN DEADLINE, never a second number: [iam.Principal.ReauthAt]
// is the instant the proof stops counting, composed by the guard from when the
// session proved identity and this node's `step_up` window — the one setting
// the gate uses — so a client warned here and refused there is warned at the
// threshold that refuses. A zero deadline is stale: nothing proved yet is due
// by definition, which is the honest reading of a session opened by a route
// that does not prove identity at all — see the Tier A token exchange.
//
// IT USED TO SUBTRACT THE WINDOW A SECOND TIME, reading ReauthAt as the
// instant of proof while the guard wrote it as the deadline, so a fresh proof
// counted for twice the window here.
func (s *Service) stepUpDue(p iam.Principal) bool {
	return !p.Fresh(s.now())
}

// Logout ends THIS session.
//
// THE COOKIE IS CLEARED WHATEVER THE WRITE DID. A logout that answered 503
// because the log could not be reached would leave a person looking at a
// signed-in page having asked to leave, on a shared machine — so the cookie
// goes first, the record is published, and a failure to publish is reported in
// the log rather than to the person walking away from the screen.
//
// That is safe because the cookie is the only thing the browser holds: without
// it nothing is presented, and the record catching up later merely makes the
// row agree with what already happened.
//
// # Unguarded, or the promise above is not one
//
// The request guard answers `503 identity_unavailable` on a node whose identity
// estate it cannot read, BEFORE any handler runs — and this route was behind
// it, so on exactly that node no sign-out cleared anything. It is exempt now
// ([auth.PathAuthLogout] is on the guard's exact list), which it can afford
// because it trusts nothing the guard would have told it: every bearer it ends
// is read here under its signature and this node's rows. The guard still
// hands through whatever it resolved, for [callerName]; the origin check still
// judges the post.
//
// # A session already over is cleared, and nothing else
//
// The cookie is read under its signature AND this node's rows, and only a
// session they still hold — live, or opened on a node ahead of this one —
// is closed and announced. It used to be read under the signature alone, so
// any cookie ever signed under a live key — expired, revoked, a removed
// person's leftover — published a close and announced another
// `iam_session_ended` on every post: a row per request authored by whoever
// held a cookie the engine no longer accepts, and a second ending named for a
// session a record had already ended. A node that cannot read its rows still
// records the close the person asked for, and announces nothing it cannot say
// was live.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	s.signOut(w, r)
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// signOut is [Service.Logout] up to its answer: the cookie cleared under
// every name, and each session a cookie named closed and announced when this
// node's rows still hold it. Both sign-outs answer after it — the plain one
// with a body, the provider one with a redirect — so they end a session
// identically.
//
// EVERY BEARER THE BROWSER HOLDS, under either name ([session.Held]), where
// the guard authenticates only the name this deployment issues: a browser
// still holding the name an http deployment issued before it moved to https
// is signed in by nothing any more, and its session is closed here rather
// than left live behind a cookie the sign-out merely forgot.
func (s *Service) signOut(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w)
	for _, cookie := range session.Held(r, s.boot.API.ExternalBase()) {
		s.endHeld(r, s.signer.Validate(r.Context(), s.directoryFor(), cookie))
	}
}

// endHeld closes and announces one session a sign-out found, when this node's
// rows still hold it — see [Service.Logout].
func (s *Service) endHeld(r *http.Request, presented session.Validation) {
	bearer := presented.Bearer
	if bearer.Lineage == uuid.Nil {
		// NOTHING TO END, and it is not an error: a client that clears
		// its own cookie and posts here is asking for exactly what
		// already happened.
		return
	}
	lineage := bearer.Lineage.String()
	announce := false
	switch presented.Row {
	case session.RowValid, session.RowBehind:
		announce = true
	case session.RowStalled:
		// THE ROWS CANNOT SAY, so the close the person asked for is
		// recorded and nothing is announced: a row saying this session
		// ended here would be false if a record had already ended it.
	default:
		log.DebugContext(r.Context(), "api_sign_out_already_over",
			"lineage", lineage, "row", string(presented.Row))
		return
	}
	// THE PERSON COMES OFF THE SAME VERIFIED BEARER as the lineage, and
	// the record is filed under their bucket — where a node that cannot
	// decode it has to say it is behind about them.
	closed, err := s.writer.CloseSession(r.Context(), lineage, bearer.Person,
		"signed out", "logout:"+lineage)
	switch {
	case err != nil:
		log.WarnContext(r.Context(), "api_sign_out_record_failed",
			"error", err, "lineage", lineage)
	case !landed(closed):
		// UNKNOWN IS NOT LANDED. Nothing can say whether the record is on
		// the log, so nothing is said about it; the op id is derived from
		// the lineage, so the next sign-out of this session is the same
		// operation.
		log.WarnContext(r.Context(), "api_sign_out_record_unresolved",
			"lineage", lineage, "op_id", closed.OpID)
	case announce:
		// ONLY ONCE THE RECORD LANDED — applied here, or durable and
		// pending here. A cleared cookie is this browser forgetting; the
		// session ending is the record every node reads, and a row saying
		// it ended when the write did not land would be the one row in
		// the trail that is false.
		s.audit.Emit(r.Context(), types.IAMSessionEnded{
			Person: bearer.Person, Lineage: lineage,
			Reason: types.EndLogout, By: callerName(r),
			OperatorID: callerOperator(r),
		})
	}
	log.InfoContext(r.Context(), "api_sign_out", "lineage", lineage)
}

// providerSession says what became of the person's session at the identity
// provider, on a sign-out that could not send the browser there.
type providerSession string

// providerNotEnded is the one value: the session here is over and the one at
// the provider is not, which the person has to know if they are walking away
// from a shared machine.
const providerNotEnded providerSession = "not_ended"

// providerLogoutAnswer is what a provider sign-out answers when it cannot
// redirect: the plain sign-out's own status, and what it could not do.
type providerLogoutAnswer struct {
	Status          string          `json:"status"`
	ProviderSession providerSession `json:"provider_session"`
	Detail          string          `json:"detail"`
}

// LogoutProvider ends THIS session and then sends the browser to the identity
// provider to end the person's session THERE — OpenID Connect's RP-initiated
// logout.
//
// # Why it exists beside the plain sign-out
//
// Signing out here ends this engine's session and leaves the provider's: the
// next "sign in with the provider" is answered from it without anybody typing
// anything. On a machine somebody else uses next, that is the same as not
// having signed out. So this route ends the session here exactly as
// [Service.Logout] does — the same close, the same only-a-live-session rule,
// the cookie cleared first — and answers `303` to the provider's
// `end_session_endpoint`, naming this client and asking to be sent back to the
// dashboard. A 303 because the caller is a browser that POSTed: it is what
// makes the browser follow with a GET, as a top-level navigation the provider's
// own page can answer.
//
// # What it cannot promise
//
// It sends no `id_token_hint`, because this engine keeps no ID token once a
// sign-in completes: a provider that requires one asks the person to confirm,
// or refuses, at its own page — and the session HERE is over either way. And a
// provider that publishes no `end_session_endpoint`, or whose discovery cannot
// be reached, is answered as the plain sign-out is, saying the provider's
// session was not ended: the local half happened, and the person is told the
// other did not rather than being sent nowhere.
//
// `post_logout_redirect_uri` is `api.external_url` + `/dashboard`, and the
// provider only honours one registered for this client — so it is registered
// beside the callback, or the provider leaves the person on its own page.
//
// UNGUARDED for [Service.Logout]'s reason, and here it matters twice: a node
// that cannot read its identity estate must still clear the cookie AND still
// send the browser to the provider, which is the half a person on a shared
// machine is relying on.
func (s *Service) LogoutProvider(w http.ResponseWriter, r *http.Request) {
	s.signOut(w, r)

	notEnded := func(why string) {
		httpjson.Write(w, http.StatusOK, providerLogoutAnswer{
			Status: "signed out", ProviderSession: providerNotEnded, Detail: why,
		})
	}
	metadata, err := s.provider.Metadata(r.Context())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_discovery_failed", "error", err)
		notEnded("signed out here; the identity provider could not be reached, " +
			"so your session there was not ended — sign out at the provider too")
		return
	}
	if metadata.EndSessionEndpoint == "" {
		notEnded("signed out here; the identity provider publishes no " +
			"end_session_endpoint, so your session there was not ended — sign " +
			"out at the provider too")
		return
	}
	// AN HTTPS ADDRESS ON A HOST, as the issuer itself must be, or the
	// browser is not sent there at all. It used to be enough that the value
	// parsed: a RELATIVE one was then a path on this deployment and a
	// plain-http one an address anybody on the path could answer, and either
	// redirect read to the person as a sign-out at their provider that did
	// not happen.
	target, err := url.Parse(metadata.EndSessionEndpoint)
	if err == nil && (target.Scheme != "https" || target.Host == "") {
		err = fmt.Errorf("%q is not an https address on a host",
			metadata.EndSessionEndpoint)
	}
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_end_session_unusable",
			"endpoint", metadata.EndSessionEndpoint, "error", err)
		notEnded("signed out here; the identity provider's end_session_endpoint " +
			"is not an https address, so your session there was not ended — " +
			"sign out at the provider too")
		return
	}
	query := target.Query()
	query.Set("client_id", s.provider.Config().ClientID)
	query.Set("post_logout_redirect_uri", s.boot.API.ExternalBase()+auth.PathDashboard)
	target.RawQuery = query.Encode()
	http.Redirect(w, r, target.String(), http.StatusSeeOther)
}

// callerName is the name a row about this request records as its author, or
// empty where the guard resolved nobody — a sign-out on a cookie this node
// could no longer serve is still a sign-out, and its author is then the
// person the bearer names rather than a principal.
func callerName(r *http.Request) string {
	principal, resolution := iam.From(r.Context())
	if resolution != iam.Resolved {
		return ""
	}
	return iam.ActorFor(principal).Name
}

// callerOperator is the credential [callerName] acted through — a machine
// token's `pat:<id>`, which acts as its owner, a browser session's
// `session:<lineage>`, or a Tier A token's own login — so an event can tell a
// token's gesture from its owner's, and one sign-in's from another's.
func callerOperator(r *http.Request) string {
	principal, resolution := iam.From(r.Context())
	if resolution != iam.Resolved {
		return ""
	}
	return iam.ActorFor(principal).OperatorID
}

// LogoutEverywhere ends every session this person holds, by bumping their own
// revocation epoch.
//
// THE EPOCH AND NOT A SWEEP OF ROWS, because it is the only move that is
// immediate on every node: a bearer carries the epoch it was opened at, so one
// write ends every session in flight without any node having to find them.
func (s *Service) LogoutEverywhere(w http.ResponseWriter, r *http.Request) {
	principal, resolution := iam.From(r.Context())
	if resolution != iam.Resolved {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	s.clearSession(w)

	// THE CALLER'S OWN SUBJECT, which for a Tier A token is its login: the
	// epoch under it is what every session exchanged from that token is
	// checked against.
	person := subjectOf(r, principal)
	revoked, err := s.writer.Revoke(r.Context(), person,
		"logout-all:"+person+":"+s.now().UTC().Format(time.RFC3339Nano),
		"signed out everywhere")
	if err != nil {
		// REPORTED, unlike the single logout above, and the difference
		// is what the caller asked for: clearing this browser's cookie
		// does not end the OTHER sessions, so a failure here means the
		// thing they asked for did not happen and they have to ask
		// again.
		log.WarnContext(r.Context(), "api_sign_out_all_failed",
			"error", err, "person", person)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentity(err))
		return
	}
	if !landed(revoked) {
		// AND AN UNKNOWN IS NOT A SIGN-OUT EVERYWHERE, for the same
		// reason: the caller cannot be told their other sessions ended.
		unresolved(w, r, "api_sign_out_all_unresolved", revoked)
		return
	}
	s.audit.Emit(r.Context(), types.IAMSessionEnded{
		Person: person, Reason: types.EndLogoutAll,
		By:         iam.ActorFor(principal).Name,
		OperatorID: iam.ActorFor(principal).OperatorID,
	})
	log.InfoContext(r.Context(), "api_sign_out_all", "person", person)
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "signed out everywhere"})
}

// presentedSession is the session this request's cookie names, judged by its
// signature and this node's rows — the zero validation for a request carrying
// none.
//
// # It is the SIGNATURE that decides the lineage, never the field
//
// A lineage read straight out of a cookie without checking the mac is a
// lineage the CALLER CHOSE, and closing a session on one is a denial of
// service against anybody whose session id leaks: a proxy log, a referrer, a
// screenshot. So this goes through [session.Signer.Validate], which parses
// under the keyring and yields a bearer only when the signature verifies — a
// malformed cookie's bearer is the zero value, and its lineage the nil uuid.
//
// # And the ROWS decide whether there is anything left to end
//
// The verdict comes back beside the bearer, because a sign-out is a CLAIM
// that the session was live until now: a cookie that is expired, revoked, or
// names a session a record already ended still carries a verified bearer, and
// a caller that read only the bearer closed and announced every one of those
// again. [Service.Logout] reads the row; the step-up answer and a named
// sign-out read only the bearer, because what they need from it — the
// absolute deadline, whether the named lineage is this cookie's — is the
// bearer's own.
//
// THE NAME THIS DEPLOYMENT ISSUES, by [session.Presented]'s rule — the
// guard's own, so what this reads is what the request was authenticated with.
// The sign-out alone reaches the other name too, through [session.Held].
func (s *Service) presentedSession(r *http.Request) session.Validation {
	cookie := session.Presented(r, s.boot.API.ExternalBase())
	if cookie == "" {
		return session.Validation{}
	}
	return s.signer.Validate(r.Context(), s.directoryFor(), cookie)
}

// clearSession ends the session in the browser under every name a bearer can
// be held under — see [session.Clears].
func (s *Service) clearSession(w http.ResponseWriter) {
	for _, clear := range session.Clears(s.boot.API.ExternalBase()) {
		http.SetCookie(w, clear)
	}
}

// LogoutOne ends ONE named session, which is how somebody signs out of a
// laptop they left in an office without signing out of the browser they are
// doing it from.
//
// # It is somebody's OWN session, and that is checked against the estate
//
// A lineage is not a secret — it is in a cookie, a proxy log, a screenshot —
// so a route that closed whatever lineage it was handed would let anybody end
// anybody's session. The person who OWNS it is what decides, read from this
// node's own rows rather than claimed by the caller.
//
// The one exception is the same one every surface here has: a principal
// carrying `fleet:operate` may end a session they do not own, because ending
// somebody's session is what an operator does when a laptop is stolen and its
// holder cannot be reached.
func (s *Service) LogoutOne(w http.ResponseWriter, r *http.Request) {
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(iam.Reason(r.Context())))
		return
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	lineage := r.PathValue("lineage")
	if lineage == "" {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}

	owner, live, err := s.directory.SessionStanding(r.Context(), lineage, s.now())
	if err != nil {
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(err))
		return
	}
	if owner == "" {
		// ABSENT IS NOT AN ERROR HERE. A session that has already ended
		// is exactly what the caller asked for, and a 404 would send
		// somebody looking for a session they successfully closed.
		//
		// IT IS ALSO THE SAME ANSWER A SESSION THEY DO NOT OWN GETS
		// BELOW, which is what keeps this from being an oracle for
		// which lineages exist.
		httpjson.Write(w, http.StatusOK, map[string]string{"status": "ended"})
		return
	}
	if !mayEnd(r, principal, owner) {
		httpjson.Fail(w, http.StatusForbidden, httpjson.CodeUnauthorized)
		return
	}
	if !live {
		// ALREADY OVER — its row ended, its deadline passed, its person
		// revoked everywhere or the company invalidated — so there is
		// nothing to close and no ending to announce: whatever ended it
		// already said so. The answer is the same `ended`, and the cookie
		// still goes if it was this one.
		s.clearIfPresented(w, r, lineage)
		httpjson.Write(w, http.StatusOK, map[string]string{"status": "ended"})
		return
	}
	closed, err := s.writer.CloseSession(r.Context(), lineage, owner,
		"signed out from another session", "logout-one:"+lineage)
	if err != nil {
		log.WarnContext(r.Context(), "api_sign_out_one_failed",
			"error", err, "lineage", lineage)
		httpjson.Unavailable(w, httpjson.CodeUnavailable, auth.RetryIdentity(err))
		return
	}
	if !landed(closed) {
		unresolved(w, r, "api_sign_out_one_unresolved", closed)
		return
	}
	// A PERSON ENDING THEIR OWN is a logout; an operator ending somebody
	// else's is a revocation, and the trail has to say which, because the
	// second is the row an investigation of a stolen laptop is looking for.
	reason := types.EndLogout
	if owner != subjectOf(r, principal) {
		reason = types.EndRevoked
	}
	s.audit.Emit(r.Context(), types.IAMSessionEnded{
		Person: owner, Lineage: lineage, Reason: reason,
		By:         iam.ActorFor(principal).Name,
		OperatorID: iam.ActorFor(principal).OperatorID,
	})
	s.clearIfPresented(w, r, lineage)
	log.InfoContext(r.Context(), "api_sign_out_one",
		"lineage", lineage, "by", principal.Login)
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "ended"})
}

// clearIfPresented clears this request's cookie when it names the lineage a
// named sign-out ended, so a person who ends the session they are using is not
// left looking at a signed-in page.
func (s *Service) clearIfPresented(w http.ResponseWriter, r *http.Request, lineage string) {
	for _, cookie := range session.Held(r, s.boot.API.ExternalBase()) {
		held := s.signer.Validate(r.Context(), s.directoryFor(), cookie)
		if lineage == held.Bearer.Lineage.String() {
			s.clearSession(w)
			return
		}
	}
}

// mayEnd reports whether this caller may end a session that person holds.
//
// # Both values come from this node, and neither from the caller
//
// The owner is read out of this node's own rows and the caller is what the
// guard resolved. A lineage is not a secret — cookie, proxy log, screenshot —
// so a rule that trusted anything the request carried about whose session it
// is would let anybody end anybody's.
//
// THE OPERATOR EXCEPTION is `fleet:operate`, and it is what somebody does when
// a laptop is stolen and its holder cannot be reached. It is the deployment's
// grant rather than a colleague level, because ending a person's sessions is
// an act on the DEPLOYMENT's security rather than on the company's work.
func mayEnd(r *http.Request, p iam.Principal, owner string) bool {
	if owner != "" && owner == subjectOf(r, p) {
		return true
	}
	return p.Can(iam.GrantFleetOperate)
}

// subjectOf is the subject the caller's OWN sessions are opened under: a
// person's id, or — for a caller acting on a Tier A token, by its bearer or by
// a session exchanged from it — the token's login, which is what the exchange
// opens its sessions under because a token has no directory row.
//
// ONE ANSWER FOR THE THREE GESTURES THAT ASK, so "end my session", "end my
// other sessions" and "sign me out everywhere" agree about who "me" is: a
// token's caller compared against its derived id owned none of the sessions
// it had opened, and signing out everywhere bumped an epoch nothing read.
func subjectOf(r *http.Request, p iam.Principal) string {
	if entry, tierA := auth.TierA(r.Context()); tierA {
		return iam.TokenLogin(entry.ID)
	}
	return p.ID.String()
}
