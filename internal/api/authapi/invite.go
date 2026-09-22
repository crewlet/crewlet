package authapi

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// THE INVITATION, AND WHY ITS TWO METHODS ARE DIFFERENT OPERATIONS.
//
// A GET RENDERS AND NEVER SPENDS. A link is followed by things that are not
// the person it was sent to: a mail client prefetching, a security scanner
// opening every URL in a message, a chat app building a preview card. Every
// one of those is a GET, and an invitation spent by one is an account created
// for somebody who never saw it — or, more often, a person told their link was
// already used by whoever scanned their mailbox.
//
// So the GET answers what the form needs to render — which address it is for,
// who sent it, whether it is still good — and changes nothing. The POST is the
// person, having typed a password.

// inviteView is what the form renders from.
type inviteView struct {
	// Email is the address this invitation is for, OPENED for this one
	// answer: the person is about to type it into a form and refusing to
	// show it would make them guess which of their addresses it was sent
	// to.
	Email string `json:"email"`

	// InvitedBy is who sent it, which is the first thing somebody
	// checks before they act on a link.
	InvitedBy string `json:"invited_by,omitempty"`

	// MinPasswordLength is the floor, so a form refuses before it posts.
	MinPasswordLength int `json:"min_password_length"`
}

// inviteRedeem is what redeeming presents.
type inviteRedeem struct {
	Login    string `json:"login"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// ViewInvite renders an invitation without spending it.
//
// UNGUARDED, because holding the link IS the credential — and throttled per
// source, because the id is a value somebody could otherwise walk.
func (s *Service) ViewInvite(w http.ResponseWriter, r *http.Request) {
	source := s.sourceOf(r)
	if !s.admit(w, r, source) {
		return
	}
	held, ok := s.invitation(w, r)
	if !ok {
		return
	}
	email, err := s.openSealed(r, held)
	if err != nil {
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, retryIdentity)
		return
	}
	httpjson.Write(w, http.StatusOK, inviteView{
		Email: email, InvitedBy: held.InvitedBy,
		MinPasswordLength: iam.MinPasswordChars,
	})
}

// RedeemInvite creates the person an invitation was issued for.
func (s *Service) RedeemInvite(w http.ResponseWriter, r *http.Request) {
	source := s.sourceOf(r)
	if !s.admit(w, r, source) {
		return
	}
	held, ok := s.invitation(w, r)
	if !ok {
		return
	}
	body, err := httpjson.ReadBody(w, r, maxLoginBody)
	if err != nil {
		return
	}
	var in inviteRedeem
	if err := json.Unmarshal(body, &in); err != nil {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}
	if err := credential.CheckStrength(in.Password); err != nil {
		// SPECIFIC, because holding the link is already evidence this
		// invitation was issued to them and the remedy is theirs.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
		return
	}
	email, err := s.openSealed(r, held)
	if err != nil {
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, retryIdentity)
		return
	}
	verifier, err := s.hasher.Hash(in.Password)
	if err != nil {
		log.ErrorContext(r.Context(), "api_invite_hash_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}

	person := uuid.New().String()
	opID := "invite:" + held.ID + ":" + person
	if _, err := s.writer.Enrol(r.Context(), iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Name: in.Name, Email: email, Login: in.Login,
		Credentials: []iamdomain.Credential{{
			V: iamdomain.DocumentVersion, ID: uuid.New().String(),
			Method: iamdomain.MethodPassword, Verifier: verifier,
		}},
		// WHAT THE INVITATION SAID, and not a word more. The grants and
		// the reach were decided once, by whoever issued it, rather
		// than again by whoever happens to process the redemption —
		// which is also what stops a redemption being a way to ask for
		// more than was offered.
		Grants:    held.Grants,
		Colleague: held.Colleague,
		OpID:      opID, Reason: "redeemed an invitation",
	}); err != nil {
		log.ErrorContext(r.Context(), "api_invite_enrol_failed", "error", err)
		httpjson.Fail(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable)
		return
	}
	if _, err := s.writer.SpendInvitation(r.Context(), iamdomain.InvitationSpend{
		ID: held.ID, Blind: held.Blind, Person: person,
		OpID: opID + ":spend", Reason: "redeemed",
	}); err != nil {
		// LOGGED AND NOT REPORTED. The person exists and can sign in;
		// the residue is an invitation row that still reads unspent,
		// which the address claim their enrolment took already prevents
		// anybody else from using.
		log.WarnContext(r.Context(), "api_invite_spend_failed",
			"error", err, "invitation", held.ID, "person", person)
	}
	log.InfoContext(r.Context(), "api_invite_redeemed",
		"invitation", held.ID, "person", person, "login", in.Login)
	s.throttle.Flush(r.Context(), source)
	s.completeSignIn(w, r, iamdomain.Sighting{
		ID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Login: in.Login, Grants: held.Grants, Colleague: held.Colleague,
	})
}

// invitation resolves the link, answering false once it has written the
// refusal.
//
// ONE REFUSAL FOR ABSENT, REDEEMED AND EXPIRED, because the three have one
// remedy — ask for a new one — and telling them apart would say "this was
// already used" to somebody whose link merely aged out, sending them to find
// out who used it.
func (s *Service) invitation(w http.ResponseWriter, r *http.Request) (
	iamdomain.InvitationRow, bool) {

	held, err := s.directory.InvitationByID(r.Context(), r.PathValue("id"))
	if err != nil {
		log.WarnContext(r.Context(), "api_invite_lookup_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, retryIdentity)
		return iamdomain.InvitationRow{}, false
	}
	if held.ID == "" || held.Spent(s.now()) {
		httpjson.Fail(w, http.StatusGone, httpjson.CodeInviteSpent)
		return iamdomain.InvitationRow{}, false
	}
	return held, true
}

// openSealed opens the invitation's address for this one answer.
//
// UNDER THE INVITATION'S OWN KEY rather than a person's, because there is no
// person yet — minting one for an invitation that may never be redeemed would
// leave a key behind for every address anybody ever typed.
func (s *Service) openSealed(r *http.Request, held iamdomain.InvitationRow) (string, error) {
	if held.Sealed == "" {
		return "", nil
	}
	email, err := s.opener.Open(r.Context(), held.ID, iamdomain.FieldEmail, held.Sealed)
	if err != nil {
		log.WarnContext(r.Context(), "api_invite_unseal_failed",
			"error", err, "invitation", held.ID)
		return "", err
	}
	return email, nil
}
