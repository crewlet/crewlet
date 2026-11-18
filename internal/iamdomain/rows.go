package iamdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The three seams a write authority is built from. [statelog.NewPublisher]
// refuses a nil Rows, Fence or Gates by name, so a half-built write authority
// fails boot rather than appending records the fleet drops one at a time.

// NewRows builds the framework's row seam over this domain's guards, on the
// log spec names — the one the publisher writes, whose checkpoint and anchors
// the seam reads.
func NewRows(db store.ReplicatedReader, spec statelog.StreamSpec) (statelog.Rows, error) {
	return statelog.NewRows(db, Domain{}, spec, iamGuards)
}

// personGuardsStatement is whether one person's key holds a removal tombstone
// and whether it holds a row, each by that key.
const personGuardsStatement = `
	SELECT
		(SELECT COUNT(*) FROM iam_removed WHERE person_id = ?),
		(SELECT COUNT(*) FROM iam_people  WHERE id = ?)`

// iamGuards answers the two object-level facts a first write needs: whether a
// tombstone forbids this subject for ever, and whether a row already guards it.
//
// Only a person has them: only a person is removed, by a directory record that
// writes the tombstone read here. The directory is never removed and publishes
// at its anchor; a session lineage is ended, not removed, and its open is the
// create-at-zero the broker arbitrates, where a row guard would be a second
// opinion about a lineage the broker already decided.
func iamGuards(ctx context.Context, tx *sql.Tx, subj statelog.Subject) (
	deleted, guard bool, err error) {

	if ObjectKind(subj.Kind) != KindPerson {
		return false, false, nil
	}
	var removed, present int
	err = tx.QueryRowContext(ctx, personGuardsStatement,
		subj.ID, subj.ID).Scan(&removed, &present)
	if err != nil {
		return false, false, fmt.Errorf("iamdomain: read the guards on person "+
			"%s: %w", subj.ID, err)
	}
	return removed > 0, present > 0, nil
}

// ReadScope is the closure a read is about, in the alphabet a record's own
// scope resolves into, so the coverage probe is one comparison.
//
// A read about one person is their bucket. A read naming nobody (the
// directory, a duty's walk, a sweep's survey) is the whole estate: every
// deferred record concerns it, so "is this complete" honestly answers "no".
func ReadScope(personIDs ...string) statelog.ScopeSet {
	if len(personIDs) == 0 {
		return statelog.ScopeSet{Paths: []string{RootPath()}}
	}
	return PeopleScope(personIDs...).Resolve(PersonSubject(personIDs[0]))
}

// ReadScopeForBucket is the closure a per-bucket duty or sweep is about. A
// sweep holds a bucket, not a person; going through [PeopleScope] would mean
// inventing a person id that hashes to it.
func ReadScopeForBucket(b Bucket) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{b.Path()}}
}

// Fence refuses a write this node must not make.
//
// It installs its siblings' gate at the same prices: Evicted runs on every
// append, from rows this node has, and ClearForZero pays a coordination round
// trip because being wrong there is a lost update, not a duplicate. Here
// ClearForZero is on every sign-in, since a session opens at an expectation of
// zero (so a lineage opens exactly once): a node below the trim floor cannot
// tell a lineage nobody opened from one trimmed beneath it, and guessing would
// open a session twice. The same holds for any subject's first record.
type Fence struct {
	db     store.ReplicatedReader
	nodeID string

	// Floor is the fleet's trim floor at a generation, Ends the log's two ends,
	// and Committed this node's applier's live checkpoint: the reads
	// [statelog.ZeroFence] takes beside the eviction. The floor is compared with
	// the cursor passed per call, the write's own snapshot checkpoint
	// ([statelog.Snap.Checkpoint]), never the live position: the floor theorem
	// says a trimmed record is in the rows the decision was made from, and a
	// position moved past it since clears a claim that never saw it. The engine
	// sets these once the runner exists; a fence built before its applier would
	// compare against a position that does not move.
	Floor     func(ctx context.Context, generation uint32) (uint64, error)
	Ends      func(ctx context.Context) (statelog.LogEnds, error)
	Committed func() statelog.Position
}

// NewFence builds it.
func NewFence(db store.ReplicatedReader, nodeID string) *Fence {
	return &Fence{db: db, nodeID: nodeID}
}

