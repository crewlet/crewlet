package authapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// directoryFor adapts this surface's own seam to the one
// [session.Signer.Validate] takes.
//
// TWO SEAMS OVER ONE READER, deliberately: [Directory] is what a SIGN-IN may
// reach, session's what a VALIDATION needs; widening either gives a route the
// half it has no business with. It carries the guard's reading of the subject:
// a Tier A token's session names a login no person row holds, answered from the
// entry this node holds ([auth.SessionSubjects]); the bare estate would read
// its person as gone, so [Service.Logout] would close nothing.
func (s *Service) directoryFor() session.Directory { return s.sessions }

// configResponse is what a client reads before anybody has signed in.
//
// # What is in it, and what is deliberately not
//
// Enough to render the right form and nothing that says who works here: the
// password floor so a form can refuse a short password before a round trip,
// and whether a second factor is required.
//
// THERE IS NO USER LIST, no count of people, and no hint of whether any
// particular login exists. This route is unguarded, so everything on it is
// public — and the one question an attacker most wants answered here is who
// they could be.
type configResponse struct {
	// MinPasswordLength is the floor a form enforces before it posts.
	MinPasswordLength int `json:"min_password_length"`

	// SecondFactor is `required` or `optional`: whether a person signing
	// in with a password must hold a second factor.
	SecondFactor iam.SecondFactor `json:"second_factor"`
}

// Config answers what a client needs before anybody has signed in.
//
// UNGUARDED, necessarily: it is what a sign-in page reads to know what kind of
// sign-in page to be. See [configResponse] for what that costs and what it is
// therefore not allowed to carry.
func (s *Service) Config(w http.ResponseWriter, r *http.Request) {
	httpjson.Write(w, http.StatusOK, configResponse{
		MinPasswordLength: s.passwordFloor(),
		SecondFactor:      s.policy().SecondFactor,
	})
}

// sessionResponse is who the caller is.
type sessionResponse struct {
	Person string `json:"person"`

	// Name is the person's own name, as their directory row holds it —
	// what a screen greets them by when no seat names them. ABSENT for a
	// principal with no row of its own (a Tier A token), and where this
	// node could not read or open it: see [Service.ownName].
	Name   string      `json:"name,omitempty"`
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

	// Lineage is the id of the session this request carried — the one a
	// listing of the person's sessions names it by — so a client can say
	// which of them is this browser and offer no named sign-out of it.
	// ABSENT exactly where ExpiresAt is. Not a secret: it is what
	// `POST /auth/logout/{lineage}` takes, and that route decides on the
	// session's owner, never on who knows its id.
	Lineage string `json:"lineage,omitempty"`

	// ReauthAt is when this caller's proof of who they are stops counting
	// for a step-up gesture (`sign_in.step_up_seconds`) — the instant the
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
		auth.UnavailableIdentity(w, iam.Reason(r.Context()))
		return
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	// THE DEADLINE THE SESSION ENDS AT under the limits in force, read off
	// its signature. A request whose credential was a header carries no
	// session, and the zero bearer omits it.
	var expires time.Time
	var lineage string
	status := statusSignedIn
	if r.Header.Get("Authorization") == "" {
		presented := s.presentedSession(r)
		expires = presented.Ends.Absolute
		if presented.Bearer.Lineage != uuid.Nil {
			lineage = presented.Bearer.Lineage.String()
		}
		// THE BEARER'S MARK AND THE ROW'S TOGETHER, as the guard reads
		// it: on a node that has not applied the session's start there
		// is no row, and the session is still the restricted one.
		if presented.EnrolmentOnly() {
			status = statusEnrolmentRequired
		}
	}
	httpjson.Write(w, http.StatusOK, sessionResponse{
		Person: principal.ID.String(), Name: s.ownName(r.Context(), principal),
		Login:     principal.Login,
		Seat:      principal.Seat,
		Kind:      principal.Kind,
		Stage:     principal.Stage,
		Grants:    principal.Grants,
		ExpiresAt: expires,
		Lineage:   lineage,
		ReauthAt:  principal.ReauthAt,
		StepUpDue: s.stepUpDue(principal),
		Status:    status,
	})
}

