package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DECIDES: what a caller actually asks this domain to do.
//
// Each one takes ONE snapshot, forms its expectation inside it, and lets the
// broker arbitrate. What they add over the framework is this domain's own
// rules, and every one of those has to happen inside the snapshot because
// every one is a decision about state another writer is changing at the same
// time.

// Unique is one of the three values the directory keeps to one holder: a
// person's address, their login and the seat they are bound to.
//
// ITS OWN NAMED TYPE, and not seal.go's [Field], which names a SEALED value: an
// address is both, and a refusal saying which value somebody else holds is
// about the uniqueness, never the sealing.
type Unique string

// The three unique values.
const (
	UniqueEmail Unique = "email"
	UniqueLogin Unique = "login"
	UniqueSeat  Unique = "seat"
)

// ErrTaken reports an address, a login or a seat somebody else holds.
//
// IT NAMES THE HOLDER, which is the whole difference between a refusal
// somebody can act on and one they can only retry: "that address is taken" is
// a support ticket, "that address belongs to person X" is an answer. A
// surface decides what of it to show — an administrator is told who, and
// somebody holding an invitation link is told only that it is taken.
type ErrTaken struct {
	// Field is which value is taken.
	Field Unique

	// Value is the login or the seat in the clear, or an address's BLIND —
	// never the address, which this package never holds in the clear.
	Value string

	// Person is who holds it, or empty where an open invitation does.
	Person string

	// Invitation is the open invitation holding the value, or empty. An
	// invitation holds an ADDRESS and a SEAT — [Field] says which this is —
	// from its issue until it is redeemed, cancelled or ages out
	// ([heldByInvitation]), so a surface naming the remedy branches on the
	// field: an address is the invitee's, and a seat is the chart's.
	Invitation string
}

func (e *ErrTaken) Error() string {
	if e.Invitation != "" {
		return fmt.Sprintf("iamdomain: the %s %s is held by open invitation %s",
			e.Field, e.Value, e.Invitation)
	}
	return fmt.Sprintf("iamdomain: the %s %s is held by person %s", e.Field,
		e.Value, e.Person)
}

// THE TWO ENROLMENTS: an administrator's create ([Writer.Create]) and an
// invitation's redemption ([Writer.Redeem]).
//
// # One record, decided in one snapshot
//
// Both create somebody WHOLE — their row, their first credential, their
// login, their address and their seat — in ONE record on the directory
// ([OpEnrol]), formed by one decide ([Writer.enrol]) that reads the whole
// directory and refuses an address, a login or a seat somebody else holds
// ([ErrTaken], naming them) — so a refusal publishes nothing at all. There is
// no half-finished enrolment for a retry to meet: a redeemer told their login
// was taken chooses another and redeems again, and an administrator whose
// create was refused for its seat has created nobody.
//
// # Two verbs, because the two callers differ in everything but the record
//
// They were one verb told apart by whether an invitation was named, and by
// now they differ in the seat rule, in what holds a value against them, in
// the credential the record carries and in what the caller is answered: an
// administrator names any HUMAN seat nobody holds and nothing an open
// invitation holds, and is handed the person's first password link; a
// redemption binds exactly the seat its invitation names, carries the password
// the invitee chose, and is answered with the outcome alone. One verb carried
// every one of those as a branch on a field, and a caller of either could fill
// in the other's — a create naming an invitation's secret, a redemption
// carrying a token. Two verbs over one decide keep the record format one
// format and give each caller only what it may say.
//
// # A person holds a human seat for as long as they are here
//
// EVERY PERSON IS ENROLLED ONTO A HUMAN SEAT ([ErrSeatRequired]), and keeps
// one until they are removed: a person's seat is moved, never cleared
// ([Writer.SetIdentity]). A person with no seat acted under their bare login,
// in no unit, led by nobody and reached by no contact route, so every rule the
// org chart decides — who leads them, whose queue their work lands in — had
// nothing to say about them. A SERVICE ACCOUNT's seat stays optional: it acts
// as itself, and binds a seat only to act as one. ADR-0026 is the decision.
//
// # What it may confer, and on whose authority
//
// An enrolment is a grant change from nothing to what it carries, so it is
// held to [Writer.UpdatePerson]'s rule: a caller may not confer a grant they
// do not hold. It used to be held to nothing, so a party holding people:manage
// and no other grant could enrol somebody carrying secrets:read — or enrol a
// colleague holding everything and sign in as them.
//
// A REDEMPTION IS NOT THE WRITER'S TO AUTHORISE, and it names what is: it
// confers what the INVITATION's author conferred when they issued it, and was
// held to their grants then ([Writer.Invite]). The record's decide reads the
// invitation in its own snapshot and refuses anything it does not cover — more
// grants, a different address, another seat, a link already spent or aged
// out — so the node that processes a redemption decides nothing a second
// time. The node's own writer holds fleet:operate and people:manage and
// nothing else, so on its own authority it may confer those two; everything a
// redemption hands out comes from the invitation it names.
//
// THERE IS NO EXEMPTION FOR THE FIRST PERSON. A company with nobody in it
// still holds the Tier A token every serving node requires, and that token is
// a party like any other: its invitation or its create is held to its grants
// exactly as an administrator's is, so the first person is enrolled, and
// bounded, the way everybody after them is.
//
// # A seat is the running company's
//
// The seat is checked against the organisation this node runs before anything
// is published — advisory, see [Writer.humanSeat] — and held free in the
// record's own snapshot.

// Create creates a person or a service account WHOLE, on an administrator's
// authority, in ONE record on the directory — and, for a PERSON, issues their
// FIRST PASSWORD LINK in that same record.
//
// # The seat
//
// A person's create names a HUMAN seat ([Creation.Seat]); a service account's
// may name one. It is refused where somebody holds it or an OPEN INVITATION
// does ([heldByInvitation]), naming which: an invitation is a person on their
// way to that seat, and a create beside it would put two people on one.
//
// # The first password link
//
// An administrator's create IS the enrolment — the person is active from the
// moment it lands — so the one thing they lack is a way to prove themselves.
// The record carries it: a `reset` credential ([MethodReset]) whose id and
// secret are DERIVED from the person under the company's key
// ([Blinder.PasswordLinkID], [Blinder.PasswordLinkSecret]), stamped with the
// counters of the record's own snapshot, and spent through the ordinary reset
// path, which sets the person's first password and signs nobody in. Derived
// rather than minted because the create's own retry — the one an unknown
// answer asks for — is answered from the ledger without running the decide
// again, and only a derivation of the person the key derives hands that retry
// the link its first attempt issued ([Writer.firstLinkAs]). The administrator
// is shown it once, as they would be shown a reset link they issued: they hold
// every grant it opens, since [Writer.MayConfer] held the create to them.
//
// A SERVICE ACCOUNT GETS NONE: it has no password and no login page, and
// proves itself only with a token minted on it ([Writer.MintToken]).
//
// # Its person is derived from its key
//
// [Creation.PersonID] is [CreatedPersonID] of the operation key, so a create
// that reaches somebody who already exists is a second request under one key
// ([ErrOperationReused]) rather than a rewrite of them.
func (w *Writer) Create(ctx context.Context, in Creation) (Created, error) {
	if err := w.mayAdminister(OpEnrol); err != nil {
		return Created{}, err
	}
	if err := in.validate(w.Now()); err != nil {
		return Created{}, err
	}
	// THE WRITER'S OWN AUTHORITY: it reads nothing, so it is asked before
	// anything else is.
	if err := w.MayConfer(nil, in.Grants); err != nil {
		return Created{}, err
	}
	// THE SEAT BEFORE THE BLIND, because the blind is the one step here that
	// can MINT something — the company's key, on the first address a fresh
	// node ever blinds — and a create the running org refuses (a typo, an
	// agent's seat, a node running no company) must refuse having minted
	// nothing. Asked the other way round, a fresh node answered its first
	// create with whatever the mint's guard said rather than the seat's own
	// refusal and its remedy.
	var (
		seat string
		err  error
	)
	if in.Seat != "" {
		if seat, err = w.humanSeat(ctx, in.Seat); err != nil {
			return Created{}, err
		}
	}
	blinder, blind, err := w.enrolmentBlind(ctx, in.Email)
	if err != nil {
		return Created{}, err
	}
	var (
		link        *PasswordLink
		credentials []Credential
	)
	if in.Kind == iam.KindPerson {
		// A PERSON ALWAYS HAS AN ADDRESS ([Creation.validate]), so the
		// blinder that derived its blind derives the link too.
		if link, err = firstPasswordLink(blinder, in.PersonID,
			in.LinkExpiresAt); err != nil {
			return Created{}, err
		}
		credentials = []Credential{{
			V: DocumentVersion, ID: link.Credential, Method: MethodReset,
			Verifier:  credential.ResetVerifier(link.Credential, link.Secret),
			ExpiresAt: link.ExpiresAt,
		}}
	}
	result, err := w.enrol(ctx, enrolment{
		personID: in.PersonID, kind: in.Kind, stage: in.Stage,
		name: in.Name, email: in.Email, blind: blind,
		login: in.Login, seat: seat,
		credentials: credentials, grants: in.Grants,
		opID: in.OpID, reason: in.Reason,
	})
	switch {
	case err != nil || result.Outcome == statelog.OutcomeUnknown || link == nil:
		// NO LINK BESIDE AN OUTCOME NOBODY CAN CONFIRM: it may be a link
		// to a person who does not exist, and the same key's retry derives
		// it again once there is an answer.
		return Created{Result: result}, err
	case result.Collapsed:
		return w.firstLinkAs(ctx, result, in.PersonID, *link)
	}
	return Created{Result: result, Link: link}, nil
}

// firstPasswordLink is the first password link a created person's record
// carries, derived from them under the company's key.
func firstPasswordLink(blinder *Blinder, personID string,
	expires time.Time) (*PasswordLink, error) {

	id, err := blinder.PasswordLinkID(personID)
	if err != nil {
		return nil, err
	}
	secret, err := blinder.PasswordLinkSecret(id)
	if err != nil {
		return nil, err
	}
	return &PasswordLink{Credential: id, Secret: secret, ExpiresAt: expires}, nil
}

// firstLinkAs is the first password link a create issued, read back after the
// framework answered its retry from the ledger ([statelog.Result.Collapsed]):
// the decide never ran for this call, so whether the link it derived still
// opens is a question for the rows, asked with the predicate the spend path
// asks ([ResetRow.Opens]).
//
// # Three answers
//
//   - IT STILL OPENS: the link, with the expiry the record stored — the one
//     its first attempt was answered with, since a retry an hour later is not
//     a later link.
//   - IT NO LONGER DOES — spent, revoked by a grant the person gained since,
//     ended by a counter, aged out, or its person removed: no link, and
//     [Created.LinkClosed] says so, because the remedy is a reset link and
//     answering the derived one would hand out a link that sets nothing.
//   - THIS NODE HAS NOT APPLIED IT: unavailable, and the same key retried once
//     it has is answered in full. Applied here with no row is the removal arm
//     above rather than this one — the create's row existed, and only a
//     removal deletes it.
func (w *Writer) firstLinkAs(ctx context.Context, result statelog.Result,
	personID string, link PasswordLink) (Created, error) {

	var (
		found bool
		row   ResetRow
	)
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		person, err := heldPerson(ctx, tx, personID,
			"the first password link its create issued")
		switch {
		case errors.Is(err, ErrNotFound):
			return nil
		case err != nil:
			return err
		}
		found = true
		counters, err := countersOf(ctx, tx, personID)
		if err != nil {
			return err
		}
		row = ResetOf(person, link.Credential, counters)
		return nil
	})
	switch {
	case err != nil:
		return Created{Result: result}, err
	case !found && result.Outcome != statelog.OutcomeApplied:
		return Created{Result: result}, fmt.Errorf("iamdomain: operation %s "+
			"created person %s and this node has not applied it yet, so it "+
			"cannot say whether their first password link still opens — retry "+
			"the same key: %w", result.OpID, personID, statelog.ErrUnavailable)
	case found && row.Opens(link.Secret, w.Now()):
		link.ExpiresAt = row.ExpiresAt
		return Created{Result: result, Link: &link}, nil
	}
	return Created{Result: result, LinkClosed: true}, nil
}

// Creation is what an administrator's create needs.
type Creation struct {
	// PersonID is [CreatedPersonID] of the create's operation key, minted by
	// the CALLER because the caller holds the key: a create whose outcome
	// nobody could establish is retried under that key, and the answer —
	// the person it created, and a person's first password link — has to
	// name the same person whether the retry is answered from the ledger
	// or decided again. A uuid7, since the first password link is derived
	// at its instant ([Blinder.PasswordLinkID]).
	PersonID string

	// Kind is a person or a machine — a service account.
	Kind iam.Kind

	// Stage is the stage the principal lands at: a PERSON's is
	// [iam.StageActive] and nothing else ([Creation.validate]), since
	// their first password link is how they get in.
	Stage iam.Stage

	// Name and Email are CLEARTEXT here and nowhere after: they are
	// sealed before the record is formed, and no payload carries either.
	// A person's address is REQUIRED — it is the interactive login key — and
	// a service account's optional.
	Name  string
	Email string

	// Login is REQUIRED, in the holder's kind's grammar: a person's is
	// dotted and a machine's coloned. See [Creation.validate] for why a
	// person needs one although their address already finds them.
	Login string

	Grants []iam.Grant

	// Seat is the HUMAN seat the created principal is bound to, by its
	// handle (ADR-0013). REQUIRED for a person ([ErrSeatRequired]), who
	// holds one for as long as they are here; optional for a service
	// account, which binds one only to act as it.
	Seat string

	// LinkExpiresAt is when a person's first password link stops opening:
	// REQUIRED, and ahead of the writer's clock, for a person — an expiry
	// read as `never` is a credential that sets their password for the
	// life of the company — and refused for a service account, which has
	// no password to set. The surface reads it off
	// [credential.EnrolmentLinkLifetime].
	LinkExpiresAt time.Time

	// OpID is the record's operation id, in the state log's grammar — a
	// create's key, a bare uuid7, already is — because the instant it
	// carries is what the ledger vouches for a retry by. See
	// [Redemption.OpID].
	OpID   string
	Reason string
}

// Created is what a create answers.
type Created struct {
	statelog.Result

	// Link is the created PERSON's first password link — on a landing, or
	// on a retry the ledger answered while the link still opens — and nil
	// for a service account, an outcome nobody can confirm, and a link that
	// no longer opens. Shown once, by the caller, and kept nowhere: the
	// estate holds only its verifier.
	Link *PasswordLink

	// LinkClosed says a retry found the person's first password link
	// spent, revoked, ended or aged out ([Writer.firstLinkAs]) — the
	// person exists and the link does not open, so what the caller hands
	// on is a password reset link instead.
	LinkClosed bool
}

// PasswordLink is a created person's first password link: the `reset`
// credential's id and the secret that opens it, which a surface joins as
// `<id>.<secret>` in the reset screen's fragment.
type PasswordLink struct {
	// Credential is the link's credential id ([Blinder.PasswordLinkID]).
	Credential string

	// Secret is the half that is the credential
	// ([Blinder.PasswordLinkSecret]); the estate keeps
	// [credential.ResetVerifier] of it and never it.
	Secret string

	// ExpiresAt is when it stops opening.
	ExpiresAt time.Time
}

