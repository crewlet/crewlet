package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
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
// is a LEGAL NAMED STATE rather than corruption: the orphan-claim duty reports
// it and the sweep collects it, and the alternative — writing the person first
// — would leave a person nobody can find, holding an address somebody else may
// then take.
func (w *Writer) Enrol(ctx context.Context, in Enrolment) (statelog.Position, error) {
	if err := w.mayAdminister(OpEnrol); err != nil {
		return statelog.Position{}, err
	}
	if err := in.validate(); err != nil {
		return statelog.Position{}, err
	}
	if w.blinder == nil || w.sealer == nil {
		return statelog.Position{}, fmt.Errorf("iamdomain: this node cannot "+
			"enrol anybody: it has %s. An enrolment has to derive the subject "+
			"the address claim arbitrates on and seal the values that belong "+
			"to the person, and a node that guessed at either would put two "+
			"people on one address", w.missingKeys())
	}

	blind, err := w.blinder.Email(in.Email)
	if err != nil {
		return statelog.Position{}, err
	}
	if err := w.sealer.Mint(ctx, in.PersonID, w.Actor, w.Now()); err != nil {
		return statelog.Position{}, err
	}
	sealedName, err := w.sealer.Seal(ctx, in.PersonID, FieldName, in.Name)
	if err != nil {
		return statelog.Position{}, err
	}
	sealedEmail, err := w.sealer.Seal(ctx, in.PersonID, FieldEmail, in.Email)
	if err != nil {
		return statelog.Position{}, err
	}

	// THE ADDRESS FIRST. It is the claim most likely to be contested —
	// two administrators adding one new joiner — so it is the one that
	// should fail before anything else has happened.
	if _, err := w.claim(ctx, KindEmail, blind, in.PersonID, sealedEmail,
		in.OpID+":email"); err != nil {
		return statelog.Position{}, err
	}
	if in.Login != "" {
		if _, err := w.claim(ctx, KindLogin, in.Login, in.PersonID, "",
			in.OpID+":login"); err != nil {
			return statelog.Position{}, err
		}
	}

	person := Person{
		V: DocumentVersion, Kind: in.Kind, Stage: in.Stage,
		NameSealed: sealedName, EmailSealed: sealedEmail,
		Credentials: in.Credentials, Grants: in.Grants,
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
	// ARBITRATED, NOT A CREATE, and the claims are why: they run first and
	// their apply leaves a RESERVATION row for this person, so a create
	// pattern would find a guarding row and refuse the enrolment that put
	// it there. What makes a retry of the whole gesture land once is the
	// operation ledger, which is the mechanism for that anyway — the
	// guarding row never was.
	result, err := w.publish(ctx, w.request(&rec, in.OpID, statelog.PatternArbitrated, nil))
	return result.Position, err
}

// Enrolment is what creating a person needs.
type Enrolment struct {
	// PersonID is minted by the CALLER, not here, and it is a uuid7.
	//
	// BY THE CALLER because an enrolment is a sequence of appends and
	// every one of them has to name the same person: an id minted inside
	// the first would not be available to form the second's payload, and
	// an id minted per append would enrol three people.
	PersonID string

	Kind  iam.Kind
	Stage iam.Stage

	// Name and Email are CLEARTEXT here and nowhere after: they are
	// sealed before the first record is formed, and no payload this
	// gesture publishes carries either.
	Name  string
	Email string

	// Login is optional. A machine takes one and a person may not have
	// chosen theirs yet — an invited person has an address and no login
	// until they redeem.
	Login string

	Credentials []Credential
	Grants      []iam.Grant

	// OpID is the operation id for the whole gesture. Each append derives
	// its own from it with a suffix, so a retry of the sequence dedupes
	// step by step rather than all-or-nothing.
	OpID   string
	Reason string
}

func (e Enrolment) validate() error {
	switch {
	case e.PersonID == "":
		return errors.New("iamdomain: an enrolment needs the person id it is " +
			"creating: every append in the sequence names it, so one minted " +
			"per append would enrol three people")
	case e.OpID == "":
		return errors.New("iamdomain: an enrolment needs an operation id — it " +
			"is a sequence of appends, and without a stable id a retry cannot " +
			"tell which of them already landed")
	case !e.Kind.Valid():
		return fmt.Errorf("iamdomain: %q is not a principal kind", e.Kind)
	case !e.Stage.Valid():
		return fmt.Errorf("iamdomain: %q is not an enrolment stage", e.Stage)
	case e.Email == "":
		return errors.New("iamdomain: an enrolment needs an address: it is " +
			"what the person is found by, and a person with none can never " +
			"sign in")
	case e.Login != "" && !iam.ValidLogin(e.Login) && !iam.ValidMachineHandle(e.Login):
		return fmt.Errorf("iamdomain: %q is neither a person's login "+
			"(segments joined by dots) nor a machine's handle (segments "+
			"joined by a colon) — both require a separator, which is what "+
			"keeps them apart from a seat handle in one author column", e.Login)
	}
	return nil
}

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
	return w.claim(ctx, kind, token, personID, "", opID)
}

