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
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DECIDES: what a caller actually asks this domain to do.
//
// Each one takes ONE snapshot, forms its expectation inside it, and lets the
// broker arbitrate. What they add over the framework is this domain's own
// rules, and every one of those has to happen inside the snapshot because
// every one is a decision about state another writer is changing at the same
// time.

// ErrClaimed reports an address, a login or a seat somebody else holds.
//
// IT NAMES THE HOLDER, which is the whole difference between a refusal
// somebody can act on and one they can only retry: "that address is taken" is
// a support ticket, "that address belongs to person X" is an answer.
type ErrClaimed struct {
	Kind   ObjectKind
	Token  string
	Holder string
}

func (e *ErrClaimed) Error() string {
	return fmt.Sprintf("iamdomain: the %s claim on %s is held by person %s",
		e.Kind, e.Token, e.Holder)
}

// Enrol creates a person and takes the claims that make them findable.
//
// A SEQUENCE, and it has to be: each claim arbitrates on its own subject and a
// record has exactly one subject, so there is no single append that can take
// an address and a login together. The order is deliberate — the CLAIMS FIRST,
// because they are what can be refused, and the person last.
//
// A SEQUENCE THAT STOPS HALFWAY leaves a claimed address with no person. That
// is a LEGAL NAMED STATE rather than corruption: the claim report names it
// once it is [OrphanGrace] old ([Reader.Claims]), and removing the
// reservation's id releases what it holds — and the alternative, writing the
// person first, would leave a person nobody can find, holding an address
// somebody else may then take.
//
// THE SWEEP DOES NOT COLLECT IT, deliberately: a reservation is holding a
// claim whose subject still carries an anchor, and deleting the row on a
// clock would leave that address arbitrated to nobody the directory can name.
// Releasing is a decision, and a removal is the record that makes it.
//
// # What it may confer, and on whose authority
//
// An enrolment is a grant change from nothing to what it carries, so it is
// held to [Writer.UpdatePerson]'s rule: a caller may not confer a grant they
// do not hold. It used to be held to nothing, so a party holding people:manage
// and no other grant could enrol somebody carrying secrets:read — or enrol a
// colleague holding everything and sign in as them.
//
// TWO ENROLMENTS ARE NOT THE WRITER'S TO AUTHORISE, and each names what is:
//
//   - A REDEMPTION ([Enrolment.Invitation]) confers what the INVITATION's
//     author conferred when they issued it, and was held to their grants then
//     ([Writer.Invite]). The person record's decide reads the invitation in its
//     own snapshot and refuses anything it does not cover — more grants, more
//     reach, a different address, a link already spent or aged out — so the
//     node that processes a redemption decides nothing a second time.
//   - THE FIRST PERSON ([Enrolment.BootstrapCode]) is the one stated
//     exemption: nobody holds a credential yet, so there is nobody whose
//     grants could bound it, and it is taken by whoever proved they can read
//     a file on the host. The decide reads the code and the directory in its
//     own snapshot and refuses unless the code is live and nobody else is
//     enrolled, so the exemption closes the moment anybody exists.
//
// The node's own writer holds fleet:operate and people:manage and nothing
// else, so on its own authority it may confer those two; everything the
// bootstrap and a redemption hand out comes from the basis they name.
//
// # A refused enrolment leaves its key, and the key duty collects it
//
// The key is minted before the first claim, because the claim carries the
// address sealed under it — so an enrolment refused on its address has minted a
// key for somebody who never existed. It is NOT destroyed here, and the reason
// is [Sealer.Mint]'s: a mint that finds a key keeps it, so the id this call was
// handed may be a LIVE person's (a retry, or a caller that reused an id), and a
// refusal that shredded "its" key would destroy theirs. Only a pass that has
// proved nobody owns the id may destroy it, which is [ShredKeys]: past
// [OrphanKeyGrace], on a node that has applied everything the log held.
func (w *Writer) Enrol(ctx context.Context, in Enrolment) (statelog.Position, error) {
	if err := w.mayAdminister(OpEnrol); err != nil {
		return statelog.Position{}, err
	}
	if err := in.validate(); err != nil {
		return statelog.Position{}, err
	}
	if in.Invitation == "" && in.BootstrapCode == "" {
		// THE WRITER'S OWN AUTHORITY, checked BEFORE the first claim:
		// it reads nothing, so there is no reason to leave a claimed
		// address behind a refusal that was knowable up front.
		if err := w.mayConfer(nil, in.Grants); err != nil {
			return statelog.Position{}, err
		}
	}
	if w.sealer == nil || (in.Email != "" && w.blinds == nil) {
		return statelog.Position{}, fmt.Errorf("iamdomain: this node cannot "+
			"enrol anybody: it has %s. An enrolment has to derive the subject "+
			"the address claim arbitrates on and seal the values that belong "+
			"to the person, and a node that guessed at either would put two "+
			"people on one address", w.missingKeys())
	}
	// THE BLINDER BEFORE THE KEY. One that cannot be established refuses
	// the enrolment before anything is written; resolved after the
	// person's key was minted, the refusal would leave a key behind for
	// somebody who never existed.
	var blinder *Blinder
	if in.Email != "" {
		resolved, err := w.blinds.Blinder(ctx)
		if err != nil {
			return statelog.Position{}, fmt.Errorf("iamdomain: this node "+
				"cannot derive the subject an address claim arbitrates on: %w",
				err)
		}
		blinder = resolved
	}

	// ONE MARK FOR THE WHOLE GESTURE: each step decides from a state holding
	// the one before it, whether this writer is shared or a sequence.
	at := w.gesture()
	if err := w.sealer.Mint(ctx, in.PersonID, w.Actor, w.Now()); err != nil {
		return statelog.Position{}, err
	}
	sealedName, err := w.sealer.Seal(ctx, in.PersonID, FieldName, in.Name)
	if err != nil {
		return statelog.Position{}, err
	}
	// AN ADDRESS IS OPTIONAL AND A KEY IS NOT. A machine identity has no
	// mailbox, so there is nothing to blind, nothing to seal and no
	// address claim to take — but its NAME is still sealed, under a key
	// minted for it like anybody else's, because a removal has to be able
	// to shred `Release pipeline, raised by Dana` as surely as a person.
	var sealedEmail, blind string
	if in.Email != "" {
		if blind, err = blinder.Email(in.Email); err != nil {
			return statelog.Position{}, err
		}
		if sealedEmail, err = w.sealer.Seal(ctx, in.PersonID, FieldEmail,
			in.Email); err != nil {
			return statelog.Position{}, err
		}
		// THE ADDRESS FIRST. It is the claim most likely to be
		// contested — two administrators adding one new joiner — so it
		// is the one that should fail before anything else has
		// happened.
		if _, err := w.claim(ctx, at, KindEmail, blind, in.PersonID,
			sealedEmail, in.OpID+":email", in.Kind); err != nil {
			return statelog.Position{}, err
		}
	}
	// THE LOGIN IS IN ITS OP ID, unlike the address's. A retry of one
	// enrolment names the same address — it is the invitation's, or the
	// administrator's typing — but may name a DIFFERENT login: a redeemer
	// told theirs was taken chooses another. Under one op id the broker
	// would acknowledge the second claim as the first inside its duplicate
	// window, and the person would be written holding the login they gave
	// up rather than the one they chose.
	if _, err := w.claim(ctx, at, KindLogin, in.Login, in.PersonID, "",
		in.OpID+":login:"+in.Login, in.Kind); err != nil {
		return statelog.Position{}, err
	}

	person := Person{
		V: DocumentVersion, Kind: in.Kind, Stage: in.Stage,
		NameSealed: sealedName, EmailSealed: sealedEmail,
		Credentials: in.Credentials, Grants: in.Grants,
		Colleague: in.Colleague,
	}
	mutation, err := EncodePerson(person)
	if err != nil {
		return statelog.Position{}, err
	}
	rec, err := w.record(PersonSubject(in.PersonID), OpEnrol, in.PersonID,
		PeopleScope(in.PersonID), mutation, in.Reason)
	if err != nil {
		return statelog.Position{}, err
	}
	// THE BASIS IS READ IN THE PERSON RECORD'S OWN SNAPSHOT, the one the
	// grants actually land from — a check against an earlier read would
	// pair an invitation somebody spent a moment ago with a person it then
	// creates. Nil for the writer's own authority, which was checked above.
	var decide func(*sql.Tx) error
	switch {
	case in.Invitation != "":
		decide = func(tx *sql.Tx) error {
			return w.redeemable(ctx, tx, in, blind)
		}
	case in.BootstrapCode != "":
		decide = func(tx *sql.Tx) error {
			return w.bootstrappable(ctx, tx, in)
		}
	}
	// ARBITRATED, NOT A CREATE, and the claims are why: they run first and
	// their apply leaves a RESERVATION row for this person, so a create
	// pattern would find a guarding row and refuse the enrolment that put
	// it there. What makes a retry of the whole gesture land once is the
	// operation ledger, which is the mechanism for that anyway — the
	// guarding row never was.
	result, err := w.publishAt(ctx, at,
		w.request(&rec, in.OpID, statelog.PatternArbitrated, decide))
	// AN ENROLMENT THAT CONFERS ANYTHING IS A GRANT CHANGE — from nothing
	// to what it carries — and the first person a bootstrap code creates,
	// holding the whole ceiling, is the one row of those an audit most
	// needs to find.
	if added, _ := grantDelta(nil, in.Grants); len(added) > 0 {
		w.announce(ctx, result, err, types.IAMGrantsChanged{
			Person: in.PersonID, Added: added, By: w.Actor,
			Version: result.Position.Packed(),
		})
	}
	return result.Position, err
}