// validate refuses a create that could not land, before anything is sealed,
// blinded or read, at the writer's clock now.
//
// THE SEAT AND THE LINK ARE ASKED LAST, after findability and the login
// grammar: those refusals name the field a caller typed wrong, and a create
// missing its seat as well is told the more specific thing first.
func (in Creation) validate(now time.Time) error {
	if err := validateEnrolment(enrolment{
		personID: in.PersonID, kind: in.Kind, stage: in.Stage,
		name: in.Name, email: in.Email, login: in.Login, grants: in.Grants,
		opID: in.OpID, reason: in.Reason,
	}); err != nil {
		return err
	}
	switch {
	case in.Kind == iam.KindPerson && in.Seat == "":
		return fmt.Errorf("%w: creating person %s names no seat — create them "+
			"on the human seat they will hold", ErrSeatRequired, in.Login)
	case in.Kind == iam.KindPerson && in.Stage != iam.StageActive:
		// ACTIVE FROM THE MOMENT IT LANDS, or the first password link is
		// a credential to nothing: a stage that may not act signs nobody
		// in once the password is set, and a stage the link cannot open
		// ([ResetStages]) was answered with a link on the first attempt
		// and with [Created.LinkClosed] on its own retry — two answers to
		// one operation. A different stage is a stage change afterwards.
		return fmt.Errorf("%w: a created person is %s from the moment the "+
			"create lands — their first password link is how they get in, and "+
			"at %q it would set a password nobody can sign in with; create "+
			"them, then change their stage", ErrInvalid, iam.StageActive,
			in.Stage)
	case in.Kind == iam.KindMachine && !in.LinkExpiresAt.IsZero():
		return fmt.Errorf("%w: a service account has no password, so its "+
			"create issues no password link and takes no link expiry",
			ErrInvalid)
	case in.Kind == iam.KindPerson && in.LinkExpiresAt.IsZero():
		return fmt.Errorf("%w: creating a person issues their first password "+
			"link, which needs an expiry — one read as `never` sets their "+
			"password for whoever holds it, for the life of the company",
			ErrInvalid)
	case in.Kind == iam.KindPerson && !in.LinkExpiresAt.After(now):
		return fmt.Errorf("%w: the first password link's expiry %s is "+
			"already past", ErrInvalid,
			in.LinkExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// Redeem creates the person an invitation was issued for, binding the seat it
// names and spending its link, in ONE record on the directory.
//
// # The invitation is the authority
//
// What the record confers — the grants, the seat — is what the invitation
// carries, read and compared in the record's own snapshot ([Writer.redeemable])
// together with the secret its link carries, so a redemption asks for nothing
// that was not offered and the node processing it decides nothing a second
// time. The writer is the node's own, holding none of what an invitation
// usually confers, and the trail announces the grants as the invitation's
// issuer's decision, which they were.
//
// # The seat is the invitation's, and only a person holds it against one
//
// The seat must still be a human seat of the company this node runs — a
// revision may have removed it, or made it an agent's, since the link was
// sent — and that refusal is the LINK's ([ErrRefused]), since its remedy is a
// new invitation. In the snapshot it is refused only where a PERSON holds it:
// the invitation being redeemed holds it already, and an older open
// invitation a restore left on the same seat must not deadlock both.
//
// AN INVITATION ISSUED BEFORE EVERY INVITATION BOUND A SEAT is unredeemable,
// refused as the link's own refusal ([ErrRefused]) rather than as a missing
// field ([ErrSeatRequired]): the invitee did not leave anything out, and the
// remedy is a new invitation onto a seat.
//
// # A link redeemed is a link used
//
// A retry the framework answers from the ledger ([statelog.Result.Collapsed])
// is refused as a link already used rather than answered as landed — see
// [Writer.enrol].
func (w *Writer) Redeem(ctx context.Context, in Redemption) (statelog.Result, error) {
	if err := w.mayAdminister(OpEnrol); err != nil {
		return statelog.Result{}, err
	}
	if err := in.validate(); err != nil {
		return statelog.Result{}, err
	}
	_, blind, err := w.enrolmentBlind(ctx, in.Email)
	if err != nil {
		return statelog.Result{}, err
	}
	seat, err := w.humanSeat(ctx, in.Seat)
	switch {
	case errors.Is(err, ErrNoCompany):
		// THE INVITATION NAMES A SEAT OF A COMPANY THE FLEET RUNS — it
		// was checked against one at the issue — so a node running none
		// is a node that has not applied it yet, and another node can
		// redeem the link. Never the issue's "create a company first",
		// which is advice to an administrator and not to an invitee.
		return statelog.Result{}, fmt.Errorf("%w: invitation %s binds seat "+
			"%q and this node runs no company yet to find it in — another "+
			"node can redeem it (%v)", statelog.ErrUnavailable, in.Invitation,
			in.Seat, err)
	case errors.Is(err, statelog.ErrUnavailable):
		return statelog.Result{}, err
	case err != nil:
		// THE LINK'S REFUSAL AND NOTHING ELSE IN THE CHAIN: the seat check's
		// own answer is an invalid value an administrator typed, and wrapped
		// here it told a surface the invitee had sent a bad request, when
		// what they hold is a link the company's chart has since retired.
		return statelog.Result{}, fmt.Errorf("%w: invitation %s binds a seat "+
			"it can no longer bind (%v)", ErrRefused, in.Invitation, err)
	}
	return w.enrol(ctx, enrolment{
		personID: in.PersonID, kind: iam.KindPerson, stage: in.Stage,
		name: in.Name, email: in.Email, blind: blind,
		login: in.Login, seat: seat,
		credentials: []Credential{in.Password}, grants: in.Grants,
		invitation: in.Invitation, invitationSecret: in.InvitationSecret,
		opID: in.OpID, reason: in.Reason,
	})
}

// Redemption is what redeeming an invitation needs.
type Redemption struct {
	// PersonID is minted per ATTEMPT by the caller, a uuid7: an attempt
	// the directory refused published nothing, so the next one is a new
	// person with nothing in its way, and what keeps the link single-use is
	// the link the record spends.
	PersonID string

	Stage iam.Stage

	// Name and Email are CLEARTEXT here and nowhere after. Email is the
	// address the invitation was issued to, opened from its row — the
	// record refuses any other, since holding somebody's link is not
	// holding their address.
	Name  string
	Email string

	// Login is the one the invitee chose: dotted, a person's grammar.
	Login string

	// Password is the ONE credential a redemption carries: the password
	// the invitee chose, as an argon2id verifier ([MethodPassword]) —
	// never a token, a second factor or a link, which nothing the invitee
	// presented could vouch for.
	Password Credential

	// Grants and Seat are what the invitation carries, and the record
	// refuses anything else ([Writer.redeemable]): more grants, or a seat
	// it did not bind.
	Grants []iam.Grant
	Seat   string

	// Invitation is the id of the invitation this redeems.
	Invitation string

	// InvitationSecret is the secret the invitation's link carries beside
	// its id ([Blinder.InvitationSecret]), REQUIRED: the id is in every
	// snapshot, backup and proxy log, so naming it proves nothing, and the
	// record that lands the person checks the secret against the
	// invitation's verifier in its own snapshot — the same check the route
	// made, asked again where the grants land from, because a record can be
	// published by more than a route.
	InvitationSecret string

	// OpID is the record's operation id.
	//
	// IN THE STATE LOG'S GRAMMAR ([statelog.NewOpID], [statelog.DeriveOpID])
	// because the instant it carries is what the ledger vouches for a retry
	// by: an id outside it is read as minted at the epoch, and once this
	// node's ledger has lost a row of the kind it arbitrates on, every retry
	// under it is answered `unknown` without being published.
	OpID   string
	Reason string
}

// validate refuses a redemption that could not land, before anything is
// sealed, blinded or read.
func (in Redemption) validate() error {
	switch {
	case in.Invitation == "":
		return errors.New("iamdomain: a redemption names the invitation it " +
			"redeems")
	case in.Email == "":
		return fmt.Errorf("%w: redeeming an invitation enrols the address it "+
			"was issued to, and this redemption names none", ErrNotFindable)
	case in.InvitationSecret == "":
		// REFUSED rather than invalid: it is what a link without its
		// secret, or an invitation issued before links carried one,
		// comes to, and its remedy is the one every way a link stops
		// working has — ask for a new one.
		return fmt.Errorf("%w: redeeming invitation %s needs the secret its "+
			"link carries beside the id — the id alone is in every snapshot "+
			"and proxy log, and opens nothing", ErrRefused, in.Invitation)
	case in.Seat == "":
		// THE LINK'S REFUSAL, never [ErrSeatRequired]: an invitation that
		// binds no seat was issued before every person held one, and the
		// invitee left nothing out — what they need is a new invitation.
		return fmt.Errorf("%w: invitation %s binds no seat — it was issued "+
			"before every person held a human seat, so it creates nobody; "+
			"ask whoever sent it for a new one", ErrRefused, in.Invitation)
	case in.Password.Method != MethodPassword || in.Password.ID == "" ||
		in.Password.Verifier == "":
		return fmt.Errorf("%w: a redemption carries the password the invitee "+
			"chose, as a %s credential with an id and a verifier, and nothing "+
			"else", ErrInvalid, MethodPassword)
	}
	return validateEnrolment(enrolment{
		personID: in.PersonID, kind: iam.KindPerson, stage: in.Stage,
		name: in.Name, email: in.Email, login: in.Login, grants: in.Grants,
		opID: in.OpID, reason: in.Reason,
	})
}

// enrolment is one [OpEnrol] record's inputs, whichever verb asked: the one
// record format both publish. invitation is empty for an administrator's
// create — the one field that tells the decide's two authorities apart.
type enrolment struct {
	personID         string
	kind             iam.Kind
	stage            iam.Stage
	name, email      string
	blind            string
	login, seat      string
	credentials      []Credential
	grants           []iam.Grant
	invitation       string
	invitationSecret string
	opID, reason     string
}

// enrolmentBlind is the company's blinder and an address's blind, once this
// writer is shown to hold what every enrolment needs — a keyring to seal the
// person's values with, and the blind-index key wherever there is an address.
// No address is no blinder and no blind.
func (w *Writer) enrolmentBlind(ctx context.Context, email string) (
	*Blinder, string, error) {

	if w.sealer == nil || (email != "" && w.blinds == nil) {
		return nil, "", fmt.Errorf("iamdomain: this node cannot enrol "+
			"anybody: it has %s. An enrolment has to derive the blind the "+
			"directory compares an address by and seal the values that belong "+
			"to the person, and a node that guessed at either would put two "+
			"people on one address", w.missingKeys())
	}
	if email == "" {
		return nil, "", nil
	}
	blinder, err := w.blinds.Blinder(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("iamdomain: this node cannot derive the "+
			"blind an address is compared by: %w", err)
	}
	blind, err := blinder.Email(email)
	if err != nil {
		return nil, "", err
	}
	return blinder, blind, nil
}

// enrol publishes one enrolment: the decide both verbs share, and the one
// place an [OpEnrol] record is formed.
//
// # The record is formed inside the decide
//
// The decide may run more than once against fresh snapshots, so everything a
// snapshot decides is formed in it — and that includes the COUNTERS a
// credential the record carries is stamped with ([stampIssued]). A first
// password link is ended by whatever moves its person's epoch or the
// company's session generation after its issue ([Credential.EndedBy]), so it
// has to carry the values its own snapshot held: formed before the decide, as
// every enrolment used to be, it carried zero for both, and the first
// company-wide invalidation — the last step of every restore — had already
// ended every link any create would issue from then on.
//
// What is NOT formed inside it is anything sealed, which depends on nothing a
// snapshot holds and would draw a fresh nonce on every run.
func (w *Writer) enrol(ctx context.Context, e enrolment) (statelog.Result, error) {
	// SEALED ONCE, before the decide.
	//
	// AN ADDRESS IS OPTIONAL AND A SEALED NAME IS NOT. A machine identity
	// has no mailbox, so there is nothing to blind and nothing to seal —
	// but its NAME is still sealed like anybody else's, because `Release
	// pipeline, raised by Dana` names a person too, and a removal erases it
	// as surely.
	sealedName, err := w.sealer.Seal(e.personID, FieldName, e.name)
	if err != nil {
		return statelog.Result{}, err
	}
	var sealedEmail string
	if e.email != "" {
		if sealedEmail, err = w.sealer.Seal(e.personID, FieldEmail,
			e.email); err != nil {
			return statelog.Result{}, err
		}
	}
	// THE SCOPE: the new person's bucket, every leaver whose tombstone the
	// seat's bind stamps, and — for a redemption — the address's bucket the
	// invitation row it spends is filed under.
	scopeOf := func(ctx context.Context, tx *sql.Tx) (_ ScopeSet, err error) {
		buckets := []Bucket{BucketOf(e.personID)}
		var leavers []string
		if e.seat != "" {
			if leavers, err = leaversOf(ctx, tx, e.seat); err != nil {
				return ScopeSet{}, err
			}
		}
		for _, leaver := range leavers {
			buckets = append(buckets, BucketOf(leaver))
		}
		if e.invitation != "" {
			buckets = append(buckets, BucketOf(e.blind))
		}
		return BucketScope(buckets...), nil
	}
	// WHO DECIDED WHAT IT CONFERS: this writer's party for an
	// administrator's create, and for a redemption whoever issued the
	// invitation ([Writer.redeemable]) — never the person redeeming it, whom
	// a party derived with [Writer.For] names as the record's author.
	decidedBy, decidedVia := w.Actor, w.OperatorID
	decide := func(tx *sql.Tx) (_ []byte, err error) {
		if err = wholeDirectory(ctx, tx); err != nil {
			return nil, err
		}
		if e.invitation != "" {
			// THE INVITATION, READ WHERE THE GRANTS LAND FROM: the one
			// read of it that decides anything. Its issuer acted through
			// a credential when they issued it, which the issue's own
			// record names; the redemption acted through none of theirs.
			decidedVia = ""
			decidedBy, err = w.redeemable(ctx, tx, e)
		} else {
			err = w.createsNobodyTwice(ctx, tx, e)
		}
		if err != nil {
			return nil, err
		}
		now := w.Now()
		if e.blind != "" {
			if err = taken(ctx, tx, UniqueEmail, e.blind, e.personID); err != nil {
				return nil, err
			}
			// AN OPEN INVITATION HOLDS ITS ADDRESS against a create,
			// naming the invitation: two people would otherwise be on
			// their way to one address. A redemption IS that invitation,
			// and [Writer.redeemable] held it to it.
			if e.invitation == "" {
				if err = heldByInvitation(ctx, tx, UniqueEmail, e.blind, now); err != nil {
					return nil, err
				}
			}
		}
		if err = taken(ctx, tx, UniqueLogin, e.login, e.personID); err != nil {
			return nil, err
		}
		if e.seat != "" {
			if err = taken(ctx, tx, UniqueSeat, e.seat, e.personID); err != nil {
				return nil, err
			}
			// AND ITS SEAT, for the address's reason. A redemption is
			// held to persons alone: the invitation being redeemed holds
			// the seat already, and a second open one a build before
			// invitations held their seats left beside it is a residue
			// its own cancellation clears — not a reason to refuse the
			// person the link was sent to, and asked of both it would
			// refuse each link for the other's sake.
			if e.invitation == "" {
				if err = heldByInvitation(ctx, tx, UniqueSeat, e.seat, now); err != nil {
					return nil, err
				}
			}
		}
		counters, err := countersOf(ctx, tx, e.personID)
		if err != nil {
			return nil, err
		}
		return EncodeEnrolled(Enrolled{
			V: DocumentVersion,
			Person: Person{
				V: DocumentVersion, Kind: e.kind, Stage: e.stage,
				NameSealed: sealedName, EmailSealed: sealedEmail,
				Credentials: stampIssued(nil, e.credentials, counters),
				Grants:      e.grants,
			},
			Holds: Identifiers{EmailBlind: e.blind, Login: e.login,
				SeatID: e.seat},
			Invitation: e.invitation,
		})
	}
	result, err := w.publishDirectory(ctx, OpEnrol, e.personID, e.opID,
		e.reason, scopeOf, decide)
	if err == nil && result.Collapsed && e.invitation != "" {
		// A REDEMPTION THAT LANDED AS A COPY THIS CALL CANNOT PROVE IS ITS
		// OWN IS A LINK ALREADY USED: the framework answers a retry from the
		// ledger before the decide runs, so the refusal the decide would
		// make of a spent link is made here instead. Answered as landed, the
		// redemption goes on to open a session — for whoever holds the link
		// and the first password, past any second factor enrolled since. It
		// is refused even where the copy happens to be this call's own
		// ambiguous append, since nothing can tell the two apart — and there
		// the refusal's own words are true: the person the link created is
		// enrolled, and signs in with that password.
		return statelog.Result{}, e.alreadyEnrolled()
	}
	// AN ENROLMENT THAT CONFERS ANYTHING IS A GRANT CHANGE — from nothing
	// to what it carries — and the first person a company enrols, under the
	// deployment's own token, is the one row of those an audit most needs to
	// find: by whoever decided it.
	if added, _ := grantDelta(nil, e.grants); len(added) > 0 {
		w.announce(ctx, result, err, types.IAMGrantsChanged{
			Person: e.personID, Added: added, By: decidedBy,
			OperatorID: decidedVia, Version: result.Position.Packed(),
		})
	}
	return result, err
}

// createsNobodyTwice refuses an administrator's create of a person who
// already exists, read inside the record's own snapshot.
//
// # An enrolment creates; it never rewrites
//
// The create's person is DERIVED from its operation key ([CreatedPersonID]),
// so the one that reaches an existing person is a second request under one
// key, and landing it would rewrite their grants, their name and their
// credentials with another request's: it is [ErrOperationReused].
//
// # The retry of the create that made them never gets here
//
// The framework answers an operation its ledger already holds BEFORE it runs a
// decide ([statelog.Snap.Held]), collapsed into the first copy — so a decide
// that runs has, by construction, an operation id this node's ledger does not
// hold, and a person already enrolled under it is somebody ANOTHER operation
// made. Where the ledger has LOST the row — its sweep, an adoption — the
// framework turns this refusal of an operation minted before the loss into
// `unknown` ([statelog.Result.Unvouched]) rather than a refusal of the
// caller's own first attempt.
//
// A REDEMPTION NEEDS NO SUCH GUARD: its person is minted afresh per attempt,
// and what keeps a link single-use is the link ([Writer.redeemable]).
func (w *Writer) createsNobodyTwice(ctx context.Context, tx *sql.Tx,
	e enrolment) error {

	enrolled, err := isEnrolled(ctx, tx, e.personID)
	if err != nil || !enrolled {
		return err
	}
	return e.alreadyEnrolled()
}

// alreadyEnrolled is the refusal an enrolment meets when what it would create
// already exists, in the terms of the authority it named.
//
//   - A REDEMPTION's link has been used: the person it created is enrolled.
//   - AN ADMINISTRATOR'S key already names somebody, and a new person is a new
//     operation under a new key.
func (e *enrolment) alreadyEnrolled() error {
	if e.invitation != "" {
		return fmt.Errorf("%w: invitation %s has already been used — the "+
			"person it created is enrolled", ErrRefused, e.invitation)
	}
	return fmt.Errorf("%w: person %s already exists, and operation %s is not "+
		"the one that created them — a new person is a new operation under a "+
		"new key", ErrOperationReused, e.personID, e.opID)
}

// isEnrolled reports whether a person row exists, read inside a decide's
// snapshot.
func isEnrolled(ctx context.Context, tx *sql.Tx, personID string) (bool, error) {
	var enrolled bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM iam_people WHERE id = ?)`, personID).
		Scan(&enrolled); err != nil {
		return false, fmt.Errorf("iamdomain: read whether %s is enrolled: %w",
			personID, err)
	}
	return enrolled, nil
}

// validateEnrolment is the half of a create's and a redemption's validation
// that is the same rule for both: an id and an operation to publish under, a
// kind the directory holds, a stage, the bounds a record is held to, a way to
// find whoever it creates, and a login in their kind's grammar.
//
// [Writer.record] still checks the reason on every record; this is the check
// that names the field an enrolment is refused for.
func validateEnrolment(e enrolment) error {
	switch {
	case e.personID == "":
		return errors.New("iamdomain: an enrolment needs the person id it is " +
			"creating")
	case e.opID == "":
		return errors.New("iamdomain: an enrolment needs an operation id — " +
			"without a stable one a retry cannot tell whether it already landed")
	case e.kind != iam.KindPerson && e.kind != iam.KindMachine:
		// THE DIRECTORY HOLDS PEOPLE AND MACHINES, and nothing else
		// enrols. A seat is the chart's and the engine is the node, so
		// a directory row of either kind is a second answer to who they
		// are — and neither has a login grammar, so the row would be
		// findable by an address alone and act as nothing the rest of
		// the engine can name.
		return fmt.Errorf("%w: %q is not a kind the directory enrols — want "+
			"%s or %s", ErrNotEnrollable, e.kind, iam.KindPerson, iam.KindMachine)
	case !e.stage.Valid():
		return fmt.Errorf("%w: %q is not an enrolment stage", ErrInvalid, e.stage)
	case len(e.name) > MaxName:
		return fmt.Errorf("%w: the name is %d bytes and the cap is %d",
			ErrInvalid, len(e.name), MaxName)
	case len(e.email) > MaxAddress:
		return fmt.Errorf("%w: the address is %d bytes and the cap is %d, the "+
			"longest RFC 5321 permits", ErrInvalid, len(e.email), MaxAddress)
	case len(e.reason) > MaxReason:
		return fmt.Errorf("%w: the reason on this enrolment is %d bytes and "+
			"the cap is %d — it is rendered into an authentication trail "+
			"beside the op that caused it, so it says WHICH cause fired "+
			"rather than narrating", ErrInvalid, len(e.reason), MaxReason)
	case e.kind == iam.KindPerson && iam.NormalizeEmail(e.email) == "":
		// JUDGED ON WHAT IT FOLDS TO, which is what finds them: an
		// address of nothing but whitespace is no address.
		return fmt.Errorf("%w: enrolling a person needs an address — it is "+
			"the interactive login key, and somebody with none can never "+
			"sign in", ErrNotFindable)
	case e.kind == iam.KindMachine && e.login == "":
		// A MACHINE NEEDS NO ADDRESS and must still be FINDABLE. It has
		// no login page and no mailbox — `svc:ci` proves itself with a
		// token — so requiring an address would have meant inventing
		// one, and a row that is not named is a credential holder
		// nobody can list, revoke or audit.
		return fmt.Errorf("%w: enrolling a %s needs a login — it has no login "+
			"page and therefore no address, so the login is the only thing "+
			"that finds it", ErrNotFindable, e.kind)
	case e.login == "":
		// A PERSON NEEDS ONE TOO, and not to be found: their address
		// already does that. A login is the NAME they sign in and are
		// shown under — on every row of the directory and beside every
		// change the trail records of their sign-ins and their
		// credentials — and a person with none was recorded as
		// `anonymous` beside every change they made, and failed
		// [iam.Principal.Validate] on every request they sent. So every
		// path that creates a person names one: an administrator types
		// it or takes the one proposed from the address, and an
		// invitation's form proposes one for the person to keep or
		// change.
		return fmt.Errorf("%w: enrolling a person needs a login — lowercase "+
			"segments joined by DOTS (jane.doe). It is the name they sign in "+
			"and are recorded under, and without one they would be recorded "+
			"as nobody", ErrInvalidLogin)
	}
	if err := machineHolds(e.kind, e.grants); err != nil {
		return err
	}
	return loginFits(e.kind, e.login)
}

// machineHolds refuses a MACHINE a grant it could never exercise: one a
// token never carries ([iam.PersonPresentGrants]). A machine has no password
// and no session — it acts only through tokens minted on it, which drop those
// grants on every request, and a Tier A token's row is no account at all — so
// a service account created holding `people:manage` listed a grant nothing it
// can present would ever carry.
func machineHolds[G ~string](kind iam.Kind, grants []G) error {
	if kind != iam.KindMachine {
		return nil
	}
	for _, g := range grants {
		if slices.Contains(iam.PersonPresentGrants, iam.Grant(g)) {
			return fmt.Errorf("%w: a machine acts only through tokens, and a "+
				"token never carries %s — it needs a person present", ErrInvalid, g)
		}
	}
	return nil
}

// redeemable refuses an enrolment its invitation does not cover, read inside
// the record's own snapshot.
//
// EVERY CLAUSE IS A WAY THE REDEMPTION COULD OTHERWISE ASK FOR MORE THAN WAS
// OFFERED: a grant the invitation did not carry, an address it was not issued
// to (holding somebody's link is not holding their address), a seat it did not
// bind, and a link that is spent or aged out.
//
// THE WRITER'S CLOCK decides the expiry, as [heldByInvitation]'s does: the
// surface already refused an aged-out link against the same clock, and what
// this buys is that the check and the grants it bounds are one snapshot. AN
// INVITATION WITH NO EXPIRY IS AGED OUT, never open for ever: the writer
// refuses to issue one, so a zero here is a record a writer never formed, and
// read as `never` it was the one link in the company nothing could retire —
// while the address hold and the listing already read it as closed, so three
// readers disagreed about one row. The retention sweep collects it with the
// invitations that aged out.
//
// IT ANSWERS WHO ISSUED IT, because that is who decided what the redemption
// confers: the person redeeming chose none of it.
func (w *Writer) redeemable(ctx context.Context, tx *sql.Tx,
	e enrolment) (string, error) {

	var (
		held           string
		expires, spent int64
		document       []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT email_blind, expires_at, redeemed_at, document
		  FROM iam_invites WHERE id = ?`, e.invitation).
		Scan(&held, &expires, &spent, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", fmt.Errorf("%w: invitation %s is not held on this node, so "+
			"nothing says what redeeming it confers", ErrRefused, e.invitation)
	case err != nil:
		return "", fmt.Errorf("iamdomain: read invitation %s: %w", e.invitation, err)
	}
	invitation, err := DecodeInvitation(document)
	if err != nil {
		return "", fmt.Errorf("iamdomain: open invitation %s: %w", e.invitation, err)
	}
	switch {
	case !invitationAdmits(invitation.Verifier, e.invitationSecret):
		// THE SECRET, ASKED WHERE THE GRANTS LAND FROM. The route asked
		// it too, but a record can be published by more than a route, and
		// naming an invitation's id — which every snapshot and proxy log
		// holds — is not holding its link. An invitation issued before
		// links carried a secret has no verifier and admits nobody.
		return "", fmt.Errorf("%w: the secret presented is not the one invitation "+
			"%s's link carries", ErrRefused, e.invitation)
	case held != e.blind:
		return "", fmt.Errorf("%w: invitation %s was issued to another address — "+
			"holding somebody's link is not holding their address",
			ErrRefused, e.invitation)
	case spent != 0:
		return "", fmt.Errorf("%w: invitation %s has already been used",
			ErrRefused, e.invitation)
	case expires == 0 || !w.Now().Before(time.UnixMilli(expires)):
		return "", fmt.Errorf("%w: invitation %s has aged out", ErrRefused,
			e.invitation)
	}
	for _, g := range e.grants {
		if !g.Valid() || !slices.Contains(invitation.Grants, g) {
			return "", fmt.Errorf("%w: redeeming invitation %s confers %v, and "+
				"%s is not among them — a redemption hands out what was "+
				"offered and nothing more", ErrRefused, e.invitation,
				invitation.Grants, g)
		}
	}
	// THE SEAT IS THE INVITATION'S, and exactly it: one the invitation
	// did not carry would be a redemption asking for more than was
	// offered, and one it carried and the enrolment dropped would spend
	// the link without the binding its issuer decided on. An invitation
	// that carries none was issued before every person held a seat, and
	// [Redemption.validate] refused it before anything was read.
	if e.seat != invitation.Seat {
		return "", fmt.Errorf("%w: invitation %s binds seat %q, and the "+
			"redemption names %q — a redemption binds what was offered and "+
			"nothing else", ErrRefused, e.invitation, invitation.Seat, e.seat)
	}
	return invitation.InvitedBy, nil
}

