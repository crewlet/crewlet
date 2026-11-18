package iamdomain

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The read side of the identity estate.
//
// Every answer is three-valued: an error is the unknown arm, never folded into
// "not X", or every credential reads as invalid for an outage's length; so
// nothing here returns `(value, bool)`. A multi-fact answer is ONE snapshot
// ([Reader.Resolve]): a revocation landing between two reads makes a verdict
// that held at no instant. Nothing here decrypts: sealed bytes are opened only
// where somebody is shown them. Nothing reads at a level, which would take a
// barrier per request: answers are this node's rows beside their lag ([Vouch],
// [Reader.At]), and a bearer's unapplied session waits for the position it
// states ([Reader.AwaitApplied]), appending nothing. internal/engine's
// register sizes the identity log for this read model.

// Reader answers questions about this node's copy of the identity estate.
type Reader struct {
	db store.ReplicatedReader

	reached func() statelog.Position
	lag     func() time.Duration
	await   func(context.Context, statelog.Position) error
}

// ReaderOptions is what a reader is built from.
type ReaderOptions struct {
	// DB is the replicated estate. REQUIRED.
	DB store.ReplicatedReader

	// Reached is the position this node's rows are current through: its
	// applier's reached position ([statelog.Runner.Reached]), never the
	// committed checkpoint, since a barrier whose checkpoint has not committed
	// changes no row, and a record that writes one is reached only once
	// committed. Lag is how far behind the log it is. Each is nil-safe and
	// answers the zero value, which is what a reader with no runner honestly has.
	//
	// Whether a retained record is about somebody is not an option: it is read
	// from the deferral index in the rows' own snapshot ([statelog.DeferredIn]),
	// never the runner's earliest deferral, which names one bucket.
	Reached func() statelog.Position
	Lag     func() time.Duration

	// Await blocks until this node's applier has reached a position
	// ([statelog.Runner.WaitReached]). NIL ANSWERS AN ERROR, never arrival: a
	// reader with no runner never reaches a position, and reporting arrival would
	// send a caller to decide on rows that are not there.
	Await func(context.Context, statelog.Position) error
}

// NewReader builds the identity estate's read side.
func NewReader(opts ReaderOptions) (*Reader, error) {
	if opts.DB.IsZero() {
		return nil, errors.New("iamdomain: a reader needs the replicated estate")
	}
	r := &Reader{db: opts.DB,
		reached: opts.Reached, lag: opts.Lag, await: opts.Await}
	if r.reached == nil {
		r.reached = func() statelog.Position { return statelog.Position{} }
	}
	if r.lag == nil {
		r.lag = func() time.Duration { return 0 }
	}
	return r, nil
}

// At is the position this node's rows were derived through.
func (r *Reader) At() statelog.Position { return r.reached() }

// errNoApplier is a wait asked of a reader with no runner behind it.
var errNoApplier = errors.New("iamdomain: this reader has no applier to wait " +
	"on, so no position will ever be reached here")

// AwaitApplied blocks until this node's applier has reached a packed position,
// or ctx ends.
//
// PACKED, the form a session bearer carries its start in (internal/api/auth's
// session arm). The stream is this domain's own: a packed position names none,
// a bearer is only ever about this log, and an applier that has committed
// nothing has no stream name to read.
func (r *Reader) AwaitApplied(ctx context.Context, position uint64) error {
	if r.await == nil {
		return errNoApplier
	}
	return r.await(ctx, statelog.Unpack(Domain{}.Stream().Name, int64(position)))
}

// ErrNotFound is a lookup this node could answer and that names nobody.
//
// A sentinel, not a bool beside an error, which would be two ways to say one
// thing. A sign-in deliberately does NOT report it: see [Reader.PersonByLogin].
var ErrNotFound = errors.New("no such person")

