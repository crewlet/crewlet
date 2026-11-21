package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DECIDES: what a caller asks this domain to do. Each takes ONE snapshot,
// forms its expectation inside it and lets the broker arbitrate; every rule
// runs inside that snapshot, since another writer changes the state concurrently.

// Unique is one of the three values the directory keeps to one holder: a
// person's address, their login and their seat.
//
// Its own type, not seal.go's [Field]: a refusal naming a holder is about
// uniqueness, never sealing.
type Unique string

// The three unique values.
const (
	UniqueEmail Unique = "email"
	UniqueLogin Unique = "login"
	UniqueSeat  Unique = "seat"
)

// ErrTaken reports an address, a login or a seat somebody else holds.
//
// It NAMES THE HOLDER, so the refusal is actionable rather than retried. A
// surface decides how much to show: an administrator is told who, an
// invitation link's holder only that it is taken.
type ErrTaken struct {
	// Field is which value is taken.
	Field Unique

	// Value is the login or seat in the clear, or an address's BLIND: this
	// package never holds an address in the clear.
	Value string

	// Person is who holds it, or empty where an open invitation does.
	Person string

	// Invitation is the open invitation holding the value, or empty. It holds an
	// ADDRESS and a SEAT until redeemed, cancelled or aged out
	// ([heldByInvitation]); the remedy branches on Field: an address is the
	// invitee's, a seat the chart's.
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
// invitation's redemption ([Writer.Redeem]). Both create somebody WHOLE in ONE
// [OpEnrol] record formed by one decide ([Writer.enrol]) that refuses any value
// somebody else holds ([ErrTaken]), so a refusal leaves nothing half-made. Two
// verbs because they differ in seat rule, what holds a value against them, the
// credential and the answer. Every person lands on a human seat (package doc).
// A create confers only what its caller holds ([Writer.MayConfer]), the Tier A
// token included; a redemption, whose writer holds admin, only what its
// invitation carries ([Writer.redeemable]).

// Create creates a person or a service account WHOLE in ONE directory record,
// on an administrator's authority, and for a PERSON issues their first
// password link in it, shown once to the administrator, who holds every grant
// it opens ([Writer.MayConfer]). The seat is refused where a person or an open
// invitation holds it ([heldByInvitation]).
// The link is a `reset` credential ([MethodReset]) stamped with the snapshot's
// counters and derived from the person under the company's key, not minted, so
// a ledger-answered retry, which never reruns the decide, gets the link the
// first attempt issued ([Writer.firstLinkAs]). A service account gets none.
// [Creation.PersonID] is [CreatedPersonID] of the operation key, so a create
// reaching somebody who exists is [ErrOperationReused], never a rewrite.
func (w *Writer) Create(ctx context.Context, in Creation) (Created, error) {
	if err := w.mayAdminister(OpEnrol); err != nil {
		return Created{}, err
	}
	if err := in.validate(w.Now()); err != nil {
		return Created{}, err
	}
	// The writer's own authority reads nothing, so it is asked first.
	if err := w.MayConfer(nil, in.Grants); err != nil {
		return Created{}, err
	}
	// THE SEAT BEFORE THE BLIND: the blind may MINT the company's key, and a
	// refused create must refuse with the seat's remedy, having minted nothing.
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
		// A person always has an address ([Creation.validate]), so its blinder
		// derives the link.
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
		// No link beside an outcome nobody can confirm: it may open no person,
		// and the same key's retry derives it again.
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

// firstLinkAs reads back the first password link after the ledger answered a
// create's retry ([statelog.Result.Collapsed]): the decide never ran, so the
// rows are asked with the spend path's predicate ([ResetRow.Opens]):
//
//   - it opens: the link with its stored expiry;
//   - it no longer does (spent, revoked, ended, aged out, person removed):
//     [Created.LinkClosed], the remedy being a reset link;
//   - this node has not applied the create: [NotReadBack]. Applied with no row
//     is the removal arm, since only a removal deletes the row.
func (w *Writer) firstLinkAs(ctx context.Context, result statelog.Result,
	personID string, link PasswordLink) (Created, error) {

	var (
		found bool
		row   ResetRow
	)
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		person, err := heldPerson(ctx, tx, personID)
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
		return Created{Result: result}, &NotReadBack{Result: result}
	case found && row.Opens(link.Secret, w.Now()):
		link.ExpiresAt = row.ExpiresAt
		return Created{Result: result, Link: &link}, nil
	}
	return Created{Result: result, LinkClosed: true}, nil
}

// Creation is what an administrator's create needs.
type Creation struct {
	// PersonID is [CreatedPersonID] of the operation key, so a retry names the
	// same person. A uuid7: the first link is derived at its instant
	// ([Blinder.PasswordLinkID]).
	PersonID string

	// Kind is a person or a machine — a service account.
	Kind iam.Kind

	// Stage is where the principal lands; a PERSON's is [iam.StageActive] only
	// ([Creation.validate]).
	Stage iam.Stage

	// Name and Email are CLEARTEXT here and nowhere after: sealed before the
	// record is formed. A person's address is REQUIRED (the interactive login
	// key); a service account's is optional.
	Name  string
	Email string

	// Login is REQUIRED, in the kind's grammar: a person's dotted, a machine's
	// coloned.
	Login string

	Grants []iam.Grant

	// Seat is the HUMAN seat the principal is bound to, by its handle
	// (ADR-0013): REQUIRED for a person ([ErrSeatRequired]), optional for a
	// service account.
	Seat string

	// LinkExpiresAt is when a person's first password link stops opening:
	// REQUIRED and ahead of the writer's clock for a person (a zero would mean
	// `never`), refused for a service account. The surface reads it off
	// [credential.EnrolmentLinkLifetime].
	LinkExpiresAt time.Time

	// OpID is the record's operation id in the state log's grammar; see
	// [Redemption.OpID].
	OpID   string
	Reason string
}

// Created is what a create answers.
type Created struct {
	statelog.Result

	// Link is the created PERSON's first password link, on a landing or a
	// ledger-answered retry while it still opens; nil otherwise. Shown once and
	// kept nowhere: the estate holds only its verifier.
	Link *PasswordLink

	// LinkClosed says a retry found the link no longer opens
	// ([Writer.firstLinkAs]): the caller hands on a reset link.
	LinkClosed bool
}

// PasswordLink is a created person's first password link: the `reset`
// credential's id and its secret, which a surface joins as `<id>.<secret>`.
type PasswordLink struct {
	// Credential is the link's credential id ([Blinder.PasswordLinkID]).
	Credential string

	// Secret opens it ([Blinder.PasswordLinkSecret]); the estate keeps only
	// [credential.ResetVerifier] of it.
	Secret string

	// ExpiresAt is when it stops opening.
	ExpiresAt time.Time
}

// validate refuses a create that could not land, before anything is sealed,
// blinded or read. The seat and link come after findability and the login
// grammar, whose refusals name the field typed wrong.
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
		// ACTIVE FROM THE MOMENT IT LANDS: under a stage that may not act the
		// link opens nothing, and one it cannot open ([ResetStages]) would
		// answer a retry with [Created.LinkClosed] where the first got a link.
		return fmt.Errorf("%w: a new person starts as %s, not %q. Add them, "+
			"then change their stage", ErrInvalid, iam.StageActive,
			in.Stage)
	case in.Kind == iam.KindMachine && !in.LinkExpiresAt.IsZero():
		return fmt.Errorf("%w: a service account has no password, so leave "+
			"out the link expiry",
			ErrInvalid)
	case in.Kind == iam.KindPerson && in.LinkExpiresAt.IsZero():
		return fmt.Errorf("%w: a new person's password link "+
			"needs an expiry date",
			ErrInvalid)
	case in.Kind == iam.KindPerson && !in.LinkExpiresAt.After(now):
		return fmt.Errorf("%w: the password link's expiry date %s "+
			"has already passed", ErrInvalid,
			in.LinkExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

// Redeem creates the person an invitation was issued for, binding the seat it
// names and spending its link, in ONE directory record.
//
// The invitation is the authority: its grants, seat and link secret are
// checked in the record's snapshot ([Writer.redeemable]), and the trail names
// its issuer as deciding the grants. The seat must still be a human seat of the
// running company; that refusal is the link's ([ErrRefused]), its remedy a new
// invitation. Only a PERSON holds the seat against it: an older open
// invitation on the same seat must not deadlock both. A retry answered from
// the ledger is refused as a link already used ([Writer.enrol]).
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
		// The seat was checked against a company at issue, so a node running
		// none has not applied it yet. Never "create a company first": that is
		// advice to an administrator.
		return statelog.Result{}, fmt.Errorf("%w: this invitation is for "+
			"seat %q, and this node has no company yet. Try again shortly "+
			"(%v)", statelog.ErrUnavailable, in.Seat, err)
	case errors.Is(err, statelog.ErrUnavailable):
		return statelog.Result{}, err
	case err != nil:
		// The link's refusal alone: wrapping the seat check's error would tell
		// the invitee they sent a bad request.
		return statelog.Result{}, forbidden("this invitation is for a "+
			"seat that's no longer available").about("invitation %s: %v",
			in.Invitation, err)
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
	// PersonID is minted per ATTEMPT, a uuid7: a refused attempt published
	// nothing, and the spent link keeps it single-use.
	PersonID string

	Stage iam.Stage

	// Name and Email are CLEARTEXT here and nowhere after. Email is the address
	// the invitation was issued to; the record refuses any other (holding
	// somebody's link is not holding their address).
	Name  string
	Email string

	// Login is the one the invitee chose: dotted, a person's grammar.
	Login string

	// Password is the ONE credential a redemption carries: the chosen password
	// as an argon2id verifier ([MethodPassword]). Nothing the invitee presented
	// could vouch for any other.
	Password Credential

	// Grants and Seat are what the invitation carries; the record refuses
	// anything else ([Writer.redeemable]).
	Grants []iam.Grant
	Seat   string

	// Invitation is the id of the invitation this redeems.
	Invitation string

	// InvitationSecret is the secret the link carries beside its id
	// ([Blinder.InvitationSecret]), REQUIRED: the id is in every snapshot,
	// backup and proxy log, and more than the route can publish a record, so
	// the record checks it against the verifier in its own snapshot.
	InvitationSecret string

	// OpID is the record's operation id, in the state log's grammar
	// ([statelog.NewOpID], [statelog.DeriveOpID]): the ledger vouches for a
	// retry by its instant, so an id outside it reads as minted at the epoch
	// and, once the ledger loses a row, every retry answers `unknown`.
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
		// Refused, not invalid: the remedy is a new link, as for any dead one.
		return forbidden("this invitation needs the full link, not just "+
			"its id").about("invitation %s", in.Invitation)
	case in.Seat == "":
		// A caller's fault: every invitation holds a seat ([Writer.Invite]), and
		// [ErrSeatRequired] would ask the invitee for a field they lack.
		return fmt.Errorf("%w: redeeming an invitation binds the human seat "+
			"it holds, and this redemption names none", ErrInvalid)
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

// enrolment is one [OpEnrol] record's inputs. invitation is empty for a create:
// the one field telling the decide's two authorities apart.
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
// writer holds a keyring and, for an address, the blind-index key. No address
// is no blinder and no blind.
func (w *Writer) enrolmentBlind(ctx context.Context, email string) (
	*Blinder, string, error) {

	if w.sealer == nil || (email != "" && w.blinds == nil) {
		return nil, "", fmt.Errorf("iamdomain: this node can't add "+
			"people: it has %s", w.missingKeys())
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
// Everything a snapshot decides is formed inside the decide, which may run
// more than once, including the COUNTERS a credential is stamped with
// ([stampIssued]): a first link is ended by any move of its person's epoch or
// the session generation after its issue ([Credential.EndedBy]), so stamped
// zero it would end at the first company-wide invalidation. Sealing stays
// outside: it reads no snapshot and would draw a fresh nonce on every run.
func (w *Writer) enrol(ctx context.Context, e enrolment) (statelog.Result, error) {
	// SEALED ONCE, before the decide. A machine has no mailbox, but its name
	// (`Release pipeline, raised by Dana`) can name a person, and a removal
	// erases it.
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
	// seat's bind stamps, and for a redemption its invitation's address bucket.
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
	// WHO DECIDED WHAT IT CONFERS: this writer's party for a create, the
	// invitation's issuer for a redemption ([Writer.redeemable]), never the
	// redeemer.
	decidedBy, decidedVia := w.Actor, w.OperatorID
	decide := func(tx *sql.Tx) (_ []byte, err error) {
		if err = wholeDirectory(ctx, tx); err != nil {
			return nil, err
		}
		if e.invitation != "" {
			// The one read of the invitation that decides. The redemption
			// acted through none of its issuer's credentials.
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
			// AN OPEN INVITATION HOLDS ITS ADDRESS against a create. A
			// redemption IS that invitation ([Writer.redeemable]).
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
			// AND ITS SEAT. A redemption is held to persons alone: asked of
			// both invitations, each link would refuse the other.
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
		// A REDEMPTION COLLAPSED INTO A COPY THIS CALL CANNOT PROVE ITS OWN IS
		// A LINK ALREADY USED: the ledger answers before the decide, so its
		// spent-link refusal is made here. Answered as landed, it would open a
		// session for whoever holds the link, past any second factor enrolled
		// since. This call's own ambiguous append is refused too, truly: the
		// person is enrolled.
		return statelog.Result{}, e.alreadyEnrolled()
	}
	// An enrolment conferring anything is a grant change from nothing,
	// announced by whoever decided it.
	if added, _ := grantDelta(nil, e.grants); len(added) > 0 {
		w.announce(ctx, result, err, types.IAMGrantsChanged{
			Person: e.personID, Added: added, By: decidedBy,
			OperatorID: decidedVia, Version: result.Position.Packed(),
		})
	}
	return result, err
}

// createsNobodyTwice refuses a create of a person who already exists: the
// person derives from the operation key ([CreatedPersonID]), so reaching an
// existing one is a second request under one key that would rewrite them
// ([ErrOperationReused]).
//
// The create's own retry never gets here: the ledger answers before any decide
// ([statelog.Snap.Held]), and where it lost the row this refusal becomes
// `unknown` ([statelog.Result.Unvouched]). A redemption needs no guard: its
// person is fresh per attempt and the link keeps it single-use.
func (w *Writer) createsNobodyTwice(ctx context.Context, tx *sql.Tx,
	e enrolment) error {

	enrolled, err := isEnrolled(ctx, tx, e.personID)
	if err != nil || !enrolled {
		return err
	}
	return e.alreadyEnrolled()
}

// alreadyEnrolled is the refusal when what an enrolment would create exists,
// in the terms of its authority: a REDEMPTION's link has been used; an
// ADMINISTRATOR's key already names somebody, and a new person takes a new key.
func (e *enrolment) alreadyEnrolled() error {
	if e.invitation != "" {
		return forbidden("this invitation has already been used").
			about("invitation %s", e.invitation)
	}
	// Names neither person nor request: the person derives from the key.
	return fmt.Errorf("%w: it already added somebody. Use a new key to add "+
		"someone new", ErrOperationReused)
}

// isEnrolled reports whether a person row exists, in a decide's snapshot.
func isEnrolled(ctx context.Context, tx *sql.Tx, personID string) (bool, error) {
	var enrolled bool
	if err := tx.QueryRowContext(ctx, enrolledStatement, personID).
		Scan(&enrolled); err != nil {
		return false, fmt.Errorf("iamdomain: read whether %s is enrolled: %w",
			personID, err)
	}
	return enrolled, nil
}

// enrolledStatement is whether a row holds one person's key.
const enrolledStatement = `SELECT EXISTS(SELECT 1 FROM iam_people WHERE id = ?)`

// invitationStateStatement is one invitation's address, deadline, redemption
// and document, by its key: what a redemption is judged against
// ([Writer.redeemable]) and what a retried issue compares its terms with
// ([Writer.issuedBefore]).
const invitationStateStatement = `
	SELECT email_blind, expires_at, redeemed_at, document
	  FROM iam_invites WHERE id = ?`

// validateEnrolment is the validation a create and a redemption share: ids,
// kind, stage, bounds, findability and the login grammar, naming the field an
// enrolment is refused for. [Writer.record] still checks every reason.
func validateEnrolment(e enrolment) error {
	switch {
	case e.personID == "":
		return errors.New("iamdomain: an enrolment needs the person id it is " +
			"creating")
	case e.opID == "":
		return errors.New("iamdomain: an enrolment needs an operation id — " +
			"without a stable one a retry cannot tell whether it already landed")
	case e.kind != iam.KindPerson && e.kind != iam.KindMachine:
		// The directory holds people and machines only: a seat is the chart's
		// and the engine the node, so a row of either would be a second
		// answer to who they are.
		return fmt.Errorf("%w: %q isn't a kind (use %s or %s)",
			ErrNotEnrollable, e.kind, iam.KindPerson, iam.KindMachine)
	case !e.stage.Valid():
		return fmt.Errorf("%w: %q is not an enrolment stage", ErrInvalid, e.stage)
	case len(e.name) > MaxName:
		return fmt.Errorf("%w: the name is %d bytes, and the limit is %d",
			ErrInvalid, len(e.name), MaxName)
	case len(e.email) > MaxAddress:
		return fmt.Errorf("%w: the email address is %d bytes, "+
			"and the limit is %d", ErrInvalid, len(e.email), MaxAddress)
	case len(e.reason) > MaxReason:
		return fmt.Errorf("%w: the reason is %d bytes, and the limit is %d. "+
			"Keep it to a short label", ErrInvalid, len(e.reason), MaxReason)
	case e.kind == iam.KindPerson && iam.NormalizeEmail(e.email) == "":
		// Judged on what it folds to, which is what finds them.
		return fmt.Errorf("%w: a person needs an email address to sign in",
			ErrNotFindable)
	case e.kind == iam.KindMachine && e.login == "":
		// A machine needs no address but must be FINDABLE: an unnamed row is
		// a credential holder nobody can list, revoke or audit.
		return fmt.Errorf("%w: a %s needs a login", ErrNotFindable, e.kind)
	case e.login == "":
		// A PERSON NEEDS ONE TOO: without one they are recorded as
		// `anonymous` and fail [iam.Principal.Validate].
		return fmt.Errorf("%w: a person needs a login: lowercase words "+
			"joined by dots, like jane.doe", ErrInvalidLogin)
	}
	return loginFits(e.kind, e.login)
}

// redeemable refuses an enrolment its invitation does not cover, in the
// record's snapshot: a grant it did not carry, an address it was not issued to,
// a seat it did not bind, a link spent or aged out. It answers who issued it,
// who decided what the redemption confers.
//
// The writer's clock decides expiry, as [heldByInvitation]'s does. A zero
// expiry is AGED OUT, never open for ever: no writer forms one, and the address
// hold and the listing read it as closed too.
func (w *Writer) redeemable(ctx context.Context, tx *sql.Tx,
	e enrolment) (string, error) {

	var (
		held           string
		expires, spent int64
		document       []byte
	)
	err := tx.QueryRowContext(ctx, invitationStateStatement, e.invitation).
		Scan(&held, &expires, &spent, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", forbidden("this node doesn't have this invitation").
			about("invitation %s", e.invitation)
	case err != nil:
		return "", fmt.Errorf("iamdomain: read invitation %s: %w", e.invitation, err)
	}
	invitation, err := DecodeInvitation(document)
	if err != nil {
		return "", fmt.Errorf("iamdomain: open invitation %s: %w", e.invitation, err)
	}
	switch {
	case !invitationAdmits(invitation.Verifier, e.invitationSecret):
		// The secret is asked here because more than the route can publish a
		// record, and the id is in every snapshot and proxy log. No verifier
		// admits nobody.
		return "", forbidden("that isn't this invitation's link").
			about("invitation %s", e.invitation)
	case held != e.blind:
		return "", forbidden("this invitation is for a different email "+
			"address").about("invitation %s", e.invitation)
	case spent != 0:
		return "", forbidden("this invitation has already been used").
			about("invitation %s", e.invitation)
	case expires == 0 || !w.Now().Before(time.UnixMilli(expires)):
		return "", forbidden("this invitation has expired").
			about("invitation %s", e.invitation)
	}
	// Read through [iam.Expand]: admin offers every app, and a newer peer's
	// unknown name is kept and ignored rather than refusing the link.
	offered := iam.Expand(invitation.Grants)
	for _, g := range e.grants {
		if !slices.Contains(offered, g) {
			return "", forbidden("this invitation doesn't give %s", g).
				about("invitation %s gives %s", e.invitation,
					grantWords(invitation.Grants))
		}
	}
	// Exactly the invitation's seat: another asks for more than was offered,
	// none spends the link without the binding its issuer decided.
	if e.seat != invitation.Seat {
		return "", forbidden("this invitation is for seat %q, not %q",
			invitation.Seat, e.seat).about("invitation %s", e.invitation)
	}
	return invitation.InvitedBy, nil
}

// loginFits refuses a login outside its kind's grammar, never "either one"
// ([iam.ValidLoginFor]): a person taking `token:ops` would make the `ops` Tier
// A token act as their seat. Checked AS TYPED, so `Sarah.Chen` is refused
// naming itself, not lowered into a login nobody wrote.
func loginFits(kind iam.Kind, login string) error {
	if iam.ValidLoginFor(kind, login) {
		return nil
	}
	shape := "Use lowercase words joined by dots, like jane.doe"
	if kind == iam.KindMachine {
		shape = "Use lowercase words joined by a colon, like ci:release (or " +
			"token:<id> for an API token)"
	}
	return fmt.Errorf("%w: %q isn't a valid %s login. %s, up to %d characters",
		ErrInvalidLogin, login, kind, shape, iam.MaxLogin)
}

// ErrInvalidLogin reports a login outside its kind's grammar: its own sentinel
// so a surface answers 400.
var ErrInvalidLogin = errors.New("invalid login")

// ErrInvalid reports a value outside a bound this domain holds a record to: a
// reason past [MaxReason], a stage that is not one. A surface answers 400.
var ErrInvalid = errors.New("invalid value")

// ErrSeatRequired reports a write that would leave a PERSON without a human
// seat: a create or an invitation naming none, or an identity change clearing
// one. It wraps [ErrInvalid], so surfaces still answer 400, and is its own
// sentinel so a surface can name the field and remedy (`seat_required`): a
// seat is moved, or freed by removal, never cleared (ADR-0032).
var ErrSeatRequired = fmt.Errorf("%w: every person needs a human seat",
	ErrInvalid)

// ErrNoCompany reports a seat that cannot be checked because this node runs no
// company yet, so holds no human seat.
//
// Not [statelog.ErrUnavailable] ("come back"): waiting never clears it, a
// company declaring a human seat does (409 `no_active_revision`). It is the
// domain's reading of [session.ErrNoCompany].
var ErrNoCompany = errors.New("this node has no company yet, so " +
	"there are no seats")

// ErrNotEnrollable reports an enrolment of a kind the directory does not hold.
var ErrNotEnrollable = errors.New("only people and service " +
	"accounts can be added")

// ErrNotFindable reports an enrolment creating somebody nothing can look up: a
// person without an address (the interactive login key), or a machine without
// a login. One sentinel covers their shared consequence, a credential holder
// nobody can list, revoke or audit.
var ErrNotFindable = errors.New("missing an email or login")

// SetIdentity changes an enrolled person's LOGIN, their SEAT binding, or both,
// in ONE directory record stating the post-state, so the value given up is
// free the moment it lands.
//
// A PERSON's seat is moved, never cleared ([ErrSeatRequired], ADR-0032),
// judged on the kind the SNAPSHOT holds and before the no-op arm; a SERVICE
// ACCOUNT may be unbound. A login outside its grammar ([loginFits]), or a login
// or seat somebody or an open invitation holds ([heldByInvitation]) is refused
// naming the holder ([ErrTaken]); a change that changes nothing publishes no
// record. Every seat it binds is a human seat ([Writer.humanSeat]).
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
		// A LOGIN IS NEVER CLEARED: a principal with none is recorded as
		// nobody, and a `token:<id>` machine would lose its Tier A token.
		return statelog.Result{}, fmt.Errorf("%w: a login can be "+
			"changed but not removed", ErrInvalidLogin)
	}
	// The seat is asked of the running organisation (in memory) before the
	// snapshot, and recorded by its handle.
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
	// THE SCOPE: the person's bucket, and the leavers of a seat it binds,
	// whose removal tombstones the bind stamps ([reboundRemovals]).
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
			// The rule is about what the row IS, not what the caller says.
			return nil, fmt.Errorf("%w: they're a person, so their seat is "+
				"moved to another human seat or freed by removing them, "+
				"never cleared", ErrSeatRequired)
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
			// NOTHING TO MOVE: an answer, not a refusal.
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
			// AN OPEN INVITATION HOLDS ITS SEAT: binding somebody else there
			// would refuse its redemption.
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

	// Seat is the HUMAN seat to bind them to, by its handle (ADR-0013), or nil
	// to leave it. "" unbinds a SERVICE ACCOUNT and is refused for a person
	// ([ErrSeatRequired]).
	Seat *string

	OpID   string
	Reason string
}

// Revoke bumps a person's revocation epoch, ending every session they hold.
//
// NO GRANT IS REQUIRED: the fastest response to a stolen cookie must not need
// an administrator. The new epoch is read in the snapshot and STATED on the
// record, never incremented by the applier: two nodes at one checkpoint may
// have seen the same set in different orders.
func (w *Writer) Revoke(ctx context.Context, personID, opID, reason string) (
	statelog.Result, error) {

	if personID == "" || opID == "" {
		return statelog.Result{}, errors.New("iamdomain: a revocation needs " +
			"a person and an operation id")
	}
	// FORMED INSIDE THE DECIDE, which may rerun on a fresh snapshot, so the
	// mutation must be the last run's: [Writer.request] takes the record by
	// pointer.
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
// token. It is the last step of a restore, which rolls back revocations made
// after the copy; only a fleet-wide generation bump ends those sessions without
// knowing which they were (why a token is minted at the generation,
// [Writer.MintToken]).
//
// Unlike [Writer.Revoke] it is ADMIN'S and never through a machine token
// ([Writer.mayAdminister]; internal/authz's row also asks a person present and
// a recent proof). A Tier A token is no machine token, so the restore
// runbook's CLI step runs when nobody can sign in. The generation is stated on
// the record for [Writer.Revoke]'s reason.
func (w *Writer) InvalidateAll(ctx context.Context, opID, reason string) (
	statelog.Result, error) {

	if err := w.mayAdminister(OpInvalidate); err != nil {
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
		// NEVER BUMPED IS ZERO, so the first bump lands at 1.
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
		return statelog.Result{}, fmt.Errorf("%w: %q isn't a stage "+
			"this engine knows",
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

// Remove is the one operation here with no inverse, and it rides the DIRECTORY
// subject: it frees a login, an address and a seat.
//
// It reads what it releases inside the snapshot, for the record to carry: the
// tombstone outlives the row and answers "who held this address" (as BLINDS).
// It takes nothing, so it skips [wholeDirectory]: the framework's probe refuses
// a retained record about the same person, and [removedPersonIn] a second
// removal.
func (w *Writer) Remove(ctx context.Context, personID, opID, reason string) (
	statelog.Result, error) {

	if err := w.mayAdminister(OpRemove); err != nil {
		return statelog.Result{}, err
	}
	if personID == "" || opID == "" {
		return statelog.Result{}, errors.New("iamdomain: a removal needs a " +
			"person and an operation id")
	}
	// THE SCOPE: the person's bucket and their address's, whose rows the erasure
	// clears ([eraseSealed]).
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
		// NO ROW STILL PUBLISHES: a person this snapshot lacks is removed with
		// an EMPTY released set, and the tombstone holds the id against every
		// later record naming it.
		held, err := heldIdentifiers(ctx, tx, personID)
		if err != nil {
			return nil, err
		}
		return EncodeRemoval(Removal{V: GateRecordVersion, Released: held})
	}
	return w.publishDirectory(ctx, OpRemove, personID, opID, reason, scopeOf,
		decide)
}

// heldIdentifiersStatement is the three unique values one person's row holds,
// by its key.
const heldIdentifiersStatement = `
	SELECT email_blind, login, seat_id FROM iam_people WHERE id = ?`

// heldIdentifiers reads the unique values a person holds in one snapshot —
// nothing, for a person this snapshot does not hold.
func heldIdentifiers(ctx context.Context, tx *sql.Tx, personID string) (
	Identifiers, error) {

	var held Identifiers
	err := tx.QueryRowContext(ctx, heldIdentifiersStatement,
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
// NO GRANT: signing in is how a person acquires authority. The epoch and the
// generation are READ HERE, in the snapshot, never stated by the caller: a
// bearer is over once either moves past what it carries, and a caller's value
// from another transaction, if stale, makes every new session read as ended.
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
	// THE LAST RUN'S COUNTERS, which the landed record carries.
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
			UserAgent:         clipUserAgent(in.UserAgent),
			Remote:            in.Remote,
		})
		return err
	}
	req := w.request(ctx, &rec, in.OpID, statelog.PatternCreate, decide)
	// The one write here that does not wait for its own row: see
	// [SessionStart.NoWait].
	req.NoWait = in.NoWait
	result, err := w.publish(ctx, req)
	if err == nil && result.Collapsed {
		// A collapsed answer is a copy this call cannot prove its own, so the
		// counters may describe a decision nothing published. The generation is
		// on no row to read back, and a bearer carrying a wrong one survives a
		// fleet-wide invalidation. See [ErrCollapsed].
		return SessionOpened{Result: result}, ErrCollapsed
	}
	opened.Result = result
	return opened, err
}

// MaxSessionUserAgent is the most of a User-Agent header a session keeps, in
// bytes: past the longest a browser sends (under 300), while an arbitrarily
// long header does not ride every session row on every data node.
const MaxSessionUserAgent = 512

// clipUserAgent is ua held to [MaxSessionUserAgent] bytes, cut at a rune
// boundary.
func clipUserAgent(ua string) string {
	ua = strings.TrimSpace(ua)
	if len(ua) <= MaxSessionUserAgent {
		return ua
	}
	cut := MaxSessionUserAgent
	for cut > 0 && !utf8.RuneStart(ua[cut]) {
		cut--
	}
	return ua[:cut]
}

// SessionOpened is what a bearer for a session that has just begun carries
// beside its lineage.
type SessionOpened struct {
	// Result is the write's own answer. Its POSITION, which the bearer carries,
	// lets a node below it tell "not seen yet" from "ended"; an UNKNOWN outcome
	// has none, so no bearer is minted from it (zero reads as ended everywhere).
	Result statelog.Result

	// Epoch is the person's revocation epoch and Generation the fleet's session
	// generation, as the record's snapshot held them. A bearer carrying
	// anything below either is over.
	Epoch      uint64
	Generation uint64
}

// epochOf is one person's revocation epoch, read inside a decide. No row is
// zero, a real value: [Writer.Revoke] increments from it.
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

// generationOf is the fleet's session generation, read inside a decide. NEVER
// BUMPED IS ZERO, which a bearer minted before any invalidation carries.
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

// SessionStart is what opening a session needs. No epoch and no generation:
// both are read inside the decide ([Writer.OpenSession]).
type SessionStart struct {
	Lineage           string
	Person            string
	AbsoluteExpiresAt time.Time
	OpID              string

	// ProvedAt is when the holder proved who they are, or zero for a session
	// opened on no proof ([Session.ProvedAt]). The WRITER's clock, like the
	// absolute deadline beside it.
	ProvedAt time.Time

	// EnrolmentOnly opens a session that may do nothing but enrol a second
	// factor ([Session.EnrolmentOnly]); the SIGN-IN decides it.
	EnrolmentOnly bool

	// UserAgent and Remote are the browser and address that asked
	// ([Session.UserAgent]), empty where nothing names them.
	UserAgent string
	Remote    string

	// NoWait asks for the answer the broker's acknowledgement establishes,
	// rather than waiting for this node's applier. Correct for a SIGN-IN only
	// (never a session replacing another): its bearer carries the record's
	// POSITION, and a node below it serves on the signature and epoch alone
	// ([session.RowBehind]), so waiting costs 250-500 ms of a parked fetch for
	// a row nothing in the answer reads. Nowhere else: a directory write
	// answers once this node's rows carry it, since the next read comes here.
	NoWait bool
}

// CloseSession ends one session, keeping its row until the sweep collects it.
//
// The caller names the session's PERSON: validation asks whether a deferral
// covers the person's bucket, so the close is filed there (under the lineage, a
// node that could not decode it kept serving the session). The decide confirms
// the person against this node's row where there is one.
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
			return forbidden("that session isn't theirs").
				about("session %s belongs to person %s, not %s", lineage,
					owner, person)
		}
		return nil
	}
	result, err := w.publish(ctx, w.request(ctx, &rec, opID, statelog.PatternArbitrated, decide))
	return result, err
}

// missingKeys names which key-bearing seam this writer lacks.
func (w *Writer) missingKeys() string {
	switch {
	case w.blinds == nil && w.sealer == nil:
		return "no email lookup key and no keyring"
	case w.blinds == nil:
		return "no email lookup key"
	default:
		return "no keyring to seal with"
	}
}

// taken refuses a value somebody other than except holds, read INSIDE a
// directory decide's snapshot, naming the holder. The statement is picked from
// [takenStatements], spelled whole: nothing a caller passes reaches the SQL,
// and the plan gate reads every statement it can pick.
func taken(ctx context.Context, tx *sql.Tx, field Unique, value,
	except string) error {

	statement, ok := takenStatements[field]
	if !ok {
		return fmt.Errorf("iamdomain: %q is not a value the directory keeps "+
			"to one holder", field)
	}
	var holder string
	err := tx.QueryRowContext(ctx, statement, value, except).Scan(&holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read who holds the %s %s: %w", field,
			value, err)
	}
	return &ErrTaken{Field: field, Value: value, Person: holder}
}