// loginFits refuses a login that is not in its holder's kind's grammar.
//
// THE GRAMMAR IS THE HOLDER'S, never "either one": [iam.ValidLoginFor] argues
// the case, and it is the Tier A token namespace. A person who could take
// `token:ops` would make the deployment's `ops` credential act as their seat,
// because the directory row under a token's login is what binds it.
//
// Checked on the value AS TYPED, so `Sarah.Chen` is refused naming itself
// instead of being quietly lowered into a login its holder never wrote.
func loginFits(kind iam.Kind, login string) error {
	if iam.ValidLoginFor(kind, login) {
		return nil
	}
	shape := "a person's login is lowercase segments joined by DOTS (jane.doe)"
	if kind == iam.KindMachine {
		shape = "a machine's login is lowercase segments joined by a COLON " +
			"(ci:release, or token:<id> for a Tier A token)"
	}
	return fmt.Errorf("%w: %q is not a login a %s may hold — %s, at most %d "+
		"characters. Each kind has its own separator, which keeps it apart "+
		"from a seat handle and from the other kind's names, and the bound is "+
		"a seat handle's, since the two share every author column",
		ErrInvalidLogin, login, kind, shape, iam.MaxLogin)
}

// ErrInvalidLogin reports a login outside its holder's kind's grammar.
//
// ITS OWN SENTINEL because it is the caller's to fix and a surface answers it
// 400 — it is a value somebody typed, and reporting it as a failure of the
// engine would send them looking for an outage.
var ErrInvalidLogin = errors.New("iamdomain: that login does not fit its holder's kind")

// ErrInvalid reports a value outside a bound this domain holds a record to: a
// reason past [MaxReason], a stage that is not one.
//
// ITS OWN SENTINEL for [ErrInvalidLogin]'s reason: it is a value the caller
// supplied and can correct, so a surface answers it 400 naming the field — and
// before it existed these fell through to a 500, which tells somebody who
// wrote a long sentence that the engine is broken.
var ErrInvalid = errors.New("iamdomain: a value is outside the bounds this estate holds it to")

// ErrSeatRequired reports a write that would leave a PERSON without a human
// seat: a create naming none, an invitation naming none, or an identity
// change clearing a person's.
//
// IT WRAPS [ErrInvalid], so every surface that already answers an invalid
// value 400 still does — and is its own sentinel so a surface can name the
// field and the remedy (`seat_required`): a person holds a human seat for as
// long as they are here, which is moved to another seat or freed by removing
// them, never cleared. ADR-0026 is the decision.
var ErrSeatRequired = fmt.Errorf("%w: a person holds a human seat for as long "+
	"as they are here — name one", ErrInvalid)

// ErrNoCompany reports a seat that cannot be checked because this node runs
// no company yet — so it holds no human seat for anybody to be bound to.
//
// NOT [statelog.ErrUnavailable], which says "come back": a fresh install that
// has not created its company answers the same on every node and after every
// wait, and a 503 telling an operator to retry sent them round that loop for
// ever. What clears it is a company that declares a human seat, which is the
// remedy a surface names (409 `no_active_revision`). It is the domain's
// reading of [session.ErrNoCompany], the seat seam's own answer.
var ErrNoCompany = errors.New("iamdomain: this node runs no company yet, so it " +
	"has no human seat to bind anybody to")

// ErrNotEnrollable reports an enrolment of a kind the directory does not hold.
var ErrNotEnrollable = errors.New("iamdomain: the directory enrols people and machines only")

// ErrNotFindable reports an enrolment that would create somebody nothing can
// look up.
//
// ITS OWN SENTINEL because the two arms have different fixes and a caller
// renders them differently: a person needs an address (the interactive login
// key) and a machine needs a login (it has no login page, so there is nothing
// else to find it by). What they share is the consequence — a credential
// holder nobody can list, revoke or audit — which is why one sentinel covers
// both.
var ErrNotFindable = errors.New("iamdomain: nothing could find this identity")

