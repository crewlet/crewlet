package coord

import (
	"context"
	"time"
)

// The FLEET-SHARED state, beyond ownership.
//
// A lease answers "who runs this seat". These four answer the other questions
// a fleet has to agree on, and they are here — beside [Backend], certified by
// the same suite — for one reason: THEY WERE ON THE NODE'S OWN DATABASE, and
// internal/store is documented "one file, one process". Every one of them was
// therefore per-node while its own comments described a fleet:
//
//   - The notification valve called itself "the shared fixed-window counter",
//     so a company on four nodes ran four valves and a seat could emit four
//     times its configured rate.
//   - The webhook dedupe registry claimed first-claim-wins across a company.
//     A vendor retrying a delivery to a different ingress node found no
//     claim, and the same push woke the same seat twice.
//   - The turn-completion ledger is what stops a redelivered trigger being
//     worked again. A redelivery that landed on a peer had no ledger to
//     consult.
//   - The config plane's apply status said it was "read back by every peer on
//     every reconcile tick AND rendered in the fleet view". Each node was
//     reading its own row and drawing a fleet of one.
//
// None of these is a lease: nothing here is owned, held or fenced. They are
// counters, claims and records, and the coordination store is simply where a
// fleet keeps what a fleet has to share.
//
// # Every contract here fails in a stated direction
//
// A coordination store that cannot be reached must not be able to invent a
// decision. Each interface says what an error means for its caller, because
// the safe direction is different for each: a dedupe claim that cannot be
// read must NOT suppress the delivery, while a rate valve that cannot be read
// must not open the floodgates.

// The bucket retentions.
//
// Each is a BUCKET's age rather than a per-call TTL, because that is what a
// KV backend can actually enforce — see internal/coord/kv's package doc for
// the measurement — so each is fixed when the bucket is created and a caller
// asking for something longer has to be refused rather than quietly clamped.
//
// They live here, beside the contract, so there is ONE place to read what a
// fleet retains. Each is sized from the cadence of the subsystem that reads
// it, named below; a backend takes them as configuration rather than
// defaulting them, because a bucket created with the wrong age is wrong for
// its lifetime.
const (
	// RateWindow is the notification valve's window, and the unit an
	// operator's `notification_rate_limit: 5` is written in: five per seat
	// per SECOND. Wider would let a genuine loop run longer before
	// tripping — two seats answering each other saturate a second easily
	// — and narrower would trip on an ordinary burst from one push.
	RateWindow = time.Second

	// ClaimTTL is how long an inbound delivery stays claimed.
	//
	// Sized to cover queue redelivery and an operator's replay, NOT a
	// vendor's own retry schedule — those back off for far longer (Plane
	// starts at ~600 s) and only fire when the API layer failed to answer
	// 2xx, which is exactly when the delivery was never claimed at all.
	// Too long is visible in one direction only: a deliberate replay ten
	// minutes later vanishes into a claim nothing will clear.
	ClaimTTL = 5 * time.Minute

	// LedgerRetention is how long a turn completion is remembered. It has
	// to outlast both the queue's redelivery horizon and the scheduler's
	// catchup window: a record deleted while a tick could still evaluate
	// lets that fire run twice, which is the one thing the ledger exists
	// to prevent.
	LedgerRetention = 7 * 24 * time.Hour

	// CooldownMax is the longest credential cooldown anything sets, and
	// therefore the bucket's age. A cooldown carries its own end instant,
	// so the bucket only has to outlive the longest one.
	CooldownMax = 24 * time.Hour

	// StatusFreshness bounds how old a node's apply status may be and
	// still count. A node that stops reporting must VANISH from the fleet
	// view rather than linger as a healthy row nobody is writing, and the
	// bucket's own expiry is what does that.
	StatusFreshness = 4 * ReconcileInterval

	// ReconcileInterval is how often a node republishes its apply status.
	// It is the configplane's cadence, restated here because
	// StatusFreshness is a multiple of it and a package that imported the
	// configplane for one constant would invert the dependency.
	ReconcileInterval = 15 * time.Second
)

// Counter is the fleet's shared fixed-window counter, behind the notification
// valve.
//
// A FIXED window, not a sliding one: the operator's number reads as "N per
// seat per second", and the arithmetic a fixed window needs is one increment
// against one key — which is the only shape that stays correct when four
// nodes increment it at once.
type Counter interface {
	// Allow increments the bucket's count for the window containing now
	// and reports whether it stayed within limit.
	//
	// FAILS CLOSED: an error means "not allowed", because the valve exists
	// to stop a loop and a store outage is exactly when a loop is most
	// likely to be what is wrong. The caller logs and drops the
	// notification; nothing here retries.
	Allow(ctx context.Context, bucket string, limit int, window time.Duration, now time.Time) (bool, error)
}

// Claims is the fleet's first-claim-wins registry with a TTL, behind webhook
// deduplication.
type Claims interface {
	// Claim records key and reports whether THIS caller was first.
	//
	// FAILS OPEN — an error yields (false, err) and the caller must
	// PROCESS the delivery. A vendor's push suppressed because the store
	// blinked is a wake that never happens, and nothing else will notice;
	// a duplicated wake is a turn the completion ledger collapses.
	Claim(ctx context.Context, key string, ttl time.Duration, now time.Time) (bool, error)

	// Release drops a claim, so a deliberate replay of the same delivery
	// is not suppressed by the first attempt's own record.
	Release(ctx context.Context, key string) error
}

