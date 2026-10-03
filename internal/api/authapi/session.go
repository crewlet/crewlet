package authapi

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

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
// backend name, the password floor so a form can refuse a short password
// before a round trip, and whether a second factor is required.
//
// THERE IS NO USER LIST, no count of people, and no hint of whether any
// particular login exists. This route is unguarded, so everything on it is
// public — and the one question an attacker most wants answered here is who
// they could be.
type configResponse struct {
	// Backend is how this deployment signs people in: local or none.
	Backend config.AuthBackend `json:"backend"`

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
	httpjson.Write(w, http.StatusOK, out)
}

// sessionResponse is who the caller is.
type sessionResponse struct {
	Person string      `json:"person"`
	Login  string      `json:"login"`
	Seat   string      `json:"seat,omitempty"`
	Kind   iam.Kind    `json:"kind"`
	Stage  iam.Stage   `json:"stage"`
	Grants []iam.Grant `json:"grants"`

	// ExpiresAt is the absolute deadline of the session this request
	// carried, which no re-issue moves. ABSENT for a caller presenting a
	// bearer rather than a cookie — a Tier A token or a personal access
	// token has its own lifetime and no session to end.
	ExpiresAt time.Time `json:"expires_at,omitzero"`

	// ReauthAt is when this caller's proof of who they are stops counting
	// for a step-up gesture (`api.auth.session.step_up`) — the instant the
	// authority table judges against, so a screen counting down to it
	// counts to the refusal itself. Absent when nothing was proved.
	ReauthAt time.Time `json:"reauth_at,omitzero"`

	// StepUpDue reports whether the next step-up gesture will ask this
	// caller to confirm who they are, so a client can say so before they
	// start rather than after.
	StepUpDue bool `json:"step_up_due"`

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
		// outage signs the whole company out and sends everybody back
		// to the sign-in form at once.
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
		Seat:      principal.Seat,
		Kind:      principal.Kind,
		Stage:     principal.Stage,
		Grants:    principal.Grants,
		ExpiresAt: expires,
		ReauthAt:  principal.ReauthAt,
		StepUpDue: s.stepUpDue(principal),
		Status:    status,
	})
}

