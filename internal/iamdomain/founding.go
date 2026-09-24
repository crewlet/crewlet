package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// THE FOUNDING: how a company with nobody in it acquires its first person.
//
// Everything about the one-time founder code lives here — its states, the
// reads that say which one it is in, the gestures that mint, withdraw and
// spend it, and the basis the first person's enrolment is held to — because
// they are one state machine on one subject, and reading it spread across the
// enrolment's decides and the directory's listings is how a rule about it came
// to be stated in three places that disagreed.

// ErrBootstrapClosed reports the first-person exemption asked for once it is
// over for good: the company has started — somebody is enrolled, or a code
// already created somebody — so there is somebody whose grants bound whoever
// comes next. A surface wraps it for `api.auth.bootstrap: closed` too, which
// is the same answer given by the configuration rather than the estate.
//
// ITS OWN SENTINEL, wrapped beside [ErrRefused], because the surfaces answer
// it differently from a dead code: this one is permanent and says "ask
// somebody who already has an account", and a caller told it about a code that
// merely aged out gives up on a company that is still waiting for them.
var ErrBootstrapClosed = errors.New("iamdomain: the first-person exemption " +
	"is closed")

// ErrBootstrapCodeDead reports a one-time code the log does not hold as live
// and nobody redeemed: absent, aged out, or withdrawn by a re-issue.
//
// THE REMEDY IS ONE COMMAND — `crewlet iam bootstrap-code`, or a restart of the
// node whose file holds it — which is what makes it a different answer from
// [ErrBootstrapClosed] rather than a flavour of it.
var ErrBootstrapCodeDead = errors.New("iamdomain: that one-time code is not " +
	"live on the log")

// bootstrappable refuses the first-person exemption once it no longer
// applies, read inside whatever snapshot it is handed.
//
// TWO FACTS AND BOTH ARE NEEDED. The code must be LIVE by [BootstrapCode.State]
// — or already redeemed by THIS founder, which is a retry of a bootstrap whose
// spend landed — because a code is what proves the caller can read a file on
// the host; and nobody ELSE may be enrolled by [anybodyEnrolled], because the
// exemption exists only for the instant there is nobody whose grants could
// bound it. Both are the predicates every other reader of those questions
// asks, so the route, the boot path and this decide cannot disagree.
//
// THE GRANTS ARE NOT BOUNDED BY A WRITER, which is the exemption itself — but
// a grant this build cannot name is still refused, because conferring a
// spelling nothing can check is not conferring the ceiling.
func (w *Writer) bootstrappable(ctx context.Context, tx *sql.Tx, in Enrolment) error {
	code, err := bootstrapCodeIn(ctx, tx, in.BootstrapCode)
	if err != nil {
		return err
	}
	switch state := code.State(w.Now()); {
	case state == CodeLive:
	case state == CodeRedeemed && code.Person == in.PersonID:
	case state == CodeRedeemed:
		// SOMEBODY ELSE WAS CREATED WITH IT, which is the company
		// having started — the enrolled check below would say so too,
		// but only once this node has applied that person.
		return fmt.Errorf("%w: %w: the one-time code already created "+
			"somebody else", ErrRefused, ErrBootstrapClosed)
	default:
		return fmt.Errorf("%w: %w: the one-time code is %s — a code file is "+
			"worth only what the log says about it, and a fresh one is "+
			"`crewlet iam bootstrap-code` away", ErrRefused,
			ErrBootstrapCodeDead, state)
	}
	somebody, err := anybodyEnrolled(ctx, tx, in.PersonID)
	if err != nil {
		return err
	}
	if somebody {
		return fmt.Errorf("%w: %w: this company already has somebody in "+
			"it, and an administrator enrols or invites everybody after "+
			"them", ErrRefused, ErrBootstrapClosed)
	}
	for _, g := range in.Grants {
		if !g.Valid() {
			return fmt.Errorf("%w: %q is not a grant this build knows",
				ErrRefused, g)
		}
	}
	return nil
}

// BootstrapCode is one founder code as the log holds it — the way into a
// company that has nobody in it, and what became of it.
type BootstrapCode struct {
	ID        string
	MintedBy  string
	ExpiresAt time.Time

	// MintedAt is the broker's instant for the record that minted it —
	// identical on every node, which is what lets the person a redemption
	// creates be derived from it ([BootstrappedPersonID]).
	MintedAt time.Time

	// SpentAt is the broker's instant a record stopped this code working —
	// a redemption or a withdrawal — and zero while none has.
	SpentAt time.Time

	// Person is who a redemption created, and empty on a withdrawal: the
	// row keeps WHICH way a code stopped, because "this code was used" and
	// "an operator replaced it" are opposite events (see
	// [Bootstrap.Withdrawn]).
	Person string
}

// CodeState is what one founder code is, at one instant.
//
// A NAMED STATE rather than a live flag, because the surface that redeems a
// code answers each differently and must never guess between them: a LIVE
// code creates the first person, a REDEEMED one means the company has started,
// and an AGED-OUT or WITHDRAWN one — or one the log never received — is a file
// its holder can replace with one command. Folded into one bool, the last three
// were answered as the second, and a founder holding a code a day old was told
// their company had started.
type CodeState string