// ownName is a principal's own name as their directory row holds it, opened on
// this node's keyring, or "".
//
// A COURTESY ON THIS ANSWER AND NEVER A CONDITION OF IT: who somebody is was
// decided by the guard, and refusing it over a name this node could not read
// or open would sign a person out over their greeting. So a principal with no
// row — a Tier A token, whose id no enrolment wrote — answers none quietly,
// and a read or an open that fails answers none and is logged.
func (s *Service) ownName(ctx context.Context, p iam.Principal) string {
	if p.ID == uuid.Nil {
		return ""
	}
	id := p.ID.String()
	row, err := s.directory.Person(ctx, id)
	switch {
	case errors.Is(err, iamdomain.ErrNotFound):
		return ""
	case err != nil:
		log.WarnContext(ctx, "api_session_name_unread", "person", id, "error", err)
		return ""
	}
	name, err := s.sealer.Open(id, iamdomain.FieldName, string(row.NameSealed))
	if err != nil {
		log.WarnContext(ctx, "api_session_name_sealed", "person", id, "error", err)
		return ""
	}
	return name
}

// stepUpDue reports whether this caller's proof of identity is old enough that
// the next step-up gesture will ask them to confirm it.
//
// THE PRINCIPAL'S OWN DEADLINE, never a second number: [iam.Principal.ReauthAt]
// is the instant the proof stops counting, composed by the guard from the
// proof's time and this node's `step_up` window, so a client is warned at the
// threshold that refuses. It is a deadline, not the instant of proof: do not
// subtract the window again. A zero deadline is stale, the honest reading of a
// session opened by a route that proves nothing (the Tier A token exchange).
func (s *Service) stepUpDue(p iam.Principal) bool {
	return !p.Fresh(s.now())
}

// Logout ends THIS session. THE COOKIE IS CLEARED WHATEVER THE WRITE DID: a 503
// would leave a person on a shared machine looking at a signed-in page. The
// record's failure is logged, not shown; the cookie is all the browser holds.
//
// The route is UNGUARDED ([auth.PathAuthLogout], the guard's exact list): the
// guard's `503 identity_unavailable` would run first on a node that cannot read
// its estate, and nothing here trusts what the guard says. Every bearer, under
// either name ([session.Held]), is read under its signature and this node's
// rows; only a session they still hold is closed and announced
// (TestASignOutEndsOnlyASessionStillLive). A node that cannot read its rows
// still records the close, announcing nothing it cannot say was live.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	s.signOut(w)
	s.endEveryHeld(r)
	httpjson.Write(w, http.StatusOK, map[string]string{"status": "signed out"})
}