// Resolve answers everything one bearer is checked against, in ONE snapshot.
//
// It satisfies [session.Directory]; the facts travel together because read
// separately they make a verdict that held at no instant. AN ERROR IS ALWAYS
// THE UNKNOWN ARM (503), never "this session does not exist".
func (r *Reader) Resolve(ctx context.Context, lineage, person string) (
	session.Identity, error) {

	out := session.Identity{
		Applied: uint64(r.reached().Packed()),
		Lag:     r.lag(),
	}
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		// THE DEFERRAL IS READ IN THE ROWS' OWN SNAPSHOT: a record this node
		// could not decode whose scope covers this person's bucket. The read
		// still succeeds, its rows just not known complete, so it is a field.
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

// Vouch is how far this node can vouch for the rows one read answered: its
// applier's lag, and whether it holds a record it could not decode whose scope
// covers the person those rows are about.
//
// FACTS, NOT A VERDICT: what they mean is the caller's table (the session
// package's, the engine's for a token binding and a login's record); a
// threshold here would second-guess [statelog.StallGrace]. One value, so no
// caller asks about the lag and forgets the deferral.
type Vouch struct {
	Lag      time.Duration
	Deferred bool
}

// PersonByLoginVouched is [Reader.PersonByLogin] and how far this node can
// vouch for its answer, the row and the deferral read in ONE snapshot.
//
// For callers that decide on a directory row rather than a credential (a Tier
// A token's seat binding, a login's record): two snapshots would pair rows from
// before a reprocessed record with "nothing deferred", as [Reader.Resolve] and
// [Reader.MachineToken] avoid too. An ABSENT row is vouched for over the ROOT:
// an undecodable record may be the enrolment or rename that took this login.
// Three-valued on the deferral: a failed index read says nothing either way.
func (r *Reader) PersonByLoginVouched(ctx context.Context, login string) (
	Sighting, Vouch, error) {

	var (
		out   Sighting
		vouch Vouch
	)
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		if login != "" {
			if err := sightingIn(ctx, tx, sightingByLogin, login, &out); err != nil {
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
// THE BUCKET, not the person: a deferral's scope is a bucket path, the person
// being inside a payload nobody here could decode (scope.go). EVERY retained
// record, through [statelog.DeferredIn], since the earliest is usually
// somebody else's. An empty person probes the root, which every record covers.
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
		// ABSENT IS A FINDING, not a failure: session.Row turns it, with the
		// bearer's start position, into "ended and swept" or "not applied yet".
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read the session row: %w", err)
	}
	doc, err := DecodeSession(document)
	if err != nil {
		// THE UNKNOWN ARM: a newer peer's session this build cannot open has
		// facts this node cannot state, and answered as absent it is a 401.
		return fmt.Errorf("iamdomain: open session %q: %w", lineage, err)
	}
	out.Found = true
	out.Ended = endedAt != 0
	out.Epoch = uint64(epoch)
	out.ProvedAt = doc.ProvedAt
	out.EnrolmentOnly = doc.EnrolmentOnly
	return nil
}

// personStandingStatement is one person's stage, login, seat and document, by
// their row's key: what a session's person ([readPersonRow]) and a token's
// owner ([readTokenOwner]) are read as.
const personStandingStatement = `
	SELECT stage, login, seat_id, document
	  FROM iam_people WHERE id = ?`

// readPersonRow fills one person's facts, opening the document for the two
// that live in it.
func readPersonRow(ctx context.Context, tx *sql.Tx, person string,
	out *session.PersonRow) error {

	if person == "" {
		return nil
	}
	var (
		stage, login, seat string
		document           []byte
	)
	err := tx.QueryRowContext(ctx, personStandingStatement, person).
		Scan(&stage, &login, &seat, &document)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read the person row: %w", err)
	}
	doc, err := DecodePerson(document)
	if err != nil {
		// THE UNKNOWN ARM, not "no such person": answering 401 to a newer
		// peer's row would sign out everybody enrolled since the upgrade began.
		return fmt.Errorf("iamdomain: open person %q: %w", person, err)
	}
	out.Found = true
	// THE COLUMN, NOT THE DOCUMENT: every predicate here reads the column,
	// and the status op moves a stage without a new document, so the
	// document can still say `active` for a suspended person.
	out.Stage = iam.Stage(stage)
	out.Login = login
	out.Grants = doc.Grants
	out.Seat = seat
	return nil
}

// readEpoch fills a session subject's revocation epoch, in the person's
// snapshot for [Reader.Resolve]'s reason.
//
// READ WHETHER OR NOT A PERSON ROW EXISTS: a session exchanged from a Tier A
// token names the token's login, which nothing enrols, and "sign out
// everywhere" from it bumps the epoch under that login.
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
		// NO ROW IS EPOCH ZERO, a real value: nobody has revoked anything for
		// this subject, and its bearers carry zero too.
		return nil
	case err != nil:
		return fmt.Errorf("iamdomain: read the revocation epoch: %w", err)
	}
	*out = uint64(epoch)
	return nil
}