const (
	// CodeAbsent is a code this node's log holds no row for: never minted,
	// minted by a record that never landed, or swept a week past its
	// expiry.
	CodeAbsent CodeState = "absent"

	// CodeLive is minted, unspent, not withdrawn and inside its lifetime.
	CodeLive CodeState = "live"

	// CodeAgedOut is past its expiry with nothing having spent it.
	CodeAgedOut CodeState = "aged_out"

	// CodeWithdrawn was superseded by a re-issue before anybody used it.
	CodeWithdrawn CodeState = "withdrawn"

	// CodeRedeemed created somebody.
	CodeRedeemed CodeState = "redeemed"
)

// CodeStates is every state, for validation and for a test that walks them.
var CodeStates = []CodeState{
	CodeAbsent, CodeLive, CodeAgedOut, CodeWithdrawn, CodeRedeemed,
}

// Valid reports whether a state is one this build knows.
func (s CodeState) Valid() bool { return slices.Contains(CodeStates, s) }

// State is what this code is at now.
//
// THE ONE PREDICATE for whether a code works, read by every party that asks:
// the redeeming route on any ingress node, the boot path deciding whether its
// file is worth keeping, the re-issue deciding what to withdraw, and the person
// record's own decide ([Writer.Enrol]). Written once per caller, the copies had
// already disagreed about a code with no expiry — the decide read it as
// eternal and the listing as dead.
//
// SPENT OUTRANKS AGED OUT, because a spend is a record and an expiry is a
// reading of a clock: a code somebody redeemed stays redeemed once its hours
// run out.
//
// NO EXPIRY IS AGED OUT, the fail-closed reading. Every mint states one
// ([Writer.MintBootstrap] refuses a mint that does not), so a row without it
// is a withdrawal of a code this node never saw minted — and a code that never
// aged would be a superuser claim in a file for ever.
func (c BootstrapCode) State(now time.Time) CodeState {
	switch {
	case c.ID == "":
		return CodeAbsent
	case !c.SpentAt.IsZero() && c.Person != "":
		return CodeRedeemed
	case !c.SpentAt.IsZero():
		return CodeWithdrawn
	case c.ExpiresAt.IsZero() || !now.Before(c.ExpiresAt):
		return CodeAgedOut
	}
	return CodeLive
}

// BootstrapCode resolves one founder code by its id, on this file's three
// answers: the zero value for a code this node's log holds no row for, and an
// error for a node that could not tell.
//
// ONE ROW WHATEVER BECAME OF IT, which is what lets a caller holding a code
// that no longer works be told why rather than told they typed it wrong: the
// id is the SHA-256 of a value with thirty-two bytes of entropy, so presenting
// a code that resolves to ANY row is proof of holding a real one.
func (r *Reader) BootstrapCode(ctx context.Context, id string) (BootstrapCode, error) {
	var out BootstrapCode
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = bootstrapCodeIn(ctx, tx, id)
		return err
	})
	return out, err
}

