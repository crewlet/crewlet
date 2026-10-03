package authapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/authevents"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
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
//
// THE LINK IS AN ID AND A SECRET, AND ONLY THE ID IS EVER IN A PATH.
//
// A link reads `<api.external_url>/dashboard#/invite/<id>.<secret>`: the
// dashboard's invitation screen, with the credential in the FRAGMENT, which a
// browser never sends to any server — so neither half reaches a proxy's access
// log by the person following it. The screen then calls these routes with the
// id in the path and the secret beside it, never in a URL: the view in the
// [inviteSecretHeader] header and the redemption in its JSON body. The id
// alone is in every snapshot, backup
// and access log and opens nothing; what the estate keeps of the secret is its
// verifier ([iamdomain.InvitationRow.Admits]).
//
// A WRONG SECRET IS AN ABSENT INVITATION: one refusal, padded, for every way a
// link fails to open ([Service.refuseInvitation]), and counted as a failed
// attempt exactly as an id nobody issued is — answered differently, a guessed
// secret against a real id would say the id exists, and the id is the half
// that leaks.

// inviteSecretHeader carries an invitation link's secret to the view.
//
// A HEADER because the view is a GET and a GET has no body, and never the
// query string, which every access log between the browser and this node
// records: the secret rode in the URL's fragment precisely so that none of
// them would see it.
const inviteSecretHeader = "X-Crewlet-Invite-Secret"

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

	// Login is the login this form PROPOSES, derived from the address by
	// [iam.LoginFromAddress]: every person enrols with one, and somebody
	// following a link has typed nothing yet. The person keeps it or
	// changes it — the redemption takes whatever they post and nothing
	// is derived silently, because a name recorded beside everything they
	// do is one they saw first. Absent when nothing in the address fits
	// the grammar, and the form then asks for one outright.
	Login string `json:"login,omitempty"`

	// MinPasswordLength is the floor, so a form refuses before it posts.
	MinPasswordLength int `json:"min_password_length"`

	// Seat is the seat redeeming this invitation BINDS the person to, as
	// the company's chart calls it now, or absent for an invitation that
	// binds none. It is part of what the person is agreeing to, so the
	// form shows it before anything is spent.
	Seat *inviteSeat `json:"seat,omitempty"`
}

// inviteSeat is the seat an invitation binds, as its view shows it.
type inviteSeat struct {
	// Handle is the seat the invitation binds. Empty when the company this
	// node runs no longer holds it as a human seat, which the redemption
	// then refuses as a link that no longer works.
	Handle string `json:"handle,omitempty"`

	// Name is the seat's display name, or empty.
	Name string `json:"name,omitempty"`
}

// inviteRedeem is what redeeming presents.
type inviteRedeem struct {
	// Secret is the half of the link after the id — REQUIRED, and in the
	// body rather than the path for [inviteSecretHeader]'s reason.
	Secret string `json:"secret"`

	// Login is REQUIRED: every person enrols with one, in the person
	// grammar (jane.doe). The view proposes one; an absent one is refused
	// 400 by the enrolment, naming the rule.
	Login    string `json:"login"`
	Name     string `json:"name"`
	Password string `json:"password"`
}

// ViewInvite renders an invitation without spending it.
//
// UNGUARDED, because holding the link IS the credential — its secret, in
// [inviteSecretHeader], 256 bits nobody can walk — and on no curve, because a
// link names nobody until it opens: see [Service.uncounted].
func (s *Service) ViewInvite(w http.ResponseWriter, r *http.Request) {
	adm := s.uncounted(r)
	held, ok := s.presentedInvitation(w, r, adm, r.PathValue("id"),
		r.Header.Get(inviteSecretHeader))
	if !ok {
		return
	}
	email, err := s.openSealed(r, held)
	if err != nil {
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(err))
		return
	}
	view := inviteView{
		Email: email, InvitedBy: held.InvitedBy,
		Login:             iam.LoginFromAddress(email),
		MinPasswordLength: s.passwordFloor(),
	}
	if held.Seat != "" {
		view.Seat = s.inviteSeatOf(r.Context(), held.Seat)
	}
	httpjson.Write(w, http.StatusOK, view)
}

// inviteSeatOf is the seat an invitation binds, as the company this node runs
// holds it now: named, when it is still a human seat there, and with no handle
// when it is not — the redemption refuses that link, and the page says so
// before anybody types a password. A lookup that fails shows the stored handle
// alone: the page is a courtesy, and the redemption asks again.
func (s *Service) inviteSeatOf(ctx context.Context, handle string) *inviteSeat {
	if s.seats == nil {
		return &inviteSeat{Handle: handle}
	}
	seat, found, err := s.seats.Seat(ctx, handle)
	switch {
	case err != nil:
		return &inviteSeat{Handle: handle}
	case !found || seat.Kind != session.SeatKindHuman:
		return &inviteSeat{}
	}
	return &inviteSeat{Handle: seat.Handle, Name: seat.Name}
}

