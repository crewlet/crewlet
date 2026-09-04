// Package projection is each node's local, rebuildable copy of the fleet's
// document families — the read side of the native tracker and knowledge base.
//
// # Why a copy exists at all
//
// The record of truth is [coord.Documents], and it has to be: every node has
// to agree on a work item, so it cannot live in a node's own store (see
// migrations 0010-0013 for what that cost the last time). But a coordination
// bucket answers one question well — read this key — and a board asks a
// different one entirely: every open item in this project, newest first,
// filtered by assignee, searched by text. Answering that from the bucket is
// O(keys) message deliveries per screen.
//
// So the bucket holds the truth and every node keeps a projection of it, in
// its own database, maintained by following the bucket's watch. That is the
// same shape [learning/memsync] uses for a seat's memory, extended for the
// three things a company-wide dataset needs that a seat's memory did not:
// many writers rather than one owner, deletes that travel because nothing
// re-converges a deleted item, and a catch-up at node BOOT rather than at
// seat acquisition.
//
// # The two facts a caller has to keep apart
//
// A cursor and hydration are different claims, and conflating them is the
// failure this package is shaped around.
//
//   - The CURSOR says how far the live watch has been applied. It is a tail
//     position, and a tail position is only meaningful relative to a stream
//     that still holds it.
//   - HYDRATED says a boot reconcile has established that every key the
//     bucket holds is present here.
//
// A watch resumed from a revision above the stream's last sequence delivers
// nothing and reports caught-up immediately. That is not hypothetical: it is
// what a cold restore from an older backup produces, what an in-memory bucket
// recreated at sequence 1 produces, and what cloning a node's data directory
// from a peer produces. A node in that state sits at a plausible cursor over
// an empty projection, for ever, and every screen it serves says the company
// has no work. So nothing waits on the cursor: a seat's mailbox attaches only
// after [Projector.Hydrated], and hydration is established by a per-key
// reconcile rather than by comparing numbers.
//
// # What is NOT here
//
// The write path. A mutation is [internal/work]'s or [internal/pages]', and
// it goes to coordination — never to these tables. A row written here that
// coordination did not see is a row the next reconcile silently erases, and
// it would look exactly like the engine losing somebody's work.
//
// Best effort is also NOT the contract, unlike [knowledge]. A read that
// cannot be served raises: "this item does not exist" is an answer a seat
// acts on — it files a duplicate, it abandons work it was told to do — and a
// projection that has not caught up must never be able to say it.
package projection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ErrNotHydrated reports a read taken before the boot reconcile finished.
//
// A SENTINEL RATHER THAN AN EMPTY RESULT, on this package's central rule: an
// empty answer from an unhydrated projection is indistinguishable from a
// company with no work, and every caller that receives it acts on the wrong
// one. Callers surface it as "not ready" and retry; nothing treats it as
// absence.
var ErrNotHydrated = errors.New("projection: not hydrated yet")

// ErrRevisionTooNew reports a wait whose revision never arrived in time.
//
// The caller's own write DID land — it was acknowledged by coordination —
// so this is never "the write failed". It says this node has not seen it
// yet, which a REST response reports as applied: false and a tool call
// reports by naming the record it wrote rather than reading it back.
var ErrRevisionTooNew = errors.New("projection: revision not applied here yet")

// The batching limits for one apply transaction.
//
// A single transaction over a whole bucket is not an option: measured on a
// multi-million-row import, one transaction grew the write-ahead log past
// 9 GB, where 20-page batches held it at 1 MB. And a transaction per change
// is the other failure — a fsync per record turns a boot reconcile over a
// hundred thousand keys into an hour.
const (
	// applyBatch is how many changes one transaction carries.
	//
	// Sixty-four: enough that the per-transaction cost amortises, small
	// enough that the largest possible batch (64 page bodies at the 512 KiB
	// cap) is 32 MiB rather than unbounded.
	applyBatch = 64

	// applyLinger is how long a partial batch waits for company.
	//
	// 250ms, which is the latency a person's own write adds before it is
	// visible on their screen — and it is spent only when the batch is not
	// already full, so a busy fleet never waits at all. Longer buys
	// negligible batching on an idle company and makes every dashboard
	// interaction feel slow.
	applyLinger = 250 * time.Millisecond

	// waitBudget bounds [Projector.WaitApplied].
	//
	// Two seconds. A watch round trip on a healthy fleet is milliseconds;
	// this is sized for a node under load or a leader election in flight,
	// not for a broken one. Past it the honest answer is "not here yet",
	// because a longer wait holds an HTTP handler open on a fleet that has
	// a real problem — and read-your-writes is a convenience, while a
	// blocked request thread is an outage.
	waitBudget = 2 * time.Second

	// reconcileNoise is how often a slow boot reconcile says it is still
	// going.
	//
	// The pass is O(keys) headers and its cost at hundreds of thousands of
	// keys is UNMEASURED, so it says so loudly rather than looking hung: a
	// node that is not claiming seats yet and prints nothing is
	// indistinguishable from one that has wedged.
	reconcileNoise = 10 * time.Second
)

// Family is a document family, re-exported so a caller of this package does
// not import coordination to name one.
type Family = coord.Family

// The families this package projects.
const (
	FamilyWork  = coord.FamilyWork
	FamilyPages = coord.FamilyPages
)

// Projected is every family a projector follows, in a stable order.
//
// NOT [coord.Families]: the vector family is written by the indexer against
// its own source versions and read by the searcher, never applied as a change
// feed, so a projector that followed it would burn a watch on records it has
// no apply rule for.
func Projected() []Family { return []Family{FamilyWork, FamilyPages} }

// Status is what a projector reports about one family, for the node status
// the fleet view renders.
type Status struct {
	Family Family

	// Hydrated is the fact a mailbox waits on. See the package doc.
	Hydrated bool

	// Revision is the cursor: the last coordination revision applied here.
	Revision uint64

	// Pending is how many changes are buffered and not yet applied. A
	// number that keeps climbing is a projector that cannot keep up, which
	// is a different problem from one that has stalled.
	Pending int

	// Dropped counts changes the buffer could not hold. NON-ZERO IS A
	// REBUILD, not a gap to tolerate: the projection has missed writes, and
	// the only correct response is to re-run the reconcile.
	Dropped uint64

	// LastApplied is when the last batch committed, so a person can tell a
	// quiet company from a stopped projector.
	LastApplied time.Time

	// Err is the last apply or watch failure, empty when healthy.
	Err string
}

// Lag is how far behind the family's own head this node is, or 0 when it is
// caught up. Reported rather than acted on: the control plane's rule is that
// lag alone never sheds a node, and /ready deliberately does not gate on it.
func (s Status) Lag(head uint64) uint64 {
	if head <= s.Revision {
		return 0
	}
	return head - s.Revision
}

// Healthy reports whether this family can answer reads.
func (s Status) Healthy() bool { return s.Hydrated && s.Err == "" && s.Dropped == 0 }

// applyError names the change an apply failed on.
//
// The KEY, not the row: a projector failure is diagnosed by fetching that key
// from the bucket and comparing it with what landed, and a message naming a
// table and a revision sends the reader to the wrong half.
func applyError(family Family, key string, revision uint64, err error) error {
	return fmt.Errorf("projection: apply %s %s at revision %d: %w",
		family, key, revision, err)
}

// waitCtx bounds a read-your-writes wait, taking whichever of the caller's
// deadline and the budget expires first.
func waitCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, waitBudget)
}
