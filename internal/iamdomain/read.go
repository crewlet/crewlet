package iamdomain

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
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
// A person's name and address are SEALED under the fleet keyring. Every read
// below returns the sealed bytes and leaves opening them to a surface that has
// a reason to — so a value is opened only where somebody is shown it, and a
// read that decides who may do what never touches one.

// Reader answers questions about this node's copy of the identity estate.
type Reader struct {
	db  *store.DB
	log *statelog.Reader

	committed func() statelog.Position
	lag       func() time.Duration
	await     func(context.Context, statelog.Position) error
}

// ReaderOptions is what a reader is built from.
type ReaderOptions struct {
	// DB is the replicated estate. REQUIRED.
	DB *store.DB

	// Log is this domain's read authority. REQUIRED: without it every read
	// level is a label rather than a guarantee, and a degradation invisible
	// in the answer is worse than a refusal.
	Log *statelog.Reader

	// Committed is this node's applied position and Lag how far behind the
	// log it is. Each is nil-safe and answers the zero value, which is what
	// a reader with no runner behind it honestly has.
	//
	// WHETHER A RECORD THIS NODE RETAINED IS ABOUT SOMEBODY is not an
	// option: it is read out of the deferral index inside the snapshot the
	// rows are read in ([statelog.DeferredIn]). It was the runner's
	// earliest deferral, which is about one bucket and — as the runner read
	// it — carried no scope at all, so no retained record was ever about
	// anybody and every "this node cannot vouch for this person" answer
	// the session table and a token's binding turn on was dead.
	Committed func() statelog.Position
	Lag       func() time.Duration

	// Await blocks until this node's applier has committed through a
	// position — the runner's own wait. NIL ANSWERS AN ERROR rather than
	// nil, for the other two's reason read the other way round: a reader
	// with no runner behind it will never reach a position, and a wait
	// that reported arrival would send a caller to decide on rows that are
	// not there.
	Await func(context.Context, statelog.Position) error
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
		committed: opts.Committed, lag: opts.Lag, await: opts.Await}
	if r.committed == nil {
		r.committed = func() statelog.Position { return statelog.Position{} }
	}
	if r.lag == nil {
		r.lag = func() time.Duration { return 0 }
	}
	return r, nil
}

// At is the position this node's rows were derived through.
func (r *Reader) At() statelog.Position { return r.committed() }

// errNoApplier is a wait asked of a reader with no runner behind it.
var errNoApplier = errors.New("iamdomain: this reader has no applier to wait " +
	"on, so no position will ever be reached here")

// AwaitApplied blocks until this node's applier has committed through a packed
// position, or ctx ends.
//
// PACKED, because that is the form a session bearer carries its start in, and
// the one caller is the request guard waiting for exactly that position — see
// internal/api/auth's session arm. The stream is this DOMAIN's own, named by
// the domain rather than read back off the applier: a packed position does not
// carry one, a bearer is only ever about this log, and an applier that has
// committed nothing yet has no position to read a stream name off.
func (r *Reader) AwaitApplied(ctx context.Context, position uint64) error {
	if r.await == nil {
		return errNoApplier
	}
	return r.await(ctx, statelog.Unpack(Domain{}.Stream().Name, int64(position)))
}

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
		Lag:     r.lag(),
	}
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		// THE DEFERRAL IS READ IN THE ROWS' OWN SNAPSHOT, and it is the
		// framework's coverage answer: this node holds a record it could
		// not decode whose scope covers this person's bucket. The read
		// SUCCEEDS and its rows are simply not known to be complete,
		// which is why this is a field rather than an error.
		var err error
		if out.Deferred, err = deferredFor(ctx, tx, person); err != nil {
			return err
		}
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

// Vouch is how far this node can vouch for the rows one read answered: how far
// its applier is behind the log, and whether it holds a record it could not
// decode whose scope covers the person those rows are about.
//
// FACTS AND NOT A VERDICT. What a lag or a deferral MEANS is the caller's
// table — internal/iam/session's for a session, the engine's for a Tier A
// token's binding and a login's record — and a threshold chosen here would be
// a second opinion about the one event [statelog.StallGrace] already names.
// One value rather than two answers so that a caller cannot ask about the lag
// and forget the deferral, which reads as a caught-up node while it holds a
// newer peer's record about exactly this person.
type Vouch struct {
	Lag      time.Duration
	Deferred bool
}

