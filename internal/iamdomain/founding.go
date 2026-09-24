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
// reads that say which one a code is in, the gestures that mint, re-issue and
// take it, and the basis the first person's enrolment is held to — because
// they are one state machine on one subject, and reading it spread across the
// enrolment's decides and the directory's listings is how a rule about it came
// to be stated in three places that disagreed.
//
// # Exactly one founder, arbitrated on ONE subject
//
// The first person is the one enrolment that confers the whole ceiling on a
// party whose grants bound nothing, so exactly one may ever land. An enrolment
// is a SEQUENCE — the address, the login, then the person, each on its own
// subject — and person subjects never contend: two founders redeeming two live
// codes at once (a fleet offers one per node) each passed their own person
// record's decide, and the company got two founders carrying the ceiling.
//
// So a founding TAKES THE EXEMPTION before it claims anything: one record on
// [BootstrapSubject], the company's one bootstrap object, naming the code and
// the person it creates ([Writer.takeExemption]). Its decide reads every code
// in its own snapshot and refuses while another code's take is current, so two
// attempts contend at the broker and the loser is told a founding is in
// progress before it has claimed a thing. A take is CURRENT until the code it
// was taken with ages out, or until the attempt is released; it is FINISHED
// with the same code, and the person record's own decide
// ([Writer.bootstrappable]) refuses unless this attempt's take is the current
// one.
//
// # Every earlier attempt is ended before a new one claims anything
//
// A take that lapsed is not a take that stopped: the request that made it may
// still be publishing. So the attempt that takes the exemption next RELEASES
// every earlier one first — a removal of its person on that person's own
// subject ([Writer.releaseAttempt]), which contends with the earlier attempt's
// person record on the one subject where the two could otherwise both land.
// Exactly one of them wins: the earlier attempt finished (and this one is
// refused, the company having started), or it is removed, its reservation's
// address and login released, and every record it had still to publish
// dropped by the removal gate. That is what makes "exactly one founder" hold
// at no matter what instant a lapse falls.
//
// It is also what lets the documented remedy work. An attempt that stopped
// after claiming the founder's address and login left a reservation holding
// both — and the founder is derived from the code, so the fresh code a
// re-issue or a restart hands out derives a DIFFERENT person, whose claims the
// old reservation refused as "that address belongs to somebody" although
// nobody was enrolled. Released before the new attempt claims, the old one
// holds nothing.
//
// The earlier attempts are found two ways, because each alone misses some:
// the persons the log's codes were taken for (an attempt whose take landed and
// nothing else, which has no row to find), and every reservation whose id has
// the founder shape [FounderAttempt] recognises (an attempt older than the
// retention sweep, whose code row is gone while its reservation is not).
//
// # What the one subject does NOT serialise
//
// Minting. A node that boots on an empty estate offers its own code
// ([Writer.MintBootstrap]), and every such mint lands — a fleet offers one per
// node and a founder may be typing any of them. What is exclusive is TAKING
// one. The re-issue ([Writer.ReissueBootstrap]) is the gesture that leaves
// exactly one: it withdraws every live code, releases a current take, and
// mints only in a snapshot where nothing else is outstanding.

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

// ErrBootstrapCodeDead reports a one-time code that can create nobody: absent
// from the log, aged out, withdrawn by a re-issue, or taken by an attempt a
// later one released.
//
// THE REMEDY IS ONE COMMAND — `crewlet iam bootstrap-code`, or a restart of the
// node whose file holds it — which is what makes it a different answer from
// [ErrBootstrapClosed] rather than a flavour of it.
var ErrBootstrapCodeDead = errors.New("iamdomain: that one-time code is not " +
	"live on the log")

// ErrBootstrapInProgress reports a founding refused because another code's
// take holds the first-person exemption.
//
// ITS OWN SENTINEL because its remedies are neither of the other two: the
// founding in progress is finished with the code it was begun with; it lapses
// when that code's lifetime ends; and `crewlet iam bootstrap-code` releases it
// at once. Answered as "closed" it would send a founder away from a company
// nobody has entered; answered as "dead" it would send them for a fresh code
// the same refusal awaits.
var ErrBootstrapInProgress = errors.New("iamdomain: another founding holds " +
	"the first-person exemption")