// RedeemInvite creates the person an invitation was issued for — and binds the
// seat it names, when it names one, as the first step of the same enrolment.
//
// THE BODY IS READ BEFORE THE INVITATION, because the secret is in it — and
// nothing in it is answered until the invitation has opened: a password too
// short is the invited person's to fix, told to nobody else.
func (s *Service) RedeemInvite(w http.ResponseWriter, r *http.Request) {
	adm := s.uncounted(r)
	body, err := httpjson.ReadBody(w, r, maxLoginBody)
	if err != nil {
		httpjson.Refuse(w, err)
		return
	}
	var in inviteRedeem
	if err = json.Unmarshal(body, &in); err != nil {
		httpjson.Fail(w, http.StatusBadRequest, httpjson.CodeInvalidBody)
		return
	}
	held, ok := s.presentedInvitation(w, r, adm, r.PathValue("id"), in.Secret)
	if !ok {
		return
	}
	if err = credential.CheckStrength(in.Password, s.passwordFloor()); err != nil {
		// SPECIFIC, because holding the link is already evidence this
		// invitation was issued to them and the remedy is theirs.
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
		return
	}
	// THE PERSON IS THE INVITATION'S, DERIVED rather than minted, so every
	// attempt at one redemption names one person. Minted per request, a
	// redemption refused halfway — a login somebody else holds, a record
	// that did not land — left its address claim holding the invitation's
	// address for an id no later attempt named, and the retry was refused
	// as "that address belongs to somebody" by its own first attempt. A
	// redeemer told their login was taken could never choose another.
	person, err := iamdomain.InvitedPersonID(held.ID)
	if err != nil {
		log.ErrorContext(r.Context(), "api_invite_person_underivable",
			"error", err, "invitation", held.ID)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	opID := redemptionOpID(held.ID)
	// AN ADDRESS SOMEBODY IS ENROLLED UNDER IS A LINK ALREADY USED — and
	// with the person derived, it is what keeps the link single-use when
	// the spend did not land: a re-redemption would otherwise re-enrol the
	// same person and reset their password with nothing but the link. A
	// RESERVATION is not somebody: it is this redemption's own stopped
	// attempt when it names this person, which the enrolment below
	// finishes, and a claim the enrolment refuses by name when it does not.
	holder, err := s.directory.PersonByEmailBlind(r.Context(), held.Blind)
	if err != nil {
		log.WarnContext(r.Context(), "api_invite_holder_unreadable",
			"error", err, "invitation", held.ID)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(err))
		return
	}
	if holder.ID != "" && !holder.Reserved {
		if holder.ID == person {
			// THIS LINK ENROLLED THEM AND ITS SPEND NEVER LANDED, so the
			// row still reads redeemable. Close it now — the same op id
			// as the first attempt's, so the ledger collapses the two.
			s.spendInvitation(r, held, person, opID)
		}
		s.refuseInvitation(w, r, adm, held.ID, true)
		return
	}
	email, err := s.openSealed(r, held)
	if err != nil {
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(err))
		return
	}
	verifier, hashed := s.hash(w, r, adm, in.Password, "invite")
	if !hashed {
		return
	}

	enrolled, err := s.writer.Enrol(r.Context(), iamdomain.Enrolment{
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
		//
		// AND THE INVITATION IS NAMED AS THE AUTHORITY, so the domain
		// holds the enrolment to it in the snapshot the grants land
		// from rather than to the node's own writer, which holds none
		// of what an invitation usually confers.
		//
		// THE SECRET GOES WITH IT, checked again where the grants land,
		// and so does the seat the invitation binds: claimed first, and
		// refused as the link's own refusal if it moved since the issue.
		Grants:           held.Grants,
		Colleague:        held.Colleague,
		Invitation:       held.ID,
		InvitationSecret: in.Secret,
		Seat:             held.Seat,
		OpID:             opID, Reason: "redeemed an invitation",
	})
	if err != nil {
		if errors.Is(err, iamdomain.ErrRefused) {
			// SPENT OR AGED OUT between the lookup above and the
			// record, which is the same answer the lookup gives: one
			// refusal for every way a link stops working.
			log.InfoContext(r.Context(), "api_invite_refused_at_the_record",
				"invitation", held.ID, "error", err)
			httpjson.Fail(w, http.StatusGone, httpjson.CodeInviteSpent)
			return
		}
		refuseEnrolment(w, r, "api_invite_enrol_failed", opID, err)
		return
	}
	if !landed(enrolled) {
		// NO SPEND AND NO SESSION ON AN ENROLMENT NOBODY CAN CONFIRM.
		// The op id is the invitation's own, so following the link again
		// is the same enrolment.
		unresolved(w, r, "api_invite_enrol_unresolved", enrolled)
		return
	}
	s.spendInvitation(r, held, person, opID)
	log.InfoContext(r.Context(), "api_invite_redeemed",
		"invitation", held.ID, "person", person, "login", in.Login)
	s.completeSignIn(w, r, iamdomain.Sighting{
		ID: person, Kind: iam.KindPerson, Stage: iam.StageActive,
		Login: in.Login, Grants: held.Grants, Colleague: held.Colleague,
		Seat: held.Seat,
	}, signIn{method: types.SignInInvite})
}