// claim is the shared body, so an enrolment's own claims and an operator's
// take exactly the same path — a second implementation is where one of them
// comes to publish at a different pattern.
func (w *Writer) claim(ctx context.Context, kind ObjectKind, token, personID,
	sealed, opID string) (statelog.Position, error) {

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
	result, err := w.publish(ctx, w.request(&rec, opID, statelog.PatternCreate, decide))
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
func (w *Writer) Release(ctx context.Context, kind ObjectKind, token, opID,
	reason string) (statelog.Position, error) {

	if err := w.mayAdminister(OpRelease); err != nil {
		return statelog.Position{}, err
	}
	subject, err := claimSubject(kind, token)
	if err != nil {
		return statelog.Position{}, err
	}
	mutation, err := EncodeClaim(Claim{V: DocumentVersion})
	if err != nil {
		return statelog.Position{}, err
	}
	// THE SCOPE IS THE ROOT ONLY IF NOBODY HOLDS IT. A release is about
	// whoever holds the token, which the decide reads — so the record's
	// scope is formed inside the snapshot rather than by the caller.
	var holder string
	decide := func(tx *sql.Tx) error {
		who, held, err := holderOf(ctx, tx, kind, token)
		if err != nil {
			return err
		}
		if !held {
			return fmt.Errorf("iamdomain: nobody holds the %s claim on %s, so "+
				"there is nothing to release", kind, token)
		}
		holder = who
		return nil
	}
	// The scope has to be formed BEFORE the decide runs, because the
	// framework reads it off the returned envelope — so a release states
	// the token's own bucket, which is where a deferral about it belongs
	// whoever turns out to hold it.
	rec, err := w.record(subject, OpRelease, "",
		BucketScope(BucketOf(token)), mutation, reason)
	if err != nil {
		return statelog.Position{}, err
	}
	result, err := w.publish(ctx, w.request(&rec, opID, statelog.PatternArbitrated, decide))
	_ = holder
	return result.Position, err
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
		var current int64
		err := tx.QueryRowContext(ctx,
			`SELECT epoch FROM iam_revocation_epochs WHERE person_id = ?`,
			personID).Scan(&current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			current = 0
		case err != nil:
			return fmt.Errorf("iamdomain: read person %s's epoch: %w",
				personID, err)
		}
		rec.Mutation, err = EncodeRevocation(Revocation{
			V: DocumentVersion, Epoch: uint64(current) + 1,
		})
		return err
	}
	result, err := w.publish(ctx,
		w.request(&rec, opID, statelog.PatternArbitrated, decide))
	return result.Position, err
}

