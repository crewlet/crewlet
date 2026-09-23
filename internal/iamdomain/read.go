package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE READ SIDE OF THE IDENTITY ESTATE, and what is particular about it.
//
// # Every answer is three-valued, and the third value is the point
//
// A read here decides whether somebody may act. Its three answers are the
// engine's own three: this person is X, this person is definitively not X, and
// this node could not tell — and the third is not a failure to be logged and
// folded into the second. Folded, every credential in the company reads as
// invalid for the length of an outage, which is how a company gets taught to
// reset working passwords during one.
//
// So nothing here returns `(value, bool)`. An error is the unknown arm,
// always, and a caller that cannot distinguish it has already lost the
// distinction.
//
// # The session read is ONE read, deliberately
//
// [Directory.Resolve] answers six facts in one snapshot, because a revocation
// landing between a session read and a person read produces a verdict that
// never existed at any instant. That is not a performance argument; it is the
// only shape in which "the replicated estate is not open" is one answer rather
// than three.
//
// # Nothing here decrypts
//
// A person's name and address are SEALED under their own key, and opening one
// means a fleet-secret read. Every read below returns the sealed bytes and
// leaves opening them to a surface that has a reason to — which is also what
// keeps an identity read a keyed lookup rather than a round trip, and why
// internal/store's reserved connection is enough for it.

var log = logging.Get("iam.read")

// Reader answers questions about this node's copy of the identity estate.
type Reader struct {
	db  *store.DB
	log *statelog.Reader

	committed  func() statelog.Position
	lag        func() time.Duration
	deferred   func() (statelog.Deferral, bool)
	generation func() uint64
}

// ReaderOptions is what a reader is built from.
type ReaderOptions struct {
	// DB is the replicated estate. REQUIRED.
	DB *store.DB

	// Log is this domain's read authority. REQUIRED, for the reason
	// internal/chart's reader states: without it every read level is a
	// label rather than a guarantee, and a degradation invisible in the
	// answer is worse than a refusal.
	Log *statelog.Reader

	// Committed is this node's applied position, Lag how far behind the
	// log it is, and Deferred whether it holds a record it could not
	// decode. Each is nil-safe and answers the zero value, which is what a
	// reader with no runner behind it honestly has.
	Committed func() statelog.Position
	Lag       func() time.Duration
	Deferred  func() (statelog.Deferral, bool)
}

// NewReader builds the identity estate's read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.DB == nil {
		return nil, errors.New("iamdomain: a reader needs the replicated estate")
	}
	if opts.Log == nil {
		return nil, errors.New("iamdomain: a reader needs its domain's read " +
			"authority — without it every read level is a label rather than a " +
			"guarantee, and an identity answer that silently degraded is one " +
			"nobody can tell from a correct refusal")
	}
	r := &Reader{db: opts.DB, log: opts.Log,
		committed: opts.Committed, lag: opts.Lag, deferred: opts.Deferred}
	if r.committed == nil {
		r.committed = func() statelog.Position { return statelog.Position{} }
	}
	if r.lag == nil {
		r.lag = func() time.Duration { return 0 }
	}
	if r.deferred == nil {
		r.deferred = func() (statelog.Deferral, bool) { return statelog.Deferral{}, false }
	}
	return r, nil
}

// At is the position this node's rows were derived through.
func (r *Reader) At() statelog.Position { return r.committed() }

// ErrNotFound is a lookup this node could answer and that names nobody.
//
// A SENTINEL AND NOT A BOOL, because the caller's third answer is an error and
// a bool beside one is two ways to say the same thing that eventually
// disagree. What it deliberately is NOT is what a sign-in reports: see
// [Reader.PersonByLogin].
var ErrNotFound = errors.New("iamdomain: no such person")