// Ledger is the fleet's record of work already done, behind the
// turn-completion guard.
type Ledger interface {
	// Worked returns the subset of keys already recorded under scope.
	//
	// FAILS OPEN in BOTH directions, which is the property the Python
	// engine paid for twice: not knowing whether work was done has one
	// safe answer and it is the pre-ledger one — do the work. A read that
	// failed closed would make a store blip look like a company that had
	// already answered everything.
	Worked(ctx context.Context, scope string, keys []string) (map[string]bool, error)

	// Record marks one key worked. Best effort, for the same reason.
	Record(ctx context.Context, scope, key, detail string, at time.Time) error
}

// Cooldowns is the fleet's shared credential cooldown ledger.
//
// A key a peer found rate-limited is a key this node should not spend a call
// discovering is rate-limited. It is deliberately NOT on the hot path: see
// [Cooldowns.Since], which a pool refreshes on a ticker rather than reading
// per request.
type Cooldowns interface {
	// Cool records that a credential is unusable until the given instant.
	// Best effort: a cooldown that did not propagate costs one peer one
	// wasted call, which is what the situation cost before this existed.
	Cool(ctx context.Context, key string, until time.Time) error

	// Since returns every cooldown recorded across the fleet that has not
	// yet lapsed, as key to the instant it lifts.
	//
	// An error yields no cooldowns, which reads as "nothing is cooled" —
	// the pre-sharing behaviour, and the one that cannot make a healthy
	// fleet refuse to use any of its credentials.
	Since(ctx context.Context, now time.Time) (map[string]time.Time, error)
}

// Activation is one entry of the config pointer every node converges on.
type Activation struct {
	// Epoch is the monotonic version of the pointer.
	//
	// It is the coordination store's own revision of the pointer key, not
	// a number this engine keeps: an append-only sequence the store
	// assigns is the one counter two nodes activating at the same instant
	// cannot both win.
	Epoch      int64
	RevisionID string
	At         time.Time
	Summary    string
}

// MaxApplyErrorLength bounds the failure text a node publishes.
//
// A wrapped error chain from a failed apply is a sentence or two; a driver's
// own message with a query in it can be kilobytes, and this value is read by
// every peer on every reconcile tick AND rendered in the fleet view. 2000
// characters keeps a real diagnosis intact and stops one node's stack trace
// becoming a download for everyone else.
//
// Applied by the BACKEND, not the caller, and asserted by the contract suite:
// a bound one implementation enforced and another did not would be a fleet
// view that worked until the node with the long error happened to be on the
// other backend.
const MaxApplyErrorLength = 2000

// TruncateApplyError applies [MaxApplyErrorLength].
func TruncateApplyError(detail string) string {
	if len(detail) <= MaxApplyErrorLength {
		return detail
	}
	return detail[:MaxApplyErrorLength] + "…"
}

// NodeApply is one node's last word about an epoch.
type NodeApply struct {
	NodeID string
	Epoch  int64
	// RevisionID is which revision this node applied at that epoch. It is
	// carried alongside the epoch rather than derived from it because the
	// fleet view is read while nodes are mid-transition, and a node still
	// on the previous revision is exactly what an operator is looking for.
	RevisionID string
	// Status is the apply outcome as a plain string. Not
	// configplane.ApplyStatus: the coordination contract is where the
	// engine's layers meet, and giving it a dependency on the posture
	// package would make every backend import the config plane to store a
	// word.
	Status    string
	Error     string
	UpdatedAt time.Time
}

// Plane is the fleet's config activation pointer and per-node apply status.
type Plane interface {
	// Activate publishes a new target revision and returns it with the
	// epoch the store assigned.
	//
	// The append and the flip are ONE WRITE: the pointer key's new
	// revision IS the epoch, so there is no window in which a node can
	// read an epoch whose revision has not been published, and no way for
	// two concurrent activations to be handed the same number.
	Activate(ctx context.Context, revisionID, summary string, at time.Time) (Activation, error)

	// Target reads the pointer, reporting whether one has ever been set.
	//
	// RAISES rather than answering empty: "nothing has been activated" and
	// "the store cannot be read" send a node down opposite paths, and
	// collapsing them makes an outage look like a company with no config.
	Target(ctx context.Context) (Activation, bool, error)

	// RecordApply publishes this node's status for an epoch. The record is
	// TTL-fresh: a node that stops reporting disappears from the fleet
	// view rather than lingering as a healthy row nobody is writing.
	RecordApply(ctx context.Context, status NodeApply) error

	// Fleet returns every node's last status, freshest first.
	Fleet(ctx context.Context) ([]NodeApply, error)
}

// Fleet is a backend that serves all of the shared state, which is what the
// contract suite certifies and what the engine wires from.
//
// One interface at the CONSTRUCTION seam and four at the call sites: the
// webhook edge takes a Claims and nothing else, the valve takes a Counter.
// A consumer that could reach the whole store would eventually use it.
type Fleet interface {
	Counter
	Claims
	Ledger
	Cooldowns
	Plane
}