// takenStatements is who else holds each unique value, each a seek of its own
// plain index: a PARTIAL index over non-empty rows cannot serve a bound value,
// which implies nothing about being non-empty.
var takenStatements = map[Unique]string{
	UniqueEmail: `SELECT id FROM iam_people WHERE email_blind = ? AND id <> ? LIMIT 1`,
	UniqueLogin: `SELECT id FROM iam_people WHERE login = ? AND id <> ? LIMIT 1`,
	UniqueSeat:  `SELECT id FROM iam_people WHERE seat_id = ? AND id <> ? LIMIT 1`,
}

// identityOfStatement is one person's kind and unique values, by key.
const identityOfStatement = `
	SELECT kind, email_blind, login, seat_id FROM iam_people WHERE id = ?`

// identityOf is an enrolled person's kind and the unique values they hold,
// read inside a decide's snapshot, or [ErrNotFound].
func identityOf(ctx context.Context, tx *sql.Tx, personID string) (
	iam.Kind, Identifiers, error) {

	var (
		kind string
		held Identifiers
	)
	err := tx.QueryRowContext(ctx, identityOfStatement,
		personID).Scan(&kind, &held.EmailBlind, &held.Login, &held.SeatID)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", Identifiers{}, fmt.Errorf("%w on this node", ErrNotFound)
	case err != nil:
		return "", Identifiers{}, fmt.Errorf("iamdomain: read person %s: %w",
			personID, err)
	}
	return iam.Kind(kind), held, nil
}