// Resolve answers everything one bearer is checked against, in ONE snapshot.
//
// It satisfies [session.Directory], which is consumer-defined there and kept
// to what validation needs. The six facts travel together because reading them
// separately produces a verdict that never existed at any instant.
//
// AN ERROR IS ALWAYS THE UNKNOWN ARM. A replicated estate that is not open, a
// store that cannot be read, a row that will not decode — every one is 503,
// and none of them may be reported as "this session does not exist".
func (r *Reader) Resolve(ctx context.Context, lineage, person string) (
	session.Identity, error) {

	out := session.Identity{
		Applied: uint64(r.committed().Packed()),
	}
	// THE DEFERRAL IS READ FIRST and from the RUNNER rather than from a
	// row, because it is the framework's own coverage answer: this node
	// holds a record it could not decode whose scope covers this person's
	// bucket. The read that follows SUCCEEDS and its rows are simply not
	// known to be complete, which is why this is a field rather than an
	// error.
	out.Lag, out.Deferred = r.Staleness(person)

	err := r.withTx(ctx, func(tx *sql.Tx) error {
		if err := readSessionRow(ctx, tx, lineage, &out.Session); err != nil {
			return err
		}
		if err := readPersonRow(ctx, tx, person, &out.Person); err != nil {
			return err
		}
		if err := readEpoch(ctx, tx, person, &out.Person.Epoch); err != nil {
			return err
		}
		return readGeneration(ctx, tx, &out.Generation)
	})
	if err != nil {
		return session.Identity{}, err
	}
	return out, nil
}

// Staleness is how far this node can vouch for one person's rows: how far its
// applier is behind the log, and whether it holds a record it could not decode
// whose scope covers that person's bucket.
//
// FACTS AND NOT A VERDICT. What a lag or a deferral MEANS is the caller's
// table — internal/iam/session's for a session, the engine's for a Tier A
// token's binding — and a threshold chosen here would be a second opinion
// about the one event [statelog.StallGrace] already names. It is one method
// rather than two so that a caller cannot ask about the lag and forget the
// deferral, which reads as a caught-up node while it holds a newer peer's
// record about exactly this person.
//
// An EMPTY person is covered by every deferral, because nothing can say which
// bucket a record it could not decode is about.
func (r *Reader) Staleness(person string) (lag time.Duration, deferred bool) {
	if held, ok := r.deferred(); ok {
		deferred = coversPerson(held, person)
	}
	return r.lag(), deferred
}

// coversPerson reports whether a deferred record's scope covers this person's
// bucket.
//
// THE BUCKET AND NOT THE PERSON, because that is all a deferral can say: its
// scope is a bucket path, and the person a record is about is inside a payload
// the deferring node could not decode. See scope.go for why the scope is flat
// and bucketed rather than per person.
func coversPerson(d statelog.Deferral, person string) bool {
	if person == "" {
		return true
	}
	want := BucketOf(person).Path()
	for _, path := range d.Scope.Paths {
		if path == want || path == RootPath() {
			return true
		}
	}
	return false
}

// readSessionRow fills one session's facts.
func readSessionRow(ctx context.Context, tx *sql.Tx, lineage string,
	out *session.SessionRow) error {

	if lineage == "" {
		return nil
	}
	var endedAt, epoch int64
	err := tx.QueryRowContext(ctx, `
		SELECT epoch, ended_at FROM iam_sessions WHERE lineage = ?`,
		lineage).Scan(&epoch, &endedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// ABSENT IS A FINDING, not a failure: it is what the bearer's own
		// start position turns into the two answers it actually is — a
		// session that ended and was swept, or one this node has not
		// applied yet. session.Row is where that is decided.
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read the session row: %w", err)
	}
	out.Found = true
	out.Ended = endedAt != 0
	out.Epoch = uint64(epoch)
	return nil
}