// InvalidateAll ends every session in the company at once.
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
// they were — every cookie in existence is below the new value.
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
	decide := func(tx *sql.Tx) error {
		var current int64
		err := tx.QueryRowContext(ctx,
			`SELECT generation FROM iam_session_generation WHERE singleton = 0`).
			Scan(&current)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// NEVER BUMPED IS ZERO, which is what a bearer minted
			// before anybody ever invalidated anything carries. The
			// first bump therefore lands at 1 and ends exactly the
			// cookies that predate it.
			current = 0
		case err != nil:
			return fmt.Errorf("iamdomain: read the session generation: %w", err)
		}
		rec.Mutation, err = EncodeInvalidation(Invalidation{
			V: GateRecordVersion, Generation: uint64(current) + 1,
			By: w.Actor,
		})
		return err
	}
	result, err := w.publish(ctx,
		w.request(&rec, opID, statelog.PatternArbitrated, decide))
	return result.Position, err
}

// SetStage moves a person between enrolment stages.
func (w *Writer) SetStage(ctx context.Context, personID string, stage iam.Stage,
	opID, reason string) (statelog.Position, error) {

	if err := w.mayAdminister(OpStatus); err != nil {
		return statelog.Position{}, err
	}
	if !stage.Valid() {
		return statelog.Position{}, fmt.Errorf("iamdomain: %q is not an "+
			"enrolment stage — only `active` may act, which is an allowlist "+
			"of one, so a stage this build cannot name would suspend somebody "+
			"by accident", stage)
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

// OpenSession records one session beginning.
//
// NO GRANT, for Revoke's reason turned round: signing IN is what a person does
// before they hold anything, and a capability check here would be asking
// somebody to prove authority they acquire by signing in.
func (w *Writer) OpenSession(ctx context.Context, in SessionStart) (
	statelog.Position, error) {

	if in.Lineage == "" || in.Person == "" || in.OpID == "" {
		return statelog.Position{}, errors.New("iamdomain: opening a session " +
			"needs a lineage, a person and an operation id")
	}
	mutation, err := EncodeSession(Session{
		V: DocumentVersion, Person: in.Person, Epoch: in.Epoch,
		AbsoluteExpiresAt: in.AbsoluteExpiresAt,
	})
	if err != nil {
		return statelog.Position{}, err
	}
	rec, err := w.record(SessionSubject(in.Lineage), OpOpen, in.Person,
		PeopleScope(in.Person), mutation, "")
	if err != nil {
		return statelog.Position{}, err
	}
	result, err := w.publish(ctx, w.request(&rec, in.OpID, statelog.PatternCreate, nil))
	return result.Position, err
}

// SessionStart is what opening a session needs.
type SessionStart struct {
	Lineage           string
	Person            string
	Epoch             uint64
	AbsoluteExpiresAt time.Time
	OpID              string
}

// CloseSession ends one session, keeping its row until the sweep collects it.
func (w *Writer) CloseSession(ctx context.Context, lineage, reason, opID string) (
	statelog.Position, error) {

	if lineage == "" || opID == "" {
		return statelog.Position{}, errors.New("iamdomain: closing a session " +
			"needs a lineage and an operation id")
	}
	mutation, err := EncodeSession(Session{V: DocumentVersion, EndedReason: reason})
	if err != nil {
		return statelog.Position{}, err
	}
	rec, err := w.record(SessionSubject(lineage), OpClose, "",
		BucketScope(BucketOf(lineage)), mutation, reason)
	if err != nil {
		return statelog.Position{}, err
	}
	result, err := w.publish(ctx, w.request(&rec, opID, statelog.PatternArbitrated, nil))
	return result.Position, err
}

// missingKeys names which of the two key-bearing seams this writer lacks, for
// a refusal somebody can act on.
func (w *Writer) missingKeys() string {
	switch {
	case w.blinder == nil && w.sealer == nil:
		return "neither a blind-index key nor a key store"
	case w.blinder == nil:
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
