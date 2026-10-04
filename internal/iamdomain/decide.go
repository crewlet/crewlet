package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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

	// Invitation is the open invitation holding an address, or empty.
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

// Enrol creates a person WHOLE — their row, their first credentials, their
// login, their address and their seat — in ONE record on the directory, and,
// for a redemption, spends the invitation in the same record.
//
// # One record, decided in one snapshot
//
// The record's decide reads the whole directory and refuses an address, a
// login or a seat somebody else holds ([ErrTaken], naming them) — so a refusal
// publishes nothing at all. There is no half-finished enrolment for a retry to
// meet: a redeemer told their login was taken chooses another and redeems
// again, and an administrator whose create was refused for its seat has
// created nobody.
//
// # What it may confer, and on whose authority
//
// An enrolment is a grant change from nothing to what it carries, so it is
// held to [Writer.UpdatePerson]'s rule: a caller may not confer a grant they
// do not hold. It used to be held to nothing, so a party holding people:manage
// and no other grant could enrol somebody carrying secrets:read — or enrol a
// colleague holding everything and sign in as them.
//
// ONE ENROLMENT IS NOT THE WRITER'S TO AUTHORISE, and it names what is: a
// REDEMPTION ([Enrolment.Invitation]) confers what the INVITATION's author
// conferred when they issued it, and was held to their grants then
// ([Writer.Invite]). The record's decide reads the invitation in its own
// snapshot and refuses anything it does not cover — more grants, a different
// address, another seat, a link already spent or aged out — so the node that
// processes a redemption decides nothing a second time.
//
// The node's own writer holds fleet:operate and people:manage and nothing
// else, so on its own authority it may confer those two; everything a
// redemption hands out comes from the invitation it names.
//
// THERE IS NO EXEMPTION FOR THE FIRST PERSON. A company with nobody in it
// still holds the Tier A token every serving node requires, and that token is
// a party like any other: its invitation is held to its grants exactly as an
// administrator's is, so the first person is invited, and bounded, the way
// everybody after them is.
//
// # A seat is the running company's
//
// An enrolment that binds a seat ([Enrolment.Seat]) has it checked against
// the organisation this node runs before anything is published — advisory,
// see [Writer.seatOf] — and held free in the record's own snapshot. An
// administrator's create may bind any seat the company holds; a redemption
// binds only the human seat its invitation names.
func (w *Writer) Enrol(ctx context.Context, in Enrolment) (statelog.Result, error) {
	if err := w.mayAdminister(OpEnrol); err != nil {
		return statelog.Result{}, err
	}
	if err := in.validate(); err != nil {
		return statelog.Result{}, err
	}
	if in.Invitation == "" {
		// THE WRITER'S OWN AUTHORITY: it reads nothing, so it is asked
		// before anything else is.
		if err := w.MayConfer(nil, in.Grants); err != nil {
			return statelog.Result{}, err
		}
	}
	if w.sealer == nil || (in.Email != "" && w.blinds == nil) {
		return statelog.Result{}, fmt.Errorf("iamdomain: this node cannot "+
			"enrol anybody: it has %s. An enrolment has to derive the blind "+
			"the directory compares an address by and seal the values that "+
			"belong to the person, and a node that guessed at either would put "+
			"two people on one address", w.missingKeys())
	}
	var blind string
	if in.Email != "" {
		blinder, err := w.blinds.Blinder(ctx)
		if err != nil {
			return statelog.Result{}, fmt.Errorf("iamdomain: this node "+
				"cannot derive the blind an address is compared by: %w", err)
		}
		if blind, err = blinder.Email(in.Email); err != nil {
			return statelog.Result{}, err
		}
	}
	seat, err := w.enrolledSeat(ctx, in)
	if err != nil {
		return statelog.Result{}, err
	}
	// SEALED ONCE, before the decide, which may run more than once: sealed
	// inside it, every run would draw a fresh nonce for the same value.
	//
	// AN ADDRESS IS OPTIONAL AND A SEALED NAME IS NOT. A machine identity
	// has no mailbox, so there is nothing to blind and nothing to seal —
	// but its NAME is still sealed like anybody else's, because `Release
	// pipeline, raised by Dana` names a person too, and a removal erases it
	// as surely.
	sealedName, err := w.sealer.Seal(in.PersonID, FieldName, in.Name)
	if err != nil {
		return statelog.Result{}, err
	}
	var sealedEmail string
	if in.Email != "" {
		if sealedEmail, err = w.sealer.Seal(in.PersonID, FieldEmail,
			in.Email); err != nil {
			return statelog.Result{}, err
		}
	}
	mutation, err := EncodeEnrolled(Enrolled{
		V: DocumentVersion,
		Person: Person{
			V: DocumentVersion, Kind: in.Kind, Stage: in.Stage,
			NameSealed: sealedName, EmailSealed: sealedEmail,
			Credentials: in.Credentials, Grants: in.Grants,
		},
		Holds:      Identifiers{EmailBlind: blind, Login: in.Login, SeatID: seat},
		Invitation: in.Invitation,
	})
	if err != nil {
		return statelog.Result{}, err
	}
	// THE SCOPE: the new person's bucket, every leaver whose tombstone the
	// seat's bind stamps, and — for a redemption — the address's bucket the
	// invitation row it spends is filed under.
	scopeOf := func(ctx context.Context, tx *sql.Tx) (_ ScopeSet, err error) {
		buckets := []Bucket{BucketOf(in.PersonID)}
		var leavers []string
		if seat != "" {
			if leavers, err = leaversOf(ctx, tx, seat); err != nil {
				return ScopeSet{}, err
			}
		}
		for _, leaver := range leavers {
			buckets = append(buckets, BucketOf(leaver))
		}
		if in.Invitation != "" {
			buckets = append(buckets, BucketOf(blind))
		}
		return BucketScope(buckets...), nil
	}
	decide := func(tx *sql.Tx) (_ []byte, err error) {
		if err = wholeDirectory(ctx, tx); err != nil {
			return nil, err
		}
		if in.Invitation != "" {
			// THE INVITATION, READ WHERE THE GRANTS LAND FROM: the one
			// read of it that decides anything.
			err = w.redeemable(ctx, tx, in, blind, seat)
		} else {
			err = w.createsNobodyTwice(ctx, tx, in)
		}
		if err != nil {
			return nil, err
		}
		if blind != "" {
			if err = taken(ctx, tx, UniqueEmail, blind, in.PersonID); err != nil {
				return nil, err
			}
			// AN OPEN INVITATION HOLDS ITS ADDRESS against a create, naming
			// the invitation: two people would otherwise be on their way to
			// one address. A redemption IS that invitation, and
			// [Writer.redeemable] held it to it.
			if in.Invitation == "" {
				if err = heldByInvitation(ctx, tx, blind, w.Now()); err != nil {
					return nil, err
				}
			}
		}
		if err = taken(ctx, tx, UniqueLogin, in.Login, in.PersonID); err != nil {
			return nil, err
		}
		if seat != "" {
			if err = taken(ctx, tx, UniqueSeat, seat, in.PersonID); err != nil {
				return nil, err
			}
		}
		return mutation, nil
	}
	result, err := w.publishDirectory(ctx, OpEnrol, in.PersonID, in.OpID,
		in.Reason, scopeOf, decide)
	if err == nil && result.Collapsed && in.Invitation != "" {
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
		return statelog.Result{}, in.alreadyEnrolled()
	}
	// AN ENROLMENT THAT CONFERS ANYTHING IS A GRANT CHANGE — from nothing
	// to what it carries — and the first person a company enrols, invited
	// under the deployment's own token, is the one row of those an audit
	// most needs to find.
	if added, _ := grantDelta(nil, in.Grants); len(added) > 0 {
		w.announce(ctx, result, err, types.IAMGrantsChanged{
			Person: in.PersonID, Added: added, By: w.Actor,
			OperatorID: w.OperatorID, Version: result.Position.Packed(),
		})
	}
	return result, err
}