// Evicted reports this node's own eviction from its own applied rows, not
// coordination: a wedged coordination path is a precondition of an eviction
// being permitted, so this node's replicated table is what stays fresh in that
// failure.
func (f *Fence) Evicted(ctx context.Context) (bool, error) {
	if f == nil || f.db.IsZero() || f.nodeID == "" {
		return false, nil
	}
	var from, readmitted sql.NullInt64
	// Through the handle, not a pool: an adoption closes the estate between its
	// rename and reopen, and a statement on a closed pool panics in database/sql.
	// [store.ReplicatedReader.Read] answers [store.ErrNoEstate], refused below.
	err := f.db.Read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `
			SELECT from_position, readmitted_position
			FROM iam_evictions WHERE node_id = ?`, f.nodeID).
			Scan(&from, &readmitted)
	})
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		// Unreadable is not "not evicted": failing open would append records
		// every other node drops, acknowledged for writes that happen nowhere.
		return false, fmt.Errorf("iamdomain: read this node's own eviction: %w", err)
	}
	if !from.Valid {
		return false, nil
	}
	// [Domain.Evictions]' `Back` negated, spelled once in back so the trim and
	// this fence never disagree.
	return !back(readmitted, from.Int64), nil
}

// back reports whether an eviction row's readmission has taken the node back:
// a readmission ABOVE the eviction it answers. One comparison for the fence,
// the trim's listing and a retry's standing, so the three never disagree.
func back(readmitted sql.NullInt64, from int64) bool {
	return readmitted.Valid && readmitted.Int64 > from
}

// ClearForZero verifies, freshly, that publishing at an expectation of zero is
// safe from this node.
//
// A read that answers unknown refuses, unlike a delivery claim's fail-open:
// failing open there costs a recoverable duplicate notification, here a claim
// overwriting one another node made, in this domain a second person holding
// somebody's address. The check is [statelog.ZeroFence], every sibling's; this
// supplies its four reads, and every refusal is an [*statelog.Unavailable]
// naming its reason.
func (f *Fence) ClearForZero(ctx context.Context, cursor statelog.Position) error {
	return statelog.ZeroFence{
		Evicted: f.Evicted, Floor: f.Floor, Ends: f.Ends, Committed: f.Committed,
	}.ClearForZero(ctx, cursor)
}

// Gates answers whether a durable record produced rows on no node.
//
// It reads the two gates [Applier.Gated] installs, by the rule [statelog.Gates]
// states, which statelogtest.RunGates certifies. They must agree: looking for a
// gate the applier never installs would read every unapplied record as
// "somebody else won".
type Gates struct{ db store.ReplicatedReader }

// NewGates builds it.
func NewGates(db store.ReplicatedReader) *Gates { return &Gates{db: db} }

// GatedAt reports whether a record at p applies nowhere, and the gate that
// answers for it.
//
// Both gates are read in one transaction, through the handle (see
// [Fence.Evicted]): each handle read resolves afresh the peer an adoption swaps,
// and the applier commits between reads, so two reads could describe the
// estate before and after one removal. The removal gate reads the body by the
// applier's own rule: see [markedPerson].
func (g *Gates) GatedAt(ctx context.Context, subj statelog.Subject,
	writer, opID string, p statelog.Position, body []byte) (statelog.Reason, bool, error) {

	if g == nil || g.db.IsZero() {
		return "", false, nil
	}
	var reason statelog.Reason
	var gated bool
	err := g.db.Read(ctx, func(tx *sql.Tx) error {
		// The removal gate first, by the rule [statelog.Gates] states: a
		// tombstone holds the person for every writer for ever, where an
		// eviction ends at readmission. A record both gates drop is dropped
		// either way; the order picks which is reported, and reporting the
		// eviction would have a caller wait for a readmission that cannot help.
		if person, ok := markedPerson(subj, body); ok {
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
				// The removal's own record (by its id) is not gated by its
				// tombstone: otherwise a removal whose acknowledgement was lost
				// resolves as "applied nowhere" and its caller is told the person
				// was not removed. It falls through to the eviction gate.
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
		// The window as [Applier.Gated] spells it: after the eviction, before
		// the readmission.
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