// readPersonRow fills one person's facts, opening the document for the two
// that live in it.
func readPersonRow(ctx context.Context, tx *sql.Tx, person string,
	out *session.PersonRow) error {

	if person == "" {
		return nil
	}
	var (
		stage, login, seat string
		chartPosition      int64
		document           []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT stage, login, seat_id, chart_position, document
		  FROM iam_people WHERE id = ?`, person).
		Scan(&stage, &login, &seat, &chartPosition, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read the person row: %w", err)
	}
	doc, err := DecodePerson(document)
	if err != nil {
		// THE UNKNOWN ARM, and deliberately not "no such person". A row
		// written by a newer peer that this build cannot open is a
		// person who exists and whose facts this node cannot state —
		// answering 401 to them would sign out everybody enrolled since
		// the upgrade started.
		return fmt.Errorf("iamdomain: open person %q: %w", person, err)
	}
	out.Found = true
	// THE COLUMN AND NOT THE DOCUMENT. The column is what every predicate
	// in this estate reads, and the status op is the one write that moves
	// a stage without authoring a whole document — so a reader deciding
	// from the document answered a different question from the rows beside
	// it, and a suspended person's session validated as `active`.
	out.Stage = iam.Stage(stage)
	out.Login = login
	out.Colleague = doc.Colleague
	out.Grants = doc.Grants
	out.Seat = seat
	out.SeatAt = uint64(chartPosition)
	return nil
}

// readEpoch fills a session subject's revocation epoch.
//
// THE REVOCATION EPOCH IS ITS OWN ROW, and it is read in the same transaction
// as the person for the reason [Reader.Resolve] exists: a revocation landing
// between the person read and the epoch read produces a verdict that never
// existed.
//
// READ WHETHER OR NOT A PERSON ROW EXISTS, because not every session's subject
// has one: a session exchanged from a Tier A token names the token's login,
// which nothing enrols, and "sign out everywhere" from it bumps the epoch under
// that login. Read only beside a person row, that revocation ended nothing.
func readEpoch(ctx context.Context, tx *sql.Tx, subject string, out *uint64) error {
	if subject == "" {
		return nil
	}
	var epoch int64
	err := tx.QueryRowContext(ctx, `
		SELECT epoch FROM iam_revocation_epochs WHERE person_id = ?`,
		subject).Scan(&epoch)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// NO ROW IS EPOCH ZERO, which is a real value rather than a
		// missing one: nobody has revoked anything for this subject, and
		// every bearer it holds carries zero too.
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read the revocation epoch: %w", err)
	}
	*out = uint64(epoch)
	return nil
}

// readGeneration fills the fleet-wide session generation.
func readGeneration(ctx context.Context, tx *sql.Tx, out *uint64) error {
	var generation int64
	err := tx.QueryRowContext(ctx,
		`SELECT generation FROM iam_session_generation WHERE singleton = 0`).
		Scan(&generation)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// A FLEET THAT HAS NEVER INVALIDATED is generation zero, and
		// every bearer carries zero, so this is the ordinary state of a
		// company rather than a missing row.
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read the session generation: %w", err)
	}
	*out = uint64(generation)
	return nil
}

// Sighting is one person as a sign-in resolves them: enough to verify a
// credential against and nothing more.
//
// DELIBERATELY NOT A PERSON DOCUMENT. A sign-in runs before anybody is
// authenticated, so what it may read is bounded by what a refusal is allowed
// to disclose — which is nothing. What is here is what the verify step needs.
type Sighting struct {
	ID          string
	Kind        iam.Kind
	Stage       iam.Stage
	Login       string
	Credentials []Credential
	Grants      []iam.Grant
	Colleague   iam.Colleague
	Seat        string
	SeatAt      uint64
}

// PersonByLogin resolves a login to the person who holds it.
//
// # Three answers, and the middle one is not an error
//
// A login nobody holds answers the zero [Sighting] and a nil error, NOT
// [ErrNotFound]. That is the one place in this file the sentinel is
// deliberately withheld, and it is the enumeration rule: a caller that could
// tell "no such login" from "wrong password" has a roster, and the two arms
// have to be indistinguishable in what they return, how long they take and
// what they log. internal/iam/credential is where the timing half lives; this
// is the shape half, and the caller runs a fixed-cost decoy against the zero
// value rather than branching on it.
//
// An error is still the unknown arm, as everywhere here.
func (r *Reader) PersonByLogin(ctx context.Context, login string) (Sighting, error) {
	return r.sighting(ctx, "login", login)
}

// PersonByEmailBlind resolves a keyed address blind to its holder, on the same
// three answers and for the same reason.
//
// THE BLIND AND NOT THE ADDRESS, because this read runs before anybody is
// authenticated and an address is personal data. iam.NormalizeEmail and this
// domain's blind are what a surface computes it with — and they must agree
// with the chart's own derivation or one address would reach a seat and a
// different person.
func (r *Reader) PersonByEmailBlind(ctx context.Context, blind string) (Sighting, error) {
	return r.sighting(ctx, "email_blind", blind)
}

// ErrSubjectAmbiguous is an identity provider's subject that more than one
// person holds a live link to.
//
// NEVER RESOLVED TO EITHER OF THEM, because the subject is the whole of what a
// provider sign-in proves: a password sign-in against a duplicated login still
// has to verify THAT person's digest, while a subject resolved to the wrong
// holder is somebody signed in as somebody else with nothing further checked.
// Nothing the broker arbitrates produces one; a restore can, and the sign-in
// stays refused until an operator removes one of the links.
var ErrSubjectAmbiguous = errors.New("iamdomain: more than one person holds " +
	"a live link to this identity provider subject")

// PersonBySubjectBlind resolves an identity provider's blinded subject to the
// person holding a LIVE link to it, on [Reader.PersonByLogin]'s three answers.
//
// THE CREDENTIAL AND NOT THE PERSON ROW, because a subject belongs to a link
// rather than to a person: the person row's blind is their ADDRESS, and a
// subject looked up there matches nobody — which is how every provider sign-in
// was refused while each piece passed its own tests. A withdrawn or expired
// link resolves nobody, for [CredentialRow.Revoked]'s rule: the callback checks
// the person's stage and nothing about the link, so this read is the only
// place a revoked link is refused.
func (r *Reader) PersonBySubjectBlind(ctx context.Context, blind string,
	now time.Time) (Sighting, error) {

	if blind == "" {
		return Sighting{}, nil
	}
	var out Sighting
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `
			SELECT DISTINCT person_id FROM iam_credentials
			 WHERE subject_blind = ? AND method = ? AND revoked_at = 0
			   AND (expires_at = 0 OR expires_at > ?)`,
			blind, string(MethodOIDC), now.UnixMilli())
		if err != nil {
			return fmt.Errorf("iamdomain: resolve a provider subject: %w", err)
		}
		var holders []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("iamdomain: read a provider subject: %w", err)
			}
			holders = append(holders, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iamdomain: read a provider subject: %w", err)
		}
		switch len(holders) {
		case 0:
			out = Sighting{}
			return nil
		case 1:
			return sightingIn(ctx, tx, "id", holders[0], &out)
		default:
			return fmt.Errorf("%w: %s", ErrSubjectAmbiguous,
				strings.Join(holders, ", "))
		}
	})
	if err != nil {
		return Sighting{}, err
	}
	return out, nil
}

// sighting is the one lookup every spelling shares.
func (r *Reader) sighting(ctx context.Context, column, token string) (Sighting, error) {
	if token == "" {
		return Sighting{}, nil
	}
	var out Sighting
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		return sightingIn(ctx, tx, column, token, &out)
	})
	if err != nil {
		return Sighting{}, err
	}
	return out, nil
}

// sightingIn resolves one person row inside a read already open.
//
// THE COLUMN IS A LITERAL chosen by this package, never a caller's string:
// every call site passes a constant, and the alternative is a surface one
// parameter away from selecting on something a request named.
func sightingIn(ctx context.Context, tx *sql.Tx, column, token string,
	out *Sighting) error {

	var (
		stage, login, seat string
		chartPosition      int64
		document           []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, stage, login, seat_id, chart_position, document
		  FROM iam_people WHERE `+column+` = ? AND shredded = 0`, token).
		Scan(&out.ID, &stage, &login, &seat, &chartPosition, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// NOBODY, and the zero value is the answer. See the doc on
		// [Reader.PersonByLogin] for why this is not a sentinel.
		*out = Sighting{}
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: resolve a person: %w", err)
	}
	doc, err := DecodePerson(document)
	if err != nil {
		return fmt.Errorf("iamdomain: open person %q: %w", out.ID, err)
	}
	out.Kind = doc.Kind
	// THE COLUMN, for [readPersonRow]'s reason: a sign-in decided from the
	// document admitted a suspended person.
	out.Stage = iam.Stage(stage)
	out.Login = login
	out.Credentials = doc.Credentials
	out.Grants = doc.Grants
	out.Colleague = doc.Colleague
	out.Seat = seat
	out.SeatAt = uint64(chartPosition)
	return nil
}