// SetIdentity changes an enrolled person's LOGIN, their SEAT binding, or both,
// in ONE record on the directory.
//
// # Post-state, so the old value is freed by the record that takes the new
//
// The record states the login and the seat the person holds from now on —
// the one the caller left alone as the snapshot holds it — so a rename, a
// bind, a move between seats and a service account's unbind are each one
// record, and the value given up is free the moment the record lands. A move
// used to be two records, the new value claimed and the old released, and a
// release that did not land was a gap in a trail no caller could close.
//
// # A person's seat is moved, never cleared
//
// A PERSON holds a human seat for as long as they are here ([ErrSeatRequired],
// ADR-0026), so an edit clearing a person's seat is refused — decided on the
// KIND THE SNAPSHOT HOLDS, never on one the caller states, and before the
// no-op arm, so the rule does not depend on what the person holds now. Their
// seat is freed by MOVING them to another, or by removing them. A SERVICE
// ACCOUNT may be unbound: it acts as itself, and holds a seat only to act as
// one. A person recorded before every person held a seat may still be renamed
// with their binding left alone; binding them is a move like any other.
//
// # Decided in the record's own snapshot
//
// A login outside its holder's kind's grammar ([loginFits]) — the kind READ
// HERE, never taken from the caller — a login somebody else holds, and a seat
// somebody else is bound to or an OPEN INVITATION holds ([heldByInvitation])
// are refused, naming who holds it ([ErrTaken]), and a refusal publishes
// nothing: nothing has moved. A change that changes nothing is applied with no
// record.
//
// EVERY SEAT IT BINDS IS A HUMAN SEAT OF THE ORGANISATION THIS NODE RUNS,
// checked before anything is published: see [Writer.humanSeat].
func (w *Writer) SetIdentity(ctx context.Context, in IdentityEdit) (
	statelog.Result, error) {

	if err := w.mayAdminister(OpIdentity); err != nil {
		return statelog.Result{}, err
	}
	switch {
	case in.PersonID == "" || in.OpID == "":
		return statelog.Result{}, errors.New("iamdomain: an identity change " +
			"needs the person and an operation id")
	case in.Login != nil && *in.Login == "":
		// A LOGIN IS NEVER CLEARED, only changed: every principal holds
		// one — it is the name a person signs in and is listed under, and
		// the name a service account's changes, or those of a person
		// recorded before every person held a seat, are recorded under —
		// so a principal with none would be recorded as nobody, and a
		// machine enrolled under `token:<id>` silently unbound from its
		// Tier A token.
		return statelog.Result{}, fmt.Errorf("%w: a login is never cleared, "+
			"only changed — every principal holds one, and it is the name "+
			"their changes are recorded under", ErrInvalidLogin)
	}
	// THE SEAT IS ASKED OF THE RUNNING ORGANISATION BEFORE THE SNAPSHOT,
	// which is in memory rather than in the replicated estate, and recorded
	// by the handle it answers to.
	var seat *string
	if in.Seat != nil {
		handle := ""
		if *in.Seat != "" {
			var err error
			if handle, err = w.humanSeat(ctx, *in.Seat); err != nil {
				return statelog.Result{}, err
			}
		}
		seat = &handle
	}
	// THE SCOPE: the person's bucket, and the leavers of a seat the record
	// binds — a bind stamps the tombstone of every removal that released
	// the seat ([reboundRemovals]), and a tombstone is filed under the
	// person who left.
	scopeOf := func(ctx context.Context, tx *sql.Tx) (ScopeSet, error) {
		people := []string{in.PersonID}
		if seat != nil && *seat != "" {
			leavers, err := leaversOf(ctx, tx, *seat)
			if err != nil {
				return ScopeSet{}, err
			}
			people = append(people, leavers...)
		}
		return PeopleScope(people...), nil
	}
	decide := func(tx *sql.Tx) ([]byte, error) {
		if err := wholeDirectory(ctx, tx); err != nil {
			return nil, err
		}
		kind, held, err := identityOf(ctx, tx, in.PersonID)
		if err != nil {
			return nil, err
		}
		if seat != nil && *seat == "" && kind == iam.KindPerson {
			// BEFORE THE NO-OP ARM, so the answer is the rule and not
			// whatever the person happens to hold: a legacy seatless
			// person asked to be unbound is told the same thing as
			// everybody else.
			return nil, fmt.Errorf("%w: %s is a person, and a person's seat "+
				"is moved to another human seat or freed by removing them — "+
				"never cleared", ErrSeatRequired, in.PersonID)
		}
		next := IdentityChange{V: DocumentVersion, Login: held.Login,
			SeatID: held.SeatID}
		if in.Login != nil {
			next.Login = *in.Login
		}
		if seat != nil {
			next.SeatID = *seat
		}
		if next.Login == held.Login && next.SeatID == held.SeatID {
			// NOTHING TO MOVE, and it is the answer rather than a
			// refusal: the edit asked for what the person already holds,
			// by a name it answers to.
			return nil, errNothingToPublish
		}
		if next.Login != held.Login {
			if err := loginFits(kind, next.Login); err != nil {
				return nil, err
			}
			if err := taken(ctx, tx, UniqueLogin, next.Login, in.PersonID); err != nil {
				return nil, err
			}
		}
		if next.SeatID != held.SeatID && next.SeatID != "" {
			if err := taken(ctx, tx, UniqueSeat, next.SeatID, in.PersonID); err != nil {
				return nil, err
			}
			// AN OPEN INVITATION HOLDS ITS SEAT as it holds its address:
			// it is a person on their way to that seat, and binding
			// somebody else there would refuse their redemption — or,
			// had the redemption not asked, put two people on it.
			if err := heldByInvitation(ctx, tx, UniqueSeat, next.SeatID,
				w.Now()); err != nil {
				return nil, err
			}
		}
		return EncodeIdentity(next)
	}
	return w.publishDirectory(ctx, OpIdentity, in.PersonID, in.OpID, in.Reason,
		scopeOf, decide)
}

// IdentityEdit is what changing somebody's login or seat needs.
type IdentityEdit struct {
	PersonID string

	// Login is the new login, or nil to leave it. "" is refused: a login
	// is never cleared, only changed.
	Login *string

	// Seat is the HUMAN seat to bind them to, by its handle (ADR-0013), or
	// nil to leave the binding as it is. "" unbinds a SERVICE ACCOUNT, and is
	// refused for a person ([ErrSeatRequired]), whose seat is moved to
	// another or freed by removing them.
	Seat *string

	OpID   string
	Reason string
}

// Revoke bumps a person's revocation epoch, ending every session they hold.
//
// NO GRANT IS REQUIRED and that is deliberate: signing out everywhere is
// something a person does to themselves, at the moment they fear somebody
// else has their cookie. Gating it behind an administrative capability would
// make the fastest response to a compromise the one that needs an
// administrator.
//
// THE NEW EPOCH IS READ INSIDE THE SNAPSHOT AND STATED ON THE RECORD, never
// incremented by the applier: an applier that did `epoch + 1` would fold over
// an arrival order, and two nodes at one checkpoint have seen the same set in
// a different order.
func (w *Writer) Revoke(ctx context.Context, personID, opID, reason string) (
	statelog.Result, error) {

	if personID == "" || opID == "" {
		return statelog.Result{}, errors.New("iamdomain: a revocation needs " +
			"a person and an operation id")
	}
	// THE RECORD IS FORMED INSIDE THE DECIDE, and that is what the
	// framework's rounds mean: a decide may run AGAIN against a fresh
	// snapshot, so the mutation the encode reads has to be the one the last
	// run produced. [Writer.request] takes the record by pointer for
	// exactly this.
	rec, err := w.record(PersonSubject(personID), OpRevoke, personID,
		PeopleScope(personID), nil, reason)
	if err != nil {
		return statelog.Result{}, err
	}
	decide := func(tx *sql.Tx) (err error) {
		current, err := epochOf(ctx, tx, personID)
		if err != nil {
			return err
		}
		rec.Mutation, err = EncodeRevocation(Revocation{
			V: DocumentVersion, Epoch: current + 1,
		})
		return err
	}
	result, err := w.publish(ctx,
		w.request(ctx, &rec, opID, statelog.PatternArbitrated, decide))
	return result, err
}

// InvalidateAll ends every session in the company at once, and every machine
// token.
//
// AN ADMINISTRATOR'S GESTURE, unlike [Writer.Revoke], and the asymmetry is the
// blast radius: revoking is something a person does to themselves at the
// moment they fear somebody else has their cookie, so gating it would make the
// fastest response to a compromise the one that needs an administrator.
// Ending EVERYBODY's sessions is a company-wide act with no self-service
// reading at all.
//
// BOTH HATS: [AdminGrant], because it ends every person's authority at once,
// AND fleet:operate, because a restore is run by whoever runs the deployment.
// It asked the first alone while the route admitted on the second alone, so
// the two disagreed about who may make it and an operator holding the route's
// grant was refused here; internal/authz's row now asks both as well, and a
// machine token can carry neither half that matters — people:manage is never
// minted onto one.
//
// WHAT IT IS FOR, stated where somebody will reach for it: the last step of a
// restore. A restore rolls this estate back to an artefact's own instant, so a
// revocation somebody performed after the copy was taken is rolled back with
// it and the session they revoked comes back. Bumping the fleet-wide
// generation is the only move that ends those sessions without knowing which
// they were — every cookie in existence is below the new value. A machine
// token is minted at the generation for the same reason ([Writer.MintToken]):
// one revoked after the copy comes back unrevoked, and nothing can say which.
//
// THE NEW GENERATION IS READ INSIDE THE SNAPSHOT AND STATED ON THE RECORD, for
// [Writer.Revoke]'s reason: an applier that incremented would fold over an
// arrival order, and two nodes at one checkpoint have seen the same set in a
// different order.
func (w *Writer) InvalidateAll(ctx context.Context, opID, reason string) (
	statelog.Result, error) {

	if err := w.mayAdminister(OpInvalidate); err != nil {
		return statelog.Result{}, err
	}
	if err := w.mayOperate(OpInvalidate); err != nil {
		return statelog.Result{}, err
	}
	if opID == "" {
		return statelog.Result{}, errors.New("iamdomain: invalidating every " +
			"session needs an operation id — without one a retry of the " +
			"gesture bumps the generation twice, which ends the sessions " +
			"opened between the two")
	}
	rec, err := w.record(InvalidationSubject(), OpInvalidate, "", RootScope(),
		nil, reason)
	if err != nil {
		return statelog.Result{}, err
	}
	var generation uint64
	decide := func(tx *sql.Tx) (err error) {
		// NEVER BUMPED IS ZERO, so the first bump lands at 1 and ends
		// exactly the cookies that predate it.
		current, err := generationOf(ctx, tx)
		if err != nil {
			return err
		}
		generation = current + 1
		rec.Mutation, err = EncodeInvalidation(Invalidation{
			V: DocumentVersion, Generation: generation,
			By: w.Actor,
		})
		return err
	}
	result, err := w.publish(ctx,
		w.request(ctx, &rec, opID, statelog.PatternArbitrated, decide))
	w.announce(ctx, result, err, types.IAMSessionGenerationBumped{
		Generation: generation, By: w.Actor, OperatorID: w.OperatorID,
		Reason: reason,
	})
	return result, err
}

// SetStage moves a person between enrolment stages.
func (w *Writer) SetStage(ctx context.Context, personID string, stage iam.Stage,
	opID, reason string) (statelog.Result, error) {

	if err := w.mayAdminister(OpStatus); err != nil {
		return statelog.Result{}, err
	}
	if !stage.Valid() {
		return statelog.Result{}, fmt.Errorf("%w: %q is not an enrolment "+
			"stage — only `active` may act, which is an allowlist of one, so a "+
			"stage this build cannot name would suspend somebody by accident",
			ErrInvalid, stage)
	}
	rec, err := w.record(PersonSubject(personID), OpStatus, personID,
		PeopleScope(personID), nil, reason)
	if err != nil {
		return statelog.Result{}, err
	}
	decide := func(tx *sql.Tx) (err error) {
		change := StatusChange{V: DocumentVersion, Stage: stage}
		if !stage.MayAct() {
			// A STAGE THAT MAY NOT ACT ENDS WHAT THEY HOLD, at the next
			// epoch — see [StatusChange.Epoch].
			var current uint64
			if current, err = epochOf(ctx, tx, personID); err != nil {
				return err
			}
			change.Epoch = current + 1
		}
		rec.Mutation, err = EncodeStatus(change)
		return err
	}
	result, err := w.publish(ctx,
		w.request(ctx, &rec, opID, statelog.PatternArbitrated, decide))
	return result, err
}

// Remove is the one operation here with no inverse, and it rides the
// DIRECTORY subject: it frees a login, an address and a seat.
//
// IT READS WHAT IT RELEASES INSIDE THE SNAPSHOT, because the record has to
// carry it: the tombstone outlives the person's row, and an operator asking
// "who held this address" after the fact has only that row to read. It carries
// the BLINDS rather than the addresses, for the reason the row's own comment
// gives.
//
// IT TAKES NOTHING, so it does not ask [wholeDirectory]: a record this node
// retained about the same person is refused by the framework's own probe over
// the removal's buckets, and a removal of somebody already removed is refused
// before it is published ([removedPersonIn]).
func (w *Writer) Remove(ctx context.Context, personID, opID, reason string) (
	statelog.Result, error) {

	if err := w.mayAdminister(OpRemove); err != nil {
		return statelog.Result{}, err
	}
	if personID == "" || opID == "" {
		return statelog.Result{}, errors.New("iamdomain: a removal needs a " +
			"person and an operation id")
	}
	// THE SCOPE: the person's bucket and their address's, whose rows their
	// erasure clears ([eraseSealed]) — an invitation and the trail row its
	// issue wrote are filed under the address, not under a person.
	scopeOf := func(ctx context.Context, tx *sql.Tx) (ScopeSet, error) {
		held, err := heldIdentifiers(ctx, tx, personID)
		if err != nil {
			return ScopeSet{}, err
		}
		buckets := []Bucket{BucketOf(personID)}
		if held.EmailBlind != "" {
			buckets = append(buckets, BucketOf(held.EmailBlind))
		}
		return BucketScope(buckets...), nil
	}
	decide := func(tx *sql.Tx) ([]byte, error) {
		// NO ROW IS NOT A REASON TO PUBLISH NOTHING. A person this snapshot
		// does not hold is removed carrying an EMPTY released set
		// ([heldIdentifiers]), which is the truth — a snapshot decided at
		// the directory's anchor holds every enrolment there is, so there
		// is nothing to give back — and the tombstone still holds the id
		// against every later record that names it.
		held, err := heldIdentifiers(ctx, tx, personID)
		if err != nil {
			return nil, err
		}
		return EncodeRemoval(Removal{V: DocumentVersion, Released: held})
	}
	return w.publishDirectory(ctx, OpRemove, personID, opID, reason, scopeOf,
		decide)
}

// heldIdentifiers reads the unique values a person holds in one snapshot —
// nothing, for a person this snapshot does not hold.
func heldIdentifiers(ctx context.Context, tx *sql.Tx, personID string) (
	Identifiers, error) {

	var held Identifiers
	err := tx.QueryRowContext(ctx, `
		SELECT email_blind, login, seat_id FROM iam_people WHERE id = ?`,
		personID).Scan(&held.EmailBlind, &held.Login, &held.SeatID)
	if errors.Is(err, sql.ErrNoRows) {
		return Identifiers{}, nil
	}
	if err != nil {
		return Identifiers{}, fmt.Errorf("iamdomain: read what person %s "+
			"holds: %w", personID, err)
	}
	return held, nil
}

// OpenSession records one session beginning, and answers the two counters the
// bearer minted for it has to carry.
//
// NO GRANT, for Revoke's reason turned round: signing IN is what a person does
// before they hold anything, and a capability check here would be asking
// somebody to prove authority they acquire by signing in.
//
// # The epoch and the generation are READ HERE, never stated by the caller
//
// A bearer is over the moment the person's revocation epoch or the fleet's
// session generation moves past the value it carries, so a session has to be
// OPENED AT the current ones. They used to be a caller's field that no caller
// filled in, so every bearer carried zero for both: the first "sign out
// everywhere" left that person unable to sign in again — each new session
// validated as already ended — and the first `invalidate-all` did the same to
// the whole company, for good. Read in the snapshot the record is formed in,
// they are the values this node holds at the instant the session began, which
// is the write authority's own rule: take ONE snapshot, decide inside it, never
// guess. A caller stating them would be stating a read from another
// transaction.
func (w *Writer) OpenSession(ctx context.Context, in SessionStart) (
	SessionOpened, error) {

	if in.Lineage == "" || in.Person == "" || in.OpID == "" {
		return SessionOpened{}, errors.New("iamdomain: opening a session " +
			"needs a lineage, a person and an operation id")
	}
	rec, err := w.record(SessionSubject(in.Lineage), OpOpen, in.Person,
		PeopleScope(in.Person), nil, "")
	if err != nil {
		return SessionOpened{}, err
	}
	// THE LAST RUN'S COUNTERS, which is what the landed record carries: a
	// decide may run again against a fresh snapshot.
	var opened SessionOpened
	decide := func(tx *sql.Tx) (err error) {
		epoch, err := epochOf(ctx, tx, in.Person)
		if err != nil {
			return err
		}
		generation, err := generationOf(ctx, tx)
		if err != nil {
			return err
		}
		opened = SessionOpened{Epoch: epoch, Generation: generation}
		rec.Mutation, err = EncodeSession(Session{
			V: DocumentVersion, Person: in.Person, Epoch: epoch,
			AbsoluteExpiresAt: in.AbsoluteExpiresAt,
			ProvedAt:          in.ProvedAt,
			EnrolmentOnly:     in.EnrolmentOnly,
		})
		return err
	}
	req := w.request(ctx, &rec, in.OpID, statelog.PatternCreate, decide)
	// THE ONE WRITE IN THIS ESTATE THAT DOES NOT WAIT FOR ITS OWN ROW, and
	// only because nothing in the answer reads it. See
	// [SessionStart.NoWait].
	req.NoWait = in.NoWait
	result, err := w.publish(ctx, req)
	if err == nil && result.Collapsed {
		// THE COUNTERS ARE THE DECIDE'S, and the framework answered this
		// call with a copy of the operation it cannot prove is this call's
		// — so the epoch and the generation above may describe a decision
		// nothing published, or none at all. The generation is on no record or row to read back, and
		// a bearer carrying one the session was not opened at is one a
		// fleet-wide invalidation between the two does not end. See
		// [ErrCollapsed].
		return SessionOpened{Result: result}, ErrCollapsed
	}
	opened.Result = result
	return opened, err
}