// humanSeat is THE check every binding takes, answering the handle the running
// organisation knows the seat by. Every binding names a HUMAN seat: a principal
// on an agent seat writes as an agent, and the request path refuses it
// ([session.ResolveSeat]); a Tier A token bound through its `token:<id>` row
// acts as the seat too.
//
// ADVISORY: the organisation is the applied configuration, unordered with this
// log, so a revision may remove the seat after this passes; the residue is a
// 403 naming the seat. It catches a typo or an agent's seat while somebody can
// fix it. No company is [ErrNoCompany]; no organisation or a failed lookup is
// [statelog.ErrUnavailable]. Never a bind made unchecked.
func (w *Writer) humanSeat(ctx context.Context, handle string) (string, error) {
	if w.seats == nil {
		return "", fmt.Errorf("%w: this node can't check seat %q yet. "+
			"Try again shortly", statelog.ErrUnavailable, handle)
	}
	seat, found, err := w.seats.Seat(ctx, handle)
	switch {
	case errors.Is(err, session.ErrNoCompany):
		return "", fmt.Errorf("%w: seat %q cannot be bound until the company "+
			"declaring it is running here (%w)", ErrNoCompany, handle, err)
	case err != nil:
		return "", fmt.Errorf("%w: this node couldn't read the org chart to "+
			"check seat %q: %w", statelog.ErrUnavailable, handle, err)
	case !found:
		return "", fmt.Errorf("%w: there's no seat %q in the org chart. If "+
			"it was just added, try again in a moment", ErrInvalid, handle)
	case seat.Kind != session.SeatKindHuman:
		return "", fmt.Errorf("%w: seat %q isn't a human seat (it's %s). "+
			"Pick a human seat", ErrInvalid,
			handle, seat.Kind)
	}
	return seat.Handle, nil
}

