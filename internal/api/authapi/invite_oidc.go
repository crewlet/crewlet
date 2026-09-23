package authapi

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// REDEEMING AN INVITATION THROUGH THE IDENTITY PROVIDER: the one way a
// provider subject is pinned to a person without an administrator.
//
// # The invitation is the authority, and the provider says only who arrived
//
// A provider sign-in proves that whoever is at the browser holds a subject at
// the company's provider. It proves nothing about which person here they are —
// the address it asserts is one a person sets themselves at most providers —
// so an unlinked subject is refused at the ordinary callback. What an
// invitation adds is the missing half: somebody who may enrol people decided
// this link creates THIS person, and whoever holds the link finishes it. So
// the redemption pins the subject the provider came back with to the person
// the invitation creates, and nothing about the provider's own assertions
// decides who that person is.
//
// # It is the password redemption with a different proof
//
// The same derived person ([iamdomain.InvitedPersonID]), the same operation
// id, the same single-use rule (an address somebody is enrolled under is a
// link already used) and the same grants — the invitation's, and the
// enrolment names it as the authority. What differs is the credential: no
// password, and a LINK claimed after the address and the login, as its own
// record on its own subject. A redemption that stops after that claim leaves
// the reservation holding it, and the same redemption through the SAME
// provider account finishes it — through a DIFFERENT account it is refused
// ([iamdomain.ErrLinked]) rather than switched.
//
// # Decided at both ends of the round trip
//
// The START refuses a link that is spent and a login outside the grammar
// before the person is sent to the provider, so a mistake costs one page
// rather than a round trip. The CALLBACK decides again, against the estate as
// it stands when the person comes back — ten minutes is long enough for the
// link to be spent by somebody else — and the invitation it decides about is
// the one SEALED into the flight, never one the way back could name.

// redemptionFlight is what a redemption START seals into the flight, answering
// false once it has written the refusal.
func (s *Service) redemptionFlight(w http.ResponseWriter, r *http.Request,
	want oidc.Flight, invite string) (oidc.Flight, bool) {

	arrived := s.now()
	source := s.sourceOf(r)
	// ADMITTED LIKE THE INVITATION'S OWN ROUTES, because it reads the
	// same row: an id walked through this door would otherwise be a free
	// lookup the GET is throttled against.
	if !s.admit(w, r, source, types.FailInvite) {
		return oidc.Flight{}, false
	}
	held, ok := s.invitationByID(w, r, arrived, source, invite)
	if !ok {
		return oidc.Flight{}, false
	}
	login := strings.TrimSpace(r.URL.Query().Get("login"))
	if login == "" {
		// THE LOGIN THE FORM WOULD HAVE PROPOSED, for a page that sent
		// the person straight to the provider: it is derived from the
		// address the invitation was issued to, exactly as the view
		// derives it, so it is a name the person was already shown.
		email, err := s.openSealed(r, held)
		if err != nil {
			httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
			return oidc.Flight{}, false
		}
		login = iam.LoginFromAddress(email)
	}
	if !iam.ValidLoginFor(iam.KindPerson, login) {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "choose a login — lowercase segments " +
				"joined by dots (jane.doe). It is the name every change you make " +
				"is recorded under while you hold no seat."})
		return oidc.Flight{}, false
	}
	want.Invite, want.Login = held.ID, login
	return want, true
}