// FoundingInProgress is the refusal [ErrBootstrapInProgress] arrives as,
// naming when the founding in progress lapses.
//
// THE INSTANT IS THE ONE FACT THE HOLDER CAN ACT ON WITHOUT A COMMAND: it is
// when their own code may be taken, and it is reachable only by presenting a
// live code — proof of reading a file on the host — so saying it discloses
// nothing a stranger could reach.
type FoundingInProgress struct {
	// Until is when the code the founding in progress was begun with ages
	// out, and its take with it.
	Until time.Time
}

func (e *FoundingInProgress) Error() string {
	return fmt.Sprintf("iamdomain: an enrolment begun with another one-time "+
		"code holds the first-person exemption until %s — it is finished with "+
		"that code, and `crewlet iam bootstrap-code` releases it at once",
		e.Until.UTC().Format(time.RFC3339))
}

// Unwrap makes the refusal [ErrBootstrapInProgress] to errors.Is.
func (e *FoundingInProgress) Unwrap() error { return ErrBootstrapInProgress }

// BootstrapCode is one founder code as the log holds it — the way into a
// company that has nobody in it, and what became of it.
type BootstrapCode struct {
	ID        string
	MintedBy  string
	ExpiresAt time.Time

	// MintedAt is the broker's instant for the record that minted it —
	// identical on every node, which is what lets the person a founding
	// creates be derived from it ([BootstrappedPersonID]).
	MintedAt time.Time

	// SpentAt is the broker's instant a record stopped this code being
	// takeable — a founding took it, or a re-issue withdrew it — and zero
	// while none has.
	SpentAt time.Time

	// Person is who a founding took this code for, and empty on a
	// withdrawal: the row keeps WHICH way a code stopped, because "a
	// founder used this code" and "an operator replaced it" are opposite
	// events (see [Bootstrap.Withdrawn]).
	Person string

	// FounderEnrolled and FounderReleased are the standing of the person
	// this code creates ([BootstrapCode.FounderID]), read in the same
	// snapshot as the row: enrolled is the company having started with
	// it, and released is its attempt ended by a later one or by a
	// re-issue ([Writer.releaseAttempt]).
	//
	// ON THE CODE rather than asked separately, because a code's state is
	// a fact about both, and a caller that read the row in one
	// transaction and the person in another would pair a take with a
	// standing that held at no single instant.
	FounderEnrolled bool
	FounderReleased bool
}

// FounderID is the person this code creates: the one a founding took it for,
// or, before any did, the one [BootstrappedPersonID] derives from it.
//
// DERIVED FROM THE CODE and never minted, so every attempt with one code names
// one person — a claim its first attempt took is one the retry already holds —
// and every node derives the same one from the broker's own mint instant.
func (c BootstrapCode) FounderID() string {
	if c.Person != "" {
		return c.Person
	}
	return BootstrappedPersonID(c.ID, c.MintedAt)
}

// CodeState is what one founder code is, at one instant.
//
// A NAMED STATE rather than a live flag, because the surface that redeems a
// code answers each differently and must never guess between them: a LIVE
// code may be taken, a TAKEN one is finished by whoever holds it, a REDEEMED
// one means the company has started, and an AGED-OUT or WITHDRAWN one — or one
// the log never received — is a file its holder can replace with one command.
// Folded into one bool, the last three were answered as the third, and a
// founder holding a code a day old was told their company had started.
type CodeState string

const (
	// CodeAbsent is a code this node's log holds no row for: never minted,
	// minted by a record that never landed, or swept a week past the
	// moment it stopped working.
	CodeAbsent CodeState = "absent"

	// CodeLive is minted, untaken, not withdrawn and inside its lifetime:
	// a founding may take it.
	CodeLive CodeState = "live"

	// CodeTaken holds the first-person exemption: a founding took it, its
	// enrolment has not landed, nothing released it, and it is inside its
	// lifetime. Whoever holds the code finishes that enrolment with it,
	// and no other code may be taken until it lapses or is released.
	CodeTaken CodeState = "taken"

	// CodeAgedOut is past its expiry with nothing having finished with it
	// — never taken, or taken by an attempt that stopped. A take lapses
	// with its code: the lifetime bounds finishing as well as starting,
	// or a taken code would be a superuser claim in a file for ever.
	CodeAgedOut CodeState = "aged_out"

	// CodeWithdrawn stopped working before it created anybody: a re-issue
	// withdrew it, or the attempt that took it was released by a later
	// one.
	CodeWithdrawn CodeState = "withdrawn"

	// CodeRedeemed created the company's first person.
	CodeRedeemed CodeState = "redeemed"
)