// heldPersonStatement is one person's document and stage column, by key.
const heldPersonStatement = `SELECT document, stage FROM iam_people WHERE id = ?`

// heldPerson is one person's document, read in the caller's snapshot for a
// read-modify-write, carrying the stage the ROW holds: the status op moves the
// column without authoring a document, so the column is the authority.
// Re-publishing the document's own stage could put back `active`, letting a
// suspended person in when their grants are corrected.
func heldPerson(ctx context.Context, tx *sql.Tx, personID string) (
	Person, error) {

	var (
		document []byte
		stage    string
	)
	err := tx.QueryRowContext(ctx, heldPersonStatement, personID).
		Scan(&document, &stage)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// The same words as [identityOf]'s.
		return Person{}, fmt.Errorf("%w on this node", ErrNotFound)
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

// SetCredentials replaces a person's credential set, reading their row inside
// the snapshot to form the new whole: a set formed from another transaction's
// read pairs an old decision with a new expectation, which the broker accepts.
//
// No grant: changing your own second factor is the ordinary case. The SURFACE
// bounds it: the route is guarded, step-up applies, and the person id comes off
// the resolved principal, not a body.
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
		person, err := heldPerson(ctx, tx, in.PersonID)
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

	// Apply forms the new set from the current one INSIDE the snapshot, a
	// function rather than a list so expectation and decision share it. It may
	// refuse, as [PersonUpdate.Apply] may, on a rule about the set the write
	// LANDS on; the refusal leaves the decide unwrapped, unpublished.
	//
	// On a collapsed result ([statelog.Result.Collapsed]) what it formed is not
	// what landed: it may never have run, or ran in an unpublished round. A
	// caller shows what it formed (recovery codes, a seed) only on a result that
	// is not collapsed, and otherwise retries under a fresh operation id.
	Apply func([]Credential) ([]Credential, error)

	OpID   string
	Reason string
}