// bootstrapCodeIn reads one code inside a transaction already open, which is
// what lets the person record's decide read it in its own snapshot and the
// reader outside one through the same scan.
func bootstrapCodeIn(ctx context.Context, tx *sql.Tx, id string) (BootstrapCode, error) {
	var (
		row                          BootstrapCode
		expires, created, redeemedAt int64
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, minted_by, expires_at, created_at, redeemed_at, person_id
		  FROM iam_bootstrap_codes WHERE id = ?`, id).
		Scan(&row.ID, &row.MintedBy, &expires, &created, &redeemedAt, &row.Person)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return BootstrapCode{}, nil
	case err != nil:
		return BootstrapCode{}, fmt.Errorf("iamdomain: read a bootstrap code: %w", err)
	}
	row.ExpiresAt = fromMillis(expires)
	row.MintedAt = fromMillis(created)
	row.SpentAt = fromMillis(redeemedAt)
	return row, nil
}

// OutstandingBootstrapCodes are the codes that are LIVE at now, by
// [BootstrapCode.State].
//
// WHAT RE-ISSUING HAS TO WITHDRAW. A live code is a way to become the
// company's first administrator with no credential at all, so an operator who
// re-issues because they lost the file must not leave the original working in
// somebody's terminal.
//
// THE CLOCK IS THE CALLER'S, which is safe in the one direction that matters:
// a writer whose clock is behind withdraws a code that had already aged out,
// which costs one extra record and changes nothing.
//
// THE SQL NARROWS AND THE PREDICATE DECIDES. The query takes every unspent row
// — a handful, one per boot that found an empty estate — and [BootstrapCode.State]
// says which are live, so this listing and a redemption cannot disagree about
// one code.
func (r *Reader) OutstandingBootstrapCodes(ctx context.Context, now time.Time) (
	[]BootstrapCode, error) {

	var out []BootstrapCode
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT id, minted_by, expires_at, created_at, redeemed_at, person_id
			  FROM iam_bootstrap_codes WHERE redeemed_at = 0
			ORDER BY created_at`)
		if err != nil {
			return fmt.Errorf("iamdomain: read the outstanding bootstrap "+
				"codes: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				row                          BootstrapCode
				expires, created, redeemedAt int64
			)
			if err := rows.Scan(&row.ID, &row.MintedBy, &expires,
				&created, &redeemedAt, &row.Person); err != nil {
				return fmt.Errorf("iamdomain: scan a bootstrap code: %w", err)
			}
			row.ExpiresAt = fromMillis(expires)
			row.MintedAt = fromMillis(created)
			row.SpentAt = fromMillis(redeemedAt)
			if row.State(now) == CodeLive {
				out = append(out, row)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
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
// the ESTATE, read in this record's own snapshot: a mint against a company
// that has started is refused with [ErrBootstrapClosed], on [anybodyEnrolled]
// — the predicate the first person's own enrolment is refused on. It used to
// be left to the caller, and the two callers asked two different questions:
// the boot path asked whether anybody was enrolled, and the re-issue asked
// whether an active, credentialled administrator existed — so a company whose
// only person was suspended was handed a code no record would ever honour.
// The callers still ask first, so a refused mint writes no file; this is the
// answer that counts.
//
// # A code always ages out
//
// A mint that states no expiry is refused, because a code with none would be
// a superuser claim sitting in a file for ever — and [BootstrapCode.State]
// reads a row without one as aged out, so it would be a code nothing honours
// either.
func (w *Writer) MintBootstrap(ctx context.Context, in BootstrapMint) (
	statelog.Result, error) {

	if in.ID == "" || in.Verifier == "" || in.OpID == "" {
		return statelog.Result{}, errors.New("iamdomain: minting a bootstrap " +
			"needs an id, a verifier and an operation id")
	}
	if in.ExpiresAt.IsZero() {
		return statelog.Result{}, fmt.Errorf("%w: a one-time code needs an "+
			"expiry — one without would be a way to become the first "+
			"administrator for as long as its file survived", ErrInvalid)
	}
	mutation, err := EncodeBootstrapDoc(Bootstrap{
		V: DocumentVersion, ID: in.ID, Verifier: in.Verifier,
		MintedBy: in.MintedBy, ExpiresAt: in.ExpiresAt,
	})
	if err != nil {
		return statelog.Result{}, err
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
		return statelog.Result{}, err
	}
	started := func(tx *sql.Tx) error {
		somebody, err := anybodyEnrolled(ctx, tx, "")
		if err != nil {
			return err
		}
		if somebody {
			return fmt.Errorf("%w: %w: this company already has somebody "+
				"in it, so no code is minted for it", ErrRefused,
				ErrBootstrapClosed)
		}
		return nil
	}
	result, err := w.publish(ctx,
		w.request(&rec, in.OpID, statelog.PatternArbitrated, started))
	return result, err
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
	reason string) (statelog.Result, error) {

	if err := w.mayAdminister(OpBootstrap); err != nil {
		return statelog.Result{}, err
	}
	if id == "" || opID == "" {
		return statelog.Result{}, errors.New("iamdomain: withdrawing a " +
			"bootstrap code needs its id and an operation id")
	}
	mutation, err := EncodeBootstrapDoc(Bootstrap{
		V: DocumentVersion, ID: id, Withdrawn: true,
	})
	if err != nil {
		return statelog.Result{}, err
	}
	rec, err := w.record(BootstrapSubject(), OpBootstrap, "",
		BucketScope(BootstrapBucket()), mutation, reason)
	if err != nil {
		return statelog.Result{}, err
	}
	result, err := w.publish(ctx,
		w.request(&rec, opID, statelog.PatternArbitrated, nil))
	return result, err
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
// person whose code was never marked spent, which is harmless: the person
// closes the exemption by existing ([anybodyEnrolled]), and a mint against a
// company holding people is refused.
func (w *Writer) SpendBootstrap(ctx context.Context, in BootstrapSpend) (
	statelog.Result, error) {

	if in.ID == "" || in.Person == "" || in.OpID == "" {
		return statelog.Result{}, errors.New("iamdomain: spending a bootstrap " +
			"needs its id, the person it created and an operation id")
	}
	mutation, err := EncodeBootstrapDoc(Bootstrap{
		V: DocumentVersion, ID: in.ID, Person: in.Person,
	})
	if err != nil {
		return statelog.Result{}, err
	}
	// BOTH BUCKETS: the bootstrap's own, because the row it spends lives
	// there, and the PERSON's, because the same apply is what names them.
	// A scope that stated one would leave the other's readers certifying a
	// copy this record changed.
	rec, err := w.record(BootstrapSubject(), OpBootstrap, in.Person,
		BucketScope(BootstrapBucket(), BucketOf(in.Person)), mutation, in.Reason)
	if err != nil {
		return statelog.Result{}, err
	}
	result, err := w.publish(ctx,
		w.request(&rec, in.OpID, statelog.PatternArbitrated, nil))
	return result, err
}

// BootstrapSpend is what redeeming the one-time code needs.
type BootstrapSpend struct {
	ID     string
	Person string
	OpID   string
	Reason string
}