// SessionOpened is what a bearer for a session that has just begun carries
// beside its lineage.
type SessionOpened struct {
	// Result is the write's own answer, all three outcomes of it. Its
	// POSITION is where the start record landed, which the bearer carries
	// so a node below it can tell "not seen yet" from "ended" — and an
	// UNKNOWN outcome has none, so no bearer may be minted from it: one
	// carrying position zero is a session every node that has applied
	// anything reads as ended.
	Result statelog.Result

	// Epoch is the person's revocation epoch and Generation the fleet's
	// session generation, both as the snapshot the record was formed in
	// held them. A bearer carrying anything below either is over.
	Epoch      uint64
	Generation uint64
}

// epochOf is one person's current revocation epoch, read inside a decide.
//
// NO ROW IS ZERO, which is a real value rather than a missing one: nobody has
// revoked anything for this person, and it is the value [Writer.Revoke]
// increments from.
func epochOf(ctx context.Context, tx *sql.Tx, personID string) (uint64, error) {
	var epoch int64
	err := tx.QueryRowContext(ctx,
		`SELECT epoch FROM iam_revocation_epochs WHERE person_id = ?`,
		personID).Scan(&epoch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("iamdomain: read person %s's epoch: %w",
			personID, err)
	}
	return uint64(epoch), nil
}

// generationOf is the fleet's session generation, read inside a decide.
//
// NEVER BUMPED IS ZERO, which is what a bearer minted before anybody ever
// invalidated anything carries.
func generationOf(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var generation int64
	err := tx.QueryRowContext(ctx,
		`SELECT generation FROM iam_session_generation WHERE singleton = 0`).
		Scan(&generation)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("iamdomain: read the session generation: %w", err)
	}
	return uint64(generation), nil
}

// countersOf is a person's [Counters], read inside a decide.
func countersOf(ctx context.Context, tx *sql.Tx, personID string) (Counters, error) {
	epoch, err := epochOf(ctx, tx, personID)
	if err != nil {
		return Counters{}, err
	}
	generation, err := generationOf(ctx, tx)
	if err != nil {
		return Counters{}, err
	}
	return Counters{Epoch: epoch, Generation: generation}, nil
}

// SessionStart is what opening a session needs.
//
// NO EPOCH AND NO GENERATION: both are read inside the decide — see
// [Writer.OpenSession] for what stating them here cost.
type SessionStart struct {
	Lineage           string
	Person            string
	AbsoluteExpiresAt time.Time
	OpID              string

	// ProvedAt is when the holder proved who they are to open it, or zero
	// for a session opened on no proof of a person — see
	// [Session.ProvedAt]. It is the WRITER's clock, authored at the proof,
	// like the absolute deadline beside it.
	ProvedAt time.Time

	// EnrolmentOnly opens a session that may do nothing but enrol a second
	// factor — see [Session.EnrolmentOnly]. The SIGN-IN decides it, from
	// what it proved and the deployment's own `api.auth.totp`.
	EnrolmentOnly bool

	// NoWait asks for the answer the broker's acknowledgement already
	// establishes, rather than waiting for this node's applier.
	//
	// A SIGN-IN IS THE ONE PLACE IN THIS ESTATE IT IS CORRECT — never a
	// session that replaces another, which is opened from a page that
	// lists them — and the reason is that
	// the bearer minted from this record carries its POSITION: every node
	// validates against its own applier, and a node below that position
	// serves reads on the signature and the epoch alone
	// ([session.RowBehind]). So the row this node would wait for is a row
	// nothing in the answer reads, and what the wait costs is 250-500 ms
	// of parked browser fetch against a 50 ms credential verify — on the
	// one request a person judges the whole product by.
	//
	// IT CANNOT BE SET ANYWHERE ELSE HERE. A directory write answers 200
	// only once this node's rows carry it, because the caller's next read
	// goes to this node and would not see what it just wrote.
	NoWait bool
}

// CloseSession ends one session, keeping its row until the sweep collects it.
//
// # The caller names the session's PERSON
//
// Validation asks whether a deferral covers the PERSON's bucket, so a close has
// to be filed there. It used to be filed under
// the bucket of the lineage — a hash no read consults — so a node that could
// not decode a sign-out went on serving the session it ended, reporting
// itself current. Both callers know the person: a sign-out reads it off a
// bearer whose signature verified, and ending a named session has just read
// the owner. The decide confirms it against this node's row when there is one;
// when there is none — a session opened a moment ago that this node has not
// applied — the caller's word is all there is, and it is a verified one.
func (w *Writer) CloseSession(ctx context.Context, lineage, person, reason,
	opID string) (statelog.Result, error) {

	if lineage == "" || person == "" || opID == "" {
		return statelog.Result{}, errors.New("iamdomain: closing a session " +
			"needs a lineage, the person it belongs to and an operation id")
	}
	mutation, err := EncodeSession(Session{V: DocumentVersion, EndedReason: reason})
	if err != nil {
		return statelog.Result{}, err
	}
	rec, err := w.record(SessionSubject(lineage), OpClose, person,
		PeopleScope(person), mutation, reason)
	if err != nil {
		return statelog.Result{}, err
	}
	decide := func(tx *sql.Tx) (err error) {
		var owner string
		err = tx.QueryRowContext(ctx,
			`SELECT person_id FROM iam_sessions WHERE lineage = ?`, lineage).
			Scan(&owner)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("iamdomain: read session %s's owner: %w",
				lineage, err)
		case owner != person:
			return fmt.Errorf("%w: session %s belongs to person %s, not %s",
				ErrRefused, lineage, owner, person)
		}
		return nil
	}
	result, err := w.publish(ctx, w.request(ctx, &rec, opID, statelog.PatternArbitrated, decide))
	return result, err
}

// missingKeys names which of the two key-bearing seams this writer lacks, for
// a refusal somebody can act on.
func (w *Writer) missingKeys() string {
	switch {
	case w.blinds == nil && w.sealer == nil:
		return "neither a blind-index key nor a keyring to seal with"
	case w.blinds == nil:
		return "no blind-index key"
	default:
		return "no keyring to seal with"
	}
}

// taken refuses a value somebody other than except holds, read INSIDE a
// directory decide's snapshot, naming who holds it.
//
// THE COLUMN IS CHOSEN HERE, by a switch over the three values, never from a
// caller's string: a column name built at a call site is a string this package
// would be interpolating into SQL from somewhere it cannot see.
func taken(ctx context.Context, tx *sql.Tx, field Unique, value,
	except string) error {

	var column string
	switch field {
	case UniqueEmail:
		column = "email_blind"
	case UniqueLogin:
		column = "login"
	case UniqueSeat:
		column = "seat_id"
	default:
		return fmt.Errorf("iamdomain: %q is not a value the directory keeps "+
			"to one holder", field)
	}
	var holder string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM iam_people WHERE `+column+` = ? AND id <> ? LIMIT 1`,
		value, except).Scan(&holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read who holds the %s %s: %w", field,
			value, err)
	}
	return &ErrTaken{Field: field, Value: value, Person: holder}
}

// identityOf is an enrolled person's kind and the unique values they hold,
// read inside a decide's snapshot, or [ErrNotFound].
func identityOf(ctx context.Context, tx *sql.Tx, personID string) (
	iam.Kind, Identifiers, error) {

	var (
		kind string
		held Identifiers
	)
	err := tx.QueryRowContext(ctx, `
		SELECT kind, email_blind, login, seat_id FROM iam_people WHERE id = ?`,
		personID).Scan(&kind, &held.EmailBlind, &held.Login, &held.SeatID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", Identifiers{}, fmt.Errorf("%w: this node holds no person "+
			"%s", ErrNotFound, personID)
	case err != nil:
		return "", Identifiers{}, fmt.Errorf("iamdomain: read person %s: %w",
			personID, err)
	}
	return iam.Kind(kind), held, nil
}

// humanSeat is THE check every binding takes — a person's create, an
// invitation, its redemption, a bind, and a service account bound to act as a
// seat — answering the handle the running organisation knows the seat by.
//
// # Every binding names a HUMAN seat
//
// An agent seat has an inbox and a turn loop and no person to hold it: a
// principal bound to one would be a human writing under an agent's identity
// in every audit row in the company, and the request path refuses it on every
// request ([session.ResolveSeat], 403 naming the seat). A service account's
// binding is held to the same rule because it is the same binding — a Tier A
// token bound through its `token:<id>` row acts as the seat on every request
// exactly as a person does. The rule used to be asked of an invitation and its
// redemption alone, so an administrator's create and a bind could put somebody
// on an agent's seat the request path then refused for ever.
//
// # It is ADVISORY, and its doc is the whole of what it promises
//
// The organisation is the configuration this node applied, not this log, and
// nothing orders the two — so this read and a revision removing the seat are
// concurrent by construction, and a bind that passed here can still land after
// the seat is gone. The residue is a legal named state the session layer
// answers with a 403 NAMING THE SEAT, and the dangling-binding report names it
// too. What it buys is that binding somebody to a seat handle nobody has ever
// declared — a typo, the overwhelmingly common failure — or to an agent's seat
// is refused at the moment somebody can still fix it, rather than becoming a
// person who cannot act and a 403 nobody can explain.
//
// # A node that cannot say REFUSES, in one of two ways
//
// A node running NO COMPANY is [ErrNoCompany]: it holds no human seat at all,
// and no wait changes that — a company declaring one does. A writer handed no
// organisation, or a lookup that failed, is the unknown arm,
// [statelog.ErrUnavailable], a 503 a retry clears. Neither is a bind made
// unchecked.
func (w *Writer) humanSeat(ctx context.Context, handle string) (string, error) {
	if w.seats == nil {
		return "", fmt.Errorf("%w: iamdomain: this writer has no organisation "+
			"to check seat %q against", statelog.ErrUnavailable, handle)
	}
	seat, found, err := w.seats.Seat(ctx, handle)
	switch {
	case errors.Is(err, session.ErrNoCompany):
		return "", fmt.Errorf("%w: seat %q cannot be bound until the company "+
			"declaring it is running here (%w)", ErrNoCompany, handle, err)
	case err != nil:
		return "", fmt.Errorf("%w: iamdomain: read the organisation this node "+
			"runs to check seat %q: %w", statelog.ErrUnavailable, handle, err)
	case !found:
		// ErrInvalid, because it is a value the caller typed: before this
		// was classified it fell through to a 500, telling an
		// administrator who mistyped a handle that the engine was broken.
		return "", fmt.Errorf("%w: the company this node runs has no seat %q. "+
			"This check is ADVISORY — it reads the configuration this node "+
			"applied — so it is here to catch a typo; if a revision added the "+
			"seat moments ago, retry", ErrInvalid, handle)
	case seat.Kind != session.SeatKindHuman:
		return "", fmt.Errorf("%w: seat %q is a %s seat, and every binding — a "+
			"person's, or a service account's or a Tier A token's machine row — "+
			"names a HUMAN seat: an agent seat has no person to hold it, and "+
			"whoever is bound to one is refused on every request", ErrInvalid,
			handle, seat.Kind)
	}
	return seat.Handle, nil
}

// heldPerson is one person's document as a read-modify-write forms the next
// one from, read inside the caller's snapshot — carrying the stage the ROW
// holds.
//
// # Why the stage is taken from the column
//
// The status op moves a stage without authoring a document, so the column is
// the authority and the document is the copy. A decide that re-published the
// document's own stage would put back whatever it said when it was last
// authored — which after a suspension written by an applier that moved the
// column alone is `active`, so an administrator correcting a suspended
// person's grants would silently let them back in. Taking the column is what
// makes the copy unable to outvote the fact.
//
// forming names what the caller is building, for the refusal.
func heldPerson(ctx context.Context, tx *sql.Tx, personID, forming string) (
	Person, error) {

	var (
		document []byte
		stage    string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT document, stage FROM iam_people WHERE id = ?`, personID).
		Scan(&document, &stage)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Person{}, fmt.Errorf("%w: person %q is not enrolled on this "+
			"node, so %s cannot be formed here", ErrNotFound, personID, forming)
	case err != nil:
		return Person{}, fmt.Errorf("iamdomain: read person %q: %w", personID, err)
	}
	person, err := DecodePerson(document)
	if err != nil {
		return Person{}, fmt.Errorf("iamdomain: open person %q: %w", personID, err)
	}
	person.Stage = iam.Stage(stage)
	return person, nil
}

// SetCredentials replaces a person's credential set, reading their own row
// inside the snapshot to form the new whole.
//
// # FULL POST-STATE, read inside the decide
//
// A person's document is authored whole, so a caller adding a second factor
// cannot form the new value without the current one — and it must not read it
// in another transaction. The write authority's own sentence is the reason:
// take ONE snapshot, decide and form the expectation inside it, never guess.
// A set assembled from a read taken earlier pairs an old decision with a new
// expectation, and the broker accepts it.
//
// # No grant, and the caller's own rule instead
//
// Changing your own second factor is something a person does for themselves,
// so a capability check here would refuse the ordinary case. What bounds it is
// the SURFACE: the route is guarded, step-up applies, and the person id comes
// off the resolved principal rather than out of a body.
func (w *Writer) SetCredentials(ctx context.Context, in CredentialSet) (
	statelog.Result, error) {

	switch {
	case in.PersonID == "" || in.OpID == "":
		return statelog.Result{}, errors.New("iamdomain: setting credentials " +
			"needs a person and an operation id")
	case in.Apply == nil:
		return statelog.Result{}, errors.New("iamdomain: setting credentials " +
			"needs the function that forms the new set inside the snapshot — " +
			"a caller holding the current set read it in another transaction, " +
			"which is the pairing the write authority forbids")
	}
	var mutation []byte
	decide := func(tx *sql.Tx) error {
		person, err := heldPerson(ctx, tx, in.PersonID, "their credential set")
		if err != nil {
			return err
		}
		if person.Credentials, err = in.Apply(person.Credentials); err != nil {
			return err
		}
		counters, err := countersOf(ctx, tx, in.PersonID)
		if err != nil {
			return err
		}
		if person.Credentials, err = fitHeld(person.Credentials, w.Now(),
			counters, ErrInvalid); err != nil {
			return err
		}
		mutation, err = EncodePerson(person)
		return err
	}
	rec, err := w.record(PersonSubject(in.PersonID), OpUpdate, in.PersonID,
		PeopleScope(in.PersonID), nil, in.Reason)
	if err != nil {
		return statelog.Result{}, err
	}
	req := w.request(ctx, &rec, in.OpID, statelog.PatternArbitrated, func(tx *sql.Tx) (err error) {
		if err = decide(tx); err != nil {
			return err
		}
		rec.Mutation = mutation
		return nil
	})
	result, err := w.publish(ctx, req)
	return result, err
}

// CredentialSet is what changing somebody's credentials needs.
type CredentialSet struct {
	PersonID string

	// Apply forms the new set from the current one, INSIDE the snapshot.
	//
	// A FUNCTION RATHER THAN A LIST, because the caller does not hold the
	// current set and must not read it separately: it is handed what the
	// decide read and returns what should replace it, which is the only
	// shape in which the expectation and the decision come from one
	// snapshot.
	//
	// IT MAY REFUSE, as [PersonUpdate.Apply] may, for a rule this package
	// cannot judge but that is about the set the write LANDS on — an
	// enrolment-only session enrolling over a second factor its person
	// has come to hold since it opened is one. The refusal travels out of
	// the decide unwrapped and nothing is published: decided from a read
	// the caller made first, it would be decided on a set that may have
	// moved.
	//
	// WHAT IT FORMED IS NOT WHAT LANDED ON A COLLAPSED RESULT
	// ([statelog.Result.Collapsed]): the framework answered the call with
	// a copy of the operation it cannot prove is this call's, so this
	// function may never have run, or run in a round nothing published.
	// This package cannot tell which of a caller's values that matters
	// for — only the caller knows it minted recovery codes or a seed to
	// show — so it answers the result whole, and a caller that shows what
	// it formed here shows it only on a result that is not collapsed, and
	// otherwise starts again under a fresh operation id.
	Apply func([]Credential) ([]Credential, error)

	OpID   string
	Reason string
}