// MintToken adds a machine token (a personal access token, or a service
// account's) to its owner's credentials, reading the OWNER in its own
// snapshot: what a token may carry is a fact about its owner now, and on the
// owner's subject a mint and a demotion contend. It refuses a grant the owner
// lacks ([iam.Expand]; a token narrows; admin is mintable, kept off presence
// gestures by internal/authz and [Writer.mayAdminister]) or the minting party
// lacks ([Writer.MayConfer]), another person's mint or a service account's
// without admin ([Writer.mayMintFor]), an owner who may not act or binds a Tier
// A token, and an expiry missing, past or beyond [credential.MaxTokenLifetime].
// Stamped with the owner's epoch and the company's generation, it ends when
// either moves; nil grants resolve here ([Writer.tokenGrants]).
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
		return TokenMinted{}, fmt.Errorf("%w: the token label is %d bytes, "+
			"and the limit is %d", ErrInvalidToken,
			len(in.Label), MaxTokenLabel)
	case in.ExpiresAt.IsZero():
		return TokenMinted{}, fmt.Errorf("%w: a token needs an expiry date",
			ErrInvalidToken)
	}
	now := w.Now()
	switch {
	case !in.ExpiresAt.After(now):
		return TokenMinted{}, fmt.Errorf("%w: the expiry date %s "+
			"has already passed",
			ErrInvalidToken, in.ExpiresAt.UTC().Format(time.RFC3339))
	case in.ExpiresAt.Sub(now) > credential.MaxTokenLifetime:
		return TokenMinted{}, fmt.Errorf("%w: a token can last %d days at most",
			ErrInvalidToken, int(credential.MaxTokenLifetime/(24*time.Hour)))
	}

	rec, err := w.record(PersonSubject(in.PersonID), OpUpdate, in.PersonID,
		PeopleScope(in.PersonID), nil, in.Reason)
	if err != nil {
		return TokenMinted{}, err
	}
	// THE LAST RUN'S ANSWER, which the landed record carries.
	var minted TokenMinted
	decide := func(tx *sql.Tx) (err error) {
		owner, err := heldPerson(ctx, tx, in.PersonID)
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
		// That row is a Tier A token's place: a token minted on it would act
		// under the config file's name.
		if owner.Kind == iam.KindMachine &&
			strings.HasPrefix(login, iam.TokenLoginPrefix) {
			return forbidden("%s belongs to an API token, not an "+
				"account, so it can't have access tokens", login)
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
				// THE SAME MINT, RETRIED: a second copy of one id would be two
				// credentials a revocation names as one.
				continue
			}
			kept = append(kept, c)
		}
		kept = append(kept, token)
		// THE NEW TOKEN IS LIVE, so making room never drops it: only a
		// credential already not verifying gives up its place.
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
		// A collapsed answer is a copy this call cannot prove its own, so these
		// grants may describe nothing published; the copy that landed is a token
		// nobody was shown, and it expires. See [ErrCollapsed].
		return TokenMinted{Result: result}, ErrCollapsed
	}
	minted.Result = result
	return minted, err
}

