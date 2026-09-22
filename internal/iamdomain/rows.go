package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE THREE SEAMS A WRITE AUTHORITY IS BUILT FROM, and the one thing each is
// for.
//
// [statelog.NewPublisher] refuses a nil Rows, Fence or Gates by name, which is
// what makes a domain with a half-built write authority a boot failure rather
// than a node that appends records the fleet drops one at a time. This file is
// the identity estate's three.

// NewRows builds the framework's row seam over this domain's guards.
func NewRows(db *store.DB) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, iamGuards)
}

// iamGuards answers the two object-level facts a first write needs: whether a
// tombstone forbids this subject for ever, and whether a row already guards it.
//
// ONE KIND HAS THEM, and that is the whole shape of this domain rather than an
// omission. A PERSON is the only object here that can be removed; every other
// kind is an ADDRESS — an email blind, a login, a seat binding, a session
// lineage — and an address is not removed, it is RELEASED, which leaves it
// claimable again by design. A claim's own subject keeps its arbitration
// anchor either way, so the next claim on it is an ordinary conditional write
// rather than a create at zero.
//
// THE CLAIM SUBJECTS ANSWER FALSE FOR THE GUARD TOO, and that is deliberate:
// their whole mechanism is the create-at-zero the broker arbitrates, so a row
// guard here would be a second opinion about a claim the broker has already
// decided.
func iamGuards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (
	deleted, guard bool, err error) {

	if ObjectKind(subj.Kind) != KindPerson {
		return false, false, nil
	}
	var removed, present int
	err = tx.QueryRowContext(ctx, `
		SELECT
			(SELECT COUNT(*) FROM iam_removed WHERE person_id = ?),
			(SELECT COUNT(*) FROM iam_people  WHERE id = ?)`,
		subj.ID, subj.ID).Scan(&removed, &present)
	if err != nil {
		return false, false, fmt.Errorf("iamdomain: read the guards on person "+
			"%s: %w", subj.ID, err)
	}
	return removed > 0, present > 0, nil
}

// ReadScope is the closure a READ is about.
//
// It is the same alphabet a record's own scope resolves into, which is what
// makes the coverage probe one comparison rather than a translation between
// two vocabularies.
//
// A READ ABOUT ONE PERSON IS THEIR BUCKET. A read that names nobody — the
// directory, a duty's walk, a sweep's own survey — is the WHOLE ESTATE, not
// because it touches everything but because a read that cannot say who it is
// about is one every deferred record concerns, and the honest answer to "is
// this complete" is then "no".
func ReadScope(personIDs ...string) statelog.ScopeSet {
	if len(personIDs) == 0 {
		return statelog.ScopeSet{Paths: []string{RootPath()}}
	}
	return PeopleScope(personIDs...).Resolve(PersonSubject(personIDs[0]))
}

// ReadScopeForBucket is the closure a per-bucket duty or sweep is about.
//
// ITS OWN CONSTRUCTOR rather than a caller reaching for [PeopleScope], because
// a sweep holds a BUCKET and not a person: going through a person id would
// mean inventing one whose hash lands where the caller already is, which is a
// derivation with no inverse and no reason to exist.
func ReadScopeForBucket(b Bucket) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{b.Path()}}
}

// Fence refuses a write this node must not make.
//
// THE SAME TWO PRICES ITS SIBLINGS PAY, because this domain installs the same
// gate: Evicted runs on every append and is answered from rows this node
// already has, and ClearForZero pays a coordination round trip because being
// wrong there is a LOST UPDATE rather than a duplicate.
//
// AND HERE THE LOST UPDATE IS A DUPLICATE IDENTITY. Every claim in this domain
// publishes at an expectation of ZERO — that is what makes the subject the
// uniqueness check — so ClearForZero is not an occasional path taken by one
// operation, it is on the hot path of every enrolment, every address change
// and every sign-in. A node below the trim floor cannot tell an address nobody
// has claimed from one whose claim has been trimmed beneath it, and guessing
// makes two people hold one address with nothing left able to notice.
type Fence struct {
	db     *store.DB
	nodeID string

	// Cursor is this node's committed position, and Floor the published
	// trim floor. Both are set by the engine after the runner exists,
	// because a fence built before its applier would compare against a
	// position that does not move.
	Cursor func() statelog.Position
	Floor  func(ctx context.Context) (uint64, error)
}

