package statelog

import (
	"context"
	"database/sql"
	"fmt"
)

// Rows is what the PUBLISHER reads from this node's own durable tables, and
// it hands back ONE CONSISTENT READ rather than a sequence of them.
//
// # Why the seam has this shape and not a set of getters
//
// An expectation is a claim about the LOG — what is the last message on this
// subject — and a decision is a claim about the ROWS. The two are only safe
// together when they were read from the SAME committed state, and the applier
// commits rows, the arbitration anchor and the checkpoint in one transaction
// precisely so that one deferred read observes a matching pair.
//
// Two separate reads never do, and the failure is not theoretical. With the
// anchor read after the row, an applier commit landing between them pairs an
// OLD decision with a NEW expectation — the broker matches the expectation,
// accepts the append, and two writers mint the same counter value. A
// duplicate key: the exact failure a counter mint exists to prevent.
//
// # Why reading here is safe at all
//
// A projection-backed key mint forbids exactly this inversion, and in its own
// words: it never reads the projection, because a stale local number would
// mint a key that already exists. That rule was right while the local number
// WAS the decision. Here the BROKER checks the expectation, so a stale snapshot is a
// claim that gets refused rather than a decision that gets committed. The
// reason holds only while the expectation travels with the decision it was
// read beside, which is what Snapshot's single transaction enforces.
type Rows interface {
	// Snapshot opens ONE read transaction, lets the domain decide from
	// rows inside it, and returns the framework's own inputs read in the
	// SAME transaction.
	//
	// The transaction is CLOSED before Snapshot returns, and nothing that
	// can block runs inside it — no broker call, no coordination read, no
	// model. A read transaction held across a round trip is a reader
	// holding a snapshot open while the world moves, which is what the
	// store's own short-transaction rule exists to stop.
	//
	// decide is handed the CHECKPOINT the transaction reads, before it
	// decides anything: it is the one value the record's generation can be
	// taken from, and it is [Snap.Checkpoint] — the same read, not a
	// second one — so the generation a record is stamped with is the
	// generation of the rows it was decided from. See [Stamp].
	Snapshot(ctx context.Context, subj Subject, s ScopeSet,
		decide func(tx *sql.Tx, checkpoint Position) (Decision, error)) (Snap, error)

	// Op answers where an operation was applied on this node.
	//
	// CONCLUSIVE ONLY at or below this node's applied position and only
	// above its own adoption instant: the ops table is this node's
	// applier's own record, so "absent" below the checkpoint means "not
	// applied here YET" and "absent" below an adoption means "scrubbed
	// out of the snapshot I arrived with". Either read as "somebody else
	// won" republishes a write that already landed.
	Op(ctx context.Context, opID string) (Position, bool, error)
}

// Decision is what a domain decided, inside the snapshot, from rows it read
// there — the record it wants to publish, and the caller-facing shape of a
// refusal it can settle locally.
type Decision struct {
	// Payload is the encoded record. Empty means the domain decided
	// there is nothing to publish, which is a legitimate outcome and not
	// an error: an update that changes no field is a no-op the caller
	// should be told succeeded.
	//
	// ITS ENVELOPE IS THE ONLY ONE THERE IS. The publisher decodes it with
	// [Domain.Envelope] before anything is appended and refuses a record
	// that does not carry the [Stamp] it was decided under — so there is
	// no second envelope beside the payload for a domain to fill in
	// differently. There was one, and nothing but a fallback generation
	// ever read it: every domain left its writer and generation empty
	// there and in the payload alike, and the eviction gate compared an
	// empty writer against every eviction in the fleet for the life of
	// the deployment.
	Payload []byte

	// Version is the object's version as the decision read it, which the
	// caller's own if_match compared against and which travels back in
	// the result. It is NEVER what the expectation is formed from — see
	// the package doc's three-values table.
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
	// Decision is what the domain decided, from rows inside the
	// transaction.
	Decision Decision

	// Anchor is the arbitration anchor for (stream, subject) — THE
	// EXPECTATION, and the only thing an expectation is ever formed
	// from. A zero anchor means this node has consumed nothing on this
	// subject in this generation, which is a probe rather than a
	// licence to publish at zero.
	Anchor Position

	// Deferral and Deferred are the step-0 scope probe. SCOPE-KEYED and
	// never subject-keyed: a record deferred under one object leaves a
	// NEIGHBOUR's row stale with nothing on the neighbour's own subject
	// to say so, and the retry-at-zero branch is unsafe for exactly that
	// neighbour.
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
	// transaction read it: the prefix the rows the decision was made from
	// reflect. A domain that has never committed on this stream is at the
	// zero position, which is what an applier that has read nothing holds.
	//
	// IT IS WHAT THE EXPECTATION-ZERO FENCE COMPARES, and nothing later can
	// stand in for it. The floor theorem in this package's doc concludes that
	// a trimmed record on this subject is already reflected in THIS
	// decision's rows, which needs C to be the position those rows were at.
	// The live checkpoint only moves forward, so it is the permissive
	// direction: a record applied after this snapshot, then trimmed, passes a
	// check against the live position while the decision about to be
	// published at zero never saw it — a lost update of exactly that record.
	Checkpoint Position
}

// ErrNoDecision reports a domain that returned neither a payload nor an
// error, which is a bug in the domain rather than an outcome: the publisher
// has nothing to publish and nothing to tell the caller.
var ErrNoDecision = fmt.Errorf("statelog: the domain decided nothing and refused nothing")