// PersonByLoginVouched is [Reader.PersonByLogin] and how far this node can
// vouch for its answer, the row and the deferral read in ONE snapshot.
//
// For the callers that decide on a DIRECTORY ROW rather than on a credential:
// a Tier A token's seat binding and the record a login keeps its holder's work
// under. They read the row and then asked [Reader] separately whether a
// record this node retained covered its person — two snapshots, so a record
// reprocessed between them paired rows from before it with "nothing
// deferred", a verdict that held at no instant. [Reader.MachineToken] and
// [Reader.Resolve] read the two together for the same reason.
//
// An ABSENT row is vouched for over the ROOT — every deferral covers it —
// because nothing can say which bucket a record this node could not decode is
// about, and it may be the enrolment that claims this login. A reservation is
// vouched for over its person's bucket, which its row names. THREE-VALUED on
// the deferral: the index is a read, and one that failed says nothing either
// way.
func (r *Reader) PersonByLoginVouched(ctx context.Context, login string) (
	Sighting, Vouch, error) {

	var (
		out   Sighting
		vouch Vouch
	)
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		if login != "" {
			if err := sightingIn(ctx, tx, "login", login, &out); err != nil {
				return err
			}
		}
		var err error
		vouch.Deferred, err = deferredFor(ctx, tx, out.ID)
		return err
	})
	if err != nil {
		return Sighting{}, Vouch{}, err
	}
	vouch.Lag = r.lag()
	return out, vouch, nil
}

// deferredFor reports, inside the caller's snapshot, whether this node holds a
// record it could not decode whose scope covers this person's bucket.
//
// THE BUCKET AND NOT THE PERSON, because that is all a deferral can say: its
// scope is a bucket path, and the person a record is about is inside a payload
// the deferring node could not decode. See scope.go for why the scope is flat
// and bucketed rather than per person. And EVERY retained record, through the
// framework's own index ([statelog.DeferredIn]) — the earliest one is usually
// about somebody else, and a later one about exactly this person is the case
// the answer exists for. A root-scoped record covers everybody, and an empty
// person is covered by every record, since the probe is then the root.
func deferredFor(ctx context.Context, tx *sql.Tx, person string) (bool, error) {
	var scope ScopeSet
	if person == "" {
		scope = RootScope()
	} else {
		scope = PeopleScope(person)
	}
	_, hit, err := statelog.DeferredIn(ctx, tx, Domain{},
		scope.Resolve(PersonSubject(person)))
	if err != nil {
		return false, fmt.Errorf("iamdomain: ask whether a record this node "+
			"retained is about this person: %w", err)
	}
	return hit, nil
}

// reservation reports whether a row's kind column marks it as a RESERVATION:
// the half of an enrolment its claims write before the content record fills
// it in, with no kind, no stage and an empty document.
//
// THE KIND AND NOT THE DOCUMENT, because the kind is the column the content
// record's apply sets and nothing else ever clears: an empty document could
// also be a row some later build writes differently, and a reader deciding
// from it would call a newer peer's person a reservation. A row whose kind is
// set and whose document will not decode is still the unknown arm.
func reservation(kind string) bool { return kind == "" }