// CodeStates is every state, for validation and for a test that walks them.
var CodeStates = []CodeState{
	CodeAbsent, CodeLive, CodeTaken, CodeAgedOut, CodeWithdrawn, CodeRedeemed,
}

// Valid reports whether a state is one this build knows.
func (s CodeState) Valid() bool { return slices.Contains(CodeStates, s) }

// State is what this code is at now.
//
// THE ONE PREDICATE for whether a code works, read by every party that asks:
// the redeeming route on any ingress node, the boot path deciding whether its
// file is worth keeping, the re-issue deciding what to withdraw, and the
// founding's own decides ([Writer.takeExemption], [Writer.bootstrappable]).
// Written once per caller, the copies had already disagreed about a code with
// no expiry — the decide read it as eternal and the listing as dead.
//
// A FOUNDER WHO EXISTS OUTRANKS EVERYTHING ELSE, because an enrolment is a
// record and an expiry is a reading of a clock: the code that created the
// company's first person stays redeemed once its hours run out. And a
// released founder outranks the take, because a released attempt's every
// later record is dropped — its code can create nobody.
//
// NO EXPIRY IS AGED OUT, the fail-closed reading. Every mint states one
// ([Writer.MintBootstrap] refuses a mint that does not), so a row without it
// is a withdrawal of a code this node never saw minted — and a code that never
// aged would be a superuser claim in a file for ever.
func (c BootstrapCode) State(now time.Time) CodeState {
	expired := c.ExpiresAt.IsZero() || !now.Before(c.ExpiresAt)
	switch {
	case c.ID == "":
		return CodeAbsent
	case !c.SpentAt.IsZero() && c.Person == "":
		return CodeWithdrawn
	case c.FounderEnrolled:
		return CodeRedeemed
	case c.FounderReleased:
		return CodeWithdrawn
	case expired:
		return CodeAgedOut
	case !c.SpentAt.IsZero():
		return CodeTaken
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
// what lets the founding's decides read it in their own snapshots and the
// reader outside one through the same scan.
func bootstrapCodeIn(ctx context.Context, tx *sql.Tx, id string) (BootstrapCode, error) {
	row, err := scanCode(tx.QueryRowContext(ctx, `
		SELECT id, minted_by, expires_at, created_at, redeemed_at, person_id
		  FROM iam_bootstrap_codes WHERE id = ?`, id))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return BootstrapCode{}, nil
	case err != nil:
		return BootstrapCode{}, fmt.Errorf("iamdomain: read a bootstrap code: %w", err)
	}
	return row, founderStanding(ctx, tx, &row)
}

// codesIn is every code the log holds, each with its founder's standing, read
// inside a transaction already open.
//
// EVERY ROW AND NOT A NARROWED QUERY, because the question the callers ask —
// is another code taken, is anything outstanding — is answered by
// [BootstrapCode.State] and by nothing a WHERE clause could restate without
// becoming a second copy of it. The table holds a handful of rows: one per
// boot that found an empty estate, per re-issue and per founding, swept a week
// after each stops working.
func codesIn(ctx context.Context, tx *sql.Tx) ([]BootstrapCode, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, minted_by, expires_at, created_at, redeemed_at, person_id
		  FROM iam_bootstrap_codes ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: read the bootstrap codes: %w", err)
	}
	out, err := scanCodes(rows)
	if err != nil {
		return nil, err
	}
	// THE STANDING AFTER THE SCAN, never inside it: a second statement on
	// the transaction while the first's rows are still open is one the
	// driver may serialise behind them.
	for i := range out {
		if err := founderStanding(ctx, tx, &out[i]); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// scanCodes drains a query over the codes table and closes it.
func scanCodes(rows *sql.Rows) ([]BootstrapCode, error) {
	defer rows.Close()
	var out []BootstrapCode
	for rows.Next() {
		row, err := scanCode(rows)
		if err != nil {
			return nil, fmt.Errorf("iamdomain: scan a bootstrap code: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iamdomain: read the bootstrap codes: %w", err)
	}
	return out, nil
}

// scanCode fills one code's row half from a scanner positioned on it.
func scanCode(row interface{ Scan(...any) error }) (BootstrapCode, error) {

	var (
		out                          BootstrapCode
		expires, created, redeemedAt int64
	)
	if err := row.Scan(&out.ID, &out.MintedBy, &expires, &created,
		&redeemedAt, &out.Person); err != nil {
		return BootstrapCode{}, err
	}
	out.ExpiresAt = fromMillis(expires)
	out.MintedAt = fromMillis(created)
	out.SpentAt = fromMillis(redeemedAt)
	return out, nil
}

// founderStanding fills in whether the person a code creates is enrolled or
// released, in the snapshot the code's own row was read in.
func founderStanding(ctx context.Context, tx *sql.Tx, code *BootstrapCode) error {
	founder := code.FounderID()
	if err := tx.QueryRowContext(ctx, `
		SELECT
			EXISTS(SELECT 1 FROM iam_people  WHERE id = ? AND kind <> ''),
			EXISTS(SELECT 1 FROM iam_removed WHERE person_id = ?)`,
		founder, founder).Scan(&code.FounderEnrolled,
		&code.FounderReleased); err != nil {
		return fmt.Errorf("iamdomain: read the standing of founder %s: %w",
			founder, err)
	}
	return nil
}

// takeExemption is the first step of a founding: the first-person exemption
// taken on the company's one bootstrap subject, and every earlier attempt
// released.
//
// # The take
//
// One record on [BootstrapSubject] — the existing spend, naming the code and
// the person it creates — published only from a snapshot in which nobody is
// enrolled, the code is live or already this attempt's, and no OTHER code is
// taken. Every take is on the one subject, so two attempts contend at the
// broker: the loser re-decides, finds the winner's take, and is refused with
// [ErrBootstrapInProgress] having claimed nothing. A retry of this attempt —
// its take already landed — publishes nothing and carries on.
//
// # BEFORE THE KEY, and why
//
// A take can be refused — a dead code, a founding in progress, a company that
// started — and it is the first thing that can be. Enrol mints the person's
// key after it, so a refusal here leaves no key behind for somebody who never
// existed.
//
// # The release of every earlier attempt
//
// Read in the take's own snapshot, which is complete for the one question it
// is asked: every earlier attempt TOOK before this one did, and every take is
// on the subject this snapshot's expectation covers. So the persons of the
// codes taken before, and every founder-shaped reservation, are released
// before this attempt claims an address — see this file's header for why each
// is needed and why a release rather than anything softer.
func (w *Writer) takeExemption(ctx context.Context, at *statelog.Position,
	in Enrolment) (statelog.Result, error) {

	mutation, err := EncodeBootstrapDoc(Bootstrap{
		V: DocumentVersion, ID: in.BootstrapCode, Person: in.PersonID,
	})
	if err != nil {
		return statelog.Result{}, err
	}
	// THE BOOTSTRAP'S OWN BUCKET AND THE PERSON'S: the row it writes lives
	// in the first and names somebody who lives in the second, and a scope
	// that stated one would leave the other's readers certifying a copy
	// this record changed.
	rec, err := w.record(BootstrapSubject(), OpBootstrap, in.PersonID,
		BucketScope(BootstrapBucket(), BucketOf(in.PersonID)), mutation,
		"the first person's enrolment began")
	if err != nil {
		return statelog.Result{}, err
	}
	// THE LAST RUN'S LIST, which is the one the landed record was decided
	// with: a decide may run again against a fresh snapshot.
	var earlier []string
	decide := func(tx *sql.Tx) error {
		var err error
		earlier, err = w.takeable(ctx, tx, in.BootstrapCode, in.PersonID)
		return err
	}
	taken, err := w.publishAt(ctx, at, w.request(&rec, in.OpID+":take",
		statelog.PatternArbitrated, decide))
	if err != nil || taken.Outcome == statelog.OutcomeUnknown {
		return taken, err
	}
	for _, person := range earlier {
		released, err := w.releaseAttempt(ctx, at, person,
			in.OpID+":release:"+person)
		if err != nil {
			return released, err
		}
		if released.Outcome == statelog.OutcomeUnknown {
			// NOTHING IS CLAIMED BESIDE A RELEASE NOBODY CAN CONFIRM: the
			// attempt it would end may yet finish, and this one's claims
			// would then be built on a company that has started.
			return unresolved(in.OpID), nil
		}
	}
	return taken, nil
}

// takeable decides a take in its own snapshot, answering the earlier attempts
// to release — or [errNothingToPublish] beside them when this attempt's take
// already landed, and a refusal naming which of the three ways the exemption
// is not this attempt's to take.
func (w *Writer) takeable(ctx context.Context, tx *sql.Tx, codeID,
	person string) ([]string, error) {

	if err := notStarted(ctx, tx, ""); err != nil {
		return nil, err
	}
	codes, err := codesIn(ctx, tx)
	if err != nil {
		return nil, err
	}
	now := w.Now()
	var (
		mine    BootstrapCode
		earlier []string
		seen    = map[string]bool{person: true}
	)
	for _, code := range codes {
		if code.ID == codeID {
			mine = code
			continue
		}
		state := code.State(now)
		switch state {
		case CodeTaken:
			return nil, fmt.Errorf("%w: %w", ErrRefused,
				&FoundingInProgress{Until: code.ExpiresAt})
		case CodeRedeemed:
			return nil, fmt.Errorf("%w: %w: another one-time code already "+
				"created the company's first person", ErrRefused,
				ErrBootstrapClosed)
		}
		// AN EARLIER ATTEMPT is a code somebody took whose founder is
		// neither enrolled (refused above) nor released: its take lapsed,
		// and its request may still be publishing.
		if code.Person != "" && !code.FounderReleased && !seen[code.Person] {
			seen[code.Person] = true
			earlier = append(earlier, code.Person)
		}
	}
	reservations, err := founderReservations(ctx, tx)
	if err != nil {
		return nil, err
	}
	for _, id := range reservations {
		if !seen[id] {
			seen[id] = true
			earlier = append(earlier, id)
		}
	}

	switch state := mine.State(now); {
	case mine.ID != "" && mine.FounderID() != person:
		return nil, fmt.Errorf("%w: the one-time code creates person %s and "+
			"this founding names %s — the founder is derived from the code, "+
			"never chosen", ErrRefused, mine.FounderID(), person)
	case state == CodeLive:
		return earlier, nil
	case state == CodeTaken:
		// THIS ATTEMPT'S OWN TAKE, from a request that stopped after it:
		// nothing to publish, and the releases and the claims carry on
		// from where it stopped.
		return earlier, errNothingToPublish
	case state == CodeRedeemed:
		return nil, fmt.Errorf("%w: %w: the one-time code already created "+
			"the company's first person", ErrRefused, ErrBootstrapClosed)
	default:
		return nil, fmt.Errorf("%w: %w: the one-time code is %s — a code file "+
			"is worth only what the log says about it, and a fresh one is "+
			"`crewlet iam bootstrap-code` away", ErrRefused,
			ErrBootstrapCodeDead, state)
	}
}

// founderReservations are the RESERVATIONS whose id has the founder shape:
// what earlier founding attempts left behind, however long ago.
//
// THE SHAPE AND NOT A CODE ROW, because a code row is swept a week after it
// stops working and a reservation is not swept at all: an attempt that
// stopped after claiming the founder's address, found only through its code,
// would hold that address for ever once the week was up.
func founderReservations(ctx context.Context, tx *sql.Tx) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM iam_people WHERE kind = '' ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("iamdomain: read the reservations: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("iamdomain: scan a reservation: %w", err)
		}
		if FounderAttempt(id) {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// releaseAttempt ends one founding attempt that is not the current one: its
// person is REMOVED, which releases every claim its reservation holds and
// drops every record it had still to publish.
//
// # A removal, and never anything softer
//
// The attempt may still be running — a take lapses on a clock, not when the
// request that made it stops — so what ends it has to CONTEND with the one
// record that would make it the company's founder: its person record, on its
// person's subject. A removal is on that subject. Exactly one of the two
// lands, and each outcome is a correct one: the person record first, and this
// decide finds an enrolled person and refuses with [ErrBootstrapClosed],
// because the company has started; the removal first, and its gate drops the
// person record and every claim still in flight, so nothing the attempt does
// later can resurrect the address it held. Releasing the claims alone would
// leave a person record free to land on a reservation holding none of them.
//
// A PERSON WITH NO ROW IS STILL REMOVED — an attempt whose take landed and
// nothing else — because the gate is what stops its claims arriving later.
// One already removed publishes nothing.
func (w *Writer) releaseAttempt(ctx context.Context, at *statelog.Position,
	person, opID string) (statelog.Result, error) {

	if err := w.mayAdminister(OpRemove); err != nil {
		return statelog.Result{}, err
	}
	rec, err := w.record(PersonSubject(person), OpRemove, person,
		PeopleScope(person), nil, "an unfinished founding, ended by a later one")
	if err != nil {
		return statelog.Result{}, err
	}
	decide := func(tx *sql.Tx) error {
		var removed bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM iam_removed WHERE person_id = ?)`,
			person).Scan(&removed); err != nil {
			return fmt.Errorf("iamdomain: read whether person %s was "+
				"removed: %w", person, err)
		}
		if removed {
			return errNothingToPublish
		}
		var (
			kind   string
			claims Claims
		)
		err := tx.QueryRowContext(ctx, `
			SELECT kind, email_blind, login, seat_id FROM iam_people WHERE id = ?`,
			person).Scan(&kind, &claims.EmailBlind, &claims.Login, &claims.SeatID)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			claims = Claims{}
		case err != nil:
			return fmt.Errorf("iamdomain: read person %s's claims: %w", person, err)
		case !reservation(kind):
			return fmt.Errorf("%w: %w: an earlier founding finished — person "+
				"%s is enrolled, so this company has started", ErrRefused,
				ErrBootstrapClosed, person)
		}
		rec.Mutation, err = EncodeRemoval(Removal{
			V: GateRecordVersion, Released: claims,
		})
		return err
	}
	return w.publishAt(ctx, at,
		w.request(&rec, opID, statelog.PatternArbitrated, decide))
}

// bootstrappable refuses the first person's enrolment unless THIS attempt
// holds the exemption, read inside the person record's own snapshot — the one
// the grants land from.
//
// THE CODE MUST BE TAKEN BY THIS FOUNDER AND CURRENT — or already redeemed by
// them, which is a retry of an enrolment that landed. A take is what made this
// attempt the only one; a take that lapsed or was released is one a later
// attempt may have superseded, and an enrolment landing on it would be the
// second founder the take exists to rule out. The release that ends such an
// attempt contends with this very record, so the check here and that release
// cannot both pass.
//
// AND NOBODY ELSE MAY BE ENROLLED, by [notStarted], because the exemption
// exists only for the instant there is nobody whose grants could bound it —
// the same predicate the route, the boot path and every other decide asks.
//
// THE GRANTS ARE NOT BOUNDED BY A WRITER, which is the exemption itself. The
// one bound they do meet — every grant is one this build can name — is a
// property of the values rather than of any snapshot, so it is
// [Enrolment.validate]'s, checked before the take like every other bound.
func (w *Writer) bootstrappable(ctx context.Context, tx *sql.Tx, in Enrolment) error {
	code, err := bootstrapCodeIn(ctx, tx, in.BootstrapCode)
	if err != nil {
		return err
	}
	if code.ID != "" && code.FounderID() != in.PersonID {
		return fmt.Errorf("%w: the one-time code creates person %s and this "+
			"enrolment names %s — the founder is derived from the code, never "+
			"chosen", ErrRefused, code.FounderID(), in.PersonID)
	}
	switch state := code.State(w.Now()); state {
	case CodeTaken, CodeRedeemed:
	case CodeLive:
		// UNREACHABLE THROUGH [Writer.Enrol], which takes the code before
		// anything else and waits for its own take here: a live code at
		// the person record is an enrolment that skipped the take, and
		// the take is the whole of what makes this attempt the only one.
		return fmt.Errorf("%w: %w: the one-time code was never taken — a "+
			"founding takes the first-person exemption before it claims "+
			"anything", ErrRefused, ErrBootstrapCodeDead)
	default:
		return fmt.Errorf("%w: %w: the one-time code is %s — a code file is "+
			"worth only what the log says about it, and a fresh one is "+
			"`crewlet iam bootstrap-code` away", ErrRefused,
			ErrBootstrapCodeDead, state)
	}
	return notStarted(ctx, tx, in.PersonID)
}

// notStarted refuses with [ErrBootstrapClosed] once somebody other than
// `besides` is enrolled, and passes on an estate with nobody in it — the
// founding's own reading of [anybodyEnrolled].
func notStarted(ctx context.Context, tx *sql.Tx, besides string) error {
	somebody, err := anybodyEnrolled(ctx, tx, besides)
	if err != nil {
		return err
	}
	if somebody {
		return fmt.Errorf("%w: %w: this company already has somebody in it, "+
			"and an administrator enrols or invites everybody after them",
			ErrRefused, ErrBootstrapClosed)
	}
	return nil
}

// MintBootstrap issues one node's way into a company that has nobody in it.
//
// # Every node's mint lands, and that is deliberate
//
// It is published on [BootstrapSubject], the company's one bootstrap object,
// so its life sits beside every other code's in order — but its decide admits
// any number of live codes, so two nodes minting at once both land: a fleet
// booting on an empty estate offers one code per node, and a founder may be
// typing any of them. What the one subject makes exclusive is TAKING a code
// ([Writer.takeExemption]), and what leaves exactly one live is a re-issue
// ([Writer.ReissueBootstrap]), never a boot.
//
// # No grant, and what stands in for one
//
// Nobody holds a credential yet, so a capability check here would be asking
// somebody to prove authority the code exists to confer. What bounds it is
// the ESTATE, read in this record's own snapshot: a mint against a company
// that has started is refused with [ErrBootstrapClosed], on [anybodyEnrolled]
// — the predicate the first person's own enrolment is refused on. The callers
// ask first, so a refused mint writes no file; this is the answer that counts.
//
// # A code always ages out
//
// A mint that states no expiry is refused, because a code with none would be
// a superuser claim sitting in a file for ever — and [BootstrapCode.State]
// reads a row without one as aged out, so it would be a code nothing honours
// either.
func (w *Writer) MintBootstrap(ctx context.Context, in BootstrapMint) (
	statelog.Result, error) {

	rec, err := w.mintRecord(in)
	if err != nil {
		return statelog.Result{}, err
	}
	started := func(tx *sql.Tx) error { return notStarted(ctx, tx, "") }
	return w.publish(ctx,
		w.request(&rec, in.OpID, statelog.PatternArbitrated, started))
}

// ReissueBootstrap withdraws every outstanding code, releases an unfinished
// founding, and mints one — so that at the position its mint lands at,
// exactly one code is live and no enrolment is in progress.
//
// # Why the mint decides it, and not the withdrawals
//
// Withdrawals read from one snapshot and published one by one leave the
// question open between them: a node that boots, or a second operator who
// re-issues, lands a code nothing withdrew, and the re-issue's "exactly one"
// becomes two. So the MINT is what is guarded. Its decide reads every code in
// its own snapshot and publishes only where nothing but itself is live or
// taken; anything else comes back as the next round's work — each live code
// withdrawn, a taken code's attempt released — and the mint is tried again.
// Every one of those records is on the one bootstrap subject or waited for
// before the mint's snapshot, so the snapshot the mint lands from is the whole
// truth at its position. Two re-issues at once each withdraw the other's code
// in turn, and the one whose mint lands last is the one that is live.
//
// # An administrator's gesture
//
// Unlike a boot's mint it takes [AdminGrant]: it ends codes somebody may be
// holding and an enrolment somebody may be finishing, which is authority over
// who becomes the company's first person. Refused with [ErrBootstrapClosed]
// once anybody is enrolled, by the predicate every other founding decide asks.
//
// THE WITHDRAWALS GO FIRST. A process that stops between them and the mint
// leaves a company with no live code, which running it again fixes; the other
// order would leave two.
func (w *Writer) ReissueBootstrap(ctx context.Context, in BootstrapMint) (
	statelog.Result, error) {

	if err := w.mayAdminister(OpBootstrap); err != nil {
		return statelog.Result{}, err
	}
	rec, err := w.mintRecord(in)
	if err != nil {
		return statelog.Result{}, err
	}
	// ONE MARK FOR THE WHOLE GESTURE, so each round's mint decides from a
	// snapshot holding the withdrawals and releases before it.
	at := w.gesture()
	// THE LAST RUN'S LIST, with the state each code was decided in: a code
	// that ages out between the decide and its withdrawal needs none.
	var outstanding []stated
	decide := func(tx *sql.Tx) error {
		outstanding = outstanding[:0]
		if err := notStarted(ctx, tx, ""); err != nil {
			return err
		}
		codes, err := codesIn(ctx, tx)
		if err != nil {
			return err
		}
		for _, code := range codes {
			if code.ID == in.ID {
				continue
			}
			if state := code.State(w.Now()); state == CodeLive ||
				state == CodeTaken {
				outstanding = append(outstanding, stated{code, state})
			}
		}
		if len(outstanding) > 0 {
			return errOutstanding
		}
		return nil
	}
	for round := 1; ; round++ {
		minted, err := w.publishAt(ctx, at,
			w.request(&rec, in.OpID, statelog.PatternArbitrated, decide))
		if !errors.Is(err, errOutstanding) {
			return minted, err
		}
		if round == reissueRounds {
			return statelog.Result{}, fmt.Errorf("%w: a re-issue cleared "+
				"%d rounds of outstanding codes and more kept landing — "+
				"something is minting or taking codes in a loop",
				statelog.ErrConflict, reissueRounds)
		}
		for _, code := range outstanding {
			var (
				step statelog.Result
				err  error
			)
			switch code.state {
			case CodeLive:
				step, err = w.withdraw(ctx, at, code.ID,
					in.OpID+":withdraw:"+code.ID)
			case CodeTaken:
				step, err = w.releaseAttempt(ctx, at, code.FounderID(),
					in.OpID+":release:"+code.FounderID())
			}
			if err != nil {
				return step, err
			}
			if step.Outcome == statelog.OutcomeUnknown {
				return unresolved(in.OpID), nil
			}
		}
	}
}

// reissueRounds bounds how many times a re-issue clears what is outstanding
// before it says so.
//
// SIXTEEN, the framework's own bound on consecutive lost races on one subject
// (statelog's casRounds), for the framework's reason: each round here is a
// race lost to a code minted or taken on the bootstrap subject while this one
// cleared the last round's, and sixteen in a row is something minting in a
// loop rather than a fleet that happened to boot during the command — which is
// the thing to report rather than to chase.
const reissueRounds = 16

// stated is one outstanding code with the state its round decided it in.
type stated struct {
	BootstrapCode
	state CodeState
}

// errOutstanding is what a re-issue's mint answers from a snapshot holding a
// code that still works, which [Writer.ReissueBootstrap] turns into the next
// round's withdrawals rather than an error.
var errOutstanding = errors.New("iamdomain: a code other than the one being " +
	"re-issued is still outstanding")

// withdraw supersedes one code nobody took, as a re-issue's first half.
//
// GUARDED IN ITS OWN SNAPSHOT: only a code still LIVE is withdrawn. A
// withdrawal is a spend that names nobody, and landed on a code a founding
// took a moment ago it would erase the person that take named — the one fact
// that lets the next founding find the attempt and end it. The record is on
// the one bootstrap subject, so a take landing first makes its expectation
// stale and the re-decide sees it.
//
// IT IS NOT A REDEMPTION WITH NO PERSON. The two leave the same row state and
// are opposite events; see [Bootstrap.Withdrawn].
func (w *Writer) withdraw(ctx context.Context, at *statelog.Position, id,
	opID string) (statelog.Result, error) {

	mutation, err := EncodeBootstrapDoc(Bootstrap{
		V: DocumentVersion, ID: id, Withdrawn: true,
	})
	if err != nil {
		return statelog.Result{}, err
	}
	rec, err := w.record(BootstrapSubject(), OpBootstrap, "",
		BucketScope(BootstrapBucket()), mutation,
		"superseded by a re-issued code")
	if err != nil {
		return statelog.Result{}, err
	}
	decide := func(tx *sql.Tx) error {
		code, err := bootstrapCodeIn(ctx, tx, id)
		if err != nil {
			return err
		}
		if code.State(w.Now()) != CodeLive {
			return errNothingToPublish
		}
		return nil
	}
	return w.publishAt(ctx, at,
		w.request(&rec, opID, statelog.PatternArbitrated, decide))
}

// mintRecord forms a mint's record, refusing one that could not land.
func (w *Writer) mintRecord(in BootstrapMint) (MutationRecord, error) {
	if in.ID == "" || in.Verifier == "" || in.OpID == "" {
		return MutationRecord{}, errors.New("iamdomain: minting a bootstrap " +
			"needs an id, a verifier and an operation id")
	}
	if in.ExpiresAt.IsZero() {
		return MutationRecord{}, fmt.Errorf("%w: a one-time code needs an "+
			"expiry — one without would be a way to become the first "+
			"administrator for as long as its file survived", ErrInvalid)
	}
	mutation, err := EncodeBootstrapDoc(Bootstrap{
		V: DocumentVersion, ID: in.ID, Verifier: in.Verifier,
		MintedBy: in.MintedBy, ExpiresAt: in.ExpiresAt,
	})
	if err != nil {
		return MutationRecord{}, err
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
	return w.record(BootstrapSubject(), OpBootstrap, "",
		BucketScope(BootstrapBucket()), mutation, in.Reason)
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