// endEveryHeld closes every session this browser holds a cookie for, under
// either name — see [Service.Logout] and, for a sign-in that replaces them,
// [Service.openSignIn].
func (s *Service) endEveryHeld(r *http.Request) {
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
	//
	// A FRESH OPERATION, as every sign-out is ([iamdomain.SessionOpsRetention]
	// says why): it was derived from the lineage, whose instant is when the
	// session BEGAN, and a session subject's ledger rows go after an hour —
	// so the sign-out of any session older than that carried an id the
	// ledger could not vouch for, and was answered `unknown` without being
	// published. A second sign-out is a second close of a closed session,
	// which changes nothing.
	opID := statelog.NewOpID(s.now(), "logout")
	closed, err := s.signingOut(r, lineage).CloseSession(r.Context(), lineage,
		bearer.Person, "signed out", opID)
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

// signingOut is who a sign-out of one session is written as: the person
// signing out, where the guard resolved them through that very session — the
// cookie this deployment issues — and the node otherwise. The route is
// unguarded and reads every bearer itself, so a second cookie under the other
// name, or a node that could not read who holds this one, has nobody the
// guard vouched for to name.
func (s *Service) signingOut(r *http.Request, lineage string) Writer {
	principal, resolution := iam.From(r.Context())
	if resolution == iam.Resolved && principal.Via == iam.SessionName(lineage) {
		return s.behalf(principal)
	}
	return s.writer
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
	s.signOut(w)

	// THE CALLER'S OWN SUBJECT, which for a Tier A token is its login: the
	// epoch under it is what every session exchanged from that token is
	// checked against.
	person := subjectOf(r, principal)
	opID := statelog.NewOpID(s.now(), "logout-all")
	revoked, err := s.behalf(principal).Revoke(r.Context(), person, opID,
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
// The SIGNATURE decides the lineage: one read from an unchecked cookie is one
// the CALLER CHOSE, a denial of service against anybody whose session id leaks.
// [session.Signer.Validate] yields a bearer only when the signature verifies.
// The ROWS decide whether anything is left to end, so the verdict comes beside
// the bearer: an expired or ended session still carries a verified one.
// [Service.Logout] reads the row; the step-up and a named sign-out need only
// the bearer's own fields. THE NAME THIS DEPLOYMENT ISSUES, by
// [session.Presented]'s rule; only the sign-out reaches the other, [session.Held].
func (s *Service) presentedSession(r *http.Request) session.Validation {
	cookie := session.Presented(r, s.boot.API.ExternalBase())
	if cookie == "" {
		return session.Validation{}
	}
	return s.signer.Validate(r.Context(), s.directoryFor(), cookie)
}

// clearSession ends the session in the browser under every name a bearer can
// be held under ([session.End]).
func (s *Service) clearSession(w http.ResponseWriter) {
	session.End(w, s.boot.API.ExternalBase())
}

// signOut is [Service.clearSession] for a person's own sign-out of this
// browser, which also empties its HTTP cache ([session.SignOut] says why only
// there).
func (s *Service) signOut(w http.ResponseWriter) {
	session.SignOut(w, s.boot.API.ExternalBase())
}

// LogoutOne ends ONE named session, which is how somebody signs out of a
// laptop they left in an office without signing out of the browser they are
// doing it from.
//
// A lineage is not a secret, so the OWNER is read from this node's rows, not
// claimed by the caller, and whether the caller may end it is
// [authz.ActionSessionEnd], the verb `DELETE /iam/people/{id}/sessions` is
// mounted on: one rule for one session or all, their own on no proof, anybody
// else's on admin, a `step_up` and a person present. No check of its own may
// be added here: the operating permission a machine token may carry would end
// any session with no step-up. An SRE with a stolen laptop asks an admin, or
// uses a Tier A token holding admin, fresh by construction.
func (s *Service) LogoutOne(w http.ResponseWriter, r *http.Request) {
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		auth.UnavailableIdentity(w, iam.Reason(r.Context()))
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
		auth.UnavailableIdentity(w, err)
		return
	}
	if owner == "" {
		// ABSENT IS NOT AN ERROR: a session already ended is what the
		// caller asked for, and a 404 would send them looking. The
		// absence is PROVED against the identity log's end
		// ([Directory.SessionStanding]); read bare, a node that had not
		// applied the session's start would say `ended` of a running one.
		//
		// IT IS NOT THE ANSWER A SESSION SOMEBODY ELSE HOLDS GETS BELOW,
		// the table's refusal, which says only that a lineage the caller
		// holds names a session they may not end. Answering that `ended`
		// too would tell them a session they did not end is over.
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
	closed, err := s.behalf(principal).CloseSession(r.Context(), lineage, owner,
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

// clearIfPresented signs this browser out when the request's cookie names the
// lineage a named sign-out ended, so a person who ends the session they are
// using is not left looking at a signed-in page.
func (s *Service) clearIfPresented(w http.ResponseWriter, r *http.Request, lineage string) {
	for _, cookie := range session.Held(r, s.boot.API.ExternalBase()) {
		held := s.signer.Validate(r.Context(), s.directoryFor(), cookie)
		if lineage == held.Bearer.Lineage.String() {
			s.signOut(w)
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
