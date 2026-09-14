// Package jsprovision is the one place that decides how long a replicated
// JetStream create gets, how often a forming cluster is re-asked, and what
// "the cluster is still forming" looks like on the wire.
//
// # Why this is a package rather than a constant beside each caller
//
// Two subsystems provision replicated JetStream objects at boot and they are
// the same call underneath: [internal/queue/jetstream] creates the engine's
// streams and durable consumers, and [internal/coord/kv] creates the
// coordination buckets — a bucket IS a stream. Written twice, the rule drifted
// in the way this repository has already paid for elsewhere: the two carried
// their own spelling of the same five decisions (the per-create budget, the
// read-back budget, the retry cadence, JetStream's "no suitable peers" code,
// and the predicate over it), each doc comment asserting it matched the other
// with nothing enforcing that it did. When the clustered case turned out to
// need a larger budget, both would have had to learn it separately.
//
// It imports the JetStream client and nothing from the rest of the engine, so
// either side can take it without taking the other.
//
// # Why the clustered budget is four times the solo one
//
// A SOLO create is local file-store setup: no peers, no raft, no election. A
// CLUSTERED one is a raft round trip against a metadata group whose members
// are themselves still booting, on a host bringing several of them up at once
// — and on a fleet every node issues the whole sequence at the same instant.
// One number for both is wrong for one of them, which is the conclusion
// [internal/queue/jetstream]'s accept budget already reached: it was raised
// from the solo thirty seconds to two minutes for exactly this symptom, while
// the provisioning calls DOWNSTREAM of it kept the flat thirty. A create
// cannot defensibly be more impatient than the readiness wait that precedes
// it.
//
// Thirty seconds was measured failing: over a two-day window the engine's own
// CI lost a cluster-start attempt in roughly two runs in five and failed the
// end-to-end job outright five times, every one of them a single create
// blowing this budget while its peer was still coming up — a different object
// each time, which is what says the object was never the problem. Reproduced
// on a four-core host under the race detector: the same create completes in
// seconds idle and exceeds thirty under ordinary CPU contention.
//
// # And why the sequence is bounded separately
//
// Each budget below bounds ONE create, and a boot makes many of them in a row
// — fifteen coordination buckets, and the engine's own streams beside them.
// Nothing bounded the sequence, so the real worst case was already the product
// rather than the term, and raising the term alone would have multiplied it.
// [SequenceBudget] is the wall-clock ceiling over a whole bring-up; a caller
// applies it once to the context it passes down, and because
// [context.WithTimeout] only ever shortens, each create inside then takes the
// lesser of its own budget and what is left of the sequence's.
package jsprovision