// redeemThroughProvider finishes a redemption the flight carried, once the ID
// token has verified.
func (s *Service) redeemThroughProvider(w http.ResponseWriter, r *http.Request,
	arrived time.Time, attempt authevents.Failure, flight oidc.Flight,
	claims oidc.Claims, refresh string) {

	held, ok := s.invitationByID(w, r, arrived, attempt.Client, flight.Invite)
	if !ok {
		return
	}
	person, err := iamdomain.InvitedPersonID(held.ID)
	if err != nil {
		log.ErrorContext(r.Context(), "api_invite_person_underivable",
			"error", err, "invitation", held.ID)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	// ONE OPERATION ID WITH THE PASSWORD REDEMPTION, because it is one
	// redemption whichever proof finishes it: a first attempt by password
	// that stopped after its claims and a retry through the provider name
	// the same person, and their shared steps collapse in the ledger.
	opID := "invite:" + held.ID
	holder, err := s.directory.PersonByEmailBlind(r.Context(), held.Blind)
	if err != nil {
		log.WarnContext(r.Context(), "api_invite_holder_unreadable",
			"error", err, "invitation", held.ID)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return
	}
	if holder.ID != "" && !holder.Reserved {
		// ENROLLED ALREADY — see [Service.RedeemInvite]: the link is
		// used, and a second redemption must not re-enrol the person or
		// pin a second account to them with nothing but the link.
		if holder.ID == person {
			s.spendInvitation(r, held, person, opID)
		}
		s.refuseSpentInvitationID(w, r, arrived, attempt.Client, held.ID)
		return
	}
	email, err := s.openSealed(r, held)
	if err != nil {
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return
	}
	blinder, err := s.blinder.Blinder(r.Context())
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_blind_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return
	}
	blind, err := blinder.Subject(claims.Issuer, claims.Subject)
	if err != nil {
		log.WarnContext(r.Context(), "api_oidc_blind_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return
	}
	attempt.Person = person
	enrolled, err := s.writer.Enrol(r.Context(), iamdomain.Enrolment{
		PersonID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		// THE NAME THE PROVIDER ASSERTS is a display name the person can
		// change, and nothing is keyed on it; the ADDRESS is the
		// invitation's and never the provider's, for the linking rule.
		Name: claims.Name, Email: email, Login: flight.Login,
		Grants: held.Grants, Colleague: held.Colleague, Invitation: held.ID,
		Link: &iamdomain.Link{Issuer: claims.Issuer, Blind: blind},
		OpID: opID, Reason: "redeemed an invitation through the identity provider",
	})
	if err != nil {
		var claimed *iamdomain.ErrClaimed
		switch {
		case errors.Is(err, iamdomain.ErrRefused):
			log.InfoContext(r.Context(), "api_invite_refused_at_the_record",
				"invitation", held.ID, "error", err)
			httpjson.Fail(w, http.StatusGone, httpjson.CodeInviteSpent)
		case errors.As(err, &claimed) && claimed.Kind == iamdomain.KindLink:
			s.refuseSubjectConflict(w, r, attempt, "that identity provider "+
				"account is already linked to somebody in this company — "+
				"finish this invitation with your own account, or with a "+
				"password", err)
		case errors.Is(err, iamdomain.ErrLinked):
			s.refuseSubjectConflict(w, r, attempt, "this invitation was begun "+
				"with a different identity provider account — finish it with "+
				"that account, or ask for a new invitation", err)
		default:
			refuseEnrolment(w, r, "api_invite_enrol_failed", err)
		}
		return
	}
	if !landed(enrolled) {
		// NO SPEND, NO LINK ANNOUNCED AND NO SESSION on an enrolment
		// nobody can confirm, as the password redemption answers it: the
		// op id is the invitation's own, so following the link again is
		// the same enrolment.
		unresolved(w, r, "api_invite_enrol_unresolved", enrolled)
		return
	}
	s.spendInvitation(r, held, person, opID)
	log.InfoContext(r.Context(), "api_invite_redeemed",
		"invitation", held.ID, "person", person, "login", flight.Login,
		"through", "oidc")
	s.throttle.Flush(r.Context(), attempt.Client)
	s.completeSignIn(w, r, iamdomain.Sighting{
		ID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Login: flight.Login, Grants: held.Grants, Colleague: held.Colleague,
	}, signIn{
		method: types.SignInOIDC, acr: claims.ACR, redirect: flight.Return,
		refresh: refresh, groupGrants: s.boot.API.Auth.OIDC.GrantsFor(claims.Groups),
	})
}

// refuseSubjectConflict answers a provider account this company cannot sign
// in as the person the round trip was for: linked to somebody else, the
// invitation already begun with another account, or — at the ordinary
// callback — linked to two people by a restore.
//
// # A 409 that is safe to be specific
//
// It is reached only past an ID token that VERIFIED, so the caller has proved
// they hold this account at the provider, and what it tells them is about
// their own account: somebody here is pinned to it. It never says who. And it
// is not a 503, which it used to be at the ordinary callback: waiting does not
// clear a link somebody has to remove, and a browser told to retry would
// retry a round trip at the provider for ever.
//
// AUDITED AS A FAILED ATTEMPT, because it is the one refusal on this surface
// that is evidence rather than noise — a provider account offered as proof of
// an identity it is not pinned to — and logged at ERROR with the domain's own
// refusal, which names the holder for the operator who has to decide.
func (s *Service) refuseSubjectConflict(w http.ResponseWriter, r *http.Request,
	attempt authevents.Failure, detail string, cause error) {

	log.ErrorContext(r.Context(), "api_oidc_subject_conflict",
		"person", attempt.Person, "error", cause)
	s.audit.Failed(r.Context(), attempt)
	httpjson.FailWith(w, http.StatusConflict, httpjson.CodeSubjectConflict,
		map[string]string{"detail": detail})
}