// Enrolment is what creating a person needs.
type Enrolment struct {
	// PersonID is minted by the CALLER, not here, and it is a uuid7.
	//
	// BY THE CALLER because an enrolment is a sequence of appends and
	// every one of them has to name the same person: an id minted inside
	// the first would not be available to form the second's payload, and
	// an id minted per append would enrol three people. A redemption
	// DERIVES it from the credential rather than minting it
	// ([InvitedPersonID], [BootstrappedPersonID]), so a retry names the
	// person its first attempt already claimed for.
	PersonID string

	Kind  iam.Kind
	Stage iam.Stage

	// Name and Email are CLEARTEXT here and nowhere after: they are
	// sealed before the first record is formed, and no payload this
	// gesture publishes carries either.
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

	// Colleague is how far into the company's own work this person
	// reaches. Its zero is the CLOSED end and a real setting — see
	// [Person.Colleague].
	Colleague iam.Colleague

	// Invitation is the id of the invitation this enrolment REDEEMS, or
	// empty. When set, what the enrolment confers is bounded by what the
	// invitation says rather than by the writer's own grants — see
	// [Writer.Enrol].
	Invitation string

	// BootstrapCode is the id of the one-time code this enrolment redeems,
	// or empty. When set, it is the first person, and the one stated
	// exemption from the conferral rule — see [Writer.Enrol].
	BootstrapCode string

	// OpID is the operation id for the whole gesture. Each append derives
	// its own from it with a suffix, so a retry of the sequence dedupes
	// step by step rather than all-or-nothing.
	OpID   string
	Reason string
}

// validate refuses an enrolment that could not land, BEFORE the first claim.
//
// EVERY BOUND THE SEQUENCE WILL MEET IS CHECKED HERE, and that is the point of
// the method rather than a tidiness: an enrolment is a sequence of appends, and
// a bound first met at the person record — the last of them — is met after the
// address and the login are already claimed. The refusal then leaves a
// reservation holding both, and the caller's corrected retry is refused as
// "claimed" by the half its own first attempt left behind. [Writer.record]
// still checks the reason on every record, which is the backstop for the
// gestures that are one append; this is the check that runs while nothing has
// been published.
func (e Enrolment) validate() error {
	switch {
	case e.Invitation != "" && e.BootstrapCode != "":
		return errors.New("iamdomain: an enrolment redeems an invitation or " +
			"the one-time code, never both — each is a different authority " +
			"for what it confers, and one record cannot be bounded by two")
	case e.Invitation != "" && e.Email == "":
		return fmt.Errorf("%w: redeeming an invitation enrols the address it "+
			"was issued to, and this enrolment names none", ErrNotFindable)
	case e.PersonID == "":
		return errors.New("iamdomain: an enrolment needs the person id it is " +
			"creating: every append in the sequence names it, so one minted " +
			"per append would enrol three people")
	case e.OpID == "":
		return errors.New("iamdomain: an enrolment needs an operation id — it " +
			"is a sequence of appends, and without a stable id a retry cannot " +
			"tell which of them already landed")
	case e.Kind != iam.KindPerson && e.Kind != iam.KindMachine:
		// THE DIRECTORY HOLDS PEOPLE AND MACHINES, and nothing else
		// enrols. A seat is the chart's and the engine is the node, so
		// a directory row of either kind is a second answer to who they
		// are — and neither has a login grammar, so the row would be
		// findable by an address alone and act as nothing the rest of
		// the engine can name.
		return fmt.Errorf("%w: %q is not a kind the directory enrols — want "+
			"%s or %s", ErrNotEnrollable, e.Kind, iam.KindPerson, iam.KindMachine)
	case !e.Stage.Valid():
		return fmt.Errorf("%w: %q is not an enrolment stage", ErrInvalid, e.Stage)
	case len(e.Reason) > MaxReason:
		return fmt.Errorf("%w: the reason on this enrolment is %d bytes and "+
			"the cap is %d — it is rendered into an authentication trail "+
			"beside the op that caused it, so it says WHICH cause fired "+
			"rather than narrating", ErrInvalid, len(e.Reason), MaxReason)
	case e.Colleague != "" && !e.Colleague.Valid():
		// UNSET IS THE CLOSED END and a real setting — see
		// [Person.Colleague] — so only a value this build cannot place on
		// its ladder is refused. Stored, it would read as no reach on
		// this build and as whatever a newer one means by it on the next.
		return fmt.Errorf("%w: %q is not a colleague level — want one of %v",
			ErrInvalid, e.Colleague, iam.Colleagues)
	case e.Kind == iam.KindPerson && e.Email == "":
		return fmt.Errorf("%w: enrolling a person needs an address — it is "+
			"the interactive login key, and somebody with none can never "+
			"sign in", ErrNotFindable)
	case e.Kind == iam.KindMachine && e.Login == "":
		// A MACHINE NEEDS NO ADDRESS and must still be FINDABLE. It has
		// no login page and no mailbox — `svc:ci` proves itself with a
		// token — so requiring an address would have meant inventing
		// one, and a row that is not named is a credential holder
		// nobody can list, revoke or audit.
		return fmt.Errorf("%w: enrolling a %s needs a login — it has no login "+
			"page and therefore no address, so the login is the only thing "+
			"that finds it", ErrNotFindable, e.Kind)
	case e.Login == "":
		// A PERSON NEEDS ONE TOO, and not to be found: their address
		// already does that. A login is the NAME a principal acts and is
		// written under while they hold no seat — iam.ActorFor records an
		// unbound person under it — and a person with none was recorded
		// as `anonymous` beside every change they made, and failed
		// [iam.Principal.Validate] on every request they sent. So every
		// path that creates a person names one: an administrator types
		// it, the first operator types it, and an invitation's form
		// proposes one from the address for the person to keep or change.
		return fmt.Errorf("%w: enrolling a person needs a login — lowercase "+
			"segments joined by DOTS (jane.doe). It is the name every change "+
			"they make is recorded under while they hold no seat, and without "+
			"one they would be recorded as nobody", ErrInvalidLogin)
	}
	return loginFits(e.Kind, e.Login)
}