// MachineToken answers everything one presented machine token is checked
// against, in ONE snapshot: the credential row, its owner now, the owner's
// epoch, the company's session generation, and how far this node can vouch.
//
// The request guard's machine-token arm reads it on a data node, which answers
// a node holding no identity rows from it too (internal/estate withholds the
// verifier). One snapshot for [Reader.Resolve]'s reason; what the rows mean is
// [credential.CheckToken]'s. ONE KEYED READ, never a scan: the token carries
// its id in the clear and is presented on every pipeline request. AN ERROR IS
// ALWAYS THE UNKNOWN ARM: answering a newer peer's row "no such token" would
// break a pipeline's credential for the length of an upgrade.
func (r *Reader) MachineToken(ctx context.Context, id string) (credential.TokenRow, error) {
	out := credential.TokenRow{Applied: uint64(r.reached().Packed())}
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
			// ABSENT IS A FINDING (the mint position makes it "gone" or "not
			// yet"), and every deferral covers it: no bucket is known.
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
		// THE OWNER'S BUCKET, in the snapshot of the row that named them.
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
	err := tx.QueryRowContext(ctx, personStandingStatement, id).
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
	// THE COLUMN, for [readPersonRow]'s reason: else a suspended
	// owner's token is served.
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
		// NEVER INVALIDATED is generation zero, which every bearer carries:
		// a company's ordinary state, not a missing row.
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
// NOT A PERSON DOCUMENT: a sign-in runs unauthenticated, and its refusal may
// disclose nothing.
type Sighting struct {
	ID          string
	Kind        iam.Kind
	Stage       iam.Stage
	Login       string
	Credentials []Credential
	Grants      []iam.Grant
	Seat        string
}

// PersonByLogin resolves a login to the person who holds it.
//
// A login nobody holds answers the zero [Sighting] and a nil error, NOT
// [ErrNotFound]: the enumeration rule. "No such login" and "wrong password"
// must not differ in return, timing or logs; internal/iam/credential owns the
// timing, this the shape, and the caller spends a decoy's derivation on the
// zero value. An error is still the unknown arm.
func (r *Reader) PersonByLogin(ctx context.Context, login string) (Sighting, error) {
	return r.sighting(ctx, sightingByLogin, login)
}

// PersonByEmailBlind resolves a keyed address blind to its holder, on
// [Reader.PersonByLogin]'s three answers and for its reason.
//
// THE BLIND, not the address: the read is unauthenticated and an address is
// personal data. A surface computes it with iam.NormalizeEmail and this
// domain's blind, which must agree with the party registry's fold or one
// address would reach a seat and a different person.
func (r *Reader) PersonByEmailBlind(ctx context.Context, blind string) (Sighting, error) {
	return r.sighting(ctx, sightingByBlind, blind)
}

// sightingQuery is one of the two sightings: the holders of one value — LIMIT
// 2, because two rows are the unknown arm ([sightingIn]) — and what that value
// is called in a refusal a person reads.
type sightingQuery struct {
	statement, value string
}

// The two sightings, each a seek of its value's own plain index.
var (
	sightingByLogin = sightingQuery{value: "login", statement: `
		SELECT id, stage, login, seat_id, document
		  FROM iam_people WHERE login = ? LIMIT 2`}
	sightingByBlind = sightingQuery{value: "address", statement: `
		SELECT id, stage, login, seat_id, document
		  FROM iam_people WHERE email_blind = ? LIMIT 2`}
)

// sighting is the one lookup every spelling shares.
func (r *Reader) sighting(ctx context.Context, by sightingQuery, token string) (Sighting, error) {
	if token == "" {
		return Sighting{}, nil
	}
	var out Sighting
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		return sightingIn(ctx, tx, by, token, &out)
	})
	if err != nil {
		return Sighting{}, err
	}
	return out, nil
}

// sightingIn resolves one person row inside a read already open.
//
// THE STATEMENT IS ONE OF THIS PACKAGE'S TWO ([sightingByLogin],
// [sightingByBlind]), each spelled whole, never a caller's: a passed-in column
// is one parameter from selecting on what a request named.
//
// TWO ROWS HOLDING ONE VALUE ARE THE UNKNOWN ARM. The directory decides each
// value on one subject, but a node that retained the record freeing a login
// and applied the later one reusing it holds both, as does a restore copying
// duplicates back. It answers an error ("ask another node"; a sign-in's
// ordinary refusal), never an arbitrary pick, which would sign somebody in as
// somebody else.
func sightingIn(ctx context.Context, tx *sql.Tx, by sightingQuery, token string,
	out *Sighting) error {

	rows, err := tx.QueryContext(ctx, by.statement, token)
	if err != nil {
		return fmt.Errorf("iamdomain: resolve a person: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var (
		found                  int
		id, stage, login, seat string
		document               []byte
	)
	for rows.Next() {
		found++
		if found > 1 {
			return fmt.Errorf("%w: this node has two people with the same "+
				"%s. Try again on another node",
				statelog.ErrUnavailable, by.value)
		}
		if err = rows.Scan(&id, &stage, &login, &seat, &document); err != nil {
			return fmt.Errorf("iamdomain: resolve a person: %w", err)
		}
	}
	if err = rows.Err(); err != nil {
		return fmt.Errorf("iamdomain: resolve a person: %w", err)
	}
	if found == 0 {
		// NOBODY: the zero value is the answer ([Reader.PersonByLogin]).
		*out = Sighting{}
		return nil
	}
	out.ID = id
	doc, err := DecodePerson(document)
	if err != nil {
		return fmt.Errorf("iamdomain: open person %q: %w", out.ID, err)
	}
	out.Kind = doc.Kind
	// THE COLUMN, for [readPersonRow]'s reason: else a suspended person
	// signs in.
	out.Stage = iam.Stage(stage)
	out.Login = login
	out.Credentials = doc.Credentials
	out.Grants = doc.Grants
	out.Seat = seat
	return nil
}

// AnyPerson reports whether this estate holds a PERSON, anybody who signs in
// with a password: `/health`'s `identity`, which tells a fresh deployment's
// operator to invite the first person. A MACHINE does not count (it signs
// nobody in), a REMOVED person is a tombstone, a SUSPENDED one is somebody. A
// COUNT, never a listing: an unauthenticated route asks it.
//
// "Somebody" is a fact; "nobody" is an absence [provedAbsent] proves against
// end, the identity log's last sequence read BEFORE this call (the engine's
// IdentityLogEnd): rows behind it ([CoversLog]) or retaining a record in any
// bucket would tell a newly joined data node's /health that a company with
// people has none. Unproved, it is the unknown arm, wrapping
// [statelog.ErrUnavailable] and [ErrNotCurrent], which clears on its own.
func (r *Reader) AnyPerson(ctx context.Context, end uint64) (bool, error) {
	var held bool
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, anyPersonStatement,
			string(iam.KindPerson)).
			Scan(&held); err != nil {
			return fmt.Errorf("iamdomain: read whether a person is enrolled: %w", err)
		}
		if held {
			return nil
		}
		return provedAbsent(ctx, tx, end, "the company's first person")
	})
	return held, err
}