// readSessionRow fills one session's facts.
func readSessionRow(ctx context.Context, tx *sql.Tx, lineage string,
	out *session.LineageRow) error {

	if lineage == "" {
		return nil
	}
	var (
		endedAt, epoch int64
		document       []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT epoch, ended_at, document FROM iam_sessions WHERE lineage = ?`,
		lineage).Scan(&epoch, &endedAt, &document)
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
	doc, err := DecodeSession(document)
	if err != nil {
		// THE UNKNOWN ARM, as for a person's row: a session a newer peer
		// wrote that this build cannot open is one whose facts this node
		// cannot state, and answering it as absent would be a 401.
		return fmt.Errorf("iamdomain: open session %q: %w", lineage, err)
	}
	out.Found = true
	out.Ended = endedAt != 0
	out.Epoch = uint64(epoch)
	out.ProvedAt = doc.ProvedAt
	out.EnrolmentOnly = doc.EnrolmentOnly
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
		kind, stage, login, seat string
		document                 []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT kind, stage, login, seat_id, document
		  FROM iam_people WHERE id = ?`, person).
		Scan(&kind, &stage, &login, &seat, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read the person row: %w", err)
	case reservation(kind):
		// A RESERVATION IS NOT THE PERSON, and it is reported as ABSENT
		// rather than as a person who may not act. The two answers are
		// opposite here: a node that has applied an enrolment's claims
		// and not yet its content record holds exactly this row, and
		// the bearer's start position is what turns "absent" into the
		// right one of gone and behind — while "found, at no stage"
		// would refuse with 401 on the one node that is merely behind.
		return nil
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
	out.Grants = doc.Grants
	out.Seat = seat
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

// MachineToken answers everything one presented machine token is checked
// against, in ONE snapshot: the credential row under its id, the owner as this
// node holds them now, the owner's current epoch, the company's current
// session generation, and how far this node can vouch.
//
// It satisfies the request guard's consumer-defined seam, and it is the
// token's [Reader.Resolve]: a revocation landing between a credential read
// and an owner read would produce a verdict that never existed at any instant.
// WHAT THE ROWS MEAN is [credential.CheckToken]'s, over the value returned.
//
// ONE KEYED READ OF ONE ROW, never a scan, because the token carries its own
// id in the clear — which is what a credential presented on every request a
// pipeline makes has to cost.
//
// AN ERROR IS ALWAYS THE UNKNOWN ARM. A row that will not decode is a newer
// peer's, and answering it as "no such token" would tell a pipeline its
// credential is broken for the length of an upgrade.
func (r *Reader) MachineToken(ctx context.Context, id string) (credential.TokenRow, error) {
	out := credential.TokenRow{Applied: uint64(r.committed().Packed())}
	var owner string
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		var (
			method           string
			verifier         []byte
			expires, revoked int64
			document         []byte
		)
		err := tx.QueryRowContext(ctx, `
			SELECT person_id, method, verifier, expires_at, revoked_at, document
			  FROM iam_credentials WHERE id = ?`, id).
			Scan(&owner, &method, &verifier, &expires, &revoked, &document)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			// ABSENT IS A FINDING, which the token's own mint position
			// turns into "gone" or "not yet" — and EVERY deferral covers
			// it, because nothing can say which bucket a record this node
			// could not decode is about.
			owner = ""
			out.Deferred, err = deferredFor(ctx, tx, owner)
			return err
		case err != nil:
			return fmt.Errorf("iamdomain: read a token: %w", err)
		}
		held, err := DecodeCredential(document)
		if err != nil {
			return fmt.Errorf("iamdomain: open token %q: %w", id, err)
		}
		out.Found = true
		out.IsToken = CredentialMethod(method) == MethodToken
		out.Verifier = string(verifier)
		out.ExpiresAt = fromMillis(expires)
		out.RevokedAt = fromMillis(revoked)
		out.Grants = held.Grants
		out.Epoch = held.Epoch
		out.Generation = held.Generation
		if err = readGeneration(ctx, tx, &out.FleetGeneration); err != nil {
			return err
		}
		// THE OWNER'S BUCKET, once the row has named them, read in the
		// same snapshot as the row that named them.
		if out.Deferred, err = deferredFor(ctx, tx, owner); err != nil {
			return err
		}
		return readTokenOwner(ctx, tx, owner, &out.Owner)
	})
	if err != nil {
		return credential.TokenRow{}, err
	}
	out.Lag = r.lag()
	return out, nil
}