// redeemable refuses an enrolment its invitation does not cover, read inside
// the person record's own snapshot.
//
// EVERY CLAUSE IS A WAY THE REDEMPTION COULD OTHERWISE ASK FOR MORE THAN WAS
// OFFERED: a grant or a reach the invitation did not carry, an address it was
// not issued to (holding somebody's link is not holding their address), and a
// link that is spent or aged out. A link redeemed by THIS person already is
// not spent against them, so a retry of a redemption whose spend landed still
// lands as the same person.
//
// THE WRITER'S CLOCK decides the expiry, as [openInvitationFor]'s does: the
// surface already refused an aged-out link against the same clock, and what
// this buys is that the check and the grants it bounds are one snapshot.
func (w *Writer) redeemable(ctx context.Context, tx *sql.Tx, in Enrolment,
	blind string) error {

	var (
		held           string
		expires, spent int64
		redeemedBy     string
		document       []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT email_blind, expires_at, redeemed_at, person_id, document
		  FROM iam_invites WHERE id = ?`, in.Invitation).
		Scan(&held, &expires, &spent, &redeemedBy, &document)
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
	case held != blind:
		return fmt.Errorf("%w: invitation %s was issued to another address — "+
			"holding somebody's link is not holding their address",
			ErrRefused, in.Invitation)
	case spent != 0 && redeemedBy != in.PersonID:
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
	if !colleagueWithin(in.Colleague, invitation.Colleague) {
		return fmt.Errorf("%w: redeeming invitation %s reaches the company's "+
			"work at %q, and the enrolment asks for %q", ErrRefused,
			in.Invitation, invitation.Colleague, in.Colleague)
	}
	return nil
}

// bootstrappable refuses the first-person exemption once it no longer
// applies, read inside the person record's own snapshot.
//
// TWO FACTS AND BOTH ARE NEEDED. The code must be LIVE — minted, not spent by
// anybody else, not withdrawn, not aged out — because a code is what proves
// the caller can read a file on the host; and nobody ELSE may be enrolled,
// because the exemption exists only for the instant there is nobody whose
// grants could bound it. A reservation row (a claim whose content record has
// not landed) is not somebody: it has no kind, may do nothing, and an
// abandoned one must not close the company's only way in for good.
//
// THE GRANTS ARE NOT BOUNDED BY A WRITER, which is the exemption itself — but
// a grant this build cannot name is still refused, because conferring a
// spelling nothing can check is not conferring the ceiling.
func (w *Writer) bootstrappable(ctx context.Context, tx *sql.Tx, in Enrolment) error {
	var (
		expires, spent int64
		redeemedBy     string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT expires_at, redeemed_at, person_id
		  FROM iam_bootstrap_codes WHERE id = ?`, in.BootstrapCode).
		Scan(&expires, &spent, &redeemedBy)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: no one-time code with that id is on the log, "+
			"so the file that holds it is not one any node accepts",
			ErrRefused)
	case err != nil:
		return fmt.Errorf("iamdomain: read the one-time code: %w", err)
	case spent != 0 && redeemedBy != in.PersonID:
		return fmt.Errorf("%w: the one-time code has been spent or "+
			"withdrawn", ErrRefused)
	case expires != 0 && !w.Now().Before(time.UnixMilli(expires)):
		return fmt.Errorf("%w: the one-time code has aged out", ErrRefused)
	}
	var somebody bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM iam_people
		  WHERE shredded = 0 AND kind != '' AND id != ?)`, in.PersonID).
		Scan(&somebody); err != nil {
		return fmt.Errorf("iamdomain: read whether anybody is enrolled: %w", err)
	}
	if somebody {
		return fmt.Errorf("%w: this company already has somebody in it, so "+
			"the first-person exemption is closed — an administrator "+
			"enrols or invites everybody after them", ErrRefused)
	}
	for _, g := range in.Grants {
		if !g.Valid() {
			return fmt.Errorf("%w: %q is not a grant this build knows",
				ErrRefused, g)
		}
	}
	return nil
}

// colleagueWithin reports whether a reach is no wider than a bound.
//
// THE ZERO VALUE IS THE CLOSED END, as it is everywhere a person's reach is
// stored, and a level this build cannot name is never within anything.
func colleagueWithin(asked, bound iam.Colleague) bool {
	rung := func(c iam.Colleague) int {
		if c == "" {
			return 0
		}
		return slices.Index(iam.Colleagues, c)
	}
	mine, theirs := rung(asked), rung(bound)
	return mine >= 0 && theirs >= 0 && mine <= theirs
}

// loginFits refuses a login that is not in its holder's kind's grammar.
//
// THE GRAMMAR IS THE HOLDER'S, never "either one": [iam.ValidLoginFor] argues
// the case, and it is the Tier A token namespace. A person who could take
// `token:ops` would make the deployment's `ops` credential act as their seat,
// because the directory row under a token's login is what binds it.
//
// Checked on the value AS TYPED rather than the folded subject id, so
// `Sarah.Chen` is refused naming itself instead of being quietly lowered into
// a login its holder never wrote.
func loginFits(kind iam.Kind, login string) error {
	if iam.ValidLoginFor(kind, login) {
		return nil
	}
	shape := "a person's login is lowercase segments joined by DOTS (jane.doe)"
	if kind == iam.KindMachine {
		shape = "a machine's login is lowercase segments joined by a COLON " +
			"(ci:release, or token:<id> for a Tier A token)"
	}
	return fmt.Errorf("%w: %q is not a login a %s may hold — %s. Each kind has "+
		"its own separator, which keeps it apart from a seat handle and from "+
		"the other kind's names", ErrInvalidLogin, login, kind, shape)
}

// ErrInvalidLogin reports a login outside its holder's kind's grammar.
//
// ITS OWN SENTINEL because it is the caller's to fix and a surface answers it
// 400 — it is a value somebody typed, and reporting it as a failure of the
// engine would send them looking for an outage.
var ErrInvalidLogin = errors.New("iamdomain: that login does not fit its holder's kind")

// ErrInvalid reports a value outside a bound this domain holds a record to: a
// reason past [MaxReason], a colleague level this build cannot name, a stage
// that is not one.
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

// Claim takes one address, login or seat binding for a person.
//
// CREATE-ONLY AT AN EXPECTATION OF ZERO, which IS the uniqueness check: there
// is no unique index in this estate and there cannot be one, so two writers
// claiming one token contend at the broker and exactly one wins.
func (w *Writer) Claim(ctx context.Context, kind ObjectKind, token, personID,
	opID string) (statelog.Position, error) {

	if err := w.mayAdminister(OpClaim); err != nil {
		return statelog.Position{}, err
	}
	// THE HOLDER'S KIND IS READ INSIDE THE SNAPSHOT, never taken from the
	// caller: a login's grammar is the kind of whoever holds it, and a
	// caller stating that kind would be stating what it read in another
	// transaction — which is the one input this check cannot trust.
	return w.claim(ctx, w.gesture(), kind, token, personID, "", opID, "")
}

// claim is the shared body, so an enrolment's own claims and an operator's
// take exactly the same path — a second implementation is where one of them
// comes to publish at a different pattern.
//
// enrolling is the kind of the person an ENROLMENT is creating, and empty
// everywhere else. It exists because the enrolment's claims run before the
// person's own record — they are what can be refused — so the row whose kind
// a login's grammar is judged against does not exist yet; the enrolment
// validated the kind itself, and it is the one caller that knows it without
// reading. Every other claim reads its holder's kind inside the snapshot.
func (w *Writer) claim(ctx context.Context, at *statelog.Position,
	kind ObjectKind, token, personID, sealed, opID string,
	enrolling iam.Kind) (statelog.Position, error) {

	if token == "" || personID == "" || opID == "" {
		return statelog.Position{}, fmt.Errorf("iamdomain: a %s claim needs a "+
			"token, a person and an operation id, and has (%q, %q, %q)",
			kind, token, personID, opID)
	}
	subject, err := claimSubject(kind, token)
	if err != nil {
		return statelog.Position{}, err
	}
	rec, err := w.record(subject, OpClaim, personID, PeopleScope(personID),
		nil, "")
	if err != nil {
		return statelog.Position{}, err
	}
	// THE SNAPSHOT READ IS WHAT NAMES THE HOLDER. The broker would refuse
	// the second claim anyway, but its refusal says "somebody was here
	// first" and nothing more — and "that address belongs to person X" is
	// the difference between an answer and a support ticket.
	//
	// IT IS ADVISORY AND SAYS SO: the row it reads can be written between
	// this decide and the append, which is precisely the race the
	// create-at-zero exists to settle. What it buys is a better refusal in
	// the common case, never correctness.
	decide := func(tx *sql.Tx) error {
		if kind == KindLogin {
			// A LOGIN IS JUDGED AGAINST ITS HOLDER'S KIND, read HERE,
			// in the snapshot the claim is decided in: a rename that
			// took a person's kind from a caller, or from a read in
			// another transaction, would let a row whose kind changed
			// between the two take a name its kind may not hold — and
			// a person holding `token:<id>` is the deployment's Tier A
			// credential acting as their seat.
			holderKind := enrolling
			if holderKind == "" {
				var err error
				if holderKind, err = enrolledKindOf(ctx, tx, personID); err != nil {
					return err
				}
			}
			if err := loginFits(holderKind, token); err != nil {
				return err
			}
		}
		holder, held, err := holderOf(ctx, tx, kind, token)
		if err != nil {
			return err
		}
		if held && holder != personID {
			return &ErrClaimed{Kind: kind, Token: token, Holder: holder}
		}
		claim := Claim{V: DocumentVersion, Person: personID, Sealed: sealed}
		if kind == KindSeat {
			// THE CHART READ GUARANTEES NOTHING, and this is the one
			// place in the domain that crosses a log. A seat lives on
			// the chart's stream; nothing orders the two, so this bind
			// and that seat's removal can both be valid and both win.
			// The residue — a person bound to a seat the chart no
			// longer has — is a legal named state the session layer
			// answers with a 403 NAMING THE SEAT. This catches a typo,
			// and that is all it is for.
			if err := seatExists(ctx, tx, token); err != nil {
				return err
			}
			// AND THE POSITION THAT READ WAS TAKEN AT GOES ON THE
			// RECORD, which is the half that IS load-bearing. It is
			// read in the SAME TRANSACTION as the seat row above, so
			// the number states exactly what was seen: any node whose
			// own chart position covers it has seen everything this
			// decide saw, and one below it has not. That is what turns
			// a seat missing from a node's view into two answers
			// instead of one wrong one.
			claim.ChartPosition = chartPositionOf(ctx, tx)
		}
		mutation, err := EncodeClaim(claim)
		if err != nil {
			return err
		}
		rec.Mutation = mutation
		return nil
	}
	result, err := w.publishAt(ctx, at,
		w.request(&rec, opID, statelog.PatternCreate, decide))
	return result.Position, err
}

// chartPositionOf is the org chart log's checkpoint on THIS node, read inside
// a decide's own transaction.
//
// ZERO WHEN IT CANNOT BE READ, and that is the safe direction rather than a
// swallowed error. The value is a FLOOR a reader compares its own position
// against, so zero says "this bind claims to have seen nothing", which every
// node's position covers — and a seat absent from the view then answers 403
// naming the seat rather than 503 for ever. The opposite default, refusing the
// bind, would make a node that has never applied a chart record unable to bind
// anybody to a seat at all, which is exactly the node a first company sets up
// on.
func chartPositionOf(ctx context.Context, tx *sql.Tx) uint64 {
	var generation, seq int64
	err := tx.QueryRowContext(ctx,
		`SELECT generation, seq FROM statelog_cursor WHERE stream = ?`,
		topics.ChartLogStream).Scan(&generation, &seq)
	if err != nil {
		return 0
	}
	// UINT64 ON THE RECORD, int64 in the column, which is this domain's
	// existing convention for a packed position ([Eviction.From] is the
	// same). A packed position is never negative — the generation is a
	// uint32 shifted 40, which cannot reach the sign bit — so the two
	// forms name one value.
	return uint64(statelog.Position{
		Stream: topics.ChartLogStream, Generation: uint32(generation),
		Seq: uint64(seq),
	}.Packed())
}

// Release gives a claim back.
//
// IT IS NOT A DELETE OF THE SUBJECT. The subject keeps its arbitration anchor,
// so a later claim on it is an ordinary conditional write rather than a create
// at zero — which is what stops a released address being racily re-taken by
// two writers who each read it as free.
//
// # The caller names the HOLDER, and the decide confirms it
//
// The record's scope is where a node that cannot decode it files the
// deferral, and a read about a person looks in THAT PERSON'S bucket — so a
// release has to state the holder's. It used to state the bucket of the TOKEN,
// a hash of a login, a blind or a seat handle that no read ever consults: a
// node deferring an unbinding went on serving the holder's row as if it still
// held the seat, and never said it was behind about them. The scope is fixed
// before the snapshot runs, so the holder cannot be discovered inside it; the
// caller states who it is releasing FROM — every caller has just read the
// person — and the decide refuses when the snapshot disagrees, naming who does
// hold it. The record names the holder too, so the trail row lands on their
// history rather than on nobody's.
//
// # A login is never released on its own
//
// Every principal holds one — it is the name an unbound person's changes are
// recorded under — so a bare release of a login would leave an enrolled
// person the rest of the engine cannot name, and a machine enrolled under
// `token:<id>` silently unbound from its Tier A token. A login is RENAMED
// instead ([Writer.Rename]), and the release that gesture ends with is its
// own; a removal gives every claim back in its own record.
func (w *Writer) Release(ctx context.Context, kind ObjectKind, token, holder,
	opID, reason string) (statelog.Position, error) {

	if kind == KindLogin {
		return statelog.Position{}, fmt.Errorf("%w: a login is never released "+
			"on its own — every principal holds one, and it is the name their "+
			"changes are recorded under. Rename it instead", ErrInvalidLogin)
	}
	return w.release(ctx, w.gesture(), kind, token, holder, opID, reason, false)
}

// release is [Writer.Release]'s body, shared with the second half of a
// replacement.
//
// replaced is true only for [Writer.replace], whose claim on the new token has
// ALREADY moved the column off the old one — so the snapshot this decide
// reads holds nobody on the old token, and that is the expected state rather
// than a refusal. What is still refused is somebody else holding it: between
// the claim and this release another writer may have taken the freed token,
// and a release publishes a clear of whoever holds the column.
func (w *Writer) release(ctx context.Context, at *statelog.Position,
	kind ObjectKind, token, holder, opID, reason string, replaced bool) (
	statelog.Position, error) {

	if err := w.mayAdminister(OpRelease); err != nil {
		return statelog.Position{}, err
	}
	if token == "" || holder == "" || opID == "" {
		return statelog.Position{}, fmt.Errorf("iamdomain: releasing a %s claim "+
			"needs the token, the person it is released from and an operation "+
			"id, and has (%q, %q, %q)", kind, token, holder, opID)
	}
	subject, err := claimSubject(kind, token)
	if err != nil {
		return statelog.Position{}, err
	}
	mutation, err := EncodeClaim(Claim{V: DocumentVersion})
	if err != nil {
		return statelog.Position{}, err
	}
	rec, err := w.record(subject, OpRelease, holder, PeopleScope(holder),
		mutation, reason)
	if err != nil {
		return statelog.Position{}, err
	}
	decide := func(tx *sql.Tx) error {
		who, held, err := holderOf(ctx, tx, kind, token)
		if err != nil {
			return err
		}
		switch {
		case !held && replaced:
			// THE REPLACEMENT'S OWN CLAIM MOVED IT, which is the state
			// this release exists to record: the record closes the old
			// subject on the trail, and its apply clears nothing.
			return nil
		case !held:
			return fmt.Errorf("iamdomain: nobody holds the %s claim on %s, so "+
				"there is nothing to release", kind, token)
		case who != holder:
			// ANOTHER PERSON HOLDS IT, and a release filed under the
			// named holder's bucket would take it from them while
			// every node that deferred it looked in the wrong place.
			return &ErrClaimed{Kind: kind, Token: token, Holder: who}
		}
		return nil
	}
	result, err := w.publishAt(ctx, at,
		w.request(&rec, opID, statelog.PatternArbitrated, decide))
	return result.Position, err
}

// Rename moves a person from one login to another: the NEW ONE IS CLAIMED
// FIRST, and the old one released after.
//
// # The order is the whole gesture
//
// A rename is two records on two subjects, so it is a sequence, and the order
// decides what a refusal leaves behind. It used to release first: a new login
// refused by its holder's grammar (`ops.bot` for a machine, `Jane.Doe` for
// anybody) or held by somebody else then left the row with NO LOGIN — a person
// recorded as nobody, and a machine under `token:<id>` silently unbound from
// its Tier A token. Claimed first, a refusal changes nothing: the claim's
// decide reads the holder's kind and the token's holder inside its own
// snapshot and publishes nothing when either refuses.
//
// The claim's apply is what moves the column — the token goes on the person's
// row and comes off every other — so by the time the release runs the old
// login is already free; the release records that on the old subject, so its
// trail ends in the release rather than in a claim naming somebody who no
// longer holds it. A release refused because another writer took the freed
// login in between is not a failure of the rename, and is not reported as
// one: the name is no longer this person's either way.
func (w *Writer) Rename(ctx context.Context, personID, from, to, opID,
	reason string) (statelog.Position, error) {

	return w.replace(ctx, KindLogin, personID, from, to, opID, reason)
}

// Rebind moves a person from one seat to another, the new one first, for
// [Writer.Rename]'s reason: a bind the chart refuses — a seat this node's chart
// does not hold, a seat somebody else is bound to — used to leave the person
// bound to nothing, having released the seat they were in.
//
// Unbinding is [Writer.Release]; this is only ever a move.
func (w *Writer) Rebind(ctx context.Context, personID, from, to, opID,
	reason string) (statelog.Position, error) {

	return w.replace(ctx, KindSeat, personID, from, to, opID, reason)
}

// replace is the shared body of [Writer.Rename] and [Writer.Rebind].
func (w *Writer) replace(ctx context.Context, kind ObjectKind, personID, from,
	to, opID, reason string) (statelog.Position, error) {

	if err := w.mayAdminister(OpClaim); err != nil {
		return statelog.Position{}, err
	}
	switch {
	case personID == "" || opID == "":
		return statelog.Position{}, fmt.Errorf("iamdomain: moving a %s claim "+
			"needs the person and an operation id, and has (%q, %q)", kind,
			personID, opID)
	case to == "":
		return statelog.Position{}, fmt.Errorf("%w: moving a %s claim needs "+
			"the %s to move to — a move to nothing is a release, and a login "+
			"is never released", ErrInvalid, kind, kind)
	case to == from:
		return statelog.Position{}, fmt.Errorf("%w: %s already holds the %s "+
			"%q, so there is nothing to move", ErrInvalid, personID, kind, to)
	}
	// ONE MARK FOR THE MOVE, so the release decides from a state holding
	// the claim that freed its token.
	mark := w.gesture()
	at, err := w.claim(ctx, mark, kind, to, personID, "", opID+":"+string(kind), "")
	if err != nil || from == "" {
		return at, err
	}
	released, err := w.release(ctx, mark, kind, from, personID,
		opID+":release-"+string(kind), reason, true)
	var claimed *ErrClaimed
	switch {
	case errors.As(err, &claimed):
		// TAKEN BETWEEN THE TWO — see [Writer.Rename]. The move landed.
		return at, nil
	case err != nil:
		return at, fmt.Errorf("iamdomain: the %s moved to %q and the record "+
			"closing %q did not land (retry with the same operation id): %w",
			kind, to, from, err)
	}
	return released, nil
}

// Revoke bumps a person's revocation epoch, ending every session they hold.
//
// NO GRANT IS REQUIRED and that is deliberate: signing out everywhere is
// something a person does to themselves, and reuse detection is something the
// engine does on their behalf at the moment somebody else has their cookie.
// Gating it behind an administrative capability would make the fastest
// response to a compromise the one that needs an administrator.
//
// THE NEW EPOCH IS READ INSIDE THE SNAPSHOT AND STATED ON THE RECORD, never
// incremented by the applier: an applier that did `epoch + 1` would fold over
// an arrival order, and two nodes at one checkpoint have seen the same set in
// a different order.
func (w *Writer) Revoke(ctx context.Context, personID, opID, reason string) (
	statelog.Position, error) {

	if personID == "" || opID == "" {
		return statelog.Position{}, errors.New("iamdomain: a revocation needs " +
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
		return statelog.Position{}, err
	}
	decide := func(tx *sql.Tx) error {
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
		w.request(&rec, opID, statelog.PatternArbitrated, decide))
	return result.Position, err
}

// InvalidateAll ends every session in the company at once, and every machine
// token.
//
// AN ADMINISTRATOR'S GESTURE, unlike [Writer.Revoke], and the asymmetry is the
// blast radius: revoking is something a person does to themselves and
// something the engine does on their behalf at the moment somebody else has
// their cookie, so gating it would make the fastest response to a compromise
// the one that needs an administrator. Ending EVERYBODY's sessions is a
// company-wide act with no self-service reading at all.
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
	statelog.Position, error) {

	if err := w.mayAdminister(OpInvalidate); err != nil {
		return statelog.Position{}, err
	}
	if opID == "" {
		return statelog.Position{}, errors.New("iamdomain: invalidating every " +
			"session needs an operation id — without one a retry of the " +
			"gesture bumps the generation twice, which ends the sessions " +
			"opened between the two")
	}
	rec, err := w.record(InvalidationSubject(), OpInvalidate, "", RootScope(),
		nil, reason)
	if err != nil {
		return statelog.Position{}, err
	}
	var generation uint64
	decide := func(tx *sql.Tx) error {
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
		w.request(&rec, opID, statelog.PatternArbitrated, decide))
	w.announce(ctx, result, err, types.IAMSessionGenerationBumped{
		Generation: generation, By: w.Actor, Reason: reason,
	})
	return result.Position, err
}

// SetStage moves a person between enrolment stages.
func (w *Writer) SetStage(ctx context.Context, personID string, stage iam.Stage,
	opID, reason string) (statelog.Position, error) {

	if err := w.mayAdminister(OpStatus); err != nil {
		return statelog.Position{}, err
	}
	if !stage.Valid() {
		return statelog.Position{}, fmt.Errorf("%w: %q is not an enrolment "+
			"stage — only `active` may act, which is an allowlist of one, so a "+
			"stage this build cannot name would suspend somebody by accident",
			ErrInvalid, stage)
	}
	mutation, err := EncodeStatus(StatusChange{V: DocumentVersion, Stage: stage})
	if err != nil {
		return statelog.Position{}, err
	}
	rec, err := w.record(PersonSubject(personID), OpStatus, personID,
		PeopleScope(personID), mutation, reason)
	if err != nil {
		return statelog.Position{}, err
	}
	result, err := w.publish(ctx, w.request(&rec, opID, statelog.PatternArbitrated, nil))
	return result.Position, err
}

// Remove is the one operation here with no inverse.
//
// IT READS THE CLAIMS INSIDE THE SNAPSHOT, because the record has to carry
// them: the tombstone outlives the person's row, and an operator asking "who
// held this address" after the fact has only that row to read. It carries the
// BLINDS rather than the addresses, for the reason the row's own comment gives.
func (w *Writer) Remove(ctx context.Context, personID, opID, reason string) (
	statelog.Position, error) {

	if err := w.mayAdminister(OpRemove); err != nil {
		return statelog.Position{}, err
	}
	if personID == "" || opID == "" {
		return statelog.Position{}, errors.New("iamdomain: a removal needs a " +
			"person and an operation id")
	}
	rec, err := w.record(PersonSubject(personID), OpRemove, personID,
		PeopleScope(personID), nil, reason)
	if err != nil {
		return statelog.Position{}, err
	}
	decide := func(tx *sql.Tx) error {
		var claims Claims
		err := tx.QueryRowContext(ctx, `
			SELECT email_blind, login, seat_id FROM iam_people WHERE id = ?`,
			personID).Scan(&claims.EmailBlind, &claims.Login, &claims.SeatID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// NO ROW IS NOT A REASON TO PUBLISH NOTHING, and this arm
			// used to be one. A person can be missing from THIS node's
			// rows for two opposite reasons — they were already
			// removed, or this node has not applied their enrolment yet
			// — and a decide that quietly published nothing turned the
			// second into a removal that silently did not happen. So it
			// publishes, carrying an EMPTY released set, which is the
			// truth: this snapshot knows of no claims to give back.
			//
			// The cost when they really were already removed is one
			// record every node's own gate drops, which is what the
			// removal gate is for — and a removal is the rarest write
			// in this domain.
			claims = Claims{}
		case err != nil:
			return fmt.Errorf("iamdomain: read person %s's claims: %w",
				personID, err)
		}
		rec.Mutation, err = EncodeRemoval(Removal{
			V: GateRecordVersion, Released: claims,
		})
		return err
	}
	result, err := w.publish(ctx,
		w.request(&rec, opID, statelog.PatternArbitrated, decide))
	return result.Position, err
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
	decide := func(tx *sql.Tx) error {
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
			GroupGrants:       slices.Clone(in.GroupGrants),
		})
		return err
	}
	req := w.request(&rec, in.OpID, statelog.PatternCreate, decide)
	// THE ONE WRITE IN THIS ESTATE THAT DOES NOT WAIT FOR ITS OWN ROW, and
	// only because nothing in the answer reads it. See
	// [SessionStart.NoWait].
	req.NoWait = in.NoWait
	result, err := w.publish(ctx, req)
	opened.Position = result.Position
	return opened, err
}

// SessionOpened is what a bearer for a session that has just begun carries
// beside its lineage.
type SessionOpened struct {
	// Position is where the start record landed, which the bearer carries
	// so a node below it can tell "not seen yet" from "ended".
	Position statelog.Position

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

	// GroupGrants are what the identity provider's groups conferred at
	// this sign-in — see [Session.GroupGrants]. They are NOT checked
	// against the writer's own grants, unlike a person's declared set:
	// they are the operator's Tier A mapping applied to what the provider
	// asserted, and every node clamps them to its own ceiling at decision
	// time rather than here.
	GroupGrants []iam.Grant

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
// For [Writer.Release]'s reason: validation asks whether a deferral covers the
// PERSON's bucket, so a close has to be filed there. It used to be filed under
// the bucket of the lineage — a hash no read consults — so a node that could
// not decode a sign-out went on serving the session it ended, reporting
// itself current. Both callers know the person: a sign-out reads it off a
// bearer whose signature verified, and ending a named session has just read
// the owner. The decide confirms it against this node's row when there is one;
// when there is none — a session opened a moment ago that this node has not
// applied — the caller's word is all there is, and it is a verified one.
func (w *Writer) CloseSession(ctx context.Context, lineage, person, reason,
	opID string) (statelog.Position, error) {

	if lineage == "" || person == "" || opID == "" {
		return statelog.Position{}, errors.New("iamdomain: closing a session " +
			"needs a lineage, the person it belongs to and an operation id")
	}
	mutation, err := EncodeSession(Session{V: DocumentVersion, EndedReason: reason})
	if err != nil {
		return statelog.Position{}, err
	}
	rec, err := w.record(SessionSubject(lineage), OpClose, person,
		PeopleScope(person), mutation, reason)
	if err != nil {
		return statelog.Position{}, err
	}
	decide := func(tx *sql.Tx) error {
		var owner string
		err := tx.QueryRowContext(ctx,
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
	result, err := w.publish(ctx, w.request(&rec, opID, statelog.PatternArbitrated, decide))
	return result.Position, err
}

// missingKeys names which of the two key-bearing seams this writer lacks, for
// a refusal somebody can act on.
func (w *Writer) missingKeys() string {
	switch {
	case w.blinds == nil && w.sealer == nil:
		return "neither a blind-index key nor a key store"
	case w.blinds == nil:
		return "no blind-index key"
	default:
		return "no key store"
	}
}

// claimSubject is the subject one claim kind arbitrates on.
func claimSubject(kind ObjectKind, token string) (Subject, error) {
	switch kind {
	case KindEmail:
		return EmailSubject(token), nil
	case KindLogin:
		return LoginSubject(token), nil
	case KindSeat:
		return SeatSubject(token), nil
	}
	return Subject{}, fmt.Errorf("iamdomain: %s is not a claim kind — an "+
		"address, a login and a seat binding are the three things two people "+
		"can race for", kind)
}

// holderOf is who currently holds a claim, read INSIDE a decide's snapshot.
func holderOf(ctx context.Context, tx *sql.Tx, kind ObjectKind, token string) (
	string, bool, error) {

	var column string
	switch kind {
	case KindEmail:
		column = "email_blind"
	case KindLogin:
		column = "login"
	case KindSeat:
		column = "seat_id"
	default:
		return "", false, fmt.Errorf("iamdomain: %s is not a claim kind", kind)
	}
	var holder string
	err := tx.QueryRowContext(ctx,
		`SELECT id FROM iam_people WHERE `+column+` = ?`, token).Scan(&holder)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("iamdomain: read the %s claim on %s: %w",
			kind, token, err)
	}
	return holder, true, nil
}

// enrolledKindOf is the kind of an ENROLLED person, read inside a decide's
// snapshot, or a refusal naming why there is none.
//
// A RESERVATION IS NOT ENROLLED. An enrolment's claims land before its content
// record, and their apply leaves a row with no kind — so a row with an empty
// kind is somebody whose enrolment has not finished on this node, and a login
// judged against it would be judged against nothing. Refused rather than
// guessed, and under [ErrNotFound], because what the caller has to do is the
// same either way: name somebody this node holds.
func enrolledKindOf(ctx context.Context, tx *sql.Tx, personID string) (iam.Kind, error) {
	var kind string
	err := tx.QueryRowContext(ctx,
		`SELECT kind FROM iam_people WHERE id = ?`, personID).Scan(&kind)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && reservation(kind):
		return "", fmt.Errorf("%w: this node holds no enrolled person %s, so it "+
			"cannot say which grammar their login must follow — a person's is "+
			"dotted and a machine's is coloned", ErrNotFound, personID)
	case err != nil:
		return "", fmt.Errorf("iamdomain: read person %s's kind: %w", personID, err)
	}
	return iam.Kind(kind), nil
}

// seatExists is the ADVISORY chart read, and its doc is the whole of what it
// promises.
//
// IT GUARANTEES NOTHING. The chart is a different domain on a different
// stream, and nothing orders the two — so this read and a seat's removal are
// concurrent by construction, and a bind that passed here can still land after
// the seat is gone. The residue is a legal named state, not corruption.
//
// WHAT IT BUYS is that binding somebody to a seat handle nobody has ever
// created — a typo, the overwhelmingly common failure — is refused at the
// moment somebody can still fix it, rather than becoming a person who cannot
// act and a 403 nobody can explain.
func seatExists(ctx context.Context, tx *sql.Tx, seatID string) error {
	var present int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM chart_seats WHERE handle = ?`, seatID).Scan(&present)
	switch {
	case err != nil:
		// UNREADABLE IS NOT ABSENT, and here it is not a refusal either:
		// this read guarantees nothing, so a node that cannot perform it
		// is in the state every node is in with respect to the other
		// log anyway. Refusing would make a chart-table outage block
		// every seat binding for a check that was never load-bearing.
		return nil
	case present == 0:
		return fmt.Errorf("iamdomain: the chart on this node has no seat %q. "+
			"This check is ADVISORY — the chart is a different log and nothing "+
			"orders the two — so it is here to catch a typo; if the seat was "+
			"created moments ago, retry", seatID)
	}
	return nil
}