// redemptionOpID is the operation an invitation's redemption is published
// under: DERIVED from the invitation, so every attempt at one redemption — a
// retry after `unknown`, a redeemer told their login was taken choosing
// another — is the same operation, and the steps its first attempt landed are
// answered from the ledger rather than claimed a second time.
//
// AT THE INVITATION'S OWN INSTANT, which its id carries (a uuid7 minted when it
// was issued — [iamdomain.Blinder.InvitationID]), in the grammar the ledger
// vouches for a retry by: it was `invite:<id>`, which carries none and read as
// minted at the epoch, so once this node's ledger had swept anything every
// redemption was answered `unknown` without being published. The issue is at
// most the invitation window ago, well inside the month the ledger keeps an
// identity operation for.
func redemptionOpID(invitation string) string {
	at, _ := statelog.OpMintedAt(invitation)
	return statelog.DeriveOpID(at, "invite-redeem", redemptionNamespace, invitation)
}

// redemptionNamespace keeps [redemptionOpID]'s ids apart from every other
// derivation. FIXED for the life of the format: a new one would make a
// redemption retried across the change a second operation.
const redemptionNamespace = "crewlet.authapi.invite-redeem"

// refuseEnrolment answers an enrolment the domain did not land, and it is the
// one place this surface decides which of those failures are the caller's.
//
// # Why this is not one 503
//
// Both enrolments this surface performs — the first operator's and an
// invitation's — used to answer every failure `503 unavailable`, which is the
// status that says "the engine is having a moment, try again". For a login
// the domain REFUSED that is false twice over: nothing will change on a
// retry, and the person is left resubmitting the same name at a form that
// never says what is wrong with it. So the refusals that are about what was
// TYPED are 400 naming the rule, a name somebody else already holds is 409,
// and only what is left — a record that could not be published or applied —
// is 503.
//
// # What the 409 does and does not say
//
// It says the login or the address is taken, which the person needs in order
// to choose another, and it does NOT say by whom. The domain's own refusal
// names the holder's id, which is right for an administrator and wrong here:
// the caller is holding an invitation link, which is evidence of who THEY are
// and of nothing about anybody else.
func refuseEnrolment(w http.ResponseWriter, r *http.Request, event, opID string,
	err error) {
	var claimed *iamdomain.ErrClaimed
	switch {
	case errors.Is(err, iamdomain.ErrInvalidLogin),
		errors.Is(err, iamdomain.ErrNotFindable),
		errors.Is(err, iamdomain.ErrNotEnrollable),
		errors.Is(err, iamdomain.ErrInvalid):
		log.InfoContext(r.Context(), event, "refused", "invalid", "error", err)
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
	case errors.As(err, &claimed):
		log.InfoContext(r.Context(), event, "refused", "claimed",
			"claim", string(claimed.Kind))
		detail := "that address already belongs to somebody in this company"
		switch claimed.Kind {
		case iamdomain.KindLogin:
			detail = "that login is already taken — choose another"
		case iamdomain.KindSeat:
			// NAMING NEITHER THE SEAT'S HOLDER NOR THE SEAT'S OTHER
			// NAMES, for the paragraph above's reason: what the caller
			// can act on is that the seat is gone, and the remedy is
			// whoever sent them.
			detail = "the seat this invitation binds is already held by " +
				"somebody in this company — ask whoever sent it for a new one"
		}
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeBadParams,
			map[string]string{"detail": detail})
	case removed(err), errors.Is(err, iamdomain.ErrOperationReused):
		// THE PERSON THIS LINK CREATES WAS CREATED AND THEN REMOVED — its
		// spend never landed, so the row still read redeemable, and the
		// person it names is one nothing will ever write again — or the
		// redemption's operation, which the invitation derives, already
		// names another record. Either way the link has stopped working,
		// which is every way a link stops working.
		log.InfoContext(r.Context(), event, "refused", "link used", "error", err)
		httpjson.Fail(w, http.StatusGone, httpjson.CodeInviteSpent)
	default:
		writeFailed(w, r, event, opID, err)
	}
}