// readTokenOwner fills a token's owner as this node holds them now.
func readTokenOwner(ctx context.Context, tx *sql.Tx, id string,
	out *credential.TokenOwner) error {

	var (
		stage, login, seat string
		document           []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT stage, login, seat_id, document
		  FROM iam_people WHERE id = ?`, id).
		Scan(&stage, &login, &seat, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read a token's owner: %w", err)
	}
	doc, err := DecodePerson(document)
	if err != nil {
		return fmt.Errorf("iamdomain: open person %q: %w", id, err)
	}
	out.Found = true
	out.ID = id
	out.Kind = doc.Kind
	// THE COLUMN, for [readPersonRow]'s reason: a token decided from the
	// document served a suspended owner.
	out.Stage = iam.Stage(stage)
	out.Login = login
	out.Grants = doc.Grants
	out.Seat = seat
	return readEpoch(ctx, tx, id, &out.Epoch)
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
	Seat        string

	// Reserved reports a RESERVATION: the row an enrolment's claims leave
	// before its content record fills it in — see [reservation]. It holds
	// the token that was looked up and nothing else: no kind, no stage and
	// no credential, so it may do nothing at all, and every caller that
	// asks what somebody may do reads it as nobody who can.
	//
	// AN ANSWER AND NOT AN ERROR. The row's document is empty by design,
	// and decoding it used to fail as though a newer peer had written it:
	// a Tier A token whose machine enrolment stopped after its login claim
	// answered 503 on every guarded route until a sweep collected the
	// row, for a binding that had never existed.
	Reserved bool
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
// is the shape half, and the caller spends a decoy's derivation against the
// zero value rather than branching on it.
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
// with the party registry's fold or one address would reach a seat and a
// different person.
func (r *Reader) PersonByEmailBlind(ctx context.Context, blind string) (Sighting, error) {
	return r.sighting(ctx, "email_blind", blind)
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
		kind, stage, login, seat string
		document                 []byte
	)
	err := tx.QueryRowContext(ctx, `
		SELECT id, kind, stage, login, seat_id, document
		  FROM iam_people WHERE `+column+` = ?`, token).
		Scan(&out.ID, &kind, &stage, &login, &seat, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// NOBODY, and the zero value is the answer. See the doc on
		// [Reader.PersonByLogin] for why this is not a sentinel.
		*out = Sighting{}
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: resolve a person: %w", err)
	case reservation(kind):
		// THE TOKEN IS HELD AND NOBODY HOLDS IT YET. The id and the
		// claimed columns are the row's; everything a caller would
		// decide with is absent, which is what makes a reservation act
		// as nobody without every caller having to know the shape.
		*out = Sighting{ID: out.ID, Login: login, Seat: seat, Reserved: true}
		return nil
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
	out.Seat = seat
	return nil
}

// AnyPerson reports whether this estate holds anybody at all — whether a
// person or a machine is enrolled.
//
// WHAT IT IS FOR is `/health`'s `identity`: a fresh deployment's identity
// estate is empty, and an operator looking at one, or a dashboard whose
// sign-in form nobody can use yet, needs to be told the next step is to
// invite the first person rather than left looking at a sign-in that fails.
// It is a COUNT rather than a listing precisely because it is asked by an
// unauthenticated route — the answer is one bit, and a roster is what it must
// never become.
//
// A RESERVATION IS NOBODY — a claim whose content record has not landed has no
// kind and may do nothing — and a REMOVED person is nobody, their row a
// tombstone. A SUSPENDED one is somebody: the company has started.
//
// # "Nobody" is an absence, and it is proved against the log's end
//
// Two ways a node's rows read as nobody while the company has somebody, and
// "nobody" is said only where neither holds — otherwise this is the unknown
// arm, wrapping [statelog.ErrUnavailable] (and, for the first, [ErrNotCurrent]),
// which clears on its own. "Somebody" needs no such proof: a row is a fact.
//
//   - BEHIND. end is the identity log's last sequence, read BEFORE this call
//     (the engine's IdentityLogEnd), and rows that have not applied through it
//     cannot say the enrolment is not among what they are missing
//     ([CoversLog]). Every node applies this log from boot, company or none,
//     so a node that has just joined a fleet with people in it is exactly the
//     node whose empty rows would otherwise tell /health and its own boot log
//     that nobody works there — and tell an operator to invite a founder into
//     a company that has one.
//   - RETAINED. Settled is not applied: this node's checkpoint moves past a
//     record it RETAINS — a newer build's, one signed under a keyring key it
//     was not restarted with — without writing its rows, so an enrolment it
//     retained reads here exactly like nobody. So the whole deferral index is
//     asked too, because a retained enrolment may be in any bucket.
func (r *Reader) AnyPerson(ctx context.Context, end uint64) (bool, error) {
	var held bool
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM iam_people WHERE kind <> '')`).
			Scan(&held); err != nil {
			return fmt.Errorf("iamdomain: read whether anybody is enrolled: %w", err)
		}
		if held {
			return nil
		}
		return provedAbsent(ctx, tx, end, "the company's first person")
	})
	return held, err
}

