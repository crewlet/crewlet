package statelog

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Rows is what the PUBLISHER reads from this node's own durable tables, as
// ONE CONSISTENT READ rather than a set of getters.
//
// An expectation (a claim about the LOG) and a decision (about the ROWS) are
// safe together only when read from the SAME committed state, which is why the
// applier commits rows, anchor and checkpoint in one transaction. Two reads
// are not: an applier commit between them pairs an OLD decision with a NEW
// expectation, the broker accepts, and two writers mint one counter value.
//
// Minting from a local read is safe only because the BROKER checks the
// expectation, so a stale snapshot is refused rather than committed, and only
// while the expectation travels with its decision, as Snapshot enforces.
type Rows interface {
	// Snapshot opens ONE read transaction, lets the domain decide from rows in
	// it, and returns the framework's inputs read in the SAME transaction,
	// closed before it returns; nothing that can block (a broker call, a
	// coordination read, a model) runs in it: the store's short-transaction rule.
	//
	// decide is handed the transaction's CHECKPOINT ([Snap.Checkpoint], the
	// same read), so a record is stamped with its rows' generation ([Stamp]).
	//
	// The write's own operation is looked up FIRST: when the ledger holds it,
	// held judges that row in the same transaction, refusing one that answers
	// for another write ([Publisher.heldHere]; its error is Snapshot's), and
	// the domain is NOT asked.
	Snapshot(ctx context.Context, subj Subject, s ScopeSet, opID string,
		held func(tx *sql.Tx, entry OpEntry) error,
		decide func(tx *sql.Tx, checkpoint Position) (Decision, error)) (Snap, error)

	// Op answers where an operation was applied on this node.
	//
	// CONCLUSIVE ONLY at or below this node's applied position and only
	// for an operation minted at or after [Rows.LostBefore]: "absent"
	// below the checkpoint means "not applied here YET", and "absent"
	// before the watermark may mean "lost". Either read as "somebody else
	// won" republishes a write that already landed.
	Op(ctx context.Context, opID string) (OpEntry, bool, error)

	// LostBefore answers the instant before which the domain's ops table may
	// have lost rows of an operation on a subject of this KIND (false when it
	// has lost none), so "absent" is conclusive only for an operation minted at
	// or after it. It travels with the ledger in a snapshot, and only a sweep
	// moves it: the table's or a kind's, this node's or, inherited with the
	// ledger an adoption installs, its donor's; an adoption itself loses
	// nothing (ledgerloss.go, [Publisher.vouches]).
	//
	// PER KIND (internal/maintenance's OpsHorizon.Kinds): the later of the
	// table's watermark and the kind's, so an hourly session sweep never reads
	// as the whole ledger losing everything older than an hour. An empty kind
	// asks the table's alone.
	LostBefore(ctx context.Context, kind string) (time.Time, bool, error)
}

// Decision is what a domain decided, inside the snapshot, from rows it read
// there — the record it wants to publish, and the caller-facing shape of a
// refusal it can settle locally.
type Decision struct {
	// Payload is the encoded record. Empty means the domain decided there
	// is nothing to publish, which is not an error: an update that changes
	// no field is a no-op the caller is told succeeded.
	//
	// ITS ENVELOPE IS THE ONLY ONE: the publisher decodes it with
	// [Domain.Envelope] before appending and refuses a record that does not
	// carry the [Stamp] it was decided under, so no second envelope beside
	// the payload can disagree with it.
	Payload []byte

	// Version is the object's version as the decision read it, which the
	// caller's if_match compared against and the result carries back. It is
	// NEVER what the expectation is formed from (package doc, "Three values,
	// three questions, and no caller reads two of them").
	Version int64
}

// Empty reports a decision with nothing to publish.
func (d Decision) Empty() bool { return len(d.Payload) == 0 }

// Deferral is a deferred record whose declared scope intersects the closure a
// write or a read is about.
type Deferral struct {
	// Position is where the deferred record sits.
	Position Position

	// Version is the record version this build could not decode, which
	// is the one number an operator needs to know which build to run.
	Version int

	// Scope is the deferred record's own declared scope, so a refusal can
	// name what is actually stale rather than the whole domain.
	Scope ScopeSet
}

// Snap is one committed state, seen once. Every field was read inside the
// same transaction as [Snap.Decision], which is the property the publisher
// rests on and the reason this is one struct rather than five calls.
type Snap struct {
	// Held is where this node's ledger records the write's own operation
	// as applied, and HeldOK whether it does; when it does, no other field
	// is read and no decision is taken.
	//
	// A RETRY OF AN OPERATION THAT LANDED IS ANSWERED, NOT DECIDED AGAIN:
	// a decision on top of the first application posts a comment twice,
	// moves a counter twice, or refuses an update as stale against its own
	// first copy. The broker does not stop it (the expectation is current,
	// and its duplicate window is two minutes while a re-run or retry
	// routinely comes later), so the ledger must.
	Held   Position
	HeldOK bool

	// Decision is what the domain decided, from rows inside the
	// transaction.
	Decision Decision

	// Anchor is the arbitration anchor for (stream, subject): THE
	// EXPECTATION, and the only thing one is formed from. Zero means this
	// node has consumed nothing on this subject in this generation, or the
	// trim removed its anchor because the log holds none of its records
	// (ADR-0080): a probe, not a licence to publish at zero.
	Anchor Position

	// Deferral and Deferred are the step-0 scope probe, SCOPE-KEYED and
	// never subject-keyed: a record deferred under one object leaves a
	// NEIGHBOUR's row stale with nothing on its own subject to say so, and
	// the retry-at-zero branch is unsafe for exactly that neighbour.
	Deferral Deferral
	Deferred bool

	// Deleted is a deletion marker on this subject, which is permanent
	// where a guarding row is not.
	Deleted bool

	// Guard is the first-writer-wins guarding row: present means the
	// object exists, which is what still holds below the trim floor
	// where the broker's own claim does not.
	Guard bool

	// Checkpoint is this node's committed checkpoint as the decision's own
	// transaction read it: the prefix the decision's rows reflect (zero for
	// a domain that never committed on this stream).
	//
	// IT IS WHAT THE EXPECTATION-ZERO FENCE COMPARES, and the live
	// checkpoint cannot stand in: the floor theorem (package doc, "THE FLOOR
	// THEOREM") needs C to be where THESE rows were, and the live one only
	// moves forward, so a record applied after this snapshot and then trimmed
	// would pass while the decision published at zero never saw it: a lost
	// update.
	Checkpoint Position
}

// anchorIn is the subject's arbitration anchor IN generation gen, or zero.
//
// ONE RULE FOR EVERY READER OF THE ANCHOR. A row keeps the generation it was
// written in and a reanchor keeps every row, so another generation's anchor is
// a sequence in a number space the log has left (a restored log counts again
// below it, a rebuilt one from one): an expectation formed from it arbitrates
// against a number the broker never assigned, and the ambiguous path would
// read "nothing landed above the anchor" for a record that had, and decide
// again.
func (s Snap) anchorIn(gen uint32) uint64 {
	if s.Anchor.Generation != gen {
		return 0
	}
	return s.Anchor.Seq
}

// ErrNoDecision reports a domain that returned neither a payload nor an
// error, which is a bug in the domain rather than an outcome: the publisher
// has nothing to publish and nothing to tell the caller.
var ErrNoDecision = fmt.Errorf("statelog: the domain decided nothing and refused nothing")