// anyPersonStatement is whether any row is a person's.
const anyPersonStatement = `SELECT EXISTS(SELECT 1 FROM iam_people WHERE kind = ?)`

// provedAbsent is the proof a missing row needs before it reads as "none":
// these rows applied every record the log held at end and retain none they
// could not apply. what names the missing record in the operator's error.
//
// THE WHOLE DEFERRAL INDEX, never one bucket's: nothing says whose bucket a
// missing row's record would be filed under.
func provedAbsent(ctx context.Context, tx *sql.Tx, end uint64, what string) error {
	prefix, err := statelog.PrefixIn(ctx, tx, Domain{})
	if err != nil {
		return fmt.Errorf("iamdomain: read how much of the log these rows "+
			"hold: %w", err)
	}
	if behind := CoversLog(prefix, end); behind != nil {
		return fmt.Errorf("%w: %w, so %s may just not have "+
			"reached this node yet", statelog.ErrUnavailable, behind,
			what)
	}
	retained, err := deferredFor(ctx, tx, "")
	if err != nil {
		return err
	}
	if retained {
		return fmt.Errorf("%w: %w. This node has a change it couldn't "+
			"apply, which may be %s", statelog.ErrUnavailable, ErrNotCurrent,
			what)
	}
	return nil
}

// vouches is [provedAbsent]'s proof as a reading: whether these rows hold every
// record the log held at end and retain none, and an error only where that
// could not be read.
func vouches(ctx context.Context, tx *sql.Tx, end uint64) (bool, error) {
	switch err := provedAbsent(ctx, tx, end, "a link"); {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrNotCurrent):
		return false, nil
	default:
		return false, err
	}
}

// ErrNotCurrent reports a snapshot of the identity estate that cannot vouch for
// an absence: it has not applied every record the log held when asked, being
// behind or having RETAINED one.
//
// A SENTINEL, because the question it answers turns on a missing row (whether
// a blind was ever derived, [Reader.HoldsBlinds]), which on such a snapshot
// proves nothing.
var ErrNotCurrent = errors.New("this node's identity data isn't up to date")

// CoversLog proves, from one snapshot's prefix and the log's last sequence read
// BEFORE that snapshot, that the snapshot's rows hold every record the log held
// — or says, wrapping [ErrNotCurrent], why they do not.
//
// END FIRST, so a record landing between can only make the answer "not
// current". AGAINST WHAT THE SNAPSHOT APPLIED, never its checkpoint: that moves
// past a RETAINED record (an unreadable record version, a keyring key this
// node lacks mid-rotation, any later record in a bucket one covers), whose
// rows this node reads as rows nobody wrote.
func CoversLog(prefix statelog.Prefix, end uint64) error {
	if prefix.Applied().Seq >= end {
		return nil
	}
	if prefix.Retains && prefix.Retained.Position.Seq <= end {
		return fmt.Errorf("%w: it can't apply the change at %s (record "+
			"version %d). It's from a newer build, or signed with a key this "+
			"node doesn't have",
			ErrNotCurrent, prefix.Retained.Position, prefix.Retained.Version)
	}
	return fmt.Errorf("%w: it has applied %d of %d changes",
		ErrNotCurrent, prefix.Applied().Seq, end)
}

// anyBlindStatement is whether any row holds an address blind: a person's, or
// any invitation's, since every invitation is to an address.
const anyBlindStatement = `
	SELECT EXISTS(SELECT 1 FROM iam_people WHERE email_blind != '')
	    OR EXISTS(SELECT 1 FROM iam_invites)`

// HoldsBlinds reports whether any row this node holds carries a value derived
// from the company's blind-index key: a person's address, or an invitation's.
//
// It decides what a missing key forces: on an estate that never held a blind,
// mint the first; on one that did, the old key was DELETED, and a new one would
// orphan every address and let a second person take each, since the directory
// compares addresses by blind. "None" is an absence proved against end, the
// log's last sequence read BEFORE this call ([provedAbsent]): (true, nil) is a
// blind held, (false, nil) none on rows holding the whole log, and an error
// wrapping [ErrNotCurrent] and [statelog.ErrUnavailable] (another node may
// vouch) refuses the mint as an unreadable store does.
func (r *Reader) HoldsBlinds(ctx context.Context, end uint64) (bool, error) {
	var held bool
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		// [Reader.AnyPerson]'s proof: "no blind here" is an absence, and its
		// [statelog.ErrUnavailable] serves a node behind its own log as "ask
		// again", never a fault.
		if err := provedAbsent(ctx, tx, end, "a blinded row"); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRowContext(ctx, anyBlindStatement).
			Scan(&count); err != nil {
			return fmt.Errorf("iamdomain: look for a blinded row: %w", err)
		}
		held = count != 0
		return nil
	})
	return held, err
}

