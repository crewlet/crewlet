package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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

	// Floor is the fleet's published trim floor at a generation and Ends
	// the log's two ends, read live, and Committed this node's applier's
	// live checkpoint — the three reads [statelog.ZeroFence] takes beside
	// the eviction. The cursor the floor is compared against is passed per
	// call: the checkpoint the write's own snapshot read
	// ([statelog.Snap.Checkpoint]), never the applier's live position,
	// because the floor theorem concludes that a trimmed record is already
	// in the rows the DECISION was made from, and a live position that has
	// moved past it since clears a claim that never saw it. Set by the
	// engine after the runner exists, because a fence built before its
	// applier would compare against a position that does not move.
	Floor     func(ctx context.Context, generation uint32) (uint64, error)
	Ends      func(ctx context.Context) (statelog.LogEnds, error)
	Committed func() statelog.Position
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
	// THE NEGATION OF [Domain.Evictions]' `Back`, spelled once for both, so
	// the trim and this node's own fence can never disagree about whether
	// it is back.
	return !back(readmitted, from.Int64), nil
}

// back reports whether an eviction row's readmission has taken the node back:
// a readmission ABOVE the eviction it answers. One comparison for the fence,
// the trim's listing and a retry's standing, so the three never disagree.
func back(readmitted sql.NullInt64, from int64) bool {
	return readmitted.Valid && readmitted.Int64 > from
}

// ClearForZero verifies, freshly, that publishing at an expectation of ZERO is
// safe from this node.
//
// A READ THAT ANSWERS UNKNOWN MUST REFUSE, which is the opposite of the
// fail-open rule a delivery claim follows: failing open there costs a duplicate
// notification, which is recoverable, and failing open here costs a CLAIM that
// overwrites one another node already made — which is not, and which in this
// domain is a second person holding somebody's address.
//
// THE CHECK IS [statelog.ZeroFence], the one every sibling's fence runs: this
// supplies the four reads. Written out here it refused an evicted node with
// [statelog.ErrConflict], which a caller reads as a colleague editing and
// retries into; compared the floor with the cursor alone, so it could not tell
// a node replaying records the log still holds from one below the LOG, which no
// replay brings back; and read a floor that named no generation, against a
// cursor that may be in another. Every refusal is now an
// [*statelog.Unavailable] naming its reason.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.ZeroFence{
		Evicted: f.Evicted, Floor: f.Floor, Ends: f.Ends, Committed: f.Committed,
	}.ClearForZero(ctx, cursor)
}

// Gates answers whether a durable record produced rows on NO node.
//
// THE SAME TWO GATES [Applier.Gated] INSTALLS, read from the publisher's side,
// by the rule [statelog.Gates] states — which every sibling's reader keeps too,
// and statelogtest.RunGates certifies for each. The two must agree: a
// resolution that looked for a gate the applier never installs would read
// every unapplied record as "somebody else won".
type Gates struct{ db *store.DB }

// NewGates builds it.
func NewGates(db *store.DB) *Gates { return &Gates{db: db} }

// GatedAt reports whether a record at p applies nowhere, and the gate that
// answers for it.
//
// ONE TRANSACTION FOR BOTH GATES, on ONE load of the replicated peer, and
// through the HANDLE rather than its pool — see [Fence.Evicted] for why a nil
// pool is reachable here. It used to be two reads, eviction first: two
// instants and possibly two FILES, since every `Replicated()` reloads the peer
// an adoption swaps and the applier commits between any two reads, so a
// removal committing between them made one half of the answer describe the
// estate before it and the other the estate after it.
//
// THE BODY IS READ for the removal gate, as the applier's own gate reads it:
// see [gatedPerson].
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position, body []byte) (statelog.Reason, bool, error) {

	if g == nil || g.db == nil {
		return "", false, nil
	}
	var reason statelog.Reason
	var gated bool
	err := g.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		// THE REMOVAL GATE FIRST, by the rule [statelog.Gates] states: a
		// tombstone holds the person for every writer for ever, where an
		// eviction is one writer's and a readmission ends it, so `deleted`
		// is the answer that stays true. A record both gates drop is
		// dropped either way, so the order decides only which one is
		// REPORTED — and it was the eviction, so a caller was told to wait
		// for a readmission that could never make the write land.
		if person, ok := gatedPerson(subj, body); ok {
			var author sql.NullString
			err := tx.QueryRowContext(ctx, `
				SELECT record_id FROM iam_removed WHERE person_id = ?`,
				person).Scan(&author)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("iamdomain: read the removal gate for "+
					"person %s: %w", person, err)
			case author.Valid && author.String == opID:
				// THE RECORD THAT REMOVED THE PERSON IS NOT GATED BY ITS
				// OWN TOMBSTONE, by its own id: without the exception a
				// removal whose acknowledgement was lost resolves as
				// "applied nowhere", and its caller is told the person
				// was not removed when they were. It falls through to
				// the eviction gate, because this says only that THIS
				// gate did not drop the record.
			default:
				reason, gated = statelog.ReasonDeleted, true
				return nil
			}
		}
		if writer == "" {
			return nil
		}
		var from, readmitted sql.NullInt64
		err := tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position
			FROM iam_evictions WHERE node_id = ?`, writer).
			Scan(&from, &readmitted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("iamdomain: read the eviction gate for node %s: %w",
				writer, err)
		}
		// THE APPLIER'S WINDOW, spelled as [Applier.Gated] spells it: half
		// open at both ends.
		at := p.Packed()
		evicted := from.Valid && at > from.Int64
		readmittedBy := readmitted.Valid && at >= readmitted.Int64
		if evicted && !readmittedBy {
			reason, gated = statelog.ReasonEvicted, true
		}
		return nil
	})
	if err != nil {
		return "", false, err
	}
	return reason, gated, nil
}

// gatedPerson is the person a record's removal gate is keyed on, from the
// subject or — where the subject does not name one — from the record's own
// body, which is exactly how [Applier.gatedPerson] reads it.
//
// ONLY THE PERSON'S OWN SUBJECT NAMES ONE. A claim arbitrates on an address, a
// login, a seat id or a session lineage, and which person each belongs to is
// in the record's PAYLOAD. The publisher hands the body of the record it
// appended ([statelog.Gates.GatedAt]), so a removal landing between a write's
// decide and its apply is named here as the gate that dropped it — without it,
// the publisher read its own acknowledged append as a ledger contract
// violation.
//
// WITH NO BODY — a record the publisher merely found above its anchor, whose
// it is being the open question — a claim is answered "not gated", and the
// re-decide that answer leads to meets the tombstone in its own snapshot
// ([Writer.request] refuses a write about a removed person before it
// publishes). A body that does not decode is not gated either, which is the
// applier's rule too: such a record is deferred rather than applied.
func gatedPerson(subj statelog.Subject, body []byte) (string, bool) {
	if ObjectKind(subj.Kind) == KindPerson && subj.ID != "" {
		return subj.ID, true
	}
	if body == nil {
		return "", false
	}
	record, err := Decode(body)
	if err != nil || record.Person == "" {
		return "", false
	}
	return record.Person, true
}