// MintBootstrap issues the company's one way in before it has anybody.
//
// # Why it arbitrates on one subject for the whole domain
//
// Two live bootstrap codes is two ways into an engine that has no other way
// in, so the mint contends at the broker on [BootstrapSubject] — one object
// for the company — and exactly one of two nodes minting at once wins.
//
// # No grant, and what stands in for one
//
// Nobody holds a credential yet, so a capability check here would be asking
// somebody to prove authority the code exists to confer. What bounds it is
// the estate: [Enrolment] of the first person spends the code, and a mint
// against a company that already has people is refused by the CALLER, which
// is the only place the question can be asked honestly — this decide runs
// inside a snapshot that would have to scan a table it has no reason to.
func (w *Writer) MintBootstrap(ctx context.Context, in BootstrapMint) (
	statelog.Position, error) {

	if in.ID == "" || in.Verifier == "" || in.OpID == "" {
		return statelog.Position{}, errors.New("iamdomain: minting a bootstrap " +
			"needs an id, a verifier and an operation id")
	}
	mutation, err := EncodeBootstrapDoc(Bootstrap{
		V: DocumentVersion, ID: in.ID, Verifier: in.Verifier,
		MintedBy: in.MintedBy, ExpiresAt: in.ExpiresAt,
	})
	if err != nil {
		return statelog.Position{}, err
	}
	// THE BOOTSTRAP'S OWN BUCKET, which is what the apply writes.
	//
	// The root scope is what this reached for first, on the reasoning that
	// a bootstrap is about the company rather than about a person — and it
	// is REFUSED, by name, for a reason that outranks it: a deferral at
	// the root freezes every suspension, every revocation and every login
	// behind one record a node could not decode. Only an eviction and a
	// reanchor may state it. A bootstrap hashes its own id into a bucket
	// exactly as a person does, so there is one to name.
	rec, err := w.record(BootstrapSubject(), OpBootstrap, "",
		BucketScope(BootstrapBucket()), mutation, in.Reason)
	if err != nil {
		return statelog.Position{}, err
	}
	result, err := w.publish(ctx,
		w.request(&rec, in.OpID, statelog.PatternArbitrated, nil))
	return result.Position, err
}