// withTx runs one read in one transaction, which makes a multi-row answer a
// snapshot rather than a sequence.
//
// A READ TRANSACTION, the only kind a [store.ReplicatedReader] can open: a
// write transaction takes the write lock at BEGIN, queuing behind and blocking
// applier commits, and the estate is derived state ONLY its applier writes; the
// type rules it out before internal/store's applier-exclusivity walk would.
// MARKED AS IDENTITY WORK ([store.Identity]), to spend the connection the store
// holds back for it: from the ordinary pool, a socket storm would queue the
// lookup that decides those very requests behind them. It belongs there as a
// keyed lookup, never a scan, asked before a request decides anything.
func (r *Reader) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	// NO GUARD FOR A CLOSED ESTATE: Read answers [store.ErrNoEstate] while no
	// file is open (an adoption between rename and reopen), the same answer.
	if err := r.db.Read(store.Identity(ctx), fn); err != nil {
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
// ITS OWN TYPE beside the [Invitation] document: the document is what a writer
// states, this what a reader establishes, with the applier's own columns (the
// blind it is filed under, whether it is spent). NO ADDRESS IN THE CLEAR, only
// the sealed bytes and the blind: an unauthenticated link holder reads it. AND
// NO SECRET, only its verifier, asked through [InvitationRow.Admits].
type InvitationRow struct {
	ID         string
	Blind      string
	Sealed     string
	InvitedBy  string
	Grants     []iam.Grant
	CreatedAt  time.Time
	ExpiresAt  time.Time
	RedeemedAt time.Time
	Person     string

	// Verifier is [Invitation.Verifier]: what the link's secret is checked
	// against, and empty for an invitation nothing can redeem.
	Verifier string

	// Seat is the seat redeeming this binds and an open invitation holds: the
	// COLUMN a hold is decided on ([heldByInvitation]), written from
	// [Invitation.Seat]. Every invitation names one ([Writer.Invite]).
	Seat string

	// Vouched is [ResetRow.Vouched], for [Reader.InvitationByID]: false on a
	// listing, which does not ask.
	Vouched bool
}

// Admits reports whether a secret presented with this invitation's id is the
// one its link carries.
//
// CONSTANT-TIME over the verifiers, so a partial match is no timing; FALSE for
// an invitation with no verifier, which its id alone must never redeem.
func (i InvitationRow) Admits(secret string) bool {
	return invitationAdmits(i.Verifier, secret)
}

// invitationAdmits is [InvitationRow.Admits]' comparison and [Writer.Redeem]'s:
// one function, so the route and the record cannot disagree about a secret.
func invitationAdmits(verifier, secret string) bool {
	if verifier == "" || secret == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(InvitationVerifier(secret)),
		[]byte(verifier)) == 1
}

// Spent reports whether this invitation can NO LONGER be redeemed, at now:
// [openInvitation]'s complement, which the directory holds a value against and
// the listing reads as outstanding.
//
// ONE PREDICATE: redeemed and expired share one remedy and one answer, or a
// surface would tell somebody whose link aged out it was "already used". NO
// EXPIRY IS SPENT, never open for ever: no writer forms one, and the retention
// sweep collects it like an expired one ([Applier.applySweep]).
func (i InvitationRow) Spent(now time.Time) bool {
	return !i.RedeemedAt.IsZero() || !now.Before(i.ExpiresAt)
}

// invitationByIDStatement is one invitation as [scanInvitation] reads it, by
// its key.
const invitationByIDStatement = `SELECT ` + invitationColumns + ` FROM iam_invites WHERE id = ?`

// InvitationByID resolves one invitation, on this file's three answers: the
// zero value for one nobody issued, and an error for a node that could not
// tell — with whether these rows vouch for it at end, the identity log's last
// sequence read BEFORE this snapshot ([ResetRow.Vouched], [CoversLog]).
func (r *Reader) InvitationByID(ctx context.Context, id string, end uint64) (
	InvitationRow, error) {

	if id == "" {
		return InvitationRow{Vouched: true}, nil
	}
	var out InvitationRow
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = scanInvitation(tx.QueryRowContext(ctx,
			invitationByIDStatement, id).Scan)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out = InvitationRow{}
		case err != nil:
			return err
		}
		out.Vouched, err = vouches(ctx, tx, end)
		return err
	})
	if err != nil {
		return InvitationRow{}, err
	}
	return out, nil
}

// ResetRow is one password reset link as the link's own screen resolves it:
// whose it is, whether it still opens, and the verifier its secret is checked
// against — never the secret, which the estate does not hold.
type ResetRow struct {
	ID       string
	PersonID string
	Login    string
	Kind     iam.Kind
	Stage    iam.Stage

	// Seat is the seat the person is bound to: the spend is theirs, and its
	// record names them as everything else they do does. A person always
	// holds one (ADR-0032).
	Seat string

	// First says the link sets the person's FIRST password: they hold no
	// password credential ([Writer.Create]'s first link, or a reset issued to
	// somebody who never set one). DERIVED from their credentials, never stored
	// on the link, so the two cannot disagree. Only wording reads it: the spend
	// is the same record either way.
	First bool

	Verifier  string
	ExpiresAt time.Time
	RevokedAt time.Time

	// Ended is a link a counter ended since its issue ([Credential.EndedBy]):
	// its person suspended, signed out everywhere or their sessions ended by
	// an administrator, or the company's invalidated after a restore.
	Ended bool

	// Vouched is whether the rows held every record the identity log held when
	// asked and retain none they could not apply ([CoversLog]). Only then is a
	// link they lack, or cannot open, one that does not open: otherwise it may
	// be in an unapplied record, and answered dead it tells its holder to ask
	// for another and counts them as a guesser.
	Vouched bool
}