// provedAbsent is the proof a missing row needs before it is read as "there is
// none": these rows applied every record the log held at end, and retain none
// they could not apply. what names the record a node behind would be missing,
// for the error an operator reads.
//
// THE WHOLE DEFERRAL INDEX, never one bucket's: the row is missing, so nothing
// here says whose bucket the missing record would be filed under.
func provedAbsent(ctx context.Context, tx *sql.Tx, end uint64, what string) error {
	prefix, err := statelog.PrefixIn(ctx, tx, Domain{})
	if err != nil {
		return fmt.Errorf("iamdomain: read how much of the log these rows "+
			"hold: %w", err)
	}
	if behind := CoversLog(prefix, end); behind != nil {
		return fmt.Errorf("%w: %w — so a row missing from them may be %s "+
			"this node has not applied yet", statelog.ErrUnavailable, behind,
			what)
	}
	retained, err := deferredFor(ctx, tx, "")
	if err != nil {
		return err
	}
	if retained {
		return fmt.Errorf("%w: %w — this node holds a record it could not "+
			"apply, which may be %s", statelog.ErrUnavailable, ErrNotCurrent,
			what)
	}
	return nil
}

// ErrNotCurrent reports a snapshot of the identity estate that cannot vouch for
// an absence: it has not applied every record the log held when the question
// was asked, whether because it is behind or because it RETAINED one.
//
// A SENTINEL, because the question that turns on it is answered by a row being
// missing — whether a blind was ever derived ([Reader.HoldsBlinds]) — and on
// such a snapshot a missing row is the one observation that proves nothing.
var ErrNotCurrent = errors.New("iamdomain: this node's identity rows do not " +
	"hold every record the log held when it was asked, so a row missing from " +
	"them proves nothing")

// CoversLog proves, from one snapshot's prefix and the log's last sequence read
// BEFORE that snapshot, that the snapshot's rows hold every record the log held
// — or says, wrapping [ErrNotCurrent], why they do not.
//
// THE END IS READ FIRST, so a record landing between the two can only make the
// answer "not current", never let a snapshot that missed it vouch for it.
//
// AND THE COMPARISON IS AGAINST WHAT THE SNAPSHOT APPLIED, never its
// checkpoint. The applier's checkpoint moves past a record it RETAINS — one
// at a record version this build cannot read, one signed under a keyring key
// this node does not hold (a rotation half done), and every later record in a
// bucket one of those covers — and a node holding such a record reads that
// record's rows as rows nobody wrote.
func CoversLog(prefix statelog.Prefix, end uint64) error {
	if prefix.Applied().Seq >= end {
		return nil
	}
	if prefix.Retains && prefix.Retained.Position.Seq <= end {
		return fmt.Errorf("%w: it holds a record at %s it could not apply "+
			"(record version %d) — a newer build's, or one signed under a "+
			"keyring key this node does not hold — and every record the log "+
			"held when it was asked is not applied here until it can",
			ErrNotCurrent, prefix.Retained.Position, prefix.Retained.Version)
	}
	return fmt.Errorf("%w: it has applied %d of the %d records the log held",
		ErrNotCurrent, prefix.Applied().Seq, end)
}