// WithdrawBootstrap supersedes a code nobody redeemed.
//
// # Why re-issuing spends what is outstanding
//
// A live code is a way to become the company's first administrator with no
// credential at all. Minting a second without ending the first leaves TWO,
// and an operator who re-issued because they lost the file has no idea the
// original is still in a terminal somewhere. So `crewlet iam bootstrap-code`
// withdraws every outstanding code and then mints, and exactly one is live
// after it runs.
//
// IT IS NOT A REDEMPTION WITH NO PERSON. The two leave the same row state and
// are opposite events; see [Bootstrap.Withdrawn].
func (w *Writer) WithdrawBootstrap(ctx context.Context, id, opID,
	reason string) (statelog.Position, error) {

	if err := w.mayAdminister(OpBootstrap); err != nil {
		return statelog.Position{}, err
	}
	if id == "" || opID == "" {
		return statelog.Position{}, errors.New("iamdomain: withdrawing a " +
			"bootstrap code needs its id and an operation id")
	}
	mutation, err := EncodeBootstrapDoc(Bootstrap{
		V: DocumentVersion, ID: id, Withdrawn: true,
	})
	if err != nil {
		return statelog.Position{}, err
	}
	rec, err := w.record(BootstrapSubject(), OpBootstrap, "",
		BucketScope(BootstrapBucket()), mutation, reason)
	if err != nil {
		return statelog.Position{}, err
	}
	result, err := w.publish(ctx,
		w.request(&rec, opID, statelog.PatternArbitrated, nil))
	return result.Position, err
}