// Opens reports whether a secret presented with this link's id is the link's,
// and the link still sets a password at now: not revoked (spending revokes it),
// not ended by a counter, not expired, and its person one a reset may reach
// ([ResetStages]).
//
// ONE PREDICATE, for [InvitationRow.Spent]'s reason, and because told apart
// the ways a link fails would tell a secret-guesser which ids exist.
func (r ResetRow) Opens(secret string, now time.Time) bool {
	return r.ID != "" && r.Kind == iam.KindPerson &&
		slices.Contains(ResetStages, r.Stage) &&
		r.RevokedAt.IsZero() && !r.Ended && now.Before(r.ExpiresAt) &&
		credential.VerifyReset(r.Verifier, r.ID, secret)
}

// ResetOf is the reset link id names among a person's credentials as a decide
// read holds them at counters, or the zero row. A spend's [PasswordSet.Check]
// judges the link by it in the record's own snapshot, with the [ResetRow.Opens]
// the link's screen asks, as does a create's retry of its first password link
// ([Writer.firstLinkAs]).
func ResetOf(person Person, id string, counters Counters) ResetRow {
	for _, c := range person.Credentials {
		if c.ID == id && c.Method == MethodReset {
			return ResetRow{ID: id, Kind: person.Kind, Stage: person.Stage,
				Verifier: c.Verifier, ExpiresAt: c.ExpiresAt,
				RevokedAt: c.RevokedAt, Ended: c.EndedBy(counters),
				First: !holdsPassword(person.Credentials)}
		}
	}
	return ResetRow{}
}

// holdsPassword reports whether a credential set holds a password — any, a
// revoked one included, because a person who once set a password is not
// setting their first.
func holdsPassword(credentials []Credential) bool {
	for _, c := range credentials {
		if c.Method == MethodPassword {
			return true
		}
	}
	return false
}

// ResetStages are the stages a password reset reaches: everybody enrolled and
// not turned away, an invited person who never finished enrolling included,
// and a created person's first link ([Writer.Create]). A suspended or retired
// person is reactivated first: a link would hand back an account an
// administrator deliberately stopped.
var ResetStages = []iam.Stage{iam.StageInvited, iam.StageEnrolling, iam.StageActive}

// resetLinkStatement is one reset link by its credential's key, with the
// person it is for joined by theirs.
const resetLinkStatement = `
	SELECT c.person_id, c.verifier, c.expires_at, c.revoked_at,
	       c.document, p.login, p.kind, p.stage, p.seat_id
	  FROM iam_credentials c
	  JOIN iam_people p ON p.id = c.person_id
	 WHERE c.id = ? AND c.method = ?`