// HoldsBlinds reports whether any row this node holds carries a value derived
// from the company's blind-index key — a person's address, or an invitation's.
//
// WHAT IT IS FOR is the one decision a missing key forces: mint one, or refuse.
// On an estate that never held a blind a fresh key is simply the first one; on
// one that did, the old key was DELETED, and a new one would orphan every
// address the estate holds and let a second person claim each of them, since
// the claim arbitrates on the blind and the new blind is a new subject.
//
// THREE-VALUED, and "none" is an ABSENCE that has to be proved: end is the
// identity log's last sequence read BEFORE this call, and a snapshot that has
// not applied every record through it — behind, or holding a record it
// RETAINED — cannot say whether a blinded row is among what it has not applied.
// So (true, nil) is a blind held, (false, nil) is none on rows that hold the
// whole log, and an error wrapping [ErrNotCurrent] is this node being unable to
// say, which refuses the mint exactly as an unreadable store does.
func (r *Reader) HoldsBlinds(ctx context.Context, end uint64) (bool, error) {
	var held bool
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		prefix, err := statelog.PrefixIn(ctx, tx, Domain{})
		if err != nil {
			return fmt.Errorf("iamdomain: read how much of the log these rows "+
				"hold: %w", err)
		}
		if err := CoversLog(prefix, end); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM iam_people WHERE email_blind != '')
			    OR EXISTS(SELECT 1 FROM iam_invites)`).
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
//
// AND NO SECRET: the row carries the link secret's VERIFIER, and the one
// question anybody may ask of it is [InvitationRow.Admits].
type InvitationRow struct {
	ID         string
	Blind      string
	Sealed     string
	InvitedBy  string
	Grants     []iam.Grant
	ExpiresAt  time.Time
	RedeemedAt time.Time
	Person     string

	// Verifier is [Invitation.Verifier]: what the link's secret is checked
	// against, and empty for an invitation nothing can redeem.
	Verifier string

	// Seat is the seat redeeming this binds, or empty — [Invitation.Seat].
	Seat string
}

// Admits reports whether a secret presented with this invitation's id is the
// one its link carries.
//
// CONSTANT-TIME, over the verifiers, so how far a guess matched is not a
// timing — and FALSE for an invitation with no verifier, which was issued
// before links carried a secret and is redeemable by nobody: admitting it on
// its id alone would be admitting exactly what the secret exists to close.
func (i InvitationRow) Admits(secret string) bool {
	return invitationAdmits(i.Verifier, secret)
}

// invitationAdmits is [InvitationRow.Admits]' comparison, and the one the
// redemption's own record asks ([Writer.Enrol]) — one function, so the route
// and the record cannot come to disagree about what a secret is.
func invitationAdmits(verifier, secret string) bool {
	if verifier == "" || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(InvitationVerifier(secret)),
		[]byte(verifier)) == 1
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
		out.Verifier = doc.Verifier
		out.Seat = doc.Seat
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

// SessionStanding is who holds one session — empty for a lineage this node
// does not hold — and whether this node's rows still hold it LIVE, both read
// in ONE snapshot.
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
//
// # And why it says whether the session is still live
//
// Because ending a session is a claim that it was live until now. Asked only
// for the owner, a named sign-out closed and announced a session a record had
// already ended — revoked, invalidated, past its absolute deadline — naming
// the caller as the one who ended it. Live is one reading of the row: not
// ended, not past its absolute deadline, not opened before the company's last
// invalidation, and at its person's current revocation epoch. An absent row is
// never live.
//
// # And an absent row is PROVED against the log's end, or not said
//
// end is the identity log's last sequence, read BEFORE this snapshot (see
// [CoversLog]), and a lineage these rows hold no record of is answered as
// nobody's only where they hold every record the log held then. A named
// sign-out answers "ended" for nobody's lineage — the session is over, or never
// was — and on rows that had not yet applied the session's start that was a
// session still running reported closed: an administrator ending a stolen
// laptop's session on a node behind the one that listed it was told it had
// ended and wrote nothing. Rows that cannot vouch for the absence answer
// [ErrNotCurrent] under [statelog.ErrUnavailable], which a surface serves as
// "ask again".
func (r *Reader) SessionStanding(ctx context.Context, lineage string,
	now time.Time, end uint64) (string, bool, error) {

	if lineage == "" {
		return "", false, nil
	}
	var (
		owner string
		live  bool
	)
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		var epoch, start, expires, ended int64
		err := tx.QueryRowContext(ctx, `
			SELECT person_id, epoch, start_position, absolute_expires_at,
			       ended_at
			FROM iam_sessions WHERE lineage = ?`, lineage).
			Scan(&owner, &epoch, &start, &expires, &ended)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			owner = ""
			return provedAbsent(ctx, tx, end, "the session's start")
		case err != nil:
			return fmt.Errorf("iamdomain: read a session's owner: %w", err)
		}
		invalidated, err := readInvalidated(ctx, tx)
		if err != nil {
			return err
		}
		switch {
		case ended != 0,
			expires > 0 && expires <= now.UnixMilli(),
			// A SESSION OPENED BEFORE THE COMPANY'S LAST INVALIDATION
			// carries the generation that invalidation ended.
			invalidated > 0 && uint64(start) < invalidated:
			return nil
		}
		current, err := epochOf(ctx, tx, owner)
		if err != nil {
			return err
		}
		live = uint64(epoch) >= current
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return owner, live, nil
}

// readInvalidated is the company's session generation — the position of its
// last invalidation, zero for a company that never had one — read inside the
// caller's snapshot.
func readInvalidated(ctx context.Context, tx *sql.Tx) (uint64, error) {
	var invalidated int64
	err := tx.QueryRowContext(ctx,
		`SELECT version FROM iam_session_generation WHERE singleton = 0`).
		Scan(&invalidated)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("iamdomain: read the session generation: %w", err)
	}
	return uint64(invalidated), nil
}

// SeatHolder is one seat binding, as contact routing reads the directory.
//
// A FACT AND NOT A VERDICT. Which stages may be reached through a seat's
// contact identities is internal/notify's rule, stated once beside the
// registry it governs; this package says who holds the seat and at what stage,
// and nothing about what that means for a Slack mention.
type SeatHolder struct {
	// Seat is the seat's IDENTITY — the handle it was created under, which
	// no rename moves (ADR-0027) — and never the handle it answers to now.
	// A reader turns it into a seat by [org.Role.Origin] over the running
	// organisation, never by comparing it to a handle: after a rename the
	// two differ, and after the old handle is reused they name different
	// seats.
	Seat string

	// Person is who holds it, or who last held it before a removal.
	Person string

	// Stage is the bound person's stage, from the COLUMN — the one every
	// predicate here reads. Empty for a RESERVATION, the half of an
	// enrolment a claim writes before the content record fills it in, and
	// empty on a removal, which has no row left to have a stage.
	Stage iam.Stage

	// Removed marks a seat whose holder was REMOVED while holding it, and
	// that nobody has been bound to since that removal.
	//
	// A removal releases every claim the person held, the seat among them,
	// so the row that bound them is gone and the seat reads as held by
	// nobody — the same as a seat nobody ever held, which routes as the
	// chart declares. Answered that way, removing somebody would route
	// MORE than suspending them: the seat's contact map still names the
	// leaver's own accounts. The tombstone is what still says who held it,
	// and it outlives every horizon here.
	//
	// IT SPEAKS FOR THE BINDING IT RELEASED AND NO OTHER. The first bind of
	// the seat after the removal ends its say for good
	// (`iam_removed.seat_rebound_by`): a successor who is bound and later
	// unbound hands the seat back to the chart like any other unbind, rather
	// than back to the leaver's tombstone — which, read from the seat's
	// current rows alone, withheld it again indefinitely whatever its
	// contact map had since been pointed at. The tombstone, the successor's
	// bind and the stamp all name the seat by its IDENTITY, so a bind under
	// a handle the seat was renamed to after the removal still ends it: keyed
	// on the handle typed at the time, the leaver's tombstone named an address
	// nobody could bind again and withheld the new holder for ever.
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
			 WHERE seat_id != ''
			 ORDER BY seat_id, id`)
		if err != nil {
			return fmt.Errorf("iamdomain: read the seat bindings: %w", err)
		}
		for rows.Next() {
			var holder SeatHolder
			var stage string
			if err = rows.Scan(&holder.Seat, &holder.Person, &stage); err != nil {
				_ = rows.Close()
				return fmt.Errorf("iamdomain: read a seat binding: %w", err)
			}
			holder.Stage = iam.Stage(stage)
			out = append(out, holder)
		}
		if err = rows.Close(); err != nil {
			return fmt.Errorf("iamdomain: read the seat bindings: %w", err)
		}
		if err = rows.Err(); err != nil {
			return fmt.Errorf("iamdomain: read the seat bindings: %w", err)
		}

		// A REMOVAL'S SEAT, UNTIL IT IS BOUND AGAIN. `seat_rebound_by` is
		// what hands the seat on for good — the first bind after the
		// removal stamps it, so a later unbind does not bring the leaver
		// back — and the NOT EXISTS keeps the current holder's standing
		// the seat's word over a duplicate a restore left behind.
		removed, err := tx.QueryContext(ctx, `
			SELECT json_extract(r.claims_json, '$.seat_id') AS seat, r.person_id
			  FROM iam_removed r
			 WHERE COALESCE(json_extract(r.claims_json, '$.seat_id'), '') != ''
			   AND r.seat_rebound_by = ''
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