// TokenMint is what minting a machine token needs.
type TokenMint struct {
	// PersonID is the owner: the person or machine the token acts as. Who mints
	// is the writer's party ([Writer.Principal]), never a caller's field
	// ([Writer.mayMintFor]).
	PersonID string

	// ID is the credential's own id, minted by the caller because the verifier
	// and the value, both formed before this runs, contain it.
	ID string

	// Verifier is [credential.TokenVerifier] over the id and the secret.
	// NEVER the secret: this log is replicated, snapshotted and backed up.
	Verifier string

	Label string

	// Grants is what the token may carry, or nil for every grant the owner
	// holds that the minting party may confer. See [Writer.MintToken].
	Grants []iam.Grant

	// ExpiresAt is REQUIRED, and at most [credential.MaxTokenLifetime]
	// away.
	ExpiresAt time.Time

	OpID   string
	Reason string
}

// TokenMinted is what a mint landed carrying, and where.
type TokenMinted struct {
	// Result is the write's own answer. Its POSITION, which the token's value
	// carries, makes a node below it answer "not yet" rather than "no such
	// token"; an UNKNOWN outcome has none, so no value may be formed from it.
	Result statelog.Result

	Grants     []iam.Grant
	ExpiresAt  time.Time
	Epoch      uint64
	Generation uint64
}

// MaxTokenLabel is a token label's limit in bytes: well above any name a person
// gives one, short enough to stay readable in a credential listing.
const MaxTokenLabel = 128

// ErrInvalidToken reports a malformed mint request, answered 400.
var ErrInvalidToken = errors.New("invalid token request")

