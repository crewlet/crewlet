package authapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PASSWORD RESET LINK, AND WHY ITS TWO METHODS ARE THE INVITATION'S.
//
// An administrator issues it through `/iam` (a `reset` credential on the
// person, a day long) and sends it out of band; this is the other end. It is
// the invitation pair in every respect that matters, for the invitation's
// reasons:
//
//   - A GET SAYS WHOSE PASSWORD IT SETS AND SPENDS NOTHING, because a link is
//     followed by mail clients prefetching, scanners and preview cards.
//   - THE LINK IS AN ID AND A SECRET and only the id is ever in a path: the
//     view takes the secret in [resetSecretHeader], the spend in its body.
//   - EVERY WAY A LINK FAILS TO OPEN IS ONE 410, in the same bytes — an id
//     that is no link, a secret that is not the id's, a link spent, revoked
//     or aged out, a person the reset no longer reaches — and only a link
//     that did not PROVE itself is a failed attempt ([types.FailReset]).
//   - IT MEETS NO CURVE ([Service.uncounted]): it names nobody until it
//     opens, and its secret is 256 bits of crypto/rand.
//
// # The spend is one record, and it signs nobody in
//
// [iamdomain.Writer.SetPassword], judged in its own snapshot by the link
// ([iamdomain.ResetRow.Opens]): the new password, the link spent with every
// other the person held, and the revocation epoch moved — every session and
// machine token they held ends. It answers the LOGIN and nothing else: the
// person signs in next, where a second factor they hold still applies, which a
// session handed out here would skip.

// resetSecretHeader carries a reset link's secret to the view, for
// [inviteSecretHeader]'s reason.
const resetSecretHeader = "X-Crewlet-Reset-Secret"

// resetView is what the reset screen renders from.
type resetView struct {
	// Login is whose password the link sets — what the screen says before
	// anybody types one.
	Login     string    `json:"login"`
	ExpiresAt time.Time `json:"expires_at"`

	// MinPasswordLength is the floor, so a form refuses before it posts.
	MinPasswordLength int `json:"min_password_length"`
}

// resetSpend is what spending a link presents.
type resetSpend struct {
	Secret   string `json:"secret"`
	Password string `json:"password"`
}

// errResetSpent is a link its spend's own snapshot found no longer opens.
var errResetSpent = errors.New("authapi: this reset link no longer opens")

// resetNamespace keeps [resetOpID]'s ids apart from every other derivation.
// FIXED for the life of the format, for [redemptionNamespace]'s reason.
const resetNamespace = "crewlet.authapi.password-reset"

// resetOpID is the operation a link's spend is published under: DERIVED from
// the link's credential, at its instant, so a retry of a spend whose answer
// was lost is answered from the ledger rather than refused as a link already
// used — [redemptionOpID]'s shape and reason.
func resetOpID(credential string) string {
	at, _ := statelog.OpMintedAt(credential)
	return statelog.DeriveOpID(at, "password-reset", resetNamespace, credential)
}

// ViewReset is `GET /auth/reset/{id}`: whose password a link sets, without
// spending it. UNGUARDED, and on no curve — see the file's own doc.
func (s *Service) ViewReset(w http.ResponseWriter, r *http.Request) {
	adm := s.uncounted(r)
	held, ok := s.presentedReset(w, r, adm, r.PathValue("id"),
		r.Header.Get(resetSecretHeader))
	if !ok {
		return
	}
	httpjson.Write(w, http.StatusOK, resetView{
		Login: held.Login, ExpiresAt: held.ExpiresAt,
		MinPasswordLength: s.passwordFloor(),
	})
}