import (
	"errors"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

const (
	// soloBudget bounds one create on a server with no peers: its own
	// file store, and nothing else.
	soloBudget = 30 * time.Second

	// clusterBudget bounds one create that is a raft round trip against a
	// metadata group whose members may still be booting.
	//
	// TWO MINUTES, which is not a new number: it is what
	// [internal/queue/jetstream]'s clusterAcceptTimeout already spends on
	// the strictly EASIER problem of accepting a connection, and this call
	// sits downstream of both that and the sixty-second readiness wait.
	// The asymmetry is the accept budget's: failing here fails the whole
	// boot, so a budget that is too short turns a busy host into a node
	// that refuses to start and then works on the retry, while one that is
	// too long only reports a genuinely wedged cluster later — and the
	// wait is cancellable, so an operator who has seen enough gets their
	// prompt back immediately.
	clusterBudget = 2 * time.Minute
)

// Budget is how long ONE replicated create gets.
func Budget(clustered bool) time.Duration {
	if clustered {
		return clusterBudget
	}
	return soloBudget
}

const (
	// soloSequenceBudget bounds a whole bring-up with no peers.
	//
	// TWO MINUTES rather than something tighter, although every create in
	// it is local file-store setup measured in milliseconds and a solo
	// node has no readiness wait to sit behind at all. The asymmetry is
	// the one the whole package turns on: there was no aggregate ceiling
	// before this, so the only way a number here can do harm is by being
	// too SMALL — it would fail a boot that used to work, which is the
	// exact bug this change exists to fix, reintroduced on the path that
	// never had it. Two minutes is still a real bound (a quarter of what
	// the per-create budgets alone would allow) and leaves roughly a
	// thousandfold headroom over what the creates actually cost.
	soloSequenceBudget = 2 * time.Minute

	// clusterSequenceBudget bounds a whole clustered bring-up.
	//
	// FIVE MINUTES, sized so that one genuinely slow create can spend its
	// whole two-minute budget and the rest of the sequence still has room
	// — by then the metadata group has proven it works, so the others are
	// fast. Anchored above the three minutes of waiting a clustered boot
	// already tolerates before it provisions anything at all
	// (clusterAcceptTimeout plus clusterReadyTimeout), and below the point
	// where a wedged cluster stops being reported inside the patience of
	// whatever is watching the node come up.
	clusterSequenceBudget = 5 * time.Minute
)

// SequenceBudget is how long a whole bring-up of many creates gets.
func SequenceBudget(clustered bool) time.Duration {
	if clustered {
		return clusterSequenceBudget
	}
	return soloSequenceBudget
}

// Clustered reports whether a replica count means this node has peers to
// agree with. Spelled once because both callers ask it of their own config
// and a disagreement about where the boundary sits would give one subsystem
// the solo budget and the other the clustered one on the same node.
func Clustered(replicas int) bool { return replicas > 1 }

// ReadBack bounds the one read that asks whether a peer won a create race, and
// the one that asks whether a create this node made is visible yet.
//
// SHORT, and deliberately not [Budget]: this is an ordinary metadata read
// against a group that has just proven it is working — it either answers in a
// round trip or the cluster has gone away, and inheriting the provisioning
// budget would multiply a failing boot's time to say so.
const ReadBack = 5 * time.Second

// PlacementRetry is how often a forming cluster is re-asked.
//
// Short enough that a cluster which forms quickly is not held back by the poll
// itself, and it runs at most a few hundred times inside a provisioning
// budget.
const PlacementRetry = 250 * time.Millisecond

// errCodeNoPeers is JetStream's "no suitable peers for placement".
//
// A NUMBER rather than a description match, because nats.go names only a
// handful of its codes and this is not one of them — and matching the text
// would break the moment the server reworded it.
const errCodeNoPeers jetstream.ErrorCode = 10005

// Unplaceable reports the transient "the cluster is still forming" error, and
// it is the ONLY one worth waiting out.
//
// Every other create failure — a bad TTL, a conflicting replica count, an auth
// failure — clears by nobody waiting, so retrying it would turn a
// configuration mistake into a two-minute hang ending in the same message.
func Unplaceable(err error) bool {
	var apiErr *jetstream.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode == errCodeNoPeers
}

// NotYetVisible reports a create that landed at the metadata layer but is not
// yet readable by the member that made it.
//
// # Why this is not the same question as Unplaceable
//
// "No suitable peers" is the metadata leader REFUSING to place an object,
// answered by waiting for more members. This is the opposite: the object was
// placed, the create returned no error, and the immediately following lookup
// on the same member still says not found because the metadata update has not
// reached it. Waiting is the answer to both, but the errors do not overlap and
// a lookup that waited out a placement failure would be waiting for something
// nobody is going to do.
//
// It is bounded by [ReadBack] rather than [Budget] wherever it is used: a
// create that has already returned successfully makes this a propagation
// delay, not a provisioning one. Treating a not-found as terminal here is what
// turned one CI run's clustered boot into `open the log
// "CREWLET_TRACKER_VECTORS": stream not found` immediately after the create of
// that same stream had succeeded.
func NotYetVisible(err error) bool {
	return errors.Is(err, jetstream.ErrStreamNotFound) ||
		errors.Is(err, jetstream.ErrBucketNotFound)
}