// tokenGrants resolves what a token carries and refuses what it may not.
//
// OMITTED, it is every permission this build knows that the owner holds
// ([iam.AdminServe]'s effective set, admin included, in [iam.AllGrants] order;
// the node a token is presented to applies its own posture) less any the
// minting party lacks, since whoever mints is shown the value
// ([Writer.MayConfer]). So where `api.auth.admin` is `refuse` a default mint
// carries the owner's apps rather than being refused for admin. ASKED, every
// name must be one the owner holds through [iam.Expand]; an unknown name is
// refused, never carried for a year.
func (w *Writer) tokenGrants(asked, owner []iam.Grant) ([]iam.Grant, error) {
	grants := asked
	if grants == nil {
		grants = []iam.Grant{}
		for _, g := range iam.AdminServe.Effective(owner) {
			if w.Can(g) {
				grants = append(grants, g)
			}
		}
	}
	holds := iam.Expand(owner)
	for _, g := range grants {
		switch {
		case !g.Valid():
			// No permission at all ([Writer.MayConfer]), not one the owner lacks.
			return nil, forbidden("%s", iam.NotAGrant(g))
		case !slices.Contains(holds, g):
			// Worded for the owner and for an administrator minting for a service account.
			return nil, forbidden("a token can only have what its owner "+
				"has, and its owner doesn't have %s", g).
				about("the owner holds %s", grantWords(owner))
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
		return forbidden("a token can only act for a person or a service "+
			"account").about("owner %s is a %s", personID, owner.Kind)
	case !owner.Stage.MayAct():
		return forbidden("they're %s, so a token can't act for them",
			owner.Stage).about("owner %s", personID)
	}
	return nil
}

// mayMintFor refuses a mint the writer's party ([Writer.Principal], never a
// call field) may not make on this owner's account, as the snapshot holds the
// owner. Whoever mints sees the value and the token acts as its owner, so a
// PERSON's token is minted by that person alone, compared by ID since a rename
// moves logins, admin included; a SERVICE ACCOUNT's needs admin, and only that
// refusal names a permission ([Refusal]). Nothing is minted through a machine
// token ([ErrTokenRefused]: it would renew for ever with nobody present) or the
// Dashboard Assistant ([ErrAssistRefused]); both are composed as the owner, so
// each lock reads the operator id's CLASS, as [Writer.mayAdminister] does.
func (w *Writer) mayMintFor(in TokenMint, owner Person) error {
	if strings.HasPrefix(w.OperatorID, iam.MachineTokenPrefix) {
		return forbidden("%w. A token can't create another, so sign in to "+
			"create one", ErrTokenRefused).about("through %s", w.OperatorID)
	}
	if strings.HasPrefix(w.OperatorID, iam.AssistPrefix) {
		return forbidden("%w. Create the token yourself from your account "+
			"page (%s)", ErrAssistRefused, w.OperatorID)
	}
	switch owner.Kind {
	case iam.KindPerson:
		if w.Principal == "" || w.Principal != in.PersonID {
			return forbidden("only they can create their own access "+
				"tokens. Admins create tokens for service accounts only").
				about("owner %s, a person", in.PersonID)
		}
	case iam.KindMachine:
		if !w.Can(iam.GrantAdmin) {
			return needs([]iam.Grant{iam.GrantAdmin}, "creating a token for "+
				"a service account needs %s", iam.GrantAdmin).
				about("service account %s", in.PersonID)
		}
	}
	return nil
}

// loginOf is one person's login, read inside a decide's snapshot.
func loginOf(ctx context.Context, tx *sql.Tx, personID string) (string, error) {
	var login string
	err := tx.QueryRowContext(ctx, personLoginStatement, personID).Scan(&login)
	if err != nil {
		return "", fmt.Errorf("iamdomain: read person %s's login: %w",
			personID, err)
	}
	return login, nil
}

// Invite mints an invitation to an address nobody in this company holds, onto
// a human seat nobody holds, as a DIRECTORY record: it holds an address and a
// seat for somebody with no person yet, so it is refused where a person or an
// open invitation holds either ([ErrTaken]), and races contend at the broker.
// Until redeemed, cancelled or aged out it holds both ([heldByInvitation]). The
// seat is first checked advisorily ([Writer.humanSeat]). It publishes the
// VERIFIER of the link's secret ([InvitationVerifier]); the caller shows the
// secret once. Its id is [Blinder.InvitationID] of the operation key, so a
// retry names the invitation its first attempt issued; a key that issued one
// on other terms, or no longer open, is [ErrOperationReused].
func (w *Writer) Invite(ctx context.Context, in InviteMint) (
	InviteIssued, error) {

	if err := w.mayAdminister(OpInvite); err != nil {
		return InviteIssued{}, err
	}
	// WHAT IT CONFERS IS DECIDED HERE, ONCE, against the issuer; the
	// redemption reads it back.
	if err := w.MayConfer(nil, in.Grants); err != nil {
		return InviteIssued{}, err
	}
	switch {
	case in.OpID == "":
		return InviteIssued{}, errors.New("iamdomain: an invitation needs an " +
			"operation key — its id is derived from it")
	case iam.NormalizeEmail(in.Email) == "":
		return InviteIssued{}, fmt.Errorf("%w: an invitation needs "+
			"an email address",
			ErrNotFindable)
	case len(in.Email) > MaxAddress:
		return InviteIssued{}, fmt.Errorf("%w: the email address is %d "+
			"bytes, and the limit is %d", ErrInvalid,
			len(in.Email), MaxAddress)
	case in.ExpiresAt.IsZero():
		return InviteIssued{}, fmt.Errorf("%w: an invitation "+
			"needs an expiry date", ErrInvalid)
	case in.Seat == "":
		return InviteIssued{}, fmt.Errorf("%w: an invitation creates a person, "+
			"so it names the human seat they will hold", ErrSeatRequired)
	}
	// THE SEAT FIRST, before anything is published or MINTED: the blinder mints
	// the company's key on a fresh node's first address, and a bad seat must
	// refuse with its own remedy and nothing left behind. Whether somebody holds
	// it is asked again in the snapshot.
	seat, err := w.humanSeat(ctx, in.Seat)
	if err != nil {
		return InviteIssued{}, err
	}
	if w.blinds == nil || w.sealer == nil {
		return InviteIssued{}, fmt.Errorf("iamdomain: this node can't create "+
			"invitations: it has %s", w.missingKeys())
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
	// THE ID IS THE OPERATION'S, derived before anything is minted: a key that
	// is not a uuid7 is refused here, with nothing left behind.
	id, err := blinder.InvitationID(in.OpID)
	if err != nil {
		return InviteIssued{}, err
	}
	// AND SO IS THE LINK'S SECRET, derived from the id, so a retry hands back
	// the first attempt's link.
	secret, err := blinder.InvitationSecret(id)
	if err != nil {
		return InviteIssued{}, err
	}
	// SEALED AS THE INVITATION'S OWN, there being no person yet: bound to its
	// id, so it opens as nothing else ([Sealer.SealInvitation]).
	sealed, err := w.sealer.SealInvitation(id, in.Email)
	if err != nil {
		return InviteIssued{}, fmt.Errorf("iamdomain: seal an "+
			"invitation's address: %w", err)
	}
	mutation, err := EncodeInvitation(Invitation{
		V: DocumentVersion, ID: id, EmailBlind: blind, Sealed: sealed,
		InvitedBy: w.Actor, Grants: in.Grants,
		Verifier: InvitationVerifier(secret), Seat: seat,
		ExpiresAt: in.ExpiresAt,
	})
	if err != nil {
		return InviteIssued{}, err
	}
	// THE BUCKET IS THE ADDRESS'S OWN, matching the apply: a bucket derived from
	// an empty person id would put every outstanding invitation in one sweep.
	scopeOf := func(context.Context, *sql.Tx) (ScopeSet, error) {
		return BucketScope(BucketOf(blind)), nil
	}
	// THIS OPERATION'S OWN INVITATION FIRST: a retry is answered with the one
	// its first attempt issued rather than refused by it. Then both tables for
	// the seat and the address, which a person or an unredeemed invitation can
	// hold.
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
		// Answered from the ledger, so the decide never compared this call's
		// terms or read the kept deadline: read both back from the row.
		if expires, err = w.issuedAs(ctx, id, blind, seat, in, result); err != nil {
			return InviteIssued{Result: result}, err
		}
	}
	return InviteIssued{Result: result, ID: id, Secret: secret,
		ExpiresAt: expires}, err
}

// issuedAs is the invitation an operation issued, read back after the ledger
// answered its retry ([statelog.Result.Collapsed]): the deadline it keeps, or
// [ErrOperationReused] where the key issued it on other terms. With no row,
// whether THIS NODE applied the copy decides:
//
//   - not applied: it landed but its terms cannot be read yet, so
//     [NotReadBack]; the same key retried once applied is answered in full;
//   - applied: the sweep collected it, which it does only to an invitation
//     redeemed or aged out ([Writer.Sweep]), so the link admits nobody:
//     [ErrOperationReused], never a retry that would meet the same absence.
func (w *Writer) issuedAs(ctx context.Context, id, blind, seat string,
	in InviteMint, result statelog.Result) (time.Time, error) {

	applied := result.Outcome == statelog.OutcomeApplied

	expires := in.ExpiresAt
	err := w.db.Read(ctx, func(tx *sql.Tx) error {
		return w.issuedBefore(ctx, tx, id, blind, seat, in, &expires)
	})
	switch {
	case errors.Is(err, errNothingToPublish):
		return expires, nil
	case err != nil:
		return time.Time{}, err
	case applied:
		return time.Time{}, fmt.Errorf("%w: the invitation it sent has been "+
			"used or has expired. Send a new one with a new key",
			ErrOperationReused)
	}
	return time.Time{}, &NotReadBack{Result: result, Invitation: true}
}

// NotReadBack is a create of a person, or an invitation's issue, that LANDED
// and whose answer this node cannot read back yet: the first password link or
// the invitation's terms are rows it has not applied. Only a retry under the
// SAME key hands them back.
//
// A type of its own so no surface answers it as a mere condition, telling an
// administrator a landed create did not land and sending the retry under a
// fresh key, refused by the first attempt's values and losing the link. It
// unwraps to [statelog.ErrUnavailable], since it clears by waiting.
type NotReadBack struct {
	// Result is the write's own outcome where the caller has it, never
	// unknown: the write landed.
	Result statelog.Result

	// Invitation is an invitation's issue; false for a person's create.
	Invitation bool
}

func (e *NotReadBack) Error() string {
	made := "the person"
	if e.Invitation {
		made = "the invitation"
	}
	return "iamdomain: " + made + " this key made landed, and this node " +
		"hasn't applied it yet, so only a retry under the same key reads it back"
}

// Unwrap makes it a condition that clears by waiting ([statelog.RetryAfter]).
func (e *NotReadBack) Unwrap() error { return statelog.ErrUnavailable }

// ErrOperationReused reports an operation key that already names something
// other than what this call asks for: an invitation for another address or on
// other terms, one no longer open, or a create of a person who exists.
//
// A refusal, not a dedupe: a retry is the SAME request, so answering a different
// one with the first attempt's object would hand over somebody else's.
var ErrOperationReused = errors.New("that Idempotency-Key was already used " +
	"for a different request")

// issuedBefore answers whether the invitation an operation derives was already
// issued, in the issue's snapshot: nil where not, [errNothingToPublish] where
// this is a retry that found its own, and [ErrOperationReused] where the key
// issued something this call is not. The TERMS are compared (address, grants,
// seat): the same address on other terms is a different request reusing the key.
func (w *Writer) issuedBefore(ctx context.Context, tx *sql.Tx, id, blind, seat string,
	in InviteMint, expiresAt *time.Time) error {

	var (
		held              string
		expires, redeemed int64
		document          []byte
	)
	err := tx.QueryRowContext(ctx, invitationStateStatement, id).
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
		return fmt.Errorf("%w: it sent an invitation with different "+
			"details. Use a new key for a new invitation", ErrOperationReused)
	case redeemed != 0:
		return fmt.Errorf("%w: the invitation it sent has been used",
			ErrOperationReused)
	case expires == 0 || !w.Now().Before(time.UnixMilli(expires)):
		// NO EXPIRY IS AGED OUT, for [Writer.redeemable]'s reason.
		return fmt.Errorf("%w: the invitation it sent has expired. Send a "+
			"new one with a new key", ErrOperationReused)
	}
	// THE DEADLINE IT WAS ISSUED WITH is the one its link keeps: a later retry
	// is not a later invitation.
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
// is built from (the one the OPERATION derives, published by this call or its
// first attempt) and the deadline the invitation keeps.
type InviteIssued struct {
	statelog.Result
	ID string

	// Secret is the half of the link that is the credential
	// ([Blinder.InvitationSecret]); the estate keeps only its verifier. Kept
	// nowhere; a retry derives it again.
	Secret string

	ExpiresAt time.Time
}

// heldByInvitation refuses a value an OPEN invitation holds (its address or
// its seat), naming the invitation, in a directory decide's snapshot. A
// redemption never asks, being held to the people's rows alone
// ([Writer.enrol]). Open is one predicate, [openInvitation], which the listing
// and [InvitationRow.Spent] mirror. The statement comes from
// [invitedStatements], never a caller.
//
// The WRITER'S CLOCK decides expiry, the one place in this domain it may (every
// stored instant is the broker's): a clock minutes out only issues or refuses
// beside a link at its edge, never puts two people on one value, since a
// redemption's decide refuses a value a person holds.
func heldByInvitation(ctx context.Context, tx *sql.Tx, field Unique, value string,
	now time.Time) error {

	statement, ok := invitedStatements[field]
	if !ok {
		return fmt.Errorf("iamdomain: an invitation holds an address and a "+
			"seat, and %q is neither", field)
	}
	var id string
	err := tx.QueryRowContext(ctx, statement, value, now.UnixMilli()).Scan(&id)
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
// `iam_invites` and one bound instant: not redeemed, not aged out (a zero
// expiry included). See [heldByInvitation].
const openInvitation = `redeemed_at = 0 AND expires_at > ?`

// invitedStatements is the newest open invitation holding each value, each a
// seek of that value's index (0041's address, 0042's seat), in the order the
// statement takes the newest by.
var invitedStatements = map[Unique]string{
	UniqueEmail: `SELECT id FROM iam_invites WHERE email_blind = ? AND ` +
		openInvitation + ` ORDER BY created_at DESC LIMIT 1`,
	UniqueSeat: `SELECT id FROM iam_invites WHERE seat_id = ? AND ` +
		openInvitation + ` ORDER BY created_at DESC LIMIT 1`,
}

// InviteMint is what issuing an invitation needs.
type InviteMint struct {
	// Email is the address it is for, in the clear; blinded for the subject and
	// sealed for the row before anything is published.
	Email string

	// Grants are what redeeming it confers, decided ONCE by its issuer, so the
	// redemption cannot ask for more than was offered.
	Grants []iam.Grant

	// Seat is the seat redeeming it BINDS the new person to, by its handle.
	// REQUIRED ([ErrSeatRequired]): a human seat nobody and no other open
	// invitation holds. See [Writer.Invite].
	Seat string

	// ExpiresAt is when it stops being redeemable. REQUIRED.
	ExpiresAt time.Time

	// OpID is the operation KEY, a uuid7, from which the invitation's id is
	// derived ([Blinder.InvitationID]).
	OpID   string
	Reason string
}

// CancelInvitation withdraws an invitation nobody has redeemed (open, or aged
// out and not yet collected), so its link opens nothing and the address AND the
// seat it held are free the moment the record lands. It rides the directory
// subject, where both are held, so a racing issue contends at the broker; it
// takes nothing, so it skips [wholeDirectory] for [Writer.Remove]'s reason. An
// invitation it does not hold is [ErrNoInvitation] (never issued, cancelled or
// collected, which a link cannot tell apart), a redeemed one
// [InvitationRedeemed]: what undoes it is removing that person.
func (w *Writer) CancelInvitation(ctx context.Context, id, opID, reason string) (
	statelog.Result, error) {

	if err := w.mayAdminister(OpCancel); err != nil {
		return statelog.Result{}, err
	}
	if id == "" || opID == "" {
		return statelog.Result{}, errors.New("iamdomain: cancelling an " +
			"invitation needs its id and an operation id")
	}
	// THE BUCKET IS THE ADDRESS'S, as the issue's was; an invitation this
	// snapshot lacks has none and the decide refuses it, so the id's bucket is
	// only a scope that encodes.
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

// invitationInStatement is one invitation's address blind and redeemer, by key.
const invitationInStatement = `
	SELECT email_blind, person_id FROM iam_invites WHERE id = ?`

// invitationIn is the address blind and redeemer of one invitation, in a
// decide's snapshot; false for an id it does not hold.
func invitationIn(ctx context.Context, tx *sql.Tx, id string) (
	InvitationRow, bool, error) {

	var held InvitationRow
	err := tx.QueryRowContext(ctx, invitationInStatement, id).
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
// issued, cancelled, or collected by the sweep.
var ErrNoInvitation = errors.New("no such invitation")

// InvitationRedeemed refuses the cancellation of an invitation somebody
// redeemed, naming the person it created.
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
// with the password it replaces. Both callers, a person CHANGING their own and
// one SPENDING a reset link, set a password somebody else may have had, and
// both revoke every reset link, so a link is never a second way back in.
//
// No grant: neither caller holds one. [PasswordSet.Check] bounds it, run in the
// record's snapshot, so a password changed since the proof, or a link spent by
// another tab, is refused with nothing published. The new credential's id is
// minted once, before the decide that may rerun.
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
		person, err := heldPerson(ctx, tx, in.PersonID)
		if err != nil {
			return err
		}
		if person.Kind != iam.KindPerson {
			return forbidden("only people have passwords").
				about("%s is a %s", in.PersonID, person.Kind)
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
			// REPLACED, not revoked: a second password on the row would
			// be a second password that works.
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

// RevokeResetLinks answers a copy of held with every outstanding reset link
// revoked at now. Three records end a person's links through it: a password
// set ([Writer.SetPassword]), a new link's issue (one per person), and an edit
// ADDING a grant ([Writer.UpdatePerson]), since a link was judged against its
// issuer's grants at its issue. What ends sessions and tokens ends links by
// moving a counter instead ([Credential.EndedBy]).
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

	// Verifier is the new password's argon2id verifier, never the password.
	Verifier string

	// Check is the caller's proof, judged in the record's snapshot against the
	// person and their [Counters], for [CredentialSet.Apply]'s reason. It may
	// refuse; it forms nothing.
	Check func(Person, Counters) error

	// Spends is the reset link this password is set from, marked
	// [Credential.Spent]; empty for a person changing their own.
	Spends string

	OpID   string
	Reason string
}

// UpdatePerson rewrites one person's document (grants, name), forming the new
// whole INSIDE the snapshot, for [Writer.SetCredentials]'s reason. It changes
// what a person holds, never their kind or stage ([keepsStanding]).
//
// The self-escalation rule is enforced HERE, the last frame every path passes:
// nobody confers a grant they do not hold, themselves included
// ([Writer.MayConfer]). A grant gained ([grantDelta]) revokes every outstanding
// reset link in the same record: a link was judged against its issuer's grants
// at its issue, and its issuer can spend it. Taking one away revokes nothing.
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
	// THE NAME IS SEALED ONCE, BEFORE THE DECIDE: a rerun would draw a fresh
	// nonce.
	sealedName := ""
	if in.Name != nil && len(*in.Name) > MaxName {
		return statelog.Result{}, fmt.Errorf("%w: the name is %d bytes, "+
			"and the limit is %d", ErrInvalid, len(*in.Name), MaxName)
	}
	if in.Name != nil {
		if w.sealer == nil {
			return statelog.Result{}, fmt.Errorf("iamdomain: this node can't "+
				"change names: it has %s", w.missingKeys())
		}
		var err error
		if sealedName, err = w.sealer.Seal(in.PersonID, FieldName,
			*in.Name); err != nil {
			return statelog.Result{}, err
		}
	}
	var mutation []byte
	// THE LAST RUN'S BEFORE AND AFTER, the pair the landed record carries.
	var before, after []iam.Grant
	decide := func(tx *sql.Tx) (err error) {
		person, err := heldPerson(ctx, tx, in.PersonID)
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
		counters, err := countersOf(ctx, tx, in.PersonID)
		if err != nil {
			return err
		}
		// A RESET LINK THIS RECORD ISSUES IS STAMPED WITH THE COUNTERS, as a
		// token is.
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
	// ONE ROW PER WRITE THAT MOVED A GRANT, none for a name-only change: it
	// answers "who can do what, and since when".
	if added, removed := grantDelta(before, after); len(added)+len(removed) > 0 {
		w.announce(ctx, result, err, types.IAMGrantsChanged{
			Person: in.PersonID, Added: added, Removed: removed, By: w.Actor,
			OperatorID: w.OperatorID, Version: result.Position.Packed(),
		})
	}
	return result, err
}

// keepsStanding refuses a content record that would move what a person IS
// rather than what they hold. A KIND never changes: login grammar, address
// requirement and seat rule were judged for the kind created, and the applier
// writes whatever kind a document states; the remedy is a removal and a new
// create. A STAGE moves only through [Writer.SetStage], which ends what a stage
// that may not act holds ([StatusChange.Epoch]); moved here, a suspension would
// end no session or token.
func keepsStanding(held, updated Person) error {
	switch {
	case updated.Kind != held.Kind:
		return fmt.Errorf("%w: an account's kind can't change (this one is a "+
			"%s). Remove it and add a %s instead", ErrInvalid,
			held.Kind, updated.Kind)
	case updated.Stage != held.Stage:
		return fmt.Errorf("%w: a stage can't be changed with an edit. Use a "+
			"stage change instead", ErrInvalid)
	}
	return nil
}

// MayConfer refuses a change that ADDS a grant this writer's party does not
// hold, compared through [iam.Expand]. Only additions are checked: nobody
// escalates by narrowing, and whoever may edit a row must be able to withdraw
// what a leaver held. An unknown name (a newer peer's) may be kept and removed,
// never added, for [iam.Principal.Can]'s reason. A permission the party lacks
// is named ([Refusal.Grants]); an unknown name names none.
//
// Exported for the surface that asks it before an edit's first record, so an
// edit moving a seat or login first is refused with nothing moved; the
// record's decide asks again in its snapshot, the authority.
func (w *Writer) MayConfer(before, after []iam.Grant) error {
	held := iam.Expand(before)
	for _, g := range after {
		if slices.Contains(held, g) || w.Can(g) {
			continue
		}
		if !g.Valid() {
			return forbidden("%s", iam.NotAGrant(g))
		}
		return needs([]iam.Grant{g}, "you can't give %s because you don't "+
			"have it", g).about("the party holds %s", grantWords(w.Grants))
	}
	return nil
}

// PersonUpdate is what rewriting one person's document needs.
type PersonUpdate struct {
	PersonID string

	// Name is a new name in the CLEAR, or nil to leave it ("" is a real
	// setting). Sealed once before the decide and placed on the document
	// [PersonUpdate.Apply] is handed, which may still overwrite it.
	Name *string

	// Apply forms the new document from the current one INSIDE the snapshot, a
	// function for [CredentialSet.Apply]'s reason. It may refuse, unwrapped,
	// for anything this package cannot judge, but may NOT move the kind or
	// stage ([keepsStanding]). On a COLLAPSED result what it formed may not be
	// what landed, as for [CredentialSet.Apply].
	Apply func(Person) (Person, error)

	OpID   string
	Reason string
}