// MintToken adds a machine token to one person's or machine's credentials —
// a personal access token for somebody's own assistant, or a service
// account's token — reading the OWNER inside the snapshot the token is formed
// in.
//
// # Why it is its own gesture rather than a CredentialSet
//
// What a token may carry is a fact about its owner NOW: a subset of the grants
// they hold, and the revocation epoch they are at. A caller reading those first and handing in a finished credential would
// be stating a read from another transaction — the pairing the write authority
// forbids — so a demotion landing between the two would mint a token carrying
// what its owner no longer holds. Minted on the owner's own subject, the mint
// and a demotion contend and exactly one of them decides first.
//
// # What it refuses, and whose rule each is
//
//   - A grant the owner does not hold: a token NARROWS its owner and never
//     widens them.
//   - secrets:read and people:manage, WHATEVER the owner holds: both are
//     gestures that need a person present — revealing a credential and
//     changing who may — and a token is what an attacker holding a pipeline's
//     environment already has.
//   - A grant the MINTING PARTY does not hold, for [Writer.MayConfer]'s rule:
//     whoever mints a token sees its value once, so minting one for somebody
//     else is holding their grants oneself.
//   - A PERSON's token minted by anybody but that person, and a service
//     account's minted without `people:manage` — [Writer.mayMintFor]: the
//     token acts as its owner, so minting one on a person's account is
//     holding a credential that acts as them.
//   - An owner who may not act, a kind that is neither a person nor a
//     machine, and a machine row that binds a Tier A token to a seat — that
//     row IS the deployment's credential's place in the directory, not an
//     account, and a token minted on it would act under the config file's
//     name.
//   - An expiry that is missing, past, or more than [credential.MaxTokenLifetime]
//     away: "forever" is unexpressible.
//
// # It is minted at the owner's epoch AND the company's generation
//
// Both are read in the snapshot and stated on the credential, and a token is
// refused once either moves past it — the same two counters that end a
// session. The epoch is the owner signed out everywhere; the generation is the
// restore runbook's invalidate-all, the one gesture that ends a credential a
// restore brought back unrevoked without knowing which one it was.
//
// NIL GRANTS MEAN "AS MUCH AS MAY BE CARRIED": every grant the owner holds
// that a token may carry — resolved here, in the snapshot, and stated on the
// record, so what the token carries is never re-derived by a node applying
// it.
func (w *Writer) MintToken(ctx context.Context, in TokenMint) (TokenMinted, error) {
	switch {
	case in.PersonID == "" || in.ID == "" || in.OpID == "":
		return TokenMinted{}, errors.New("iamdomain: minting a token needs " +
			"its owner, its own id and an operation id")
	case in.Verifier == "":
		return TokenMinted{}, errors.New("iamdomain: minting a token needs " +
			"the verifier its secret is checked against — the secret itself " +
			"is never published")
	case len(in.Label) > MaxTokenLabel:
		return TokenMinted{}, fmt.Errorf("%w: a token's label is %d bytes and "+
			"the cap is %d — it names which of somebody's tokens to revoke, "+
			"and a listing renders it on every row", ErrInvalidToken,
			len(in.Label), MaxTokenLabel)
	case in.ExpiresAt.IsZero():
		return TokenMinted{}, fmt.Errorf("%w: a token needs an expiry; one "+
			"read as `never` is a credential that outlives whoever minted it",
			ErrInvalidToken)
	}
	now := w.Now()
	switch {
	case !in.ExpiresAt.After(now):
		return TokenMinted{}, fmt.Errorf("%w: the expiry %s is already past",
			ErrInvalidToken, in.ExpiresAt.UTC().Format(time.RFC3339))
	case in.ExpiresAt.Sub(now) > credential.MaxTokenLifetime:
		return TokenMinted{}, fmt.Errorf("%w: a token may last at most %d "+
			"days; `forever` is deliberately unexpressible, because a bearer "+
			"secret nothing re-proves outlives whoever minted it",
			ErrInvalidToken, int(credential.MaxTokenLifetime/(24*time.Hour)))
	}

	rec, err := w.record(PersonSubject(in.PersonID), OpUpdate, in.PersonID,
		PeopleScope(in.PersonID), nil, in.Reason)
	if err != nil {
		return TokenMinted{}, err
	}
	// THE LAST RUN'S ANSWER, which is what the landed record carries.
	var minted TokenMinted
	decide := func(tx *sql.Tx) (err error) {
		owner, err := heldPerson(ctx, tx, in.PersonID, "a token for them")
		if err != nil {
			return err
		}
		if err = tokenOwnable(in.PersonID, owner); err != nil {
			return err
		}
		login, err := loginOf(ctx, tx, in.PersonID)
		if err != nil {
			return err
		}
		if owner.Kind == iam.KindMachine &&
			strings.HasPrefix(login, iam.TokenLoginPrefix) {
			return fmt.Errorf("%w: %s is the directory's row for a Tier A "+
				"token, which binds that token to a seat; it is not an "+
				"account, and a token minted on it would act under the "+
				"configuration file's name", ErrRefused, login)
		}
		if err = w.mayMintFor(in, owner); err != nil {
			return err
		}
		grants, err := w.tokenGrants(in.Grants, owner.Grants)
		if err != nil {
			return err
		}
		counters, err := countersOf(ctx, tx, in.PersonID)
		if err != nil {
			return err
		}
		epoch, generation := counters.Epoch, counters.Generation
		token := Credential{
			V: DocumentVersion, ID: in.ID, Method: MethodToken,
			Verifier: in.Verifier, Label: in.Label, ExpiresAt: in.ExpiresAt,
			Grants: grants, Epoch: epoch, Generation: generation,
		}
		kept := make([]Credential, 0, len(owner.Credentials)+1)
		for _, c := range owner.Credentials {
			if c.ID == in.ID {
				// THE SAME MINT, RETRIED after an answer that did not
				// arrive: the operation ledger is what collapses it,
				// and a second copy of one id on the row would be two
				// credentials a revocation names as one.
				continue
			}
			kept = append(kept, c)
		}
		kept = append(kept, token)
		// THE NEW TOKEN IS LIVE, so making room never drops it: only a
		// credential that has already stopped verifying gives up its place.
		if owner.Credentials, err = fitHeld(kept, now, counters, ErrInvalidToken); err != nil {
			return err
		}
		minted = TokenMinted{Grants: grants, ExpiresAt: in.ExpiresAt,
			Epoch: epoch, Generation: generation}
		rec.Mutation, err = EncodePerson(owner)
		return err
	}
	result, err := w.publish(ctx,
		w.request(ctx, &rec, in.OpID, statelog.PatternArbitrated, decide))
	if err == nil && result.Collapsed {
		// WHAT THE TOKEN CARRIES IS THE DECIDE'S — cut to what its owner
		// held in THAT snapshot — and the framework answered this call with
		// a copy of the operation it cannot prove is this call's, so the
		// grants above may describe a decision nothing published. The copy that landed is a token
		// nobody was shown, and it expires. See [ErrCollapsed].
		return TokenMinted{Result: result}, ErrCollapsed
	}
	minted.Result = result
	return minted, err
}

// TokenMint is what minting a machine token needs.
type TokenMint struct {
	// PersonID is the owner: the person or machine the token acts as.
	//
	// WHO IS MINTING IS NOT HERE, and was: a field the caller filled in is
	// a field the caller could fill in with anybody. It is the WRITER's
	// party — [Writer.Principal] — which [Writer.As] derives from the
	// principal itself. See [Writer.mayMintFor].
	PersonID string

	// ID is the credential's own id, minted by the caller because it is
	// inside the verifier and the value — both formed before this runs.
	ID string

	// Verifier is [credential.TokenVerifier] over the id and the secret.
	// NEVER the secret: this log is replicated, snapshotted and backed up.
	Verifier string

	Label string

	// Grants is what the token may carry, or nil for every grant the owner
	// holds that a token may carry. See [Writer.MintToken].
	Grants []iam.Grant

	// ExpiresAt is REQUIRED, and at most [credential.MaxTokenLifetime]
	// away.
	ExpiresAt time.Time

	OpID   string
	Reason string
}

// TokenMinted is what a mint landed carrying, and where.
type TokenMinted struct {
	// Result is the write's own answer. Its POSITION is where the record
	// landed, which the token's value carries so a node below it answers
	// "not yet" rather than "no such token" — and an UNKNOWN outcome has
	// none, so no value may be formed from it.
	Result statelog.Result

	Grants     []iam.Grant
	ExpiresAt  time.Time
	Epoch      uint64
	Generation uint64
}

// MaxTokenLabel is how long a token's label may be, in bytes.
//
// ONE HUNDRED AND TWENTY-EIGHT: a label says which of somebody's tokens is
// which — "release pipeline", "Claude on my laptop" — and is rendered on every
// row of a credential listing, so a paragraph there is a listing nobody can
// read. It is well above any name a person gives a token and well below what
// would make the row the size of the document it sits in.
const MaxTokenLabel = 128

// ErrInvalidToken reports a mint whose request is malformed — a value the
// caller typed, answered 400 rather than as an authority refusal.
var ErrInvalidToken = errors.New("iamdomain: that token cannot be minted as asked")

// tokenGrants resolves what a token carries and refuses what it may not.
func (w *Writer) tokenGrants(asked, owner []iam.Grant) ([]iam.Grant, error) {
	grants := asked
	if grants == nil {
		grants = make([]iam.Grant, 0, len(owner))
		for _, g := range owner {
			// NEVER THE TWO THAT NEED A PERSON PRESENT: see
			// [iam.PersonPresentGrants]. And `config:write` IS minted
			// when the owner holds it, deliberately — it is host
			// access, and applying a configuration from a pipeline is
			// what a token is for; that list says why it is not there.
			if g.Valid() && !slices.Contains(iam.PersonPresentGrants, g) {
				grants = append(grants, g)
			}
		}
	}
	for _, g := range grants {
		switch {
		case slices.Contains(iam.PersonPresentGrants, g):
			return nil, fmt.Errorf("%w: %s cannot be minted onto a token — it "+
				"is a gesture that needs a person present, and a token is what "+
				"an attacker holding a pipeline's environment already has",
				ErrRefused, g)
		case !g.Valid() || !slices.Contains(owner, g):
			return nil, fmt.Errorf("%w: a token carries a subset of what its "+
				"owner carries, and %s is not among %v", ErrRefused, g, owner)
		}
	}
	if err := w.MayConfer(nil, grants); err != nil {
		return nil, err
	}
	return slices.Clone(grants), nil
}

// tokenOwnable refuses an owner no token may act as.
func tokenOwnable(personID string, owner Person) error {
	switch {
	case owner.Kind != iam.KindPerson && owner.Kind != iam.KindMachine:
		return fmt.Errorf("%w: %s is not an enrolled person or machine, so "+
			"there is nobody for a token to act as", ErrRefused, personID)
	case !owner.Stage.MayAct():
		return fmt.Errorf("%w: %s is %q, and a token acts only as somebody "+
			"who may act", ErrRefused, personID, owner.Stage)
	}
	return nil
}

// mayMintFor refuses a mint the minting party may not make on this owner's
// account, decided on the owner as the snapshot holds them.
//
// A PERSON'S TOKEN IS MINTED BY THAT PERSON ALONE. Whoever mints a token is
// shown its value once, and the token acts as its owner — so one minted on
// somebody else's account is a credential that acts as them in the hands of
// somebody who is not them. `people:manage` was enough to do it, which let any
// administrator act as anybody, leaving no trail but the mint. The grant
// covers the one kind of account that has nobody behind it to mint for itself.
// Compared by ID and never by name, because a login is something a rename
// moves between people.
//
// A SERVICE ACCOUNT'S IS MINTED BY WHOEVER HOLDS `people:manage`: it has no
// login page, so nobody could ever mint for it as itself.
//
// WHO IS MINTING IS THE WRITER'S PARTY ([Writer.Principal]), never a field of
// the call: a rule decided on a value the caller states is exactly as strong
// as the one route that states it correctly, and a record can be published by
// a CLI, a duty or a migration none of which passes through that route.
//
// AND NOTHING IS MINTED THROUGH A MACHINE TOKEN, whoever it acts as: a token
// minted from a token renews for ever, a year at a time, with nobody present —
// the route refuses such a request, and the record refuses it again for the
// route's reason, since a party is composed as the token's OWNER and would
// otherwise pass the person arm below as that owner.
func (w *Writer) mayMintFor(in TokenMint, owner Person) error {
	if iam.ValidMachineTokenName(w.OperatorID) {
		return fmt.Errorf("%w: this party is acting through the machine "+
			"token %s, and no token is minted from another — one would renew "+
			"itself for ever with nobody present. Mint while signed in",
			ErrRefused, w.OperatorID)
	}
	switch owner.Kind {
	case iam.KindPerson:
		if w.Principal == "" || w.Principal != in.PersonID {
			return fmt.Errorf("%w: %s is a person, and a person's own access "+
				"token is minted by that person alone — whoever mints one sees "+
				"its value, and it acts as them. They mint their own while "+
				"signed in (`crewlet iam token -login <their login>`); an "+
				"administrator mints tokens for service accounts only",
				ErrRefused, in.PersonID)
		}
	case iam.KindMachine:
		if !w.Can(iam.GrantPeopleManage) {
			return fmt.Errorf("%w: minting a token for the service account %s "+
				"takes %s — it has no login page, so the token is minted "+
				"for it by whoever manages people", ErrRefused, in.PersonID,
				iam.GrantPeopleManage)
		}
	}
	return nil
}

// loginOf is one person's login, read inside a decide's snapshot.
func loginOf(ctx context.Context, tx *sql.Tx, personID string) (string, error) {
	var login string
	err := tx.QueryRowContext(ctx,
		`SELECT login FROM iam_people WHERE id = ?`, personID).Scan(&login)
	if err != nil {
		return "", fmt.Errorf("iamdomain: read person %s's login: %w",
			personID, err)
	}
	return login, nil
}