// SpendReset is `POST /auth/reset/{id}`: the new password, set from the link
// in ONE record, which ends every session the person held and signs nobody in.
func (s *Service) SpendReset(w http.ResponseWriter, r *http.Request) {
	adm := s.uncounted(r)
	body, err := httpjson.ReadBody(w, r, maxLoginBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var in resetSpend
	if err = json.Unmarshal(body, &in); err != nil {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}
	id := r.PathValue("id")
	held, ok := s.presentedReset(w, r, adm, id, in.Secret)
	if !ok {
		return
	}
	// JUDGED ONCE THE LINK HAS OPENED: the floor is the holder's to meet,
	// told to nobody else.
	if err = credential.CheckStrength(in.Password, s.passwordFloor()); err != nil {
		refuseWeak(w, err)
		return
	}
	verifier, hashed := s.hash(w, r, adm, in.Password, "reset")
	if !hashed {
		return
	}
	opID := resetOpID(held.ID)
	set, err := s.writer.SetPassword(r.Context(), iamdomain.PasswordSet{
		PersonID: held.PersonID, Verifier: verifier,
		// THE LINK AGAIN, IN THE RECORD'S OWN SNAPSHOT: one spent by
		// another tab, revoked by an administrator or its person
		// suspended since the read above opens nothing here either.
		Check: func(p iamdomain.Person) error {
			if !iamdomain.ResetOf(p, held.ID).Opens(in.Secret, s.now()) {
				return errResetSpent
			}
			return nil
		},
		OpID: opID, Reason: "set a new password from a reset link",
	})
	switch {
	case errors.Is(err, errResetSpent), errors.Is(err, iamdomain.ErrNotFound),
		removed(err):
		// THE LINK PROVED ITSELF AND NO LONGER WORKS — or its person is
		// gone — which is the 410 every dead link answers, and no guess.
		log.InfoContext(r.Context(), "api_reset_refused_at_the_record",
			"credential", held.ID, "error", err)
		s.refuseReset(w, r, adm, true)
		return
	case err != nil:
		writeFailed(w, r, "api_reset_failed", opID, err)
		return
	case !landed(set):
		unresolved(w, r, "api_reset_unresolved", set)
		return
	}
	if !set.Collapsed {
		// ANNOUNCED BY THE CALL THAT MADE IT: a retry answered from the
		// ledger is the same spend, already said.
		s.audit.Emit(r.Context(), types.IAMPasswordReset{
			Person: held.PersonID, Login: held.Login, Credential: held.ID,
			Remote: s.sourceOf(r),
		})
	}
	log.InfoContext(r.Context(), "api_password_reset", "person", held.PersonID,
		"credential", held.ID, "position", set.Position.String())
	httpjson.Write(w, http.StatusOK, map[string]string{
		"status": "password_set", "login": held.Login,
	})
}

// presentedReset resolves one reset link from the two halves of its link,
// answering false once it has written the refusal — [presentedInvitation]'s
// shape: one 410 for every way it fails to open, counted only where the link
// did not prove itself.
func (s *Service) presentedReset(w http.ResponseWriter, r *http.Request,
	adm admission, id, secret string) (iamdomain.ResetRow, bool) {

	held, err := s.directory.ResetByID(r.Context(), id)
	if err != nil {
		log.WarnContext(r.Context(), "api_reset_lookup_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(err))
		return iamdomain.ResetRow{}, false
	}
	if held.Opens(secret, s.now()) {
		return held, true
	}
	proved := held.ID != "" && credential.VerifyReset(held.Verifier, held.ID, secret)
	s.refuseReset(w, r, adm, proved)
	return iamdomain.ResetRow{}, false
}

// refuseReset is every 410 a reset link answers, in the same bytes — a failed
// attempt only where the link did not prove itself, for [refuseInvitation]'s
// reason.
func (s *Service) refuseReset(w http.ResponseWriter, r *http.Request,
	adm admission, proved bool) {

	if !proved {
		s.audit.Failed(r.Context(), authevents.Failure{
			Source: adm.source, Method: types.FailReset,
		})
	}
	httpjson.Fail(w, http.StatusGone, httpjson.CodeResetSpent)
}