// enrolledSeat is the seat an enrolment binds, as the organisation this node
// runs names it, or "" for none — asked before anything is published, and
// ADVISORY ([Writer.seatOf]).
//
// A REDEMPTION's must still be a HUMAN seat: a revision may have removed it,
// or made it an agent's, since the link was sent — and that refusal is the
// link's, under [ErrRefused], since its remedy is a new invitation. An
// administrator's create may bind any seat the company holds.
func (w *Writer) enrolledSeat(ctx context.Context, in Enrolment) (string, error) {
	if in.Seat == "" {
		return "", nil
	}
	if in.Invitation == "" {
		seat, err := w.seatOf(ctx, in.Seat)
		return seat.Handle, err
	}
	seat, err := w.humanSeat(ctx, in.Seat)
	if err != nil && !errors.Is(err, statelog.ErrUnavailable) {
		return "", fmt.Errorf("%w: invitation %s binds a seat it can no longer "+
			"bind (%w)", ErrRefused, in.Invitation, err)
	}
	return seat, err
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
	in Enrolment) error {

	enrolled, err := isEnrolled(ctx, tx, in.PersonID)
	if err != nil || !enrolled {
		return err
	}
	return in.alreadyEnrolled()
}

// alreadyEnrolled is the refusal an enrolment meets when what it would create
// already exists, in the terms of the authority it named.
//
//   - A REDEMPTION's link has been used: the person it created is enrolled.
//   - AN ADMINISTRATOR'S key already names somebody, and a new person is a new
//     operation under a new key.
func (in *Enrolment) alreadyEnrolled() error {
	if in.Invitation != "" {
		return fmt.Errorf("%w: invitation %s has already been used — the "+
			"person it created is enrolled", ErrRefused, in.Invitation)
	}
	return fmt.Errorf("%w: person %s already exists, and operation %s is not "+
		"the one that created them — a new person is a new operation under a "+
		"new key", ErrOperationReused, in.PersonID, in.OpID)
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

// Enrolment is what creating a person needs.
type Enrolment struct {
	// PersonID is minted by the CALLER, not here, and it is a uuid7.
	//
	// BY THE CALLER because an administrator's create DERIVES it from its
	// operation key ([CreatedPersonID]): a create whose outcome nobody could
	// establish is retried under that key, and the answer — the person it
	// created — has to name the same person whether the retry is answered
	// from the ledger or decided again. A redemption mints a fresh one per
	// attempt, since an attempt that did not land published nothing.
	PersonID string

	Kind  iam.Kind
	Stage iam.Stage

	// Name and Email are CLEARTEXT here and nowhere after: they are
	// sealed before the record is formed, and no payload carries either.
	Name  string
	Email string

	// Login is REQUIRED, in the holder's kind's grammar: a person's is
	// dotted and a machine's coloned. See [Enrolment.validate] for why a
	// person needs one although their address already finds them. An
	// invited person has no row at all until they redeem, and choosing
	// their login is part of redeeming.
	Login string

	Credentials []Credential
	Grants      []iam.Grant

	// Invitation is the id of the invitation this enrolment REDEEMS, or
	// empty. When set, what the enrolment confers is bounded by what the
	// invitation says rather than by the writer's own grants — see
	// [Writer.Enrol].
	Invitation string

	// InvitationSecret is the secret the invitation's link carries beside
	// its id ([Blinder.InvitationSecret]), REQUIRED with Invitation: the
	// id is in every snapshot, backup and proxy log, so naming it proves
	// nothing, and the record that lands the person checks the secret
	// against the invitation's verifier in its own snapshot — the same
	// check the route made, asked again where the grants land from,
	// because a record can be published by more than a route.
	InvitationSecret string

	// Seat is a seat this enrolment BINDS the person it creates to, by any
	// address the chart answers to it by, or empty. A redemption names the
	// seat its invitation binds, and the record refuses one that names any
	// other ([Writer.redeemable]).
	Seat string

	// OpID is the record's operation id.
	//
	// IN THE STATE LOG'S GRAMMAR ([statelog.NewOpID], [statelog.DeriveOpID])
	// — an administrator's create key, a bare uuid7, already is — because
	// the instant it carries is what the ledger vouches for a retry by: an
	// id outside it is read as minted at the epoch, and once this node's
	// ledger has lost a row of the kind it arbitrates on, every retry under
	// it is answered `unknown` without being published.
	OpID   string
	Reason string
}

// validate refuses an enrolment that could not land, before anything is
// sealed, blinded or read.
//
// [Writer.record] still checks the reason on every record; this is the check
// that names the field an enrolment is refused for.
func (in Enrolment) validate() error {
	switch {
	case in.Invitation != "" && in.Email == "":
		return fmt.Errorf("%w: redeeming an invitation enrols the address it "+
			"was issued to, and this enrolment names none", ErrNotFindable)
	case in.Invitation != "" && in.InvitationSecret == "":
		// REFUSED rather than invalid: it is what a link without its
		// secret, or an invitation issued before links carried one,
		// comes to, and its remedy is the one every way a link stops
		// working has — ask for a new one.
		return fmt.Errorf("%w: redeeming invitation %s needs the secret its "+
			"link carries beside the id — the id alone is in every snapshot "+
			"and proxy log, and opens nothing", ErrRefused, in.Invitation)
	case in.InvitationSecret != "" && in.Invitation == "":
		return errors.New("iamdomain: an enrolment carries an invitation's " +
			"secret only beside the invitation it redeems")
	case in.PersonID == "":
		return errors.New("iamdomain: an enrolment needs the person id it is " +
			"creating")
	case in.OpID == "":
		return errors.New("iamdomain: an enrolment needs an operation id — " +
			"without a stable one a retry cannot tell whether it already landed")
	case in.Kind != iam.KindPerson && in.Kind != iam.KindMachine:
		// THE DIRECTORY HOLDS PEOPLE AND MACHINES, and nothing else
		// enrols. A seat is the chart's and the engine is the node, so
		// a directory row of either kind is a second answer to who they
		// are — and neither has a login grammar, so the row would be
		// findable by an address alone and act as nothing the rest of
		// the engine can name.
		return fmt.Errorf("%w: %q is not a kind the directory enrols — want "+
			"%s or %s", ErrNotEnrollable, in.Kind, iam.KindPerson, iam.KindMachine)
	case !in.Stage.Valid():
		return fmt.Errorf("%w: %q is not an enrolment stage", ErrInvalid, in.Stage)
	case len(in.Name) > MaxName:
		return fmt.Errorf("%w: the name is %d bytes and the cap is %d",
			ErrInvalid, len(in.Name), MaxName)
	case len(in.Email) > MaxAddress:
		return fmt.Errorf("%w: the address is %d bytes and the cap is %d, the "+
			"longest RFC 5321 permits", ErrInvalid, len(in.Email), MaxAddress)
	case len(in.Credentials) > MaxHeldCredentials:
		return fmt.Errorf("%w: an enrolment carries %d credentials and a "+
			"person holds at most %d", ErrInvalid, len(in.Credentials),
			MaxHeldCredentials)
	case len(in.Reason) > MaxReason:
		return fmt.Errorf("%w: the reason on this enrolment is %d bytes and "+
			"the cap is %d — it is rendered into an authentication trail "+
			"beside the op that caused it, so it says WHICH cause fired "+
			"rather than narrating", ErrInvalid, len(in.Reason), MaxReason)
	case in.Kind == iam.KindPerson && in.Email == "":
		return fmt.Errorf("%w: enrolling a person needs an address — it is "+
			"the interactive login key, and somebody with none can never "+
			"sign in", ErrNotFindable)
	case in.Kind == iam.KindMachine && in.Login == "":
		// A MACHINE NEEDS NO ADDRESS and must still be FINDABLE. It has
		// no login page and no mailbox — `svc:ci` proves itself with a
		// token — so requiring an address would have meant inventing
		// one, and a row that is not named is a credential holder
		// nobody can list, revoke or audit.
		return fmt.Errorf("%w: enrolling a %s needs a login — it has no login "+
			"page and therefore no address, so the login is the only thing "+
			"that finds it", ErrNotFindable, in.Kind)
	case in.Login == "":
		// A PERSON NEEDS ONE TOO, and not to be found: their address
		// already does that. A login is the NAME a principal acts and is
		// written under while they hold no seat — iam.ActorFor records an
		// unbound person under it — and a person with none was recorded
		// as `anonymous` beside every change they made, and failed
		// [iam.Principal.Validate] on every request they sent. So every
		// path that creates a person names one: an administrator types
		// it, and an invitation's form proposes one from the address for
		// the person to keep or change.
		return fmt.Errorf("%w: enrolling a person needs a login — lowercase "+
			"segments joined by DOTS (jane.doe). It is the name every change "+
			"they make is recorded under while they hold no seat, and without "+
			"one they would be recorded as nobody", ErrInvalidLogin)
	}
	return loginFits(in.Kind, in.Login)
}

// redeemable refuses an enrolment its invitation does not cover, read inside
// the record's own snapshot.
//
// EVERY CLAUSE IS A WAY THE REDEMPTION COULD OTHERWISE ASK FOR MORE THAN WAS
// OFFERED: a grant the invitation did not carry, an address it was not issued
// to (holding somebody's link is not holding their address), a seat it did not
// bind, and a link that is spent or aged out.
//
// THE WRITER'S CLOCK decides the expiry, as [openInvitationFor]'s does: the
// surface already refused an aged-out link against the same clock, and what
// this buys is that the check and the grants it bounds are one snapshot.
func (w *Writer) redeemable(ctx context.Context, tx *sql.Tx, in Enrolment,
	blind, seat string) error {

	var (
		held           string
		expires, spent int64
		document       []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT email_blind, expires_at, redeemed_at, document
		  FROM iam_invites WHERE id = ?`, in.Invitation).
		Scan(&held, &expires, &spent, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: invitation %s is not held on this node, so "+
			"nothing says what redeeming it confers", ErrRefused, in.Invitation)
	case err != nil:
		return fmt.Errorf("iamdomain: read invitation %s: %w", in.Invitation, err)
	}
	invitation, err := DecodeInvitation(document)
	if err != nil {
		return fmt.Errorf("iamdomain: open invitation %s: %w", in.Invitation, err)
	}
	switch {
	case !invitationAdmits(invitation.Verifier, in.InvitationSecret):
		// THE SECRET, ASKED WHERE THE GRANTS LAND FROM. The route asked
		// it too, but a record can be published by more than a route, and
		// naming an invitation's id — which every snapshot and proxy log
		// holds — is not holding its link. An invitation issued before
		// links carried a secret has no verifier and admits nobody.
		return fmt.Errorf("%w: the secret presented is not the one invitation "+
			"%s's link carries", ErrRefused, in.Invitation)
	case held != blind:
		return fmt.Errorf("%w: invitation %s was issued to another address — "+
			"holding somebody's link is not holding their address",
			ErrRefused, in.Invitation)
	case spent != 0:
		return fmt.Errorf("%w: invitation %s has already been used",
			ErrRefused, in.Invitation)
	case expires != 0 && !w.Now().Before(time.UnixMilli(expires)):
		return fmt.Errorf("%w: invitation %s has aged out", ErrRefused,
			in.Invitation)
	}
	for _, g := range in.Grants {
		if !g.Valid() || !slices.Contains(invitation.Grants, g) {
			return fmt.Errorf("%w: redeeming invitation %s confers %v, and "+
				"%s is not among them — a redemption hands out what was "+
				"offered and nothing more", ErrRefused, in.Invitation,
				invitation.Grants, g)
		}
	}
	// THE SEAT IS THE INVITATION'S, and exactly it: one the invitation
	// did not carry would be a redemption asking for more than was
	// offered, and one it carried and the enrolment dropped would spend
	// the link without the binding its issuer decided on.
	if seat != invitation.Seat {
		return fmt.Errorf("%w: invitation %s binds seat %q, and the "+
			"enrolment names %q — a redemption binds what was offered and "+
			"nothing else", ErrRefused, in.Invitation, invitation.Seat, seat)
	}
	return nil
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
// bind, an unbind and a move between seats are each one record, and the value
// given up is free the moment the record lands. A move used to be two records,
// the new value claimed and the old released, and a release that did not land
// was a gap in a trail no caller could close.
//
// # Decided in the record's own snapshot
//
// A login outside its holder's kind's grammar ([loginFits]) — the kind READ
// HERE, never taken from the caller — a login somebody else holds and a seat
// somebody else is bound to are refused, naming who holds it ([ErrTaken]), and
// a refusal publishes nothing: nothing has moved. A change that changes
// nothing is applied with no record.
//
// A SEAT IS CHECKED AGAINST THE ORGANISATION THIS NODE RUNS before anything is
// published: see [Writer.seatOf].
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
		// one — it is the name an unbound person's changes are recorded
		// under — so a person with none would be recorded as nobody, and
		// a machine enrolled under `token:<id>` silently unbound from its
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
			found, err := w.seatOf(ctx, *in.Seat)
			if err != nil {
				return statelog.Result{}, err
			}
			handle = found.Handle
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

	// Seat is the seat to bind them to, by any address the chart answers
	// to it by, "" to unbind them, or nil to leave the binding as it is.
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
			V: GateRecordVersion, Generation: generation,
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
	mutation, err := EncodeStatus(StatusChange{V: DocumentVersion, Stage: stage})
	if err != nil {
		return statelog.Result{}, err
	}
	rec, err := w.record(PersonSubject(personID), OpStatus, personID,
		PeopleScope(personID), mutation, reason)
	if err != nil {
		return statelog.Result{}, err
	}
	result, err := w.publish(ctx, w.request(ctx, &rec, opID, statelog.PatternArbitrated, nil))
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
		return EncodeRemoval(Removal{V: GateRecordVersion, Released: held})
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
		// A RESTRICTED SESSION TRAVELS AT THE VERSION THAT STATES THE
		// RESTRICTION — its `enrolment_only` row in [versionedFields] —
		// so a node that cannot read it defers it rather than serving it
		// whole; an ordinary one carries no such key and stays at the
		// base. See [ConditionRecordVersion].
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
	// what it proved and the deployment's own `api.auth.local.totp`, and
	// the record carrying it is written at [ConditionRecordVersion].
	EnrolmentOnly bool

	// NoWait asks for the answer the broker's acknowledgement already
	// establishes, rather than waiting for this node's applier.
	//
	// THE ONE PLACE IN THIS ESTATE IT IS CORRECT, and the reason is that
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

// seatOf is the ADVISORY read of the organisation this node runs, and its doc
// is the whole of what it promises.
//
// IT GUARANTEES NOTHING. The organisation is the configuration this node
// applied, not this log, and nothing orders the two — so this read and a
// revision removing the seat are concurrent by construction, and a bind that
// passed here can still land after the seat is gone. The residue is a legal
// named state the session layer answers with a 403 NAMING THE SEAT, and the
// dangling-binding report names it too.
//
// WHAT IT BUYS is that binding somebody to a seat handle nobody has ever
// declared — a typo, the overwhelmingly common failure — is refused at the
// moment somebody can still fix it, rather than becoming a person who cannot
// act and a 403 nobody can explain.
//
// # A node that cannot say REFUSES
//
// A writer handed no organisation, or one running no company yet, is the
// unknown arm — [statelog.ErrUnavailable], a 503 a retry clears — and never a
// bind made unchecked.
func (w *Writer) seatOf(ctx context.Context, handle string) (session.Seat, error) {
	if w.seats == nil {
		return session.Seat{}, fmt.Errorf("%w: iamdomain: this writer has no "+
			"organisation to check seat %q against", statelog.ErrUnavailable, handle)
	}
	seat, found, err := w.seats.Seat(ctx, handle)
	switch {
	case err != nil:
		return session.Seat{}, fmt.Errorf("%w: iamdomain: read the organisation "+
			"this node runs to check seat %q: %w", statelog.ErrUnavailable, handle, err)
	case !found:
		// ErrInvalid, because it is a value the caller typed: before this
		// was classified it fell through to a 500, telling an
		// administrator who mistyped a handle that the engine was broken.
		return session.Seat{}, fmt.Errorf("%w: the company this node runs has "+
			"no seat %q. This check is ADVISORY — it reads the configuration "+
			"this node applied — so it is here to catch a typo; if a revision "+
			"added the seat moments ago, retry", ErrInvalid, handle)
	}
	return seat, nil
}

// humanSeat is [Writer.seatOf] for a binding only a person may hold: an
// invitation's. A seat that is not a HUMAN seat is refused — an agent seat has
// no person to hold it, and a person bound to one is refused on every request.
func (w *Writer) humanSeat(ctx context.Context, handle string) (string, error) {
	seat, err := w.seatOf(ctx, handle)
	if err != nil {
		return "", err
	}
	if seat.Kind != session.SeatKindHuman {
		return "", fmt.Errorf("%w: seat %q is a %s seat, and an invitation binds "+
			"a person — only a human seat can be held by one", ErrInvalid,
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
		if person.Credentials, err = fitHeld(person.Credentials, w.Now(),
			ErrInvalid); err != nil {
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
		epoch, err := epochOf(ctx, tx, in.PersonID)
		if err != nil {
			return err
		}
		generation, err := generationOf(ctx, tx)
		if err != nil {
			return err
		}
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
		if owner.Credentials, err = fitHeld(kept, now, ErrInvalidToken); err != nil {
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

// Invite mints an invitation to an address nobody in this company holds.
//
// # It is a directory record
//
// An invitation is an address spoken for by somebody who has no person yet, so
// it is decided where every other address is: on the directory subject, whose
// decide refuses an address a person holds or another open invitation holds
// ([ErrTaken]). Two administrators inviting one address, and an invitation
// racing a hire of the same address, contend at the broker and the loser
// decides again from rows that hold the winner.
//
// # The link is not here, and neither is its secret
//
// This publishes the invitation's id, what redeeming it confers and the
// VERIFIER of the secret the link carries beside the id
// ([Blinder.InvitationSecret], [InvitationVerifier]), and answers the secret
// for the caller to show once. The id alone is in every snapshot, backup and
// proxy log, so it opens nothing: holding the link is holding the secret.
//
// # It may bind a seat
//
// An invitation may name a seat ([InviteMint.Seat]) — a HUMAN seat the chart
// holds and nobody is bound to — and redeeming it then binds the person it
// creates to that seat, in the redemption's own record. The chart is read
// before anything is published, advisorily, and the directory in the record's
// own snapshot; the redemption's record asks the directory again.
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
	case in.Email == "":
		return InviteIssued{}, errors.New("iamdomain: an invitation needs " +
			"the address it is for — it is what the directory holds the " +
			"invitation against every person and every open invitation by")
	case len(in.Email) > MaxAddress:
		return InviteIssued{}, fmt.Errorf("%w: the address is %d bytes and the "+
			"cap is %d, the longest RFC 5321 permits", ErrInvalid,
			len(in.Email), MaxAddress)
	case in.ExpiresAt.IsZero():
		return InviteIssued{}, errors.New("iamdomain: an invitation needs " +
			"an expiry; one read as `never` is a superuser claim that stays " +
			"live in somebody's mailbox for the life of the company")
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
	// THE SEAT, checked against the organisation this node runs before
	// anything is published — a typo, an agent's seat or a node running no
	// company refuses the issue with nothing left behind. Whether somebody
	// already holds it is asked again in the issue's own snapshot below.
	var seat string
	if in.Seat != "" {
		if seat, err = w.humanSeat(ctx, in.Seat); err != nil {
			return InviteIssued{}, err
		}
	}
	// SEALED AS THE INVITATION'S OWN, because there is no person yet: bound
	// to the invitation's id, so it opens as nothing else
	// ([Sealer.SealInvitation]).
	sealed, err := w.sealer.SealInvitation(id, in.Email)
	if err != nil {
		return InviteIssued{}, fmt.Errorf("iamdomain: seal an "+
			"invitation's address: %w", err)
	}
	// EVERY INVITATION STATES A CONDITION an older build would drop — the
	// secret its redemption must present, and the seat it binds — and its
	// `verifier` row in [versionedFields] makes it travel at the version
	// that says so: an older node defers it and answers the link 410,
	// rather than redeeming it on its id.
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
	// THEN BOTH TABLES, because an address can be held by a PERSON or by an
	// invitation nobody has redeemed: reading only the people missed every
	// outstanding invitation and produced a second link for one address.
	expires := in.ExpiresAt
	decide := func(tx *sql.Tx) (_ []byte, err error) {
		expires = in.ExpiresAt
		if err = w.issuedBefore(ctx, tx, id, blind, seat, in, &expires); err != nil {
			return nil, err
		}
		if err = wholeDirectory(ctx, tx); err != nil {
			return nil, err
		}
		if seat != "" {
			if err = taken(ctx, tx, UniqueSeat, seat, ""); err != nil {
				return nil, err
			}
		}
		if err = taken(ctx, tx, UniqueEmail, blind, ""); err != nil {
			return nil, err
		}
		if err = heldByInvitation(ctx, tx, blind, w.Now()); err != nil {
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
//     aged out ([SweepRecordVersion]) — so the link the key issued admits
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
	case expires != 0 && !w.Now().Before(time.UnixMilli(expires)):
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

// heldByInvitation refuses an address an open invitation holds, naming the
// invitation — read inside a directory decide's snapshot.
func heldByInvitation(ctx context.Context, tx *sql.Tx, blind string,
	now time.Time) error {

	open, found, err := openInvitationFor(ctx, tx, blind, now)
	if err != nil {
		return err
	}
	if found {
		return &ErrTaken{Field: UniqueEmail, Value: blind, Invitation: open}
	}
	return nil
}

// openInvitationFor is the invitation on an address that has not been
// redeemed and has not aged out, read INSIDE a decide's snapshot.
//
// THE WRITER'S CLOCK DECIDES THE EXPIRY HERE, which is the one place in this
// domain that is acceptable: every instant an applier stores is the BROKER's,
// and what the clock decides is only whether an invitation still holds its
// address. A writer whose clock is minutes out issues an invitation beside one
// somebody could still have used, or refuses one beside a link that had just
// aged out — ordinary administrative outcomes, and never two people on one
// address, since a redemption's own decide refuses an address a person holds.
func openInvitationFor(ctx context.Context, tx *sql.Tx, blind string,
	now time.Time) (string, bool, error) {

	var id string
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM iam_invites
		WHERE email_blind = ? AND redeemed_at = 0 AND expires_at > ?
		ORDER BY created_at DESC LIMIT 1`,
		blind, now.UnixMilli()).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("iamdomain: read the invitations on an "+
			"address: %w", err)
	}
	return id, true, nil
}

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

	// Seat is a seat redeeming it BINDS the new person to, by its handle,
	// or empty. It must be a human seat nobody holds. See [Writer.Invite].
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
		if err = w.MayConfer(person.Grants, updated.Grants); err != nil {
			return err
		}
		if updated.Credentials, err = fitHeld(updated.Credentials, w.Now(),
			ErrInvalid); err != nil {
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
	// cannot judge — a stage the surface will not set here — and the
	// refusal travels out of the decide unwrapped.
	//
	// And on a COLLAPSED result what it formed may not be what landed, for
	// [CredentialSet.Apply]'s reason.
	Apply func(Person) (Person, error)

	OpID   string
	Reason string
}