// presentedInvitation resolves one invitation from the two halves of its link —
// the id a route's path names and the secret presented beside it — answering
// false once it has written the refusal.
//
// ONE REFUSAL FOR ABSENT, REDEEMED, EXPIRED AND A SECRET THAT IS NOT THE
// LINK'S, because the first three have one remedy — ask for a new one — and
// telling them apart would say "this was already used" to somebody whose link
// merely aged out, sending them to find out who used it; and the fourth told
// apart would say which ids exist to anybody guessing secrets against them.
// Padded to one deadline, and counted only where the link did not prove
// itself: see [Service.refuseInvitation].
func (s *Service) presentedInvitation(w http.ResponseWriter, r *http.Request,
	adm admission, id, secret string) (iamdomain.InvitationRow, bool) {

	held, err := s.directory.InvitationByID(r.Context(), id)
	if err != nil {
		log.WarnContext(r.Context(), "api_invite_lookup_failed", "error", err)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(err))
		return iamdomain.InvitationRow{}, false
	}
	switch {
	case held.ID == "" || !held.Admits(secret):
		s.refuseInvitation(w, r, adm, id, false)
		return iamdomain.InvitationRow{}, false
	case held.Spent(s.now()):
		s.refuseInvitation(w, r, adm, id, true)
		return iamdomain.InvitationRow{}, false
	}
	return held, true
}

// refuseInvitation is every 410 this surface answers: an invitation nobody
// issued, one redeemed, one aged out, and one whose address somebody is
// already enrolled under — and one presented with a secret that is not its
// link's. ONE ANSWER for all of them, in the same bytes at the same deadline,
// for [Service.presentedInvitation]'s reason.
//
// # A failed attempt only where the link did not prove itself
//
// The link is the credential, and walking ids and secrets is how somebody
// without one looks for one — so an id nobody issued and a secret that is not
// its link's are FAILED ATTEMPTS, in the audit trail's failure tally. A link
// that DID prove itself and no longer works — redeemed, aged out, its address
// enrolled — is not a guess: it is the link's holder, or a mail scanner that
// fetched it for them, and those fetch the same link again and again. Counted,
// a scanner re-reading one spent link put the address it scans from in the
// tally as a guesser — and while a link was on its source's curve, it put that
// address on the curve, which on a deployment behind one proxy address was the
// whole company. The difference is invisible to a caller who does not hold the
// link — every one of them is refused the same way — so it tells a guesser
// nothing.
func (s *Service) refuseInvitation(w http.ResponseWriter, r *http.Request,
	adm admission, id string, proved bool) {

	if !proved {
		s.audit.Failed(r.Context(), authevents.Failure{
			Client: adm.source, Method: types.FailInvite,
			// THE ID PRESENTED, keyed in memory and never kept: how many
			// DIFFERENT links one client tried in a minute is the
			// difference between a stale bookmark and a walk.
			Subject: id,
		})
	}
	s.throttle.Pad(r.Context(), adm.at)
	httpjson.Fail(w, http.StatusGone, httpjson.CodeInviteSpent)
}

// spendInvitation records a link being used, naming the person it created.
//
// LOGGED AND NOT REPORTED. The person exists and can sign in; the residue of a
// spend that did not land is an invitation row that still reads redeemable,
// and the next redemption of it finds the address enrolled, answers 410 and
// publishes this again under the same op id — so the row closes then.
func (s *Service) spendInvitation(r *http.Request, held iamdomain.InvitationRow,
	person, opID string) {

	spent, err := s.writer.SpendInvitation(r.Context(), iamdomain.InvitationSpend{
		ID: held.ID, Blind: held.Blind, Person: person,
		OpID: statelog.StepOpID(opID, "spend"), Reason: "redeemed",
	})
	if err != nil || !landed(spent) {
		log.WarnContext(r.Context(), "api_invite_spend_failed",
			"error", errText(err), "op_id", spent.OpID,
			"outcome", string(spent.Outcome), "invitation", held.ID,
			"person", person)
	}
}

// openSealed opens the invitation's address for this one answer.
//
// AS THE INVITATION'S and never a person's, because there is no person yet —
// see [iamdomain.Sealer.SealInvitation].
func (s *Service) openSealed(r *http.Request, held iamdomain.InvitationRow) (string, error) {
	if held.Sealed == "" {
		return "", nil
	}
	email, err := s.sealer.OpenInvitation(held.ID, held.Sealed)
	if err != nil {
		log.WarnContext(r.Context(), "api_invite_unseal_failed",
			"error", err, "invitation", held.ID)
		return "", err
	}
	return email, nil
}