// ResetByID resolves one reset link by its credential id, on this file's three
// answers: the zero value for an id that is not a reset link this estate
// holds, and an error for a node that could not read — with whether these
// rows vouch for what they hold at end, the identity log's last sequence read
// BEFORE this snapshot ([ResetRow.Vouched], [CoversLog]).
func (r *Reader) ResetByID(ctx context.Context, id string, end uint64) (
	ResetRow, error) {

	if id == "" {
		return ResetRow{Vouched: true}, nil
	}
	var out ResetRow
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		var (
			verifier, document []byte
			expires, revoked   int64
			kind, stage        string
		)
		err := tx.QueryRowContext(ctx, resetLinkStatement, id, string(MethodReset)).
			Scan(&out.PersonID, &verifier, &expires, &revoked, &document,
				&out.Login, &kind, &stage, &out.Seat)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			out = ResetRow{}
			out.Vouched, err = vouches(ctx, tx, end)
			return err
		case err != nil:
			return fmt.Errorf("iamdomain: read a reset link: %w", err)
		}
		if out.Vouched, err = vouches(ctx, tx, end); err != nil {
			return err
		}
		held, err := DecodeCredential(document)
		if err != nil {
			return fmt.Errorf("iamdomain: open reset link %q: %w", id, err)
		}
		// THE COUNTERS IN THE ROW'S SNAPSHOT, as the spend's check reads them
		// in its record's.
		counters, err := countersOf(ctx, tx, out.PersonID)
		if err != nil {
			return err
		}
		// AND WHETHER IT SETS THEIR FIRST PASSWORD, from the same rows.
		var holds bool
		if err = tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM iam_credentials
			               WHERE person_id = ? AND method = ?)`,
			out.PersonID, string(MethodPassword)).Scan(&holds); err != nil {
			return fmt.Errorf("iamdomain: read whether person %s holds a "+
				"password: %w", out.PersonID, err)
		}
		out.First = !holds
		out.ID = id
		out.Verifier = string(verifier)
		out.ExpiresAt = fromMillis(expires)
		out.RevokedAt = fromMillis(revoked)
		out.Ended = held.EndedBy(counters)
		out.Kind, out.Stage = iam.Kind(kind), iam.Stage(stage)
		return nil
	})
	if err != nil {
		return ResetRow{}, err
	}
	return out, nil
}

// invitationColumns are what [scanInvitation] reads, in its order.
const invitationColumns = `id, email_blind, seat_id, created_at, expires_at,
	redeemed_at, person_id, document`

// scanInvitation reads one invitation row, opening the document for the halves
// no column carries.
func scanInvitation(scan func(...any) error) (InvitationRow, error) {
	var (
		out                        InvitationRow
		document                   []byte
		created, expires, redeemed int64
	)
	if err := scan(&out.ID, &out.Blind, &out.Seat, &created, &expires,
		&redeemed, &out.Person, &document); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return InvitationRow{}, err
		}
		return InvitationRow{}, fmt.Errorf("iamdomain: read an invitation: %w", err)
	}
	doc, err := DecodeInvitation(document)
	if err != nil {
		return InvitationRow{}, fmt.Errorf("iamdomain: open invitation %q: %w",
			out.ID, err)
	}
	out.Sealed = doc.Sealed
	out.InvitedBy = doc.InvitedBy
	out.Grants = doc.Grants
	out.Verifier = doc.Verifier
	out.CreatedAt = fromMillis(created)
	out.ExpiresAt = fromMillis(expires)
	out.RedeemedAt = fromMillis(redeemed)
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

// SessionStanding is who holds one session (empty for a lineage this node
// does not hold) and whether its rows hold it LIVE, in ONE snapshot.
//
// Unlike [Reader.Resolve], which takes the person a bearer names, this asks
// whose a TYPED lineage is. A lineage is no secret (cookies, proxy logs,
// screenshots), so comparing this owner with the caller this node resolved is
// what stops anybody ending anybody's session. LIVE is [SessionRecord.Live],
// the listing's reading, because ending a session claims it was live until
// now. An absent row is never live, and is nobody's only where these rows
// cover end, the log's last sequence read BEFORE this snapshot ([CoversLog]):
// otherwise [ErrNotCurrent] under [statelog.ErrUnavailable] ("ask again"),
// never "ended" for a session whose start this node has not applied.
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
		current, err := epochOf(ctx, tx, owner)
		if err != nil {
			return err
		}
		// THE LISTING'S OWN READING ([SessionRecord.Live]), so the
		// sessions a screen offers to end are the ones this answers live.
		live = SessionRecord{
			EndedAt:   fromMillis(ended),
			ExpiresAt: fromMillis(expires),
			Superseded: sessionSuperseded(uint64(start), uint64(epoch),
				invalidated, current),
		}.Live(now)
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
// A FACT, NOT A VERDICT: which stages a seat's contact identities may reach
// is internal/notify's rule; this says who holds the seat at what stage.
type SeatHolder struct {
	// Seat is the seat's handle, which is immutable (ADR-0013): a reader
	// finds the seat by it in the running organisation.
	Seat string

	// Person is who holds it, or who last held it before a removal.
	Person string

	// Stage is the bound person's stage, from the COLUMN — the one every
	// predicate here reads. Empty on a removal, which has no row left to
	// have a stage.
	Stage iam.Stage

	// Removed marks a seat whose holder was REMOVED while holding it, and
	// that nobody has been bound to since.
	//
	// The removal released the seat, which would then route as the chart
	// declares: removing somebody would route MORE than suspending them, the
	// contact map still naming the leaver's accounts. The tombstone outlives
	// every horizon here. IT SPEAKS ONLY FOR THE BINDING IT RELEASED: the first
	// bind after it stamps `iam_removed.seat_rebound_by`, so a successor's later
	// release (or a service account's unbind) hands the seat to the chart, not
	// back to the tombstone. All three name the seat by its handle.
	Removed bool
}

// Requester is ONE seat's holder as a seat's turn composes the person it acts
// for (internal/engine's turnRequesters, the requester ceiling): the
// [SeatHolder] answer and the holder's own grants ([Person.Grants]), names this
// build does not know kept, since the asking node decides which it knows. A
// removal carries none.
//
// THE GRANTS AND NOTHING ELSE of the person, never a credential, verifier or
// sealed field: the answer crosses to the asking node (estate's
// identity.requester), which decides the ceiling on grants alone.
type Requester struct {
	SeatHolder

	// Grants are the bound person's permissions as stored, admin included:
	// the asker composes the apps they use from them (iam.Expand) and never
	// carries admin into a ceiling. Nil for a removal.
	Grants []iam.Grant
}

// Requester is who holds ONE seat, the [Reader.SeatHolders] answer for that
// seat alone in one snapshot, with the holder's grants, and whether anybody
// holds it: by the same rule, the bound person (lowest id where rows name
// several), else the removal no bind has stamped since (lowest person id,
// [SeatHolder.Removed]), else nobody (false), a seat the chart speaks for.
//
// TWO SEEKS AT MOST, whatever the directory's size: `iam_people_seat_lookup_idx`
// (its row carries the document, so grants cost one decode), then only where
// unbound `iam_removed_seat_idx` (replicated migration 0122). Every tool call
// deciding an app or relation pays it (BenchmarkRequester). An error is
// UNKNOWN, never nobody. An empty seat is refused: every unbound row holds it.
func (r *Reader) Requester(ctx context.Context, seat string) (Requester, bool, error) {
	if seat == "" {
		return Requester{}, false, fmt.Errorf("%w: a requester is asked of no seat", ErrInvalid)
	}
	var (
		out  Requester
		held bool
	)
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		out, held = Requester{}, false
		// EVERY ROW ON THE SEAT, THE LOWEST ID KEPT HERE, not by ORDER BY id:
		// under a LIMIT that walks the primary key over every person (`SCAN
		// iam_people USING INDEX sqlite_autoindex_…`), where the bare equality
		// seeks the seat index. A second row is a retained record's residue.
		rows, err := tx.QueryContext(ctx, seatBindingStatement, seat)
		if err != nil {
			return fmt.Errorf("iamdomain: read seat %s's binding: %w", seat, err)
		}
		var document []byte
		for rows.Next() {
			var (
				person, stage string
				doc           []byte
			)
			if err = rows.Scan(&person, &stage, &doc); err != nil {
				_ = rows.Close()
				return fmt.Errorf("iamdomain: read seat %s's binding: %w", seat, err)
			}
			if !held || person < out.Person {
				out = Requester{SeatHolder: SeatHolder{Seat: seat, Person: person, Stage: iam.Stage(stage)}}
				held, document = true, doc
			}
		}
		if err = rows.Close(); err != nil {
			return fmt.Errorf("iamdomain: read seat %s's binding: %w", seat, err)
		}
		if err = rows.Err(); err != nil {
			return fmt.Errorf("iamdomain: read seat %s's binding: %w", seat, err)
		}
		if held {
			// THE HOLDER'S GRANTS: an unreadable document is no answer, never a
			// person holding nothing, whom the asker would read as using no app.
			var p Person
			if p, err = DecodePerson(document); err != nil {
				return fmt.Errorf("iamdomain: open seat %s's holder %s: %w", seat, out.Person, err)
			}
			out.Grants = p.Grants
			return nil
		}
		// A REMOVAL'S SEAT, UNTIL IT IS BOUND AGAIN: [Reader.SeatHolders]'
		// rule, read only where no current holder speaks for the seat.
		var person string
		err = tx.QueryRowContext(ctx, seatRemovedHolderStatement, seat).Scan(&person)
		switch {
		case err == nil:
			out = Requester{SeatHolder: SeatHolder{Seat: seat, Person: person, Removed: true}}
			held = true
			return nil
		case errors.Is(err, sql.ErrNoRows):
			return nil
		}
		return fmt.Errorf("iamdomain: read seat %s's removed holder: %w", seat, err)
	})
	if err != nil {
		return Requester{}, false, err
	}
	return out, held, nil
}