// BootstrapMint is what issuing the one-time code needs.
type BootstrapMint struct {
	// ID is the code's own id, minted by the caller.
	ID string

	// Verifier is what a presented code is checked against, NEVER the
	// code: one readable out of a replicated database by anyone who can
	// read a replicated database is not a credential.
	Verifier string

	// MintedBy is the node that issued it, which is what an operator
	// reading a code they did not expect needs first.
	MintedBy string

	ExpiresAt time.Time
	OpID      string
	Reason    string
}

// SpendBootstrap records the one-time code being used, naming the person it
// created.
//
// # It is a SECOND record on the same subject, not a field of the enrolment
//
// The enrolment arbitrates on the person and this arbitrates on the bootstrap,
// which is what makes two nodes redeeming one code contend: the enrolment's
// own subject is a fresh uuid nobody else would name, so two of them would
// both succeed and the company would have two founders. The sequence is the
// tracker dependency's — one record has one subject — and its residue is a
// spent code with no person, which a mint against a company holding people
// refuses anyway.
func (w *Writer) SpendBootstrap(ctx context.Context, in BootstrapSpend) (
	statelog.Position, error) {

	if in.ID == "" || in.Person == "" || in.OpID == "" {
		return statelog.Position{}, errors.New("iamdomain: spending a bootstrap " +
			"needs its id, the person it created and an operation id")
	}
	mutation, err := EncodeBootstrapDoc(Bootstrap{
		V: DocumentVersion, ID: in.ID, Person: in.Person,
	})
	if err != nil {
		return statelog.Position{}, err
	}
	// BOTH BUCKETS: the bootstrap's own, because the row it spends lives
	// there, and the PERSON's, because the same apply is what names them.
	// A scope that stated one would leave the other's readers certifying a
	// copy this record changed.
	rec, err := w.record(BootstrapSubject(), OpBootstrap, in.Person,
		BucketScope(BootstrapBucket(), BucketOf(in.Person)), mutation, in.Reason)
	if err != nil {
		return statelog.Position{}, err
	}
	result, err := w.publish(ctx,
		w.request(&rec, in.OpID, statelog.PatternArbitrated, nil))
	return result.Position, err
}

