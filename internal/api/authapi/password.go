package authapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PERSON CHANGES THEIR OWN PASSWORD.
//
// # The current password is the proof, and the only one asked
//
// The route is guarded — a person signed in — and the CURRENT password is
// verified exactly as a step-up's is: on the throttle's curve for who they are
// signed in as, through the verify cap, a wrong one a counted failure answered
// with the one refusal every sign-in arm gives. That verification IS the
// recent proof the change needs, so the route does not additionally send the
// person through a step-up first.
//
// # Only a person, at a keyboard
//
// It is [authz.ActionCredentialProof] — how somebody proves who they are —
// whose row needs a PERSON PRESENT, so a request that presented a machine
// token is refused by the table exactly as an enrolment is, on the principal
// as this request proves it: the current password it presents is a proof
// taken now. And a session that is not a person's — a Tier A token exchanged
// for one — has no password to change, which is said rather than mistaken for
// a wrong password.
//
// # It ends everything else, and keeps this browser
//
// The new password and a moved revocation epoch are ONE record
// ([iamdomain.Writer.SetPassword]): somebody changes a password when they think
// somebody else has it, so every session and machine token the person held
// ends with it, every outstanding reset link too. THIS browser is then handed
// a new session exactly as a step-up hands one — the session it presented
// ended first, the new one keeping its absolute deadline — so the person stays
// signed in where they made the change.
//
// THE NEW SESSION'S PROOF is now for somebody who holds no second factor, for
// whom a password IS the whole of a step-up; for somebody who holds one it is
// the replaced session's, because the change proved the password and not the
// code, and a step-up from them asks for both.
//
// A REPLACEMENT THAT FAILS AFTER THE CHANGE LANDED answers its own failure: the
// password is changed and the cookie this request held is over with the
// epoch, so the next request signs the person out and the new password signs
// them in. A change this node has not applied yet (`pending`) opens no session
// here at all, because the epoch a new session is opened at is read from rows
// that do not yet hold the move — it says so, and the person signs in again.

// passwordChange is what a change presents.
type passwordChange struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// passwordChanged is a change answered without a session: one this node has
// not applied yet.
type passwordChanged struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// ownPasswordChanged is why a change ended this person's sessions: the
// reason its epoch move writes on every session it ends, and the reason the
// close of the session THIS browser held records too.
//
// ONE SENTENCE FOR BOTH, because the move has already ended that session by
// the time the close is written — a row keeps the first reason that ended it —
// so the person's own session list read the move's reason while the trail's
// close row read another ("replaced by a password change") about the same
// session.
const ownPasswordChanged = "changed their own password"

// errPasswordMoved refuses a change whose person's password is no longer the
// one this request verified, in the record's own snapshot: somebody changed or
// reset it in between, which ended this session too.
var errPasswordMoved = errors.New("authapi: the password this request " +
	"verified is no longer the one held")