// stepUpDue reports whether this caller's proof of identity is old enough that
// the next step-up gesture will ask them to confirm it.
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
//
// # Every bearer the browser holds
//
// Under either name ([session.Held]), where the guard authenticates only the
// name this deployment issues: a browser still holding the name an http
// deployment issued before it moved to https is signed in by nothing any
// more, and its session is closed here rather than left live behind a cookie
// the sign-out merely forgot.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	s.clearSession(w)
	for _, cookie := range session.Held(r, s.boot.API.ExternalBase()) {
		s.endHeld(r, s.signer.Validate(r.Context(), s.directoryFor(), cookie))
	}
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "signed out"})
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
	//
	// A FRESH OPERATION, as every sign-out is ([iamdomain.SessionOpsRetention]
	// says why): it was derived from the lineage, whose instant is when the
	// session BEGAN, and a session subject's ledger rows go after an hour —
	// so the sign-out of any session older than that carried an id the
	// ledger could not vouch for, and was answered `unknown` without being
	// published. A second sign-out is a second close of a closed session,
	// which changes nothing.
	opID := statelog.NewOpID(s.now(), "logout")
	closed, err := s.writer.CloseSession(r.Context(), lineage, bearer.Person,
		"signed out", opID)
	switch {
	case removed(err):
		// THEIR PERSON WAS REMOVED, and every session of theirs with them.
		log.InfoContext(r.Context(), "api_sign_out_person_removed",
			"lineage", lineage)
		return
	case err != nil:
		log.WarnContext(r.Context(), "api_sign_out_record_failed",
			"error", err, "lineage", lineage, "op_id", opID)
	case !landed(closed):
		// UNKNOWN IS NOT LANDED. Nothing can say whether the record is on
		// the log, so nothing is said about it; the next sign-out of this
		// session closes it again if this one did not.
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
	opID := statelog.NewOpID(s.now(), "logout-all")
	revoked, err := s.writer.Revoke(r.Context(), person, opID,
		"signed out everywhere")
	switch {
	case removed(err):
		// THEIR PERSON WAS REMOVED, and every session they held went with
		// them — this one included, whose cookie is gone above.
		log.InfoContext(r.Context(), "api_sign_out_all_person_removed",
			"person", person)
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeSessionRevoked)
		return
	case err != nil:
		// REPORTED, unlike the single logout above, and the difference
		// is what the caller asked for: clearing this browser's cookie
		// does not end the OTHER sessions, so a failure here means the
		// thing they asked for did not happen and they have to ask
		// again.
		writeFailed(w, r, "api_sign_out_all_failed", opID, err)
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
// # Whose session it is comes from the estate, and who may end it from the table
//
// A lineage is not a secret — it is in a cookie, a proxy log, a screenshot —
// so a route that closed whatever lineage it was handed would let anybody end
// anybody's session. The person who OWNS it is read from this node's own rows
// rather than claimed by the caller, and whether this caller may end it is
// [authz.ActionSessionEnd] — the verb `DELETE /iam/people/{id}/sessions` is
// mounted on, so ending one session and ending all of somebody's are one
// rule: their own, on no proof at all (the first thing somebody does on
// finding an intruder), or anybody else's on `people:manage`, a `step_up`
// and a person present.
//
// It used to be decided here, by a check of its own that admitted
// `fleet:operate` for anybody's session with no step-up — a grant a machine
// token may carry — so a leaked pipeline token, or a week-old cookie, could
// end any session whose lineage it knew while the table, its walks and the
// docs all said that took the directory's grant and a person present. An SRE
// who finds a stolen laptop asks whoever holds `people:manage`, or uses the
// Tier A token, which holds it and is fresh by construction.
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
		// somebody looking for a session they successfully closed. It is
		// an absence the directory PROVED against the identity log's end
		// ([Directory.SessionStanding]): read bare, a node that had not
		// applied the session's start answered `ended` about a session
		// still running, and wrote nothing.
		//
		// IT IS NOT THE ANSWER A SESSION SOMEBODY ELSE HOLDS GETS BELOW,
		// which is the table's refusal, so the difference says a lineage
		// the caller already holds names a session they may not end. That
		// is all it says — no owner, no person — and a lineage is not a
		// secret (above); answering that refusal `ended` too would tell
		// the caller a session they did not end is over.
		httpjson.Write(w, http.StatusOK, map[string]string{"status": "ended"})
		return
	}
	// THE CALLER'S OWN, IN THE TABLE'S TERMS. The directory classes compare
	// the principal's id, while a caller on a Tier A token opens its
	// sessions under the token's login ([subjectOf]) — so a session whose
	// owner is the caller's own subject is named by the id the self arm
	// compares, and every other owner as itself.
	about := owner
	if owner == subjectOf(r, principal) {
		about = principal.ID.String()
	}
	d := authz.Decide(r.Context(), principal, authz.ActionSessionEnd,
		authz.Object{Kind: authz.KindPerson, Owner: about, ID: about},
		authz.NoChart{}, s.now())
	if d.Unknown() || !d.Allowed {
		authz.EnvelopeRefusal(w, r, authz.Policy{Action: authz.ActionSessionEnd}, d)
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
	// A FRESH OPERATION, for the sign-out's reason above: derived from the
	// lineage, the close of a session older than an hour carried an id the
	// ledger could not vouch for.
	opID := statelog.NewOpID(s.now(), "logout-one")
	closed, err := s.writer.CloseSession(r.Context(), lineage, owner,
		"signed out from another session", opID)
	switch {
	case removed(err):
		// ITS PERSON WAS REMOVED, and the session with them: what the
		// caller asked for is so, and there is nothing to announce.
		s.clearIfPresented(w, r, lineage)
		httpjson.Write(w, http.StatusOK, map[string]string{"status": "ended"})
		return
	case err != nil:
		writeFailed(w, r, "api_sign_out_one_failed", opID, err)
		return
	}
	if !landed(closed) {
		unresolved(w, r, "api_sign_out_one_unresolved", closed)
		return
	}
	// A PERSON ENDING THEIR OWN is a logout; an administrator ending
	// somebody else's is a revocation, and the trail has to say which,
	// because the second is the row an investigation of a stolen laptop is
	// looking for. The ARM THAT ADMITTED says which, so the trail and the
	// decision cannot disagree about whose session this was.
	reason := types.EndLogout
	if d.Reason != authz.ReasonSelf {
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

// subjectOf is the subject the caller's OWN sessions are opened under: a
// person's id, or — for a caller acting on a Tier A token, by its bearer or by
// a session exchanged from it — the token's login, which is what the exchange
// opens its sessions under because a token has no directory row.
//
// ONE ANSWER FOR THE THREE GESTURES THAT ASK, so "end my session", "end my
// other sessions" and "sign me out everywhere" agree about who "me" is: a
// token's caller compared against its derived id owned none of the sessions
// it had opened, and signing out everywhere bumped an epoch nothing read. The
// named sign-out asks the authority table about a session this matches as the
// caller's own id, which is what the table's self arm compares.
func subjectOf(r *http.Request, p iam.Principal) string {
	if entry, tierA := auth.TierA(r.Context()); tierA {
		return iam.TokenLogin(entry.ID)
	}
	return p.ID.String()
}