// Invite mints an invitation to an address nobody in this company holds, onto
// a human seat nobody holds.
//
// # It is a directory record
//
// An invitation is an address AND a seat spoken for by somebody who has no
// person yet, so it is decided where every other address and seat is: on the
// directory subject, whose decide refuses either where a person holds it or
// another open invitation does ([ErrTaken]). Two administrators inviting one
// address or onto one seat, and an invitation racing a create of the same
// person, contend at the broker and the loser decides again from rows that
// hold the winner.
//
// # The link is not here, and neither is its secret
//
// This publishes the invitation's id, what redeeming it confers and the
// VERIFIER of the secret the link carries beside the id
// ([Blinder.InvitationSecret], [InvitationVerifier]), and answers the secret
// for the caller to show once. The id alone is in every snapshot, backup and
// proxy log, so it opens nothing: holding the link is holding the secret.
//
// # It names a seat, and HOLDS it
//
// Every invitation names a seat ([InviteMint.Seat], [ErrSeatRequired]) — a
// HUMAN seat the chart holds and nobody holds — because the person it creates
// holds one from the moment they exist (ADR-0026), and redeeming it binds them
// to that seat in the redemption's own record. The chart is read before
// anything is published, advisorily ([Writer.humanSeat]), and the directory in
// the record's own snapshot; the redemption's record asks the directory again.
// From its issue until it is redeemed, cancelled or ages out, the invitation
// holds the seat as it holds the address ([heldByInvitation]): a create, a
// bind or a second invitation onto it is refused naming the invitation, so the
// person the link was sent to is never told at the last step that somebody
// took their seat in the meantime.
//
// # Its id is its OPERATION's, derived under the company's key
//
// The id is [Blinder.InvitationID] of the operation key rather than a value the
// caller mints, so a retry of an issue whose outcome nobody could establish —
// the one retry the answer tells a caller to make — names the invitation its
// first attempt issued. The id used to be minted per request: the retry named a
// SECOND invitation, which the address refused as held by the first, so the
// retry answered 409 against its own first attempt and the link to the one
// that may have landed was never shown to anybody. A retry that finds its own
// invitation publishes nothing and answers it; a key that already issued an
// invitation for another address or on other terms, or one no longer open, is
// [ErrOperationReused] rather than a second invitation under one id.
func (w *Writer) Invite(ctx context.Context, in InviteMint) (
	InviteIssued, error) {

	if err := w.mayAdminister(OpInvite); err != nil {
		return InviteIssued{}, err
	}
	// WHAT IT CONFERS IS DECIDED HERE, ONCE, against the issuer — the
	// redemption reads it back rather than deciding again — so this is
	// the one place an invitation can be held to the rule every other
	// grant change is: a caller may not confer what they do not hold.
	if err := w.MayConfer(nil, in.Grants); err != nil {
		return InviteIssued{}, err
	}
	switch {
	case in.OpID == "":
		return InviteIssued{}, errors.New("iamdomain: an invitation needs an " +
			"operation key — its id is derived from it")
	case iam.NormalizeEmail(in.Email) == "":
		// NOT FINDABLE, as an enrolment with no address is: a value the
		// caller left out, which unclassified was a 500.
		return InviteIssued{}, fmt.Errorf("%w: an invitation needs the "+
			"address it is for — it is what the directory holds the "+
			"invitation against every person and every open invitation by",
			ErrNotFindable)
	case len(in.Email) > MaxAddress:
		return InviteIssued{}, fmt.Errorf("%w: the address is %d bytes and the "+
			"cap is %d, the longest RFC 5321 permits", ErrInvalid,
			len(in.Email), MaxAddress)
	case in.ExpiresAt.IsZero():
		return InviteIssued{}, fmt.Errorf("%w: an invitation needs an expiry; "+
			"one read as `never` is a superuser claim that stays live in "+
			"somebody's mailbox for the life of the company", ErrInvalid)
	case in.Seat == "":
		return InviteIssued{}, fmt.Errorf("%w: an invitation creates a person, "+
			"so it names the human seat they will hold", ErrSeatRequired)
	}
	// THE SEAT, checked against the organisation this node runs before
	// anything is published or MINTED — a typo, an agent's seat or a node
	// running no company refuses the issue with nothing left behind. Before
	// the blinder in particular, which mints the company's key on the first
	// address a fresh node ever blinds: asked after it, the first invitation
	// a node running no company was asked for answered whatever the mint's
	// guard said instead of the seat's own refusal and its remedy. Whether
	// somebody already holds the seat is asked again in the issue's own
	// snapshot below.
	seat, err := w.humanSeat(ctx, in.Seat)
	if err != nil {
		return InviteIssued{}, err
	}
	if w.blinds == nil || w.sealer == nil {
		return InviteIssued{}, fmt.Errorf("iamdomain: this node cannot "+
			"mint an invitation: %s", w.missingKeys())
	}
	blinder, err := w.blinds.Blinder(ctx)
	if err != nil {
		return InviteIssued{}, fmt.Errorf("iamdomain: this node cannot "+
			"derive the blind an invitation's address is compared by: %w", err)
	}
	blind, err := blinder.Email(in.Email)
	if err != nil {
		return InviteIssued{}, fmt.Errorf("iamdomain: blind an "+
			"invitation's address: %w", err)
	}
	// THE ID IS THE OPERATION'S, and it is derived before anything is
	// minted: a key that is not a uuid7 is refused here, with nothing left
	// behind.
	id, err := blinder.InvitationID(in.OpID)
	if err != nil {
		return InviteIssued{}, err
	}
	// AND SO IS THE LINK'S SECRET, derived from the id, so a retry of this
	// issue hands back the link its first attempt issued.
	secret, err := blinder.InvitationSecret(id)
	if err != nil {
		return InviteIssued{}, err
	}
	// SEALED AS THE INVITATION'S OWN, because there is no person yet: bound
	// to the invitation's id, so it opens as nothing else
	// ([Sealer.SealInvitation]).
	sealed, err := w.sealer.SealInvitation(id, in.Email)
	if err != nil {
		return InviteIssued{}, fmt.Errorf("iamdomain: seal an "+
			"invitation's address: %w", err)
	}
	// EVERY INVITATION STATES ITS CONDITIONS — the secret its redemption
	// must present, and the seat it binds.
	mutation, err := EncodeInvitation(Invitation{
		V: DocumentVersion, ID: id, EmailBlind: blind, Sealed: sealed,
		InvitedBy: w.Actor, Grants: in.Grants,
		Verifier: InvitationVerifier(secret), Seat: seat,
		ExpiresAt: in.ExpiresAt,
	})
	if err != nil {
		return InviteIssued{}, err
	}
	// THE BUCKET IS THE ADDRESS'S OWN, matching the apply: an invitation
	// has no person until it is redeemed, and a bucket derived from an
	// empty id would put every outstanding invitation in one sweep.
	scopeOf := func(context.Context, *sql.Tx) (ScopeSet, error) {
		return BucketScope(BucketOf(blind)), nil
	}
	// THIS OPERATION'S OWN INVITATION FIRST, because a retry finds the one
	// its first attempt issued on this very address — outstanding, holding
	// it — and must be answered with it rather than refused by it.
	//
	// THEN BOTH TABLES, for the seat and for the address, because either can
	// be held by a PERSON or by an invitation nobody has redeemed: reading
	// only the people missed every outstanding invitation and produced a
	// second link for one address.
	expires := in.ExpiresAt
	decide := func(tx *sql.Tx) (_ []byte, err error) {
		expires = in.ExpiresAt
		if err = w.issuedBefore(ctx, tx, id, blind, seat, in, &expires); err != nil {
			return nil, err
		}
		if err = wholeDirectory(ctx, tx); err != nil {
			return nil, err
		}
		now := w.Now()
		if err = taken(ctx, tx, UniqueSeat, seat, ""); err != nil {
			return nil, err
		}
		if err = heldByInvitation(ctx, tx, UniqueSeat, seat, now); err != nil {
			return nil, err
		}
		if err = taken(ctx, tx, UniqueEmail, blind, ""); err != nil {
			return nil, err
		}
		if err = heldByInvitation(ctx, tx, UniqueEmail, blind, now); err != nil {
			return nil, err
		}
		return mutation, nil
	}
	result, err := w.publishDirectory(ctx, OpInvite, "", in.OpID, in.Reason,
		scopeOf, decide)
	if err == nil && result.Collapsed {
		// ANSWERED FROM THE LEDGER, so the decide above never compared this
		// call's terms with the invitation the key issued, nor read the
		// deadline it keeps. Both are read back now, from the invitation's
		// own row, by the comparison the decide makes.
		applied := result.Outcome == statelog.OutcomeApplied
		if expires, err = w.issuedAs(ctx, id, blind, seat, in, applied); err != nil {
			return InviteIssued{Result: result}, err
		}
	}
	return InviteIssued{Result: result, ID: id, Secret: secret,
		ExpiresAt: expires}, err
}

// issuedAs is the invitation an operation issued, read back after the
// framework answered its retry from the ledger ([statelog.Result.Collapsed]):
// the deadline it keeps, or [ErrOperationReused] where the key issued it on
// other terms — the refusal the decide gives a retry it does run.
//
// # No row is two opposite answers, and applied tells them apart
//
// The collapsed result says whether THIS NODE applied the invitation: applied
// is its ledger's answer or its own applier's, pending is a copy the broker
// holds that has not reached this node yet.
//
//   - NOT APPLIED HERE, and no row: the invitation landed and its terms cannot
//     be read yet — unavailable, and the same key retried once this node has
//     applied it is answered in full.
//   - APPLIED HERE, and no row: the row existed and the retention sweep has
//     COLLECTED it, which it does only to an invitation that was redeemed or
//     aged out ([Writer.Sweep]) — so the link the key issued admits
//     nobody, and the answer is the one the decide gives the row it would
//     have found: [ErrOperationReused]. Read as "not applied yet" it told the
//     caller to retry the same key, which found the same absence on every
//     attempt for the rest of the ledger's month.
func (w *Writer) issuedAs(ctx context.Context, id, blind, seat string,
	in InviteMint, applied bool) (time.Time, error) {

	expires := in.ExpiresAt
	err := w.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return w.issuedBefore(ctx, tx, id, blind, seat, in, &expires)
	})
	switch {
	case errors.Is(err, errNothingToPublish):
		return expires, nil
	case err != nil:
		return time.Time{}, err
	case applied:
		return time.Time{}, fmt.Errorf("%w: operation %s issued invitation %s, "+
			"which this node applied and the retention sweep has since "+
			"collected — it was redeemed or aged out, so its link admits "+
			"nobody; issue a new one under a new key", ErrOperationReused,
			in.OpID, id)
	}
	return time.Time{}, fmt.Errorf("iamdomain: operation %s issued invitation "+
		"%s and this node has not applied it yet, so it cannot say on which "+
		"terms — retry the same key: %w", in.OpID, id, statelog.ErrUnavailable)
}

// ErrOperationReused reports an operation key that already names something
// other than what this call asks for: an invitation issued for another address
// or on other terms, one no longer open, or a create of a person who already
// exists.
//
// A REFUSAL AND NOT A DEDUPE. The key is what a retry is recognised by, and a
// retry is the SAME request: answered with the first attempt's object, a second
// DIFFERENT request under a reused key would be told its invitation was issued
// — or its person created — when what exists is somebody else's.
var ErrOperationReused = errors.New("iamdomain: that operation key already " +
	"names something else")

// issuedBefore answers whether the invitation an operation derives was already
// issued, read inside the issue's own snapshot: nil where it was not,
// [errNothingToPublish] where this is a retry that found its own, and
// [ErrOperationReused] where the key already issued something this call is not.
//
// THE TERMS ARE COMPARED, not only the address: a retry is the same request, so
// an invitation on the same address that confers other grants or another
// seat was issued by a different request that reused the key — and
// answering it as this one's would hand out a link to what somebody else
// offered. The seat is compared by its handle.
func (w *Writer) issuedBefore(ctx context.Context, tx *sql.Tx, id, blind, seat string,
	in InviteMint, expiresAt *time.Time) error {

	var (
		held              string
		expires, redeemed int64
		document          []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT email_blind, expires_at, redeemed_at, document
		  FROM iam_invites WHERE id = ?`, id).
		Scan(&held, &expires, &redeemed, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read invitation %s: %w", id, err)
	}
	stored, err := DecodeInvitation(document)
	if err != nil {
		return fmt.Errorf("iamdomain: open invitation %s: %w", id, err)
	}
	switch {
	case held != blind || !sameGrants(stored.Grants, in.Grants) ||
		stored.Seat != seat:
		return fmt.Errorf("%w: operation %s already issued invitation %s on "+
			"other terms — a retry is the same request; a new invitation needs "+
			"a new key", ErrOperationReused, in.OpID, id)
	case redeemed != 0:
		return fmt.Errorf("%w: the invitation operation %s issued has been "+
			"redeemed", ErrOperationReused, in.OpID)
	case expires == 0 || !w.Now().Before(time.UnixMilli(expires)):
		// NO EXPIRY IS AGED OUT, for [Writer.redeemable]'s reason.
		return fmt.Errorf("%w: the invitation operation %s issued has aged "+
			"out; issue a new one under a new key", ErrOperationReused, in.OpID)
	}
	// THE DEADLINE IT WAS ISSUED WITH, which is the one its link keeps: a
	// retry an hour later is not a later invitation.
	*expiresAt = time.UnixMilli(expires).UTC()
	return errNothingToPublish
}

// sameGrants reports whether two grant lists confer the same set.
func sameGrants(a, b []iam.Grant) bool {
	for _, g := range a {
		if !slices.Contains(b, g) {
			return false
		}
	}
	for _, g := range b {
		if !slices.Contains(a, g) {
			return false
		}
	}
	return true
}

// InviteIssued is what an issue answers: the write's outcome, the id the link
// is built from — the one the OPERATION derives, whether this call published it
// or found its own first attempt had — and the deadline the invitation keeps.
type InviteIssued struct {
	statelog.Result
	ID string

	// Secret is the half of the link that is the credential
	// ([Blinder.InvitationSecret]) — the value the estate keeps only the
	// verifier of. The caller shows it once, in the link, and keeps it
	// nowhere; a retry of the same issue derives it again.
	Secret string

	ExpiresAt time.Time
}

// heldByInvitation refuses a value an OPEN invitation holds — its address or
// its seat — naming the invitation, read inside a directory decide's snapshot.
//
// # Every open invitation counts, and a redemption never asks
//
// There is no exception for an invitation that should not count against
// itself, because the one gesture that would need it — redeeming the
// invitation holding the value — does not ask at all: a redemption is held to
// the PEOPLE's rows alone ([Writer.enrol]), so a second open invitation a
// build before invitations held their seats left on the same seat cannot
// deadlock both links. Every other caller is taking a value somebody else's
// invitation may hold.
//
// # Open is one predicate
//
// Not redeemed, not aged out, and naming a seat ([openInvitation]) — the
// predicate the listing reads an invitation as outstanding by and the
// redemption's own refusals mirror ([InvitationRow.Spent]). An invitation that
// names no seat was issued before every invitation did and is redeemable by
// nobody ([Redemption.validate]), so it holds nothing either: holding an
// address with a link that can never be redeemed refused every new invitation
// to it for the rest of its week, for nothing.
//
// THE COLUMN IS CHOSEN HERE, by a switch, as [taken] chooses its own — never
// from a caller's string.
//
// THE WRITER'S CLOCK DECIDES THE EXPIRY HERE, which is the one place in this
// domain that is acceptable: every instant an applier stores is the BROKER's,
// and what the clock decides is only whether an invitation still holds what it
// was issued for. A writer whose clock is minutes out issues an invitation
// beside one somebody could still have used, or refuses one beside a link that
// had just aged out — ordinary administrative outcomes, and never two people
// on one address or one seat, since a redemption's own decide refuses a value
// a person holds.
func heldByInvitation(ctx context.Context, tx *sql.Tx, field Unique, value string,
	now time.Time) error {

	var column string
	switch field {
	case UniqueEmail:
		column = "email_blind"
	case UniqueSeat:
		column = "seat_id"
	default:
		return fmt.Errorf("iamdomain: an invitation holds an address and a "+
			"seat, and %q is neither", field)
	}
	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM iam_invites
		WHERE `+column+` = ? AND `+openInvitation+`
		ORDER BY created_at DESC LIMIT 1`,
		value, now.UnixMilli()).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read the open invitations holding the "+
			"%s %s: %w", field, value, err)
	}
	return &ErrTaken{Field: field, Value: value, Invitation: id}
}

// openInvitation is THE predicate an invitation is outstanding by, over
// `iam_invites` and one bound instant: not redeemed, not aged out — a zero
// expiry included, which no writer forms — and naming a seat. See
// [heldByInvitation].
const openInvitation = `redeemed_at = 0 AND expires_at > ? AND seat_id <> ''`

// InviteMint is what issuing an invitation needs.
type InviteMint struct {
	// Email is the address it is for, in the clear. It is blinded for the
	// subject and sealed for the row before anything is published.
	Email string

	// Grants are what redeeming it confers, decided ONCE by whoever issued
	// it rather than again by whoever processes the redemption — which is
	// also what stops a redemption being a way to ask for more than was
	// offered.
	Grants []iam.Grant

	// Seat is the seat redeeming it BINDS the new person to, by its handle.
	// REQUIRED ([ErrSeatRequired]): a human seat nobody holds and no other
	// open invitation holds, which this one then holds until it is redeemed,
	// cancelled or ages out. See [Writer.Invite].
	Seat string

	// ExpiresAt is when it stops being redeemable. REQUIRED.
	ExpiresAt time.Time

	// OpID is the operation KEY, a uuid7, and the invitation's id is
	// derived from it ([Blinder.InvitationID]) — so the id is the
	// operation's rather than the caller's, and a retry under the same key
	// names the invitation its first attempt issued.
	OpID   string
	Reason string
}

// CancelInvitation withdraws an invitation nobody has redeemed — open, or aged
// out and not yet collected — so its link opens nothing and the address AND
// the seat it held are free for a new invitation, a create or a bind the
// moment the record lands.
//
// # A directory record, because the address and the seat are held there
//
// The invitation holds both on the directory subject, so the record that
// frees them rides there too: an issue to the same address or seat decided
// after it is decided from rows that no longer hold the old one, and one
// racing it contends at the broker. It TAKES nothing, so it does not ask
// [wholeDirectory], for a removal's reason.
//
// # Three answers about the row, read in the record's own snapshot
//
// An invitation this snapshot does not hold is [ErrNoInvitation] — never
// issued, cancelled already, or collected by the sweep, which a link to it
// cannot tell apart either. One somebody REDEEMED is [InvitationRedeemed],
// naming whom it created: the link is spent, and what undoes it is removing
// that person. Anything else is cancelled.
func (w *Writer) CancelInvitation(ctx context.Context, id, opID, reason string) (
	statelog.Result, error) {

	if err := w.mayAdminister(OpCancel); err != nil {
		return statelog.Result{}, err
	}
	if id == "" || opID == "" {
		return statelog.Result{}, errors.New("iamdomain: cancelling an " +
			"invitation needs its id and an operation id")
	}
	// THE BUCKET IS THE ADDRESS'S, as the issue's was and as the row's is,
	// and an invitation this snapshot does not hold has none — the decide
	// refuses it, so the id's own bucket is only a scope that encodes.
	scopeOf := func(ctx context.Context, tx *sql.Tx) (ScopeSet, error) {
		held, found, err := invitationIn(ctx, tx, id)
		if err != nil || !found {
			return BucketScope(BucketOf(id)), err
		}
		return BucketScope(BucketOf(held.Blind)), nil
	}
	decide := func(tx *sql.Tx) ([]byte, error) {
		held, found, err := invitationIn(ctx, tx, id)
		switch {
		case err != nil:
			return nil, err
		case !found:
			return nil, fmt.Errorf("%w: %s", ErrNoInvitation, id)
		case held.Person != "":
			return nil, &InvitationRedeemed{ID: id, Person: held.Person}
		}
		return EncodeCancellation(Cancellation{
			V: DocumentVersion, Invitation: id, EmailBlind: held.Blind,
		})
	}
	return w.publishDirectory(ctx, OpCancel, "", opID, reason, scopeOf, decide)
}