// ChangePassword is `POST /auth/password`.
func (s *Service) ChangePassword(w http.ResponseWriter, r *http.Request) {
	source := s.sourceOf(r)
	principal, resolution := iam.From(r.Context())
	switch resolution {
	case iam.Resolved:
	case iam.Unknown:
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable,
			auth.RetryIdentity(iam.Reason(r.Context())))
		return
	default:
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	if principal.Kind != iam.KindPerson {
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeForbidden,
			map[string]string{"detail": "a password belongs to a person, and " +
				"this request acts as a credential of the deployment's own, " +
				"which has none — sign in as the person whose password it is"})
		return
	}
	if !s.mayChangeOwnPassword(w, r, principal) {
		return
	}
	body, err := httpjson.ReadBody(w, r, maxLoginBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var in passwordChange
	if err = json.Unmarshal(body, &in); err != nil {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}
	// THE NEW ONE IS JUDGED FIRST: it is the caller's own to fix, told to
	// nobody else, and it costs no derivation to refuse.
	if err = credential.CheckStrength(in.NewPassword, s.passwordFloor()); err != nil {
		refuseWeak(w, err)
		return
	}
	// THE CURVE ON WHO THEY ARE SIGNED IN AS, which is the subject here —
	// the step-up's own, so a stolen cookie is not a way round the curve on
	// the password it would change.
	adm, ok := s.admit(w, r, credential.Attempt{
		Source: source, Subject: principal.Login}, types.FailPassword)
	if !ok {
		return
	}
	defer adm.ticket.Release()

	held, err := s.directory.PersonByLogin(r.Context(), principal.Login)
	if err != nil {
		log.WarnContext(r.Context(), "api_password_lookup_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(err))
		return
	}
	attempt := authevents.Failure{Source: source, Method: types.FailPassword}
	// THE FIELD IS NAMED, in every arm alike: the caller is signed in as the
	// person whose password this is, so it discloses nothing — and the
	// sign-in's own sentence, about "sign-in details", left somebody
	// re-checking the new password on a form that is not a sign-in.
	refuse := func(why string) {
		s.refuseSignInWith(w, r, adm, attempt, why, map[string]string{
			"field":  "current_password",
			"detail": "your current password was not accepted — check it and try again"})
	}
	if held.ID != principal.ID.String() || !stageAdmits(held.Stage) {
		if s.decoy(w, r, adm, in.CurrentPassword) {
			refuse("password change: subject not active")
		}
		return
	}
	current, found := firstCredential(held.Credentials, iamdomain.MethodPassword)
	if !found {
		if s.decoy(w, r, adm, in.CurrentPassword) {
			refuse("password change: no password credential")
		}
		return
	}
	proved, _, err := s.hasher.Verify(r.Context(), current.Verifier, in.CurrentPassword)
	if err != nil {
		abandoned(w, r, adm.source, err)
		return
	}
	if !proved {
		refuse("password change: password mismatch")
		return
	}
	replaced, ok := s.replacedSession(w, r, held)
	if !ok {
		return
	}
	adm.ticket.Succeed()
	fresh, hashed := s.hash(w, r, adm, in.NewPassword, "password")
	if !hashed {
		return
	}

	// A FRESH OPERATION PER CHANGE: a change asked again is a new one,
	// whose own snapshot reads what the first left — the verifier it
	// verified is gone if the first landed.
	opID := statelog.NewOpID(s.now(), "password-change")
	changed, err := s.behalf(principal).SetPassword(r.Context(), iamdomain.PasswordSet{
		PersonID: held.ID, Verifier: fresh,
		Check: func(p iamdomain.Person) error {
			for _, c := range p.Credentials {
				if c.ID == current.ID && c.Method == iamdomain.MethodPassword &&
					c.Verifier == current.Verifier && c.RevokedAt.IsZero() {
					return nil
				}
			}
			return errPasswordMoved
		},
		OpID: opID, Reason: ownPasswordChanged,
	})
	switch {
	case errors.Is(err, errPasswordMoved):
		s.clearSession(w)
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeStale,
			map[string]string{"detail": "this password was changed or reset " +
				"since this request checked it, which ended this session — " +
				"sign in with the new one"})
		return
	case removed(err):
		s.proofOfRemoved(w, r)
		return
	case err != nil:
		writeFailed(w, r, "api_password_change_failed", opID, err)
		return
	case !built(changed):
		unresolved(w, r, "api_password_change_unresolved", changed)
		return
	}
	s.audit.Emit(r.Context(), types.IAMPasswordChanged{
		Person: held.ID, Login: held.Login, Remote: source,
	})
	log.InfoContext(r.Context(), "api_password_changed", "person", held.ID,
		"position", changed.Position.String())
	if changed.Outcome != statelog.OutcomeApplied {
		// DURABLE AND NOT HERE YET: a session opened now would be opened at
		// the epoch this change ended — see the file's own doc.
		s.clearSession(w)
		httpjson.Write(w, http.StatusAccepted, passwordChanged{
			Status: "password_changed",
			Detail: "the password is changed and every session ended; this " +
				"node has not applied the change yet, so sign in again with " +
				"the new password",
		})
		return
	}
	how := signIn{
		method: types.SignInPassword, stepUp: true,
		replaces: replaced.Bearer.Lineage.String(),
		absolute: replaced.Bearer.AbsoluteExpiresAt,
		because:  ownPasswordChanged,
	}
	if holdsSecondFactor(held) {
		proof := replaced.Session.ProvedAt
		how.provedAt = &proof
	}
	s.completeSignIn(w, r, held, how)
}

// mayChangeOwnPassword refuses a request that may not change its own password:
// the proof verb's row, decided for the caller's own subject on the principal as
// this request proves it — the current password it presents is a proof taken
// now, so the verb's recency is met by the request itself, and what the row
// still decides is that a person is present.
func (s *Service) mayChangeOwnPassword(w http.ResponseWriter, r *http.Request,
	principal iam.Principal) bool {

	now := s.now()
	proving := principal
	// THE DEADLINE A PROOF TAKEN NOW EARNS ON THIS NODE, as the guard stamps
	// one from a session's proof instant.
	proving.ReauthAt = now.Add(s.boot.API.Auth.Session.StepUp())
	d := authz.Decide(r.Context(), proving, authz.ActionCredentialProof,
		authz.Object{Kind: authz.KindPerson, Owner: principal.ID.String()},
		authz.NoChart{}, now)
	if d.Unknown() || !d.Allowed {
		authz.EnvelopeRefusal(w, r, authz.Policy{}, d)
		return false
	}
	return true
}

// refuseWeak answers a new password the deployment will not accept: 422, with
// the rule's own sentence — the caller is the person choosing it, and the
// remedy is theirs.
func refuseWeak(w http.ResponseWriter, err error) {
	httpjson.FailWith(w, http.StatusUnprocessableEntity, httpjson.CodeInvalid,
		map[string]string{"detail": err.Error()})
}