// The statements [Reader.SeatHolders] reads in one snapshot (every binding in
// seat order; every removal no bind since has stamped and no current holder
// holds) and [Reader.Requester]'s two seeks for one seat.
//
// THE REMOVAL'S SEAT IS ITS OWN COLUMN (`iam_removed.seat_id`, replicated
// migration 0122), written by the apply and re-derived from the tombstone's
// claims ([Applier.Rederive]): a json_extract of the permanent tombstones'
// claims is a value no index reaches.
const (
	seatHoldersStatement = `
		SELECT seat_id, id, stage FROM iam_people
		 WHERE seat_id != ''
		 ORDER BY seat_id, id`
	removedHoldersStatement = `
		SELECT r.seat_id, r.person_id
		  FROM iam_removed r
		 WHERE r.seat_id != ''
		   AND r.seat_rebound_by = ''
		   AND NOT EXISTS (
		       SELECT 1 FROM iam_people p WHERE p.seat_id = r.seat_id)
		 ORDER BY r.seat_id, r.person_id`
	seatBindingStatement       = `SELECT id, stage, document FROM iam_people WHERE seat_id = ?`
	seatRemovedHolderStatement = `
		SELECT person_id FROM iam_removed
		 WHERE seat_id = ? AND seat_rebound_by = ''
		 ORDER BY person_id LIMIT 1`
)

// SeatHolders is every seat binding this node's directory holds, read in ONE
// snapshot so a bind and a removal between two reads cannot make a set that
// never existed.
//
// Three-valued: an error is UNKNOWN, never an empty answer, which is real
// ("nobody is bound") and would hand every suspended person's seat to the
// chart. Both halves are read whole, the removals one row per person ever
// removed, because the one caller rebuilds a registry whole ([Applier]'s
// directory listener says why not a diff); one seat is [Reader.Requester]'s.
func (r *Reader) SeatHolders(ctx context.Context) ([]SeatHolder, error) {
	var out []SeatHolder
	err := r.withTx(ctx, func(tx *sql.Tx) error {
		out = out[:0]
		rows, err := tx.QueryContext(ctx, seatHoldersStatement)
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

		// A REMOVAL'S SEAT, UNTIL IT IS BOUND AGAIN: the first bind after the
		// removal stamps `seat_rebound_by` for good, and NOT EXISTS lets a
		// current holder outrank a tombstone no bind has stamped yet.
		removed, err := tx.QueryContext(ctx, removedHoldersStatement)
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