// invitationIn is the address blind one invitation holds and the person a
// redemption created, read inside a decide's snapshot — and false for an id it
// does not hold.
func invitationIn(ctx context.Context, tx *sql.Tx, id string) (
	InvitationRow, bool, error) {

	var held InvitationRow
	err := tx.QueryRowContext(ctx, `
		SELECT email_blind, person_id FROM iam_invites WHERE id = ?`, id).
		Scan(&held.Blind, &held.Person)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return InvitationRow{}, false, nil
	case err != nil:
		return InvitationRow{}, false, fmt.Errorf("iamdomain: read "+
			"invitation %s: %w", id, err)
	}
	return held, true, nil
}

// ErrNoInvitation reports an invitation this estate does not hold: never
// issued, cancelled already, or collected by the sweep once it was redeemed or
// aged out.
var ErrNoInvitation = errors.New("iamdomain: no such invitation")

// InvitationRedeemed refuses the cancellation of an invitation somebody
// redeemed, naming the person it created — the link is spent, and what undoes
// it is removing them.
type InvitationRedeemed struct {
	ID     string
	Person string
}

func (e *InvitationRedeemed) Error() string {
	return fmt.Sprintf("iamdomain: invitation %s was redeemed and created "+
		"person %s — it cannot be cancelled; remove the person instead",
		e.ID, e.Person)
}

// SetPassword replaces a person's password and moves their revocation epoch,
// in ONE record ([OpPassword]): every session and machine token they hold ends
// with the password it replaces.
//
// # Two callers, one record
//
// A person CHANGING their own password, having presented the current one, and
// a person SPENDING a reset link an administrator issued them. Both are a
// password somebody else may have had — which is why the epoch moves — and
// both revoke every reset link the person still holds, the one spent
// included, so a link is never a second way back in once a password is set.
//
// # No grant, and the caller's proof instead
//
// Neither caller holds an administrative capability: the first is the person,
// the second nobody yet. What bounds it is [PasswordSet.Check], which the
// surface hands in and which runs in the record's own snapshot — the verifier
// of the password the change presented still the one held, or the reset link
// still live and its secret the link's — so a password changed by somebody
// else between the proof and the record, or a link spent by another tab, is
// refused with nothing published.
//
// THE NEW CREDENTIAL'S ID IS MINTED ONCE, before the decide that may run more
// than once, so every round forms the same document.
func (w *Writer) SetPassword(ctx context.Context, in PasswordSet) (
	statelog.Result, error) {

	switch {
	case in.PersonID == "" || in.OpID == "":
		return statelog.Result{}, errors.New("iamdomain: setting a password " +
			"needs a person and an operation id")
	case in.Verifier == "":
		return statelog.Result{}, errors.New("iamdomain: setting a password " +
			"needs the new password's verifier — the password itself is " +
			"never published")
	}
	id := uuid.NewString()
	rec, err := w.record(PersonSubject(in.PersonID), OpPassword, in.PersonID,
		PeopleScope(in.PersonID), nil, in.Reason)
	if err != nil {
		return statelog.Result{}, err
	}
	decide := func(tx *sql.Tx) error {
		person, err := heldPerson(ctx, tx, in.PersonID, "their new password")
		if err != nil {
			return err
		}
		if person.Kind != iam.KindPerson {
			return fmt.Errorf("%w: %s is a %s, and only a person holds a "+
				"password", ErrRefused, in.PersonID, person.Kind)
		}
		counters, err := countersOf(ctx, tx, in.PersonID)
		if err != nil {
			return err
		}
		if in.Check != nil {
			if err = in.Check(person, counters); err != nil {
				return err
			}
		}
		now := w.Now()
		kept := make([]Credential, 0, len(person.Credentials)+1)
		for _, c := range RevokeResetLinks(person.Credentials, now) {
			// REPLACED, not revoked: a password has no listing of its
			// own anybody reads afterwards, and a second one on the
			// row is a second password that works.
			if c.Method == MethodPassword {
				continue
			}
			if c.Method == MethodReset && c.ID == in.Spends {
				c.Spent = true
			}
			kept = append(kept, c)
		}
		kept = append(kept, Credential{
			V: DocumentVersion, ID: id, Method: MethodPassword,
			Verifier: in.Verifier,
		})
		if person.Credentials, err = fitHeld(kept, now, counters, ErrInvalid); err != nil {
			return err
		}
		rec.Mutation, err = EncodePasswordChange(PasswordChange{
			V: DocumentVersion, Person: person, Epoch: counters.Epoch + 1,
		})
		return err
	}
	return w.publish(ctx,
		w.request(ctx, &rec, in.OpID, statelog.PatternArbitrated, decide))
}

// RevokeResetLinks answers held with every reset link still outstanding in it
// revoked at now — a copy, so the caller's slice is untouched.
//
// THREE RECORDS END A PERSON'S LINKS, and each through this: a password set
// ([Writer.SetPassword]), because a link must not be a second way back in once
// a password is; a new link's issue, because a person holds at most one; and
// an edit that ADDS a grant ([Writer.UpdatePerson]), because a link is judged
// against its issuer's grants at the issue and a grant gained afterwards was
// judged against nobody who holds it. Everything that ends a person's sessions
// and tokens ends their links too, by moving a counter the link was issued
// at ([Credential.EndedBy]) rather than through this.
func RevokeResetLinks(held []Credential, now time.Time) []Credential {
	out := slices.Clone(held)
	for i, c := range out {
		if c.Method == MethodReset && c.RevokedAt.IsZero() {
			out[i].RevokedAt = now
		}
	}
	return out
}

// PasswordSet is what replacing somebody's password needs.
type PasswordSet struct {
	PersonID string

	// Verifier is the new password's argon2id verifier — never the
	// password, which no record carries.
	Verifier string

	// Check is the caller's proof, judged in the record's own snapshot
	// against the person as it holds them and their [Counters], and
	// refusing with nothing published. A FUNCTION for
	// [CredentialSet.Apply]'s reason: the proof is about the credential set
	// the write lands on, which the caller does not hold and must not read
	// separately. It may refuse; it forms nothing.
	Check func(Person, Counters) error

	// Spends is the reset link this password is set from, marked
	// [Credential.Spent] beside the revocation every link takes; empty for
	// a person changing their own.
	Spends string

	OpID   string
	Reason string
}

// UpdatePerson rewrites one person's own document — their grants, their name
// — forming the new whole INSIDE the snapshot.
//
// # The same shape as [Writer.SetCredentials], and for the same reason
//
// A person's document is authored whole, so a caller changing one field
// cannot form the new value without the current one and must not read it in
// another transaction: a set assembled from an earlier read pairs an old
// decision with a new expectation, and the broker accepts it.
//
// # The two self-escalation rules are enforced HERE
//
// Not only at the route, because a record can be published by a CLI, a duty
// and a test, none of which passes through one — and this is the last frame
// that sees every path. A caller may not confer a grant they do not
// themselves hold, on anybody, themselves included. That is one rule stated
// once rather than the design's two, because "on my own record" and "on
// somebody else's" have the same answer and splitting them is how one of the
// two arms comes to be checked and the other not.
//
// # A grant gained revokes every outstanding reset link
//
// A reset link is issued only for somebody whose every grant its issuer holds,
// judged at the issue — and the issuer is shown the link and can spend it. A
// grant the person gains while it is outstanding was judged against nobody
// holding the link, so spending it would hand that grant to a party that may
// never have held it: an administrator holding people:manage would act with
// secrets:read because somebody else granted it to the person they issued a
// link for. So the record that adds a grant revokes the link, in the snapshot
// the grant lands in, and the next link is judged against the new set. This
// is the one write that grows an existing person's grants, so it is the one
// place the rule needs stating; taking a grant away revokes nothing, since a
// link that now reaches less was judged against more.
//
// # It changes what a person holds, never what they are
//
// Their kind and their stage are refused here ([keepsStanding]): the kind is
// fixed at the enrolment, and the stage has a record of its own that ends what
// a stage that may not act holds.
func (w *Writer) UpdatePerson(ctx context.Context, in PersonUpdate) (
	statelog.Result, error) {

	if err := w.mayAdminister(OpUpdate); err != nil {
		return statelog.Result{}, err
	}
	switch {
	case in.PersonID == "" || in.OpID == "":
		return statelog.Result{}, errors.New("iamdomain: updating a person " +
			"needs a person and an operation id")
	case in.Apply == nil:
		return statelog.Result{}, errors.New("iamdomain: updating a person " +
			"needs the function that forms the new document inside the " +
			"snapshot — a caller holding the current one read it in another " +
			"transaction, which is the pairing the write authority forbids")
	}
	// THE NAME IS SEALED BEFORE THE DECIDE, once. It depends on nothing the
	// snapshot holds, and a decide may run again against a fresh one: sealed
	// inside it, every run would draw a fresh nonce for the same name, and
	// the ciphertext the record carries would be whichever run landed.
	sealedName := ""
	if in.Name != nil && len(*in.Name) > MaxName {
		return statelog.Result{}, fmt.Errorf("%w: the name is %d bytes and the "+
			"cap is %d", ErrInvalid, len(*in.Name), MaxName)
	}
	if in.Name != nil {
		if w.sealer == nil {
			return statelog.Result{}, fmt.Errorf("iamdomain: this node "+
				"cannot change a name: it has %s, and a name is sealed "+
				"before it is published", w.missingKeys())
		}
		var err error
		if sealedName, err = w.sealer.Seal(in.PersonID, FieldName,
			*in.Name); err != nil {
			return statelog.Result{}, err
		}
	}
	var mutation []byte
	// THE LAST RUN'S BEFORE AND AFTER, which is the pair the landed record
	// actually carries: a decide may run again against a fresh snapshot.
	var before, after []iam.Grant
	decide := func(tx *sql.Tx) (err error) {
		person, err := heldPerson(ctx, tx, in.PersonID, "their row")
		if err != nil {
			return err
		}
		if in.Name != nil {
			person.NameSealed = sealedName
		}
		updated, err := in.Apply(person)
		if err != nil {
			return err
		}
		if err = keepsStanding(person, updated); err != nil {
			return err
		}
		if err = w.MayConfer(person.Grants, updated.Grants); err != nil {
			return err
		}
		added, _ := grantDelta(person.Grants, updated.Grants)
		// ONLY WHAT IS ADDED, as MayConfer judges: a row already holding
		// such a grant can still be edited, and is where it is taken away.
		if err = machineHolds(person.Kind, added); err != nil {
			return err
		}
		counters, err := countersOf(ctx, tx, in.PersonID)
		if err != nil {
			return err
		}
		// A RESET LINK THIS RECORD ISSUES ENDS WITH THE COUNTERS IT WAS
		// ISSUED AT, as a token does.
		updated.Credentials = stampIssued(person.Credentials,
			updated.Credentials, counters)
		// A GRANT GAINED ENDS EVERY OUTSTANDING RESET LINK, in this record.
		if len(added) > 0 {
			updated.Credentials = RevokeResetLinks(updated.Credentials, w.Now())
		}
		if updated.Credentials, err = fitHeld(updated.Credentials, w.Now(),
			counters, ErrInvalid); err != nil {
			return err
		}
		before, after = slices.Clone(person.Grants), slices.Clone(updated.Grants)
		mutation, err = EncodePerson(updated)
		return err
	}
	rec, err := w.record(PersonSubject(in.PersonID), OpUpdate, in.PersonID,
		PeopleScope(in.PersonID), nil, in.Reason)
	if err != nil {
		return statelog.Result{}, err
	}
	req := w.request(ctx, &rec, in.OpID, statelog.PatternArbitrated, func(tx *sql.Tx) (err error) {
		if err = decide(tx); err != nil {
			return err
		}
		rec.Mutation = mutation
		return nil
	})
	result, err := w.publish(ctx, req)
	// ONE ROW PER PERSON WRITE THAT MOVED A GRANT, and none for a write
	// that changed only a name: "who can do what to this deployment, and
	// since when" is the question the row answers.
	if added, removed := grantDelta(before, after); len(added)+len(removed) > 0 {
		w.announce(ctx, result, err, types.IAMGrantsChanged{
			Person: in.PersonID, Added: added, Removed: removed, By: w.Actor,
			OperatorID: w.OperatorID, Version: result.Position.Packed(),
		})
	}
	return result, err
}

// keepsStanding refuses a content record that would move what a person IS
// rather than what they hold: their KIND or their STAGE, each of which has a
// writer of its own, or none.
//
// # A kind never changes
//
// A row is a person or a service account from its enrolment to its removal:
// the login grammar, the address requirement, the seat a person must hold and
// the grants a machine may never carry ([machineHolds]) were all judged for
// the kind it was created as. The applier writes whatever kind a document
// states, so an [PersonUpdate.Apply] that flipped one made a service account
// into a person holding no seat and a coloned login no person may have — the
// state every other write here refuses to produce. The remedy is a removal and
// a new create, which judges everything for the new kind.
//
// # A stage moves only through [Writer.SetStage]
//
// That record is the one that ends what a stage that may not act holds, at the
// next epoch ([StatusChange.Epoch]); a content record carrying a new stage
// moved the column with no epoch moved, so the sessions and tokens of somebody
// suspended this way came back the day they were reinstated.
func keepsStanding(held, updated Person) error {
	switch {
	case updated.Kind != held.Kind:
		return fmt.Errorf("%w: a principal's kind never changes — this one "+
			"is a %s; remove it and create a %s instead", ErrInvalid,
			held.Kind, updated.Kind)
	case updated.Stage != held.Stage:
		return fmt.Errorf("%w: a stage moves only through a stage change, "+
			"which ends what a stage that may not act holds — not through an "+
			"edit of the person's document", ErrInvalid)
	}
	return nil
}

// MayConfer refuses a change that ADDS a grant this writer's party does not
// hold.
//
// ONLY THE ADDITIONS ARE CHECKED, which is the difference between a rule and
// an obstruction: taking a grant AWAY from somebody is always allowed —
// nobody escalates by narrowing — and an administrator who cannot hold
// secrets:reveal must still be able to withdraw it from a leaver.
//
// AN UNKNOWN GRANT IS REFUSED for [iam.Principal.Can]'s reason: a spelling a
// newer peer wrote that this build cannot name answers false, and a denylist
// would have admitted it.
//
// EXPORTED for the surface that asks it before an edit's first record: an edit
// that moves a seat or a login before it changes grants asks it first, so a
// grant the caller may not confer is refused with nothing moved. The record's
// own decide asks again in the snapshot the grants land from, and that answer
// is the authority — the early one is read from rows that may be a moment old.
func (w *Writer) MayConfer(before, after []iam.Grant) error {
	for _, g := range after {
		if slices.Contains(before, g) || w.Can(g) {
			continue
		}
		return fmt.Errorf("%w: conferring %s needs the same grant, and this "+
			"party holds %v — a caller may not hand out what they do not "+
			"themselves hold, on their own record or on anybody else's",
			ErrRefused, g, w.Grants)
	}
	return nil
}

// PersonUpdate is what rewriting one person's document needs.
type PersonUpdate struct {
	PersonID string

	// Name is a new name in the CLEAR, or nil to leave it alone.
	//
	// A POINTER AND NOT A STRING, because "" is a real setting — somebody
	// clearing a name they would rather not have here — and a plain
	// string could not tell it from a caller who never mentioned one.
	//
	// IT IS NOT [PersonUpdate.Apply]'s TO SET. A name is sealed once,
	// before the decide that may run more than once — so it is sealed
	// before, placed on the document the apply is handed, and the apply
	// may still overwrite it if that is genuinely what the caller means.
	Name *string

	// Apply forms the new document from the current one, INSIDE the
	// snapshot. A function rather than a value for [CredentialSet.Apply]'s
	// reason: the caller does not hold the current document and must not
	// read it separately.
	//
	// IT MAY REFUSE, which a caller uses for everything this package
	// cannot judge, and the refusal travels out of the decide unwrapped.
	// What it may NOT do is move the person's kind or stage, which the
	// record refuses ([keepsStanding]).
	//
	// And on a COLLAPSED result what it formed may not be what landed, for
	// [CredentialSet.Apply]'s reason.
	Apply func(Person) (Person, error)

	OpID   string
	Reason string
}