// BootstrapSpend is what redeeming the one-time code needs.
type BootstrapSpend struct {
	ID     string
	Person string
	OpID   string
	Reason string
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
		document    []byte
		kind, stage string
	)
	err := tx.QueryRowContext(ctx,
		`SELECT document, kind, stage FROM iam_people WHERE id = ?`, personID).
		Scan(&document, &kind, &stage)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && reservation(kind):
		// A RESERVATION HAS NO DOCUMENT TO FORM THE NEXT ONE FROM, and
		// it is refused as the absence it is rather than as a document
		// that failed to open: the enrolment it belongs to has not
		// finished, and what the caller has to do is the same either
		// way — name somebody this node holds.
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
	statelog.Position, error) {

	if in.PersonID == "" || in.OpID == "" {
		return statelog.Position{}, errors.New("iamdomain: setting credentials " +
			"needs a person and an operation id")
	}
	var mutation []byte
	decide := func(tx *sql.Tx) error {
		person, err := heldPerson(ctx, tx, in.PersonID, "their credential set")
		if err != nil {
			return err
		}
		person.Credentials = in.Apply(person.Credentials)
		mutation, err = EncodePerson(person)
		return err
	}
	rec, err := w.record(PersonSubject(in.PersonID), OpUpdate, in.PersonID,
		PeopleScope(in.PersonID), nil, in.Reason)
	if err != nil {
		return statelog.Position{}, err
	}
	req := w.request(&rec, in.OpID, statelog.PatternArbitrated, func(tx *sql.Tx) error {
		if err := decide(tx); err != nil {
			return err
		}
		rec.Mutation = mutation
		return nil
	})
	result, err := w.publish(ctx, req)
	return result.Position, err
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
	Apply func([]Credential) []Credential

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
// they hold, a reach no wider than theirs, and the revocation epoch they are
// at. A caller reading those first and handing in a finished credential would
// be stating a read from another transaction — the pairing the write authority
// forbids — so a demotion landing between the two would mint a token carrying
// what its owner no longer holds. Minted on the owner's own subject, the mint
// and a demotion contend and exactly one of them decides first.
//
// # What it refuses, and whose rule each is
//
//   - A grant the owner does not hold, or a reach wider than theirs: a token
//     NARROWS its owner and never widens them.
//   - secrets:read and people:manage, WHATEVER the owner holds: both are
//     gestures that need a person present — revealing a credential and
//     changing who may — and a token is what an attacker holding a pipeline's
//     environment already has.
//   - A grant the MINTING PARTY does not hold, for [Writer.mayConfer]'s rule:
//     whoever mints a token sees its value once, so minting one for somebody
//     else is holding their grants oneself.
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
// NIL GRANTS AND AN EMPTY REACH MEAN "AS MUCH AS MAY BE CARRIED": every grant
// the owner holds that a token may carry, and the owner's own reach — resolved
// here, in the snapshot, and stated on the record, so what the token carries is
// never re-derived by a node applying it.
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
	decide := func(tx *sql.Tx) error {
		owner, err := heldPerson(ctx, tx, in.PersonID, "a token for them")
		if err != nil {
			return err
		}
		if err := tokenOwnable(in.PersonID, owner); err != nil {
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
		grants, err := w.tokenGrants(in.Grants, owner.Grants)
		if err != nil {
			return err
		}
		colleague := in.Colleague
		if colleague == "" {
			colleague = owner.Colleague
		}
		if !colleague.Valid() && colleague != "" {
			return fmt.Errorf("%w: %q is not a colleague level", ErrInvalidToken,
				colleague)
		}
		if !colleagueWithin(colleague, owner.Colleague) {
			return fmt.Errorf("%w: a token reaches the company's work no "+
				"further than its owner, who reaches it at %q, and this one "+
				"asks for %q", ErrRefused, owner.Colleague, colleague)
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
			Grants: grants, Colleague: colleague, Epoch: epoch,
			Generation: generation,
		}
		held := make([]Credential, 0, len(owner.Credentials)+1)
		for _, c := range owner.Credentials {
			if c.ID == in.ID {
				// THE SAME MINT, RETRIED after an answer that did not
				// arrive: the operation ledger is what collapses it,
				// and a second copy of one id on the row would be two
				// credentials a revocation names as one.
				continue
			}
			held = append(held, c)
		}
		owner.Credentials = append(held, token)
		minted = TokenMinted{Grants: grants, Colleague: colleague,
			ExpiresAt: in.ExpiresAt, Epoch: epoch, Generation: generation}
		rec.Mutation, err = EncodePerson(owner)
		return err
	}
	result, err := w.publish(ctx,
		w.request(&rec, in.OpID, statelog.PatternArbitrated, decide))
	minted.Position = result.Position
	return minted, err
}

// TokenMint is what minting a machine token needs.
type TokenMint struct {
	// PersonID is the owner: the person or machine the token acts as.
	PersonID string

	// ID is the credential's own id, minted by the caller because it is
	// inside the verifier and the value — both formed before this runs.
	ID string

	// Verifier is [credential.TokenVerifier] over the id and the secret.
	// NEVER the secret: this log is replicated, snapshotted and backed up.
	Verifier string

	Label string

	// Grants is what the token may carry, or nil for every grant the owner
	// holds that a token may carry. Colleague is its reach, or empty for
	// the owner's own. See [Writer.MintToken].
	Grants    []iam.Grant
	Colleague iam.Colleague

	// ExpiresAt is REQUIRED, and at most [credential.MaxTokenLifetime]
	// away.
	ExpiresAt time.Time

	OpID   string
	Reason string
}

// TokenMinted is what a mint landed carrying, and where.
type TokenMinted struct {
	// Position is where the record landed, which the token's value carries
	// so a node below it answers "not yet" rather than "no such token".
	Position statelog.Position

	Grants     []iam.Grant
	Colleague  iam.Colleague
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

// TokenRefusedGrants are the grants a machine token may never carry, whatever
// its owner holds.
//
// BOTH NEED A PERSON PRESENT: revealing a credential, and deciding who may do
// anything at all. A token is what an attacker holding a pipeline's
// environment already has.
var TokenRefusedGrants = []iam.Grant{iam.GrantSecretRead, iam.GrantPeopleManage}

// tokenGrants resolves what a token carries and refuses what it may not.
func (w *Writer) tokenGrants(asked, owner []iam.Grant) ([]iam.Grant, error) {
	grants := asked
	if grants == nil {
		grants = make([]iam.Grant, 0, len(owner))
		for _, g := range owner {
			if g.Valid() && !slices.Contains(TokenRefusedGrants, g) {
				grants = append(grants, g)
			}
		}
	}
	for _, g := range grants {
		switch {
		case slices.Contains(TokenRefusedGrants, g):
			return nil, fmt.Errorf("%w: %s cannot be minted onto a token — it "+
				"is a gesture that needs a person present, and a token is what "+
				"an attacker holding a pipeline's environment already has",
				ErrRefused, g)
		case !g.Valid() || !slices.Contains(owner, g):
			return nil, fmt.Errorf("%w: a token carries a subset of what its "+
				"owner carries, and %s is not among %v", ErrRefused, g, owner)
		}
	}
	if err := w.mayConfer(nil, grants); err != nil {
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
// # It arbitrates on the ADDRESS, not on the invitation's own id
//
// An invitation's id is a fresh uuid nobody else would name, so a subject
// keyed on it would never contend with anything — and two administrators
// inviting one address would both succeed, producing two links for one
// person, either of which creates them. The address is the thing two writers
// must not both win, so it is the subject: a create at an expectation of
// zero, on the SAME subject an enrolment's email claim takes, so an
// invitation and a hire for one address contend as well.
//
// The loser reads a newer row and answers 409 naming what is already there,
// which is [ErrClaimed]'s whole job.
//
// # The link is not here
//
// This publishes the invitation's id and what redeeming it confers. The
// SECRET a person follows is the caller's — minted by them, shown once, and
// never written to this log, because a log is replicated, snapshotted, backed
// up and donated to joining peers. What is stored is the id, which is the
// verifier: holding the link is holding the id.
func (w *Writer) Invite(ctx context.Context, in InviteMint) (
	statelog.Position, error) {

	if err := w.mayAdminister(OpInvite); err != nil {
		return statelog.Position{}, err
	}
	// WHAT IT CONFERS IS DECIDED HERE, ONCE, against the issuer — the
	// redemption reads it back rather than deciding again — so this is
	// the one place an invitation can be held to the rule every other
	// grant change is: a caller may not confer what they do not hold.
	if err := w.mayConfer(nil, in.Grants); err != nil {
		return statelog.Position{}, err
	}
	switch {
	case in.ID == "" || in.OpID == "":
		return statelog.Position{}, errors.New("iamdomain: an invitation needs " +
			"its own id and an operation id")
	case in.Email == "":
		return statelog.Position{}, errors.New("iamdomain: an invitation needs " +
			"the address it is for — it arbitrates on that address, so one " +
			"with none would contend with nothing and two would both win")
	case in.ExpiresAt.IsZero():
		return statelog.Position{}, errors.New("iamdomain: an invitation needs " +
			"an expiry; one read as `never` is a superuser claim that stays " +
			"live in somebody's mailbox for the life of the company")
	case in.Colleague != "" && !in.Colleague.Valid():
		// REFUSED AT THE INVITATION rather than at its redemption: what
		// redeeming confers is decided here, once, and a level the
		// enrolment would refuse is a link that can never be redeemed —
		// found out by the person it was sent to.
		return statelog.Position{}, fmt.Errorf("%w: %q is not a colleague "+
			"level — want one of %v", ErrInvalid, in.Colleague, iam.Colleagues)
	}
	if w.blinds == nil || w.sealer == nil {
		return statelog.Position{}, fmt.Errorf("iamdomain: this node cannot "+
			"mint an invitation: %s", w.missingKeys())
	}
	blinder, err := w.blinds.Blinder(ctx)
	if err != nil {
		return statelog.Position{}, fmt.Errorf("iamdomain: this node cannot "+
			"derive the subject an invitation's address arbitrates on: %w", err)
	}
	blind, err := blinder.Email(in.Email)
	if err != nil {
		return statelog.Position{}, fmt.Errorf("iamdomain: blind an "+
			"invitation's address: %w", err)
	}
	// SEALED UNDER THE INVITATION'S OWN ID rather than a person's, because
	// there is no person yet. The key is minted here and destroyed by the
	// key duty once no invitation row owns it ([ShredKeys]) — a refused
	// invitation's past the grace, a collected one's after the sweep deletes
	// its row — so an address somebody typed and never sent leaves no
	// cleartext anywhere, which is the same promise a removal makes, one
	// object earlier.
	if err := w.sealer.Mint(ctx, in.ID, w.Actor, w.Now()); err != nil {
		return statelog.Position{}, fmt.Errorf("iamdomain: mint an "+
			"invitation's key: %w", err)
	}
	sealed, err := w.sealer.Seal(ctx, in.ID, FieldEmail, in.Email)
	if err != nil {
		return statelog.Position{}, fmt.Errorf("iamdomain: seal an "+
			"invitation's address: %w", err)
	}
	mutation, err := EncodeInvitation(Invitation{
		V: DocumentVersion, ID: in.ID, Sealed: sealed,
		InvitedBy: w.Actor, Grants: in.Grants, Colleague: in.Colleague,
		ExpiresAt: in.ExpiresAt,
	})
	if err != nil {
		return statelog.Position{}, err
	}
	subject := EmailSubject(blind)
	// THE BUCKET IS THE ADDRESS'S OWN, matching the apply: an invitation
	// has no person until it is redeemed, and a bucket derived from an
	// empty id would put every outstanding invitation in one sweep.
	rec, err := w.record(subject, OpInvite, "",
		BucketScope(BucketOf(blind)), mutation, in.Reason)
	if err != nil {
		return statelog.Position{}, err
	}
	// THE ROW READ IS WHAT REFUSES THE SEQUENTIAL CASE, and the broker's
	// create-at-zero is what settles the concurrent one. Both are needed
	// and neither substitutes for the other: two administrators inviting
	// at the same instant contend at the broker because neither subject
	// has an anchor yet, and the second one an hour later publishes above
	// the anchor the first left — so without this read it would land.
	//
	// IT READS BOTH TABLES, because an address can be spoken for by a
	// PERSON or by an invitation nobody has redeemed. Reading only the
	// people, as a claim's own decide does, missed every outstanding
	// invitation and produced a second link for the same address.
	decide := func(tx *sql.Tx) error {
		holder, held, err := holderOf(ctx, tx, KindEmail, blind)
		if err != nil {
			return err
		}
		if held {
			return &ErrClaimed{Kind: KindEmail, Token: blind, Holder: holder}
		}
		outstanding, found, err := openInvitationFor(ctx, tx, blind, w.Now())
		if err != nil {
			return err
		}
		if found {
			return &ErrClaimed{Kind: KindEmail, Token: blind, Holder: outstanding}
		}
		return nil
	}
	result, err := w.publish(ctx,
		w.request(&rec, in.OpID, statelog.PatternCreate, decide))
	return result.Position, err
}

// openInvitationFor is the invitation on an address that has not been
// redeemed and has not aged out, read INSIDE a decide's snapshot.
//
// THE WRITER'S CLOCK DECIDES THE EXPIRY HERE, which is the one place in this
// domain that is acceptable: every instant an applier stores is the BROKER's,
// and this read is advisory — what it buys is a better refusal, never the
// arbitration. A writer whose clock is minutes out re-issues an invitation
// somebody could still have used, or refuses one that had just aged out, and
// both are ordinary administrative outcomes rather than a correctness loss.
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
	// ID is the invitation's own id, which is also the verifier a
	// redemption presents. The CALLER mints it, because the caller is
	// what shows the link once and this never sees the secret again.
	ID string

	// Email is the address it is for, in the clear. It is blinded for the
	// subject and sealed for the row before anything is published.
	Email string

	// Grants and Colleague are what redeeming it confers, decided ONCE by
	// whoever issued it rather than again by whoever processes the
	// redemption — which is also what stops a redemption being a way to
	// ask for more than was offered.
	Grants    []iam.Grant
	Colleague iam.Colleague

	// ExpiresAt is when it stops being redeemable. REQUIRED.
	ExpiresAt time.Time

	OpID   string
	Reason string
}

// UpdatePerson rewrites one person's own document — their grants, their reach
// into the work, their name — forming the new whole INSIDE the snapshot.
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
// THE BOOTSTRAP IS THE STATED EXEMPTION and it is not reached through here:
// [Writer.Enrol] creates the first person, while nobody holds a credential at
// all.
func (w *Writer) UpdatePerson(ctx context.Context, in PersonUpdate) (
	statelog.Position, error) {

	if err := w.mayAdminister(OpUpdate); err != nil {
		return statelog.Position{}, err
	}
	switch {
	case in.PersonID == "" || in.OpID == "":
		return statelog.Position{}, errors.New("iamdomain: updating a person " +
			"needs a person and an operation id")
	case in.Apply == nil:
		return statelog.Position{}, errors.New("iamdomain: updating a person " +
			"needs the function that forms the new document inside the " +
			"snapshot — a caller holding the current one read it in another " +
			"transaction, which is the pairing the write authority forbids")
	}
	// THE NAME IS SEALED BEFORE THE DECIDE, never inside it. Sealing is a
	// fleet-secret read, and a decide that performed one would be a
	// transaction whose commit depends on a coordination round trip — the
	// same reason the applier never decrypts. So it happens here, once,
	// and the decide only places the ciphertext.
	sealedName := ""
	if in.Name != nil {
		if w.sealer == nil {
			return statelog.Position{}, fmt.Errorf("iamdomain: this node "+
				"cannot change a name: it has %s, and a name is sealed under "+
				"the person's own key before it is published", w.missingKeys())
		}
		var err error
		if sealedName, err = w.sealer.Seal(ctx, in.PersonID, FieldName,
			*in.Name); err != nil {
			return statelog.Position{}, err
		}
	}
	var mutation []byte
	// THE LAST RUN'S BEFORE AND AFTER, which is the pair the landed record
	// actually carries: a decide may run again against a fresh snapshot.
	var before, after []iam.Grant
	decide := func(tx *sql.Tx) error {
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
		if err := w.mayConfer(person.Grants, updated.Grants); err != nil {
			return err
		}
		before, after = slices.Clone(person.Grants), slices.Clone(updated.Grants)
		mutation, err = EncodePerson(updated)
		return err
	}
	rec, err := w.record(PersonSubject(in.PersonID), OpUpdate, in.PersonID,
		PeopleScope(in.PersonID), nil, in.Reason)
	if err != nil {
		return statelog.Position{}, err
	}
	req := w.request(&rec, in.OpID, statelog.PatternArbitrated, func(tx *sql.Tx) error {
		if err := decide(tx); err != nil {
			return err
		}
		rec.Mutation = mutation
		return nil
	})
	result, err := w.publish(ctx, req)
	// ONE ROW PER PERSON WRITE THAT MOVED A GRANT, and none for a write
	// that changed a name or a colleague level: "who can do what to this
	// deployment, and since when" is the question the row answers.
	if added, removed := grantDelta(before, after); len(added)+len(removed) > 0 {
		w.announce(ctx, result, err, types.IAMGrantsChanged{
			Person: in.PersonID, Added: added, Removed: removed, By: w.Actor,
			Version: result.Position.Packed(),
		})
	}
	return result.Position, err
}

// mayConfer refuses a change that ADDS a grant this writer's party does not
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
func (w *Writer) mayConfer(before, after []iam.Grant) error {
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
	// IT IS NOT [PersonUpdate.Apply]'s TO SET. A name is sealed under
	// this person's own key, which is a fleet-secret read, and the apply
	// runs inside the decide's transaction — so it is sealed before,
	// placed on the document the apply is handed, and the apply may still
	// overwrite it if that is genuinely what the caller means.
	Name *string

	// Apply forms the new document from the current one, INSIDE the
	// snapshot. A function rather than a value for [CredentialSet.Apply]'s
	// reason: the caller does not hold the current document and must not
	// read it separately.
	//
	// IT MAY REFUSE, which a caller uses for everything this package
	// cannot judge — an unknown colleague level, a stage the surface will
	// not set here — and the refusal travels out of the decide unwrapped.
	Apply func(Person) (Person, error)

	OpID   string
	Reason string
}

// SpendInvitation records an invitation being used, naming the person it
// created.
//
// A SECOND RECORD ON THE INVITATION'S OWN SUBJECT, for [SpendBootstrap]'s
// reason: the enrolment beside it arbitrates on a fresh person id nobody else
// would name, so two nodes redeeming one link would both succeed and the
// company would have two people where somebody invited one. Contending here is
// what makes exactly one win.
//
// NO GRANT. The invitation IS the authority — what redeeming it confers was
// decided by whoever issued it, once, rather than again by whoever happens to
// process the redemption.
func (w *Writer) SpendInvitation(ctx context.Context, in InvitationSpend) (
	statelog.Position, error) {

	if in.ID == "" || in.Blind == "" || in.Person == "" || in.OpID == "" {
		return statelog.Position{}, errors.New("iamdomain: spending an " +
			"invitation needs its id, the address blind it arbitrates on, " +
			"the person it created and an operation id")
	}
	mutation, err := EncodeInvitation(Invitation{
		V: DocumentVersion, ID: in.ID, Person: in.Person,
	})
	if err != nil {
		return statelog.Position{}, err
	}
	// THE ADDRESS BLIND IS THE SUBJECT, not the invitation's own id, and
	// the reason is what an invitation IS: a claim on an address by
	// somebody who has no person yet. It arbitrates where the address
	// does, which is what makes an invite and an enrolment for one
	// address contend — and what makes two nodes redeeming one link
	// contend with each other.
	rec, err := w.record(EmailSubject(in.Blind), OpRedeem, in.Person,
		PeopleScope(in.Person), mutation, in.Reason)
	if err != nil {
		return statelog.Position{}, err
	}
	result, err := w.publish(ctx,
		w.request(&rec, in.OpID, statelog.PatternArbitrated, nil))
	return result.Position, err
}

// InvitationSpend is what redeeming an invitation needs.
type InvitationSpend struct {
	// ID is the invitation's own id, which the record's payload carries
	// so the applier knows which row it is filling in.
	ID string

	// Blind is the keyed address blind this record ARBITRATES on. See
	// [Writer.SpendInvitation] for why the two are different values.
	Blind string

	Person string
	OpID   string
	Reason string
}
