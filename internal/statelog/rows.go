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
	// THE OPERATION LEDGER IS READ FIRST, and when it already records
	// opID the domain does not decide at all: the snapshot reports where
	// the operation applied ([Snap.Applied]) and nothing else. A decision
	// about an operation that has landed is a second decision, formed
	// from rows the first one already moved — see [Publisher.Publish] for
	// what publishing it would do.
	//
	// The transaction is CLOSED before Snapshot returns, and nothing that
	// can block runs inside it — no broker call, no coordination read, no
	// model. A read transaction held across a round trip is a reader
	// holding a snapshot open while the world moves, which is what the
	// store's own short-transaction rule exists to stop.
	Snapshot(ctx context.Context, subj Subject, s ScopeSet, opID string,
		decide func(*sql.Tx) (Decision, error)) (Snap, error)

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
	Payload []byte

	// Envelope is the record's own envelope, which the framework reads
	// for the subject, the scope and the op id it publishes under.
	Envelope Envelope

	// Version is the object's version as the decision read it, which the
	// caller's own if_match compared against and which travels back in
	// the result. It is NEVER what the expectation is formed from — see
	// the package doc's three-values table.
	Version int64
}

// Empty reports a decision with nothing to publish.
func (d Decision) Empty() bool { return len(d.Payload) == 0 }

// Deferral is a retained record whose declared scope intersects the closure a
// write or a read is about.
//
// RETAINED IS NOT THE SAME AS UNDECODABLE. The apply loop retains every record
// its build cannot decode, and also every record it could decode whose scope
// meets one already retained — so the record a probe finds may be either, and
// [Deferral.Describe] is how a refusal says which.
type Deferral struct {
	// Position is where the retained record sits.
	Position Position

	// Version is the record version it was written at. Above the build's
	// own, it is the number an operator picks a build by; at or below it,
	// the record is held back behind one the build cannot decode.
	Version int

	// Scope is the retained record's own declared scope, so a refusal can
	// name what is actually stale rather than the whole domain.
	Scope ScopeSet
}

// Describe names a retained record the way a refusal states it, given the
// record version this build reads: by the version it cannot decode, or as
// held back behind one — never as undecodable at a version the build reads.
func (d Deferral) Describe(reads int) string {
	if d.Version > reads {
		return fmt.Sprintf("a record at version %d this build cannot decode, at %s",
			d.Version, d.Position)
	}
	return fmt.Sprintf("a record at %s that is held back behind one this build "+
		"cannot decode", d.Position)
}

// Snap is one committed state, seen once. Every field was read inside the
// same transaction as [Snap.Decision], which is the property the publisher
// rests on and the reason this is one struct rather than five calls.
type Snap struct {
	// Applied is where this node's operation ledger records the request's
	// own operation, and AlreadyApplied says whether it does. When it does,
	// every other field is zero: the domain did not decide, and the
	// publisher answers from this position without publishing.
	//
	// Only a POSITIVE answer means anything. The ledger is this node's
	// applier's own record, so an absent row says "not applied HERE yet",
	// never "not applied anywhere" — which is why absence sends the write
	// on to decide and let the broker arbitrate, as every first attempt
	// does. A domain with no ledger never reports one.
	Applied        Position
	AlreadyApplied bool

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
}

// ErrNoDecision reports a domain that returned neither a payload nor an
// error, which is a bug in the domain rather than an outcome: the publisher
// has nothing to publish and nothing to tell the caller.
var ErrNoDecision = fmt.Errorf("statelog: the domain decided nothing and refused nothing")