// AnyPerson reports whether this estate holds anybody at all.
//
// WHAT IT IS FOR is the bootstrap decision: a fresh deployment's identity
// estate is empty, and the one-time code that creates the first person must
// stop working the moment it is not. It is a COUNT rather than a listing
// precisely because it is asked by an unauthenticated route — the answer is
// one bit, and a roster is what it must never become.
func (r *Reader) AnyPerson(ctx context.Context) (bool, error) {
	var held bool
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx,
			`SELECT EXISTS(SELECT 1 FROM iam_people WHERE shredded = 0)`).
			Scan(&count); err != nil {
			return fmt.Errorf("iamdomain: count people: %w", err)
		}
		held = count != 0
		return nil
	})
	return held, err
}

// HoldsBlinds reports whether any row this node holds carries a value derived
// from the company's blind-index key — a person's address, an invitation's, or
// a provider link's subject.
//
// WHAT IT IS FOR is the one decision a missing key forces: mint one, or refuse.
// On an estate that never held a blind a fresh key is simply the first one; on
// one that did, the old key was DELETED, and a new one would orphan every
// address the estate holds and let a second person claim each of them, since
// the claim arbitrates on the blind and the new blind is a new subject.
func (r *Reader) HoldsBlinds(ctx context.Context) (bool, error) {
	var held bool
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM iam_people WHERE email_blind != '')
			    OR EXISTS(SELECT 1 FROM iam_invites)
			    OR EXISTS(SELECT 1 FROM iam_credentials WHERE subject_blind != '')`).
			Scan(&count); err != nil {
			return fmt.Errorf("iamdomain: look for a blinded row: %w", err)
		}
		held = count != 0
		return nil
	})
	return held, err
}

// withTx runs one read in one transaction, which is what makes a multi-row
// answer a snapshot rather than a sequence.
//
// [store.DB.Read] AND NEVER [store.DB.Tx], and the difference is two things at
// once. Tx takes the WRITE lock at BEGIN, so a reader using it would queue
// behind every applier commit and hold the lock against them while it read —
// and the replicated estate is derived state that ONLY its applier writes, so
// a read path holding a write transaction over it is the shape a local write
// eventually grows out of. internal/engine's applier-exclusivity walk fails
// the build on it, which is how this line got written correctly.
//
// MARKED AS IDENTITY WORK, which is what spends the connection
// [store.DB.reserve] holds back. Without the mark these reads draw from the
// ordinary pool, where a socket storm takes every connection and the identity
// lookup that would let those very requests be DECIDED queues behind all of
// them — a queue that feeds itself, since the reads that are waiting are the
// ones identity has to clear. It is the one kind of read in this tree that
// belongs there: a keyed lookup, never a scan, and it is only ever asked
// before a request can decide anything.
func (r *Reader) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	// CHAINED AND NEVER BOUND TO A VARIABLE. internal/store's
	// applier-exclusivity walk cannot follow a handle once it is assigned
	// out, so it reports one — correctly, because the difference between a
	// reader and a writer holding it is exactly what it cannot see. The
	// chain keeps the claim checkable, and it costs nothing: a nil handle
	// answers [store.ErrNoEstate] from inside Read, which is the same
	// three-valued answer a guard here would have produced.
	if err := r.db.Replicated().Read(store.Identity(ctx), fn); err != nil {
		if errors.Is(err, store.ErrNoEstate) {
			return fmt.Errorf("%w: the replicated estate is not open on this "+
				"node, so nothing can be established about any identity here",
				err)
		}
		return err
	}
	return nil
}

// InvitationRow is one invitation as a redemption resolves it.
//
// ITS OWN TYPE beside the [Invitation] document, and not the document itself,
// because the two carry different things: the document is what a WRITER
// states and this is what a READER establishes, with the applier's own
// columns — the blind it was filed under and whether it has been spent —
// which no payload carries.
//
// NO ADDRESS IN THE CLEAR. What comes back is the SEALED bytes and the blind,
// because this read runs for an unauthenticated caller holding a link — and
// what they present is the link, not a claim about whose address it is.
type InvitationRow struct {
	ID         string
	Blind      string
	Sealed     string
	InvitedBy  string
	Grants     []iam.Grant
	Colleague  iam.Colleague
	ExpiresAt  time.Time
	RedeemedAt time.Time
	Person     string
}

// Spent reports whether this invitation can still be redeemed, at now.
//
// ONE PREDICATE rather than two fields a caller compares, because the two ways
// an invitation stops working — redeemed, expired — have the same remedy and
// must have the same answer: a surface that told them apart would say "this
// was already used" to somebody whose link merely aged out, and send them
// looking for whoever used it.
func (i InvitationRow) Spent(now time.Time) bool {
	if !i.RedeemedAt.IsZero() {
		return true
	}
	return !i.ExpiresAt.IsZero() && !now.Before(i.ExpiresAt)
}

// InvitationByID resolves one invitation, on this file's three answers: the
// zero value for one nobody issued, and an error for a node that could not
// tell.
func (r *Reader) InvitationByID(ctx context.Context, id string) (InvitationRow, error) {
	if id == "" {
		return InvitationRow{}, nil
	}
	var out InvitationRow
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		var document []byte
		var expires, redeemed int64
		err := tx.QueryRowContext(ctx, `
			SELECT id, email_blind, expires_at, redeemed_at, person_id, document
			  FROM iam_invites WHERE id = ?`, id).
			Scan(&out.ID, &out.Blind, &expires, &redeemed, &out.Person, &document)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out = InvitationRow{}
			return nil
		case err != nil:
			return fmt.Errorf("iamdomain: read an invitation: %w", err)
		}
		doc, err := DecodeInvitation(document)
		if err != nil {
			return fmt.Errorf("iamdomain: open invitation %q: %w", id, err)
		}
		out.Sealed = doc.Sealed
		out.InvitedBy = doc.InvitedBy
		out.Grants = doc.Grants
		out.Colleague = doc.Colleague
		out.ExpiresAt = fromMillis(expires)
		out.RedeemedAt = fromMillis(redeemed)
		return nil
	})
	if err != nil {
		return InvitationRow{}, err
	}
	return out, nil
}

// fromMillis reads a stored instant, answering the zero time for an unset
// column rather than the epoch — which would render as 1970 on every screen
// that shows a deadline nobody set.
func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// SessionOwner is who holds one session, or empty for a lineage this node does
// not hold.
//
// # Why it is separate from [Reader.Resolve]
//
// Resolve answers what a BEARER is checked against: it takes the person the
// bearer names and reads both rows in one snapshot, so a caller already
// holding a cookie never has to ask whose session it is. This answers the
// opposite question — whose IS this — and it is asked by a surface acting on a
// lineage somebody TYPED.
//
// THAT DISTINCTION IS THE SECURITY PROPERTY. A lineage is not a secret: it
// rides in a cookie, a proxy log, a screenshot. A surface that ended whatever
// lineage it was handed would let anybody end anybody's session, and the only
// thing that stops it is comparing the owner this node holds against the
// caller this node resolved — neither of which the caller supplies.
func (r *Reader) SessionOwner(ctx context.Context, lineage string) (string, error) {
	if lineage == "" {
		return "", nil
	}
	var owner string
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT person_id FROM iam_sessions WHERE lineage = ?`, lineage).
			Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			owner = ""
			return nil
		}
		if err != nil {
			return fmt.Errorf("iamdomain: read a session's owner: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return owner, nil
}

// SeatHeld reports whether a seat handle is one somebody in this estate is
// bound to.
//
// # A BOOL HERE, deliberately, and the third value is the reader's absence
//
// Everywhere else in this file an error is the unknown arm. This one answers
// a REPORT rather than a request: the chart's continuous check asks it per
// seat while rendering, and a per-seat error would make one unreadable row
// fail a page that is otherwise correct. So a read this node cannot perform
// answers FALSE and says so in the log — and the third value is carried one
// level up, by the caller passing no reader at all on a node that does not
// run this domain (see [chartapi.Held]).
//
// That division is what stops a seats-only satellite reporting every human
// seat in the company as unheld: its copy of this estate is legitimately
// empty because it never applies the domain, so it supplies no reader rather
// than a reader that answers false for everybody.
func (r *Reader) SeatHeld(ctx context.Context, handle string) bool {
	if handle == "" {
		return false
	}
	var held bool
	if err := r.withTx(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM iam_people
				 WHERE seat_id = ? AND shredded = 0 AND stage = ?)`,
			handle, string(iam.StageActive)).Scan(&held)
	}); err != nil {
		log.Warn("iam_seat_held_unreadable", "handle", handle, "error", err)
		return false
	}
	return held
}

// HolderOf names the person bound to a seat, read INSIDE a transaction the
// caller supplies.
//
// # The transaction is the caller's, and that is the point
//
// It satisfies the chart domain's [chart.Holders], which is consulted inside
// that domain's own decide — so this read has to join the snapshot the
// decision is being made in rather than open one of its own. A second
// transaction would see a different instant, which is the specific failure
// the write authority's "take ONE snapshot" rule exists to prevent.
//
// # What it can and cannot promise
//
// It is ADVISORY across the domain boundary and the chart's own seam says so
// at length: two logs, two appliers, two anchors, so a bind and a removal can
// each pass their decide and both land. What it establishes is what THIS
// node's copy of the directory says at this instant, which is the strongest
// honest claim available and is enough for the case it exists for — somebody
// removing a seat a colleague is still using.
//
// # An error is never "nobody"
//
// Unlike [Reader.SeatHeld], which answers a report per seat and swallows a
// read it cannot perform, this answers a WRITE: a removal decided on an
// unreadable directory is one that silently orphans whoever holds the seat.
// So the error travels, and the chart refuses.
func (r *Reader) HolderOf(ctx context.Context, tx *sql.Tx, handle string) (string, error) {
	if handle == "" {
		return "", nil
	}
	var login string
	err := tx.QueryRowContext(ctx, `
		SELECT login FROM iam_people
		 WHERE seat_id = ? AND shredded = 0 AND stage = ?
		 LIMIT 1`, handle, string(iam.StageActive)).Scan(&login)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// NOBODY, and it is a finding rather than a failure: a seat the
		// chart holds that no person is bound to is the ordinary state
		// of every agent seat in the company.
		return "", nil
	case err != nil:
		return "", fmt.Errorf("iamdomain: read who holds seat %q: %w", handle, err)
	}
	if login == "" {
		// A ROW WITH NO LOGIN is a person mid-enrolment — the claims
		// land before the content record fills them in — and they hold
		// the seat as surely as anybody. Naming them by their id would
		// be worse than naming them not at all, so the refusal says
		// somebody rather than nobody.
		return "somebody who is still enrolling", nil
	}
	return login, nil
}

// SeatHolder is one seat binding, as contact routing reads the directory.
//
// A FACT AND NOT A VERDICT. Which stages may be reached through a seat's
// contact identities is internal/notify's rule, stated once beside the
// registry it governs; this package says who holds the seat and at what stage,
// and nothing about what that means for a Slack mention.
type SeatHolder struct {
	// Seat is the handle the binding names, which is the address every
	// other subsystem uses for a seat — see the `seat_id` column.
	Seat string

	// Person is who holds it, or who last held it before a removal.
	Person string

	// Stage is the bound person's stage, from the COLUMN — the one every
	// predicate here reads. Empty for a RESERVATION, the half of an
	// enrolment a claim writes before the content record fills it in, and
	// empty on a removal, which has no row left to have a stage.
	Stage iam.Stage

	// Removed marks a seat whose holder was REMOVED while holding it, and
	// that nobody has been bound to since.
	//
	// A removal releases every claim the person held, the seat among them,
	// so the row that bound them is gone and the seat reads as held by
	// nobody — the same as a seat nobody ever held, which routes as the
	// chart declares. Answered that way, removing somebody would route
	// MORE than suspending them: the seat's contact map still names the
	// leaver's own accounts. The tombstone is what still says who held it,
	// and it outlives every horizon here, so this stays true until the
	// seat is bound to somebody else.
	Removed bool
}

// SeatHolders is every seat binding this node's directory holds, read in ONE
// snapshot so a bind and a removal landing between two reads cannot produce a
// set that never existed.
//
// # Three-valued, like everything here
//
// An error is the UNKNOWN arm — the replicated estate not open, a read that
// failed — and must never be folded into an empty answer: an empty answer is
// a real one ("nobody is bound to anything"), and a caller that read an
// outage as it would hand every suspended person's seat back to the chart.
//
// # What it costs
//
// The current bindings are an index range over the partial seat-claim index;
// the removals are one row per person the company has ever removed, because a
// tombstone is permanent and records its seat inside the claims it released.
// Both are read whole, because the one caller rebuilds a registry whole — see
// [Applier]'s directory listener for why that is not a diff.
func (r *Reader) SeatHolders(ctx context.Context) ([]SeatHolder, error) {
	var out []SeatHolder
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, `
			SELECT seat_id, id, stage FROM iam_people
			 WHERE seat_id != '' AND shredded = 0
			 ORDER BY seat_id, id`)
		if err != nil {
			return fmt.Errorf("iamdomain: read the seat bindings: %w", err)
		}
		for rows.Next() {
			var holder SeatHolder
			var stage string
			if err := rows.Scan(&holder.Seat, &holder.Person, &stage); err != nil {
				_ = rows.Close()
				return fmt.Errorf("iamdomain: read a seat binding: %w", err)
			}
			holder.Stage = iam.Stage(stage)
			out = append(out, holder)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("iamdomain: read the seat bindings: %w", err)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("iamdomain: read the seat bindings: %w", err)
		}

		// A REMOVAL'S SEAT, WHILE NOBODY HOLDS IT. The NOT EXISTS is
		// what hands a seat on: bind somebody new and the tombstone stops
		// being the seat's last word.
		removed, err := tx.QueryContext(ctx, `
			SELECT json_extract(r.claims_json, '$.seat_id') AS seat, r.person_id
			  FROM iam_removed r
			 WHERE COALESCE(json_extract(r.claims_json, '$.seat_id'), '') != ''
			   AND NOT EXISTS (
			       SELECT 1 FROM iam_people p
			        WHERE p.seat_id = json_extract(r.claims_json, '$.seat_id'))
			 ORDER BY seat, r.person_id`)
		if err != nil {
			return fmt.Errorf("iamdomain: read the removed seat holders: %w", err)
		}
		for removed.Next() {
			holder := SeatHolder{Removed: true}
			if err := removed.Scan(&holder.Seat, &holder.Person); err != nil {
				_ = removed.Close()
				return fmt.Errorf("iamdomain: read a removed seat holder: %w", err)
			}
			out = append(out, holder)
		}
		if err := removed.Close(); err != nil {
			return fmt.Errorf("iamdomain: read the removed seat holders: %w", err)
		}
		return removed.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