// NewFence builds it.
func NewFence(db *store.DB, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction, FROM ITS OWN APPLIED ROWS.
//
// Not from coordination, which is the point: a wedged coordination path is a
// precondition of an eviction being permitted at all, so the source that is
// still fresh in exactly that failure is this node's own replicated table.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f == nil || f.db == nil || f.nodeID == "" {
		return false, nil
	}
	var from, readmitted sql.NullInt64
	// THROUGH THE HANDLE, NOT ITS POOL. `DB.SQL()` answers a NIL pool on a
	// replicated estate that is not open — a legitimate, documented state
	// of that peer, since an adoption closes it between its rename and its
	// reopen — and a statement issued on it panics inside database/sql.
	err := f.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position
			FROM iam_evictions WHERE node_id = ?`, f.nodeID).
			Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		// UNREADABLE IS NOT "not evicted". A fence that failed open on a
		// store it could not read is a node appending records every
		// other node drops, collecting acknowledgements for writes that
		// happen nowhere.
		return false, fmt.Errorf("iamdomain: read this node's own eviction: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	return !readmitted.Valid || readmitted.Int64 < from.Int64, nil
}

// ClearForZero verifies, freshly, that publishing at an expectation of ZERO is
// safe from this node.
//
// A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is the opposite of the
// fail-open rule a delivery claim follows: failing open there costs a duplicate
// notification, which is recoverable, and failing open here costs a CLAIM that
// overwrites one another node already made — which is not, and which in this
// domain is a second person holding somebody's address.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	if f == nil {
		return nil
	}
	evicted, err := f.Evicted(ctx)
	if err != nil {
		return err
	}
	if evicted {
		return fmt.Errorf("iamdomain: this node is evicted, so a write at an "+
			"expectation of zero would be dropped by every peer: %w",
			statelog.ErrConflict)
	}
	if f.Floor == nil {
		// NO FLOOR SEAM IS A FENCE THAT CANNOT ANSWER, and a fence that
		// cannot answer refuses. A publisher built without one would
		// otherwise publish every claim at zero against a log whose
		// beginning it has never seen.
		return fmt.Errorf("iamdomain: no published trim floor is readable, so " +
			"this node cannot establish that an absent anchor means an " +
			"unclaimed address rather than a claim trimmed beneath it")
	}
	floor, err := f.Floor(ctx)
	if err != nil {
		return fmt.Errorf("iamdomain: read the published trim floor: %w — a "+
			"floor that cannot be read is not a floor that is low, and "+
			"publishing at zero on the guess is two people holding one "+
			"address", err)
	}
	// THE FLOOR IS THE FIRST SEQUENCE THE TRIM HAS NOT LICENSED REMOVING,
	// so a node that has consumed through the one before it has consumed
	// everything that may be gone — the same comparison every sibling
	// makes, for the same reason.
	if floor > cursor.Seq+1 {
		return fmt.Errorf("iamdomain: the trim may have removed everything "+
			"below %d and this node has consumed through %d, so an absent "+
			"anchor may be a claim trimmed beneath it rather than an address "+
			"nobody has taken: %w", floor, cursor.Seq, statelog.ErrUnavailable)
	}
	return nil
}

// Gates reads the same two gates [Applier.Gated] does, from OUTSIDE a record's
// own transaction: it is what a writer asks before it publishes, where the
// applier asks after the broker has committed.
type Gates struct{ db *store.DB }

// NewGates builds it.
func NewGates(db *store.DB) *Gates { return &Gates{db: db} }

// AdoptedAt is when this node's replicated estate last came from a peer.
//
// THE PUBLISHER ASKS BECAUSE AN ADOPTION BREAKS THE RESOLUTION. Resolving an
// ambiguous publish means looking for the record's own effect in this node's
// rows, and rows that arrived in somebody else's snapshot carry effects this
// node never applied — so a write that landed after the adoption's own instant
// is the only one that can be resolved from them.
func (g *Gates) AdoptedAt(ctx context.Context) (time.Time, bool, error) {
	if g == nil || g.db == nil {
		return time.Time{}, false, nil
	}
	return statelog.AdoptedAt(ctx, g.db)
}

// GatedAt reports the gate that dropped a record at p.
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position) (statelog.Reason, bool, error) {

	if g == nil || g.db == nil {
		return "", false, nil
	}
	// THE EVICTION GATE FIRST, and in a read of its own: the two gates
	// would otherwise be read at two instants, and a record could be
	// reported ungated by an eviction that had landed between them.
	if writer != "" {
		var from, readmitted sql.NullInt64
		err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRowContext(ctx, `
				SELECT from_position, readmitted_position
				FROM iam_evictions WHERE node_id = ?`, writer).
				Scan(&from, &readmitted)
		})
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return "", false, fmt.Errorf("iamdomain: read the eviction gate "+
				"for node %s: %w", writer, err)
		default:
			at := p.Packed()
			evicted := from.Valid && at > from.Int64
			back := readmitted.Valid && at >= readmitted.Int64
			if evicted && !back {
				return statelog.ReasonEvicted, true, nil
			}
		}
	}
	// AND THE REMOVAL GATE, WHICH IS KEYED ON THE PERSON AND NOT ON THE
	// SUBJECT. That is this domain's own shape: a record about a removed
	// person routinely arbitrates on an address, a login or a session
	// lineage rather than on the person, so a gate that read the subject
	// would let every claim record through and drop only the ones that
	// happened to be on the person's own subject.
	person, ok := gatedPerson(subj)
	if !ok {
		return "", false, nil
	}
	var author sql.NullString
	err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT record_id FROM iam_removed WHERE person_id = ?`,
			person).Scan(&author)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("iamdomain: read the removal gate for "+
			"person %s: %w", person, err)
	}
	if author.Valid && author.String == opID {
		return "", false, nil
	}
	return statelog.ReasonDeleted, true, nil
}

// gatedPerson is the person a subject's removal gate is keyed on, from the
// SUBJECT ALONE.
//
// ONLY THE PERSON'S OWN SUBJECT NAMES ONE. A claim arbitrates on an address, a
// login, a seat id or a session lineage, and which person each belongs to is a
// fact about a ROW rather than about the subject — so the writer-side gate
// answers "not gated" for them and the applier's own gate, which runs inside
// the record's transaction and can read the payload, is what drops those.
//
// THAT ASYMMETRY IS SAFE IN THE DIRECTION IT FAILS: the writer-side gate is an
// optimisation that saves a publish, and missing one costs a record the
// applier then drops on every node identically. The applier's gate is the
// correctness half, and it never has to guess.
func gatedPerson(subj statelog.Subject) (string, bool) {
	if ObjectKind(subj.Kind) != KindPerson || subj.ID == "" {
		return "", false
	}
	return subj.ID, true
}
