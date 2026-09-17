// Package coord is cross-process resource ownership: TTL leases with a
// fencing token, plus the shared counters and ledgers a fleet coordinates
// through.
//
// This is the primitive every multi-node duty is built on. A node claims a
// resource, renews it on a heartbeat, and loses it by crashing or releasing.
// A resource name is SEGMENTED, and its leading segment is its [Class] — the
// kind of thing the lease is for. That is not decoration: the key a resource
// becomes carries its class as a subject token of its own, so a whole class
// is a wildcard the broker can match and a listing asks for one kind rather
// than reading every lease in the fleet. Three classes name three kinds:
//
//   - seat:{handle} — one agent seat this node runs.
//   - worker:{duty} — a fleet singleton: the maintenance sweep, the
//     scheduler tick, the lifecycle pass. A duty's TTL is sized from its own
//     cadence rather than from the seat heartbeat, so it runs from seconds to
//     hours, and every backend must honour any of them up to [MaxDutyTTL]
//     whatever its seat lease TTL is. See [MaxDutyTTL] for why that is a
//     contract rather than a backend's choice.
//   - node:{id} — the node's own PRESENCE, which is the kind that is easy to
//     forget and the one the placement math counts. Membership is not work:
//     a node holds it to say it is alive, ListLive(ClassNode) is the fleet
//     roster, and the fair-share target every node computes for itself is
//     ceil(seats / that count). A node that stops renewing its presence is
//     not merely idle — it raises everyone else's share.
//
// Three rules carry the correctness of everything above:
//
//  1. Every write on behalf of a resource is fenced by the epoch.
//     TryAcquire returns it; callers thread it into their conditional
//     writes. A zombie's late write bounces instead of corrupting state.
//     The epoch is therefore MONOTONIC FOR THE LIFETIME OF THE RESOURCE —
//     releasing a lease must not reset it, because a counter that restarts
//     hands the next owner a token the previous one is still using.
//
//  2. A lapsed lease cannot be renewed, only re-acquired — and re-acquiring
//     bumps the epoch even for the same owner. During the gap the owner's
//     in-flight work was unprotected, so it must be fenced against its own
//     past self. Only an unbroken same-owner hold keeps its epoch.
//
//  3. Owner identifies a process INCARNATION, not a machine. Two processes
//     sharing an owner string would both hold the resource at the same
//     epoch. The stable node id goes in Preferred, where restart-stability
//     is what you actually want.
//
// # The tri-state, and why Go gets it for free
//
// Every method that answers "do I hold this?" has three answers, and
// conflating two of them is the single most incident-hardened lesson carried
// into this engine. (value, error) expresses it natively:
//
//	(lease, nil)  — held. Proceed.
//	(nil, nil)    — definitively NOT held: lapsed, moved, or advanced.
//	                Shed the work it covered, now.
//	(nil, err)    — UNKNOWN. The store could not be reached or did not
//	                answer. This says NOTHING about ownership: the record
//	                is untouched and probably still held. Keep the seats,
//	                stop admitting new work, and retry until the TTL is
//	                genuinely elapsed.
//
// Treating unknown as loss tears a healthy company down over a two-second
// store blip. Treating unknown as refusal makes a node stop refreshing its
// own presence during exactly the outage it should ride out. Any non-nil
// error means unknown — there is no error worth special-casing into a
// definite answer.
//
// # Why coordination is not itself a replicated log
//
// The engine grows a durable-state framework — internal/statelog — whose
// shape is very close to this one: an ordered log of records, a conditional
// append per subject, and N identical SQL copies replaying it. A conditional
// append per subject IS this package's compare-and-set one layer down, so
// "it would not work" is not available as an answer and the refusal has to be
// argued. Five arguments, and each one is fatal on its own:
//
//  1. THE FRAMEWORK'S CENTRAL PROPERTY IS THE ONE A LEASE MUST NOT HAVE. A
//     replica can only ever be BEHIND, and that is what makes its answers
//     safe. A lease answers NOW. A replicated read of a lease table could
//     only ever say "held as of my checkpoint" — a FOURTH answer beside the
//     three above, and the one no caller can act on: a node ten seconds
//     behind would believe it holds a seat another node took nine seconds
//     ago, and would keep running turns on it.
//
//  2. A FENCING EPOCH MUST SURVIVE THE PAST BEING DELETED, AND A LOG DELETES
//     ITS PAST BY CONSTRUCTION. Rule 1 above needs the epoch monotone for the
//     resource's LIFETIME; coord/kv states the same thing from the other end
//     ("gaps in the counter are harmless; resets are not"). A node that
//     re-derived an epoch from a trimmed log would derive a LOWER one — the
//     exact reset the design forbids, produced by the retention mechanism
//     rather than by a bug. No amount of care in the framework fixes it,
//     because a log's whole contract is that old records go away.
//
//  3. THE ACTIVATION POINTER'S READERS ARE THE NODES THAT ARE BEHIND, so
//     putting it on a log is circular. Its revision is the epoch, and
//     internal/configplane reads it to decide whether THIS NODE IS BEHIND. A
//     node cannot use its position on a log to discover that its position on
//     that log is stale. There is no base case.
//
//  4. THESE RECORDS ARE BOUNDED, MUTABLE AND SHORT-LIVED — a bucket's shape
//     and a log's anti-shape. Presence renews on the 15-second reconcile
//     interval: twenty seats over five nodes is ≈ 50 M lease writes a year of
//     pure lock traffic, every byte of it retained for the log's age floor.
//     The framework makes this same argument about the tracker's own walk
//     claims at a four-hundredth of the scale, and moves them INTO
//     coordination: a claim is a lock, not state, and reproducing another
//     node's claim on a replica is meaningless because a replica must not act
//     on it.
//
//  5. THE FRAMEWORK'S OWN RETENTION GATE READS COORDINATION, so coordination
//     cannot read the framework. Every term that decides how far a log may be
//     trimmed — the positions register, the live trim holds, the eviction
//     tombstones and the backup floor — is a coordination read taken BEFORE
//     the log is purged. Putting them on the log would make the log's
//     retention a function of the log's own contents: a cycle with no base
//     case. It is also why the positions register is an AGELESS bucket, which
//     is the exact inversion of the presence bucket beside it, whose whole
//     purpose is that a node that stops reporting vanishes.
package coord

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProtocolVersion is the seat-host protocol this build speaks.
//
// A rolling upgrade puts a vN and a vN+1 node on the same coordination store
// and the same topics at once. That is fine as long as both agree on what
// HOLDING A LEASE MEANS — and catastrophic when they do not: two nodes that
// disagree about whether a seat's inbox consumer is owner-only, or about
// whether a turn claim fences a resume, will each be individually correct
// and jointly wrong.
//
// So the rule is asymmetric, deliberately: a node refuses to claim anything
// while a live lease is held at a LOWER protocol. Older nodes keep working
// (they cannot know about a check that postdates them); newer ones wait,
// visibly, until the last old lease lapses. A rolling deploy converges
// because that is what a rolling deploy does.
//
// Two consequences worth stating plainly. Schema evolution here is
// additive-only: a field the older build ignores is invisible to it, one it
// requires is a crash. And a downgrade across a bump needs a full drain,
// because an older build has no protocol check at all and will happily take
// over a newer node's expired leases.
//
// Bump this when the MEANING of holding a lease changes, never when
// something merely gains a field. The history: v2 = holding a seat means
// consulting the completion ledger; v3 = claiming
// a seat means this node satisfies the role's placement. Both were silent
// corruption in a mixed fleet, which is the bar.
const ProtocolVersion = 3

// ErrUnavailable is the canonical "store could not answer" error. Backends
// wrap their transport failures in it. Callers should not switch on it —
// ANY non-nil error means unknown — but it gives the common case one name
// in logs and lets tests assert the tri-state deliberately.
var ErrUnavailable = errors.New("coordination store unavailable")

// ErrTTLTooLong reports a claim or renew asking for a TTL the store cannot
// honour.
//
// An ERROR, never a (nil, nil) refusal: nobody else holds the resource, the
// caller asked for a deadline the store cannot keep. And never a silent clamp,
// because a heartbeat computes its next tick from [Lease.ExpiresAt], so a
// deadline quietly cut short has the holder renewing too late and losing what
// it still holds.
var ErrTTLTooLong = errors.New("coord: ttl exceeds what the store can honour")

// MaxDutyTTL is the longest TTL a fleet duty (a `worker:` resource) may be
// claimed with, and the TTL every backend must be able to honour for one.
//
// # Why a duty's ceiling is the contract's and not the seat lease's
//
// A seat lease and a duty lease want opposite TTLs. A seat is renewed on a
// heartbeat, so its TTL is a few heartbeats and a dead node's seats move within
// a minute. A duty is re-claimed once per TICK of the work it guards, and a
// tick runs from ten seconds (the scheduler) to an hour (the learning passes),
// so a duty TTL has to outlive several of its own ticks or it moves to a peer
// on ordinary jitter.
//
// The embedded KV backend fixes a lease's expiry as its BUCKET's age, so it
// can only honour TTLs up to the bucket it writes into. For as long as duties
// shared the seat lease bucket, every duty claim longer than the seat lease
// TTL (45 seconds by default) was refused, and on every `embedded-kv` fleet
// the retention sweep, the mailbox retirement, the integration reconcile, the
// skill curator and every integration setup pass failed on their lease claim
// and never ran at all, with one warning per attempt as the only symptom. The
// in-memory twin honoured any TTL, so no single-node test could see it.
//
// So the ceiling is stated HERE, every backend enforces it through
// [CheckDutyTTL], and a duty TTL the embedded KV cannot keep is refused by the
// twin too, in the tests that run against it. The contract suite certifies
// both halves on every backend: a duty at exactly this TTL is honoured, one
// beyond it is an error wrapping [ErrTTLTooLong].
//
// # The rolling upgrade a duty ceiling costs
//
// A backend that stores duties apart from seats (the embedded KV does) meets a
// build that stored them together, and two builds locking one duty in two
// places would both hold it. The rule every such backend follows: a duty claim
// is REFUSED, as the ordinary (nil, nil), while any node of a build that
// predates the move is live, and a holder's own re-claim is refused with it so
// the duty stops at its next tick. An older node never looks for the newer
// record, so this is the only side that can wait. See the kv package doc for
// how a backend tells the two builds apart, and for the one window the check
// cannot close.
//
// # Why three hours
//
// The longest duty the engine claims: the learning passes tick hourly and the
// skill curator's lease survives three of those ticks, the same
// "one missed tick must not move the duty" ratio every other duty follows. An
// engine test asserts that this is exactly the longest duty TTL, so the number
// cannot drift away from the duty that justifies it. Raising it is safe on a
// running fleet (the KV backend only ever raises its duty bucket's age);
// lowering it leaves an existing bucket older than it needs to be, which costs
// nothing, because no duty record is judged by the bucket's age.
const MaxDutyTTL = 3 * time.Hour

// CheckDutyTTL refuses a claim on a duty resource whose TTL exceeds
// [MaxDutyTTL], and accepts everything else.
//
// Backends MUST call this rather than comparing the constant themselves, so the
// rule and its message have one implementation, and so a backend that can
// honour longer TTLs (the in-memory twin can honour any) still refuses the
// ones another backend cannot.
func CheckDutyTTL(resource string, ttl time.Duration) error {
	if !IsWorkerResource(resource) || ttl <= MaxDutyTTL {
		return nil
	}
	return fmt.Errorf("%w: duty %q asked for a %v lease and a duty may hold one for at most "+
		"coord.MaxDutyTTL (%v); shorten the duty's TTL, or raise coord.MaxDutyTTL together "+
		"with the duty that needs the longer lease", ErrTTLTooLong, resource, ttl, MaxDutyTTL)
}

// Lease is a held lease. Epoch is the fencing token: thread it into writes.
type Lease struct {
	Resource string
	Owner    string
	Epoch    int64

	// ExpiresAt is the UTC deadline as the STORE understands it. A
	// heartbeat computes its next tick from this. It is the store's
	// clock, never the caller's: nodes must never compare their own wall
	// clocks to decide ownership.
	ExpiresAt time.Time

	// Preferred is a stickiness hint naming the node that last held this
	// resource. It ORDERS claims and never gates them — the hint outlives
	// the node that set it, so gating on it would strand a dead node's
	// seats forever.
	Preferred string

	Protocol int

	// Meta is what the holder IS, beyond that it holds this. Node presence
	// carries the node's roles and labels here so a peer can answer "is
	// this node eligible for this seat" with no membership service. Empty
	// for everything else. A record written by a build that predates a
	// field reads as absent, which callers treat as the old behaviour
	// rather than as a node with no roles.
	//
	// A CALLER MAY NOT DEPEND ON THE GO TYPE OF A META VALUE, only on the
	// value. Backends are free to store it however they like, and the
	// shipped two disagree: the in-memory twin hands back what was
	// written, while the embedded-NATS store round-trips through JSON, so
	// a number written as an int reads back as a float64 and a []string as
	// a []any. Both are conforming — nothing in the suite requires the
	// type to survive — so meta["replicas"].(int) is correct against one
	// and panics against the other, at a call site nothing else would ever
	// exercise.
	//
	// The alternative was to REQUIRE the JSON shape, which sounds tidier
	// and would make every backend pay for a round trip nothing needs, to
	// let callers type-assert a shape that says nothing about what the
	// value means. Read a meta value the way placement.rolesFromMeta does:
	// accept either shape, and treat anything else as absent.
	Meta map[string]any
}

// EffectiveProtocol resolves the protocol a claim is made at, normalising
// the zero value to this build. Backends MUST call this rather than reading
// the field, so the safe-zero rule has one implementation.
func (o AcquireOptions) EffectiveProtocol() int {
	if o.Protocol <= 0 {
		return ProtocolVersion
	}
	return o.Protocol
}

// StoredProtocol normalises the protocol read back from a record. A record
// written before the field existed reads as the OLDEST protocol, which is
// the fail-closed reading: it holds newer nodes back rather than letting
// them claim beside a build whose meaning of ownership they cannot know.
func StoredProtocol(raw int) int {
	if raw <= 0 {
		return 1
	}
	return raw
}

// Live reports whether the lease is unexpired relative to a store-supplied
// now. Callers that have a Lease from the store already know it was live
// when read; this exists for backends and tests reasoning about records.
func (l Lease) Live(now time.Time) bool {
	return l.ExpiresAt.After(now)
}

// AcquireOptions carries the non-identity inputs to a claim.
//
// The zero value is deliberately usable and SAFE: an AcquireOptions naming
// only an owner and a TTL claims at this build's protocol, ungated only if
// the caller says so. Every field whose zero value would be dangerous says
// what its zero means.
type AcquireOptions struct {
	// Owner is the process incarnation claiming the resource.
	Owner string
	// TTL is how long the claim survives without a renew.
	TTL time.Duration
	// Preferred is the stable node id recorded as the stickiness hint.
	Preferred string
	// Protocol is the claiming build's protocol version.
	//
	// ZERO MEANS THIS BUILD (ProtocolVersion), not "oldest". The
	// distinction is the difference between a safe omission and a
	// fleet-wide stall. A named default of 1 makes leaving it out
	// harmless; a struct zero makes leaving it out the DANGEROUS case.
	// Read as
	// "oldest", a single AcquireOptions{Owner, TTL} anywhere in the
	// engine would hold a live lease below every newer node's floor and
	// stall the fleet's claims — looking exactly like a rolling upgrade
	// that never finishes.
	//
	// A STORED record with no protocol is the opposite case and still
	// reads as 1: that record genuinely predates the concept, so the
	// oldest reading is the honest one. Backends normalise on read.
	Protocol int
	// Meta rides with the record; see Lease.Meta.
	Meta map[string]any

	// Ungated skips the lower-protocol refusal, and exactly two callers
	// need it.
	//
	// Node presence: membership is not work. A newer-protocol node that
	// cannot register itself during the rolling upgrade the gate exists
	// for is invisible in the membership read — its peers then divide the
	// seats by a count that excludes it and each take a larger share,
	// while its own capacity also excludes itself.
	//
	// Singleton duties: a duty record left at protocol 1 by a build that
	// predates the gate would block every seat claim fleet-wide the moment
	// the version moved. Duty claims still carry THIS build's protocol, so
	// they never become the thing that blocks.
	Ungated bool
}

// Backend is the lease surface. Implemented by the in-memory twin, by the
// embedded KV, and by any external coordination store. All implementations
// run under ONE contract suite; a backend the suite has not certified does
// not exist.
//
// Every method follows the tri-state described in the package doc.
//
// A BACKEND MUST SERVE ITS OWN WRITES, PER RESOURCE. A claim, renew or
// release that has returned must be visible to this caller's next read of
// THAT resource. Prefix listings are free to lag: ListLive is how a node
// discovers peers, and a peer discovered a second late is a placement that
// converges a second later.
//
// It reads like an implementation detail and it is the whole basis of mutual
// exclusion: a claim you cannot read back cannot exclude anybody, and the
// seat host reads ListLive and FleetProtocolFloor immediately after claiming.
// Both certified backends make it true for free — one is a mutex over a map,
// the other a single-connection KV — so it went unstated, and the suite
// enforces it in about twenty places without ever naming it. A backend author
// who did not know would meet it as twenty failures with no common theme.
//
// What it forecloses is asynchronous replication across the coordination
// store, which nothing has asked for. Taking it up means changing this
// sentence deliberately, not discovering it.
type Backend interface {
	// TryAcquire claims resource for the owner, or reports that someone
	// else holds it.
	//
	// Succeeds when the resource is unclaimed, its lease has expired, or
	// the owner already holds it — in which case it doubles as a renew and
	// KEEPS the epoch. The epoch increments on every ownership change and
	// on a same-owner re-acquire after expiry.
	//
	// Refuses — the same (nil, nil) — while any live lease is held at a
	// lower protocol, unless Ungated. Ask FleetProtocolFloor once per
	// claim sweep to tell a protocol refusal apart from a peer simply
	// holding the resource. A duty claim may also be refused, Ungated or
	// not, during the storage-layout upgrade [MaxDutyTTL] describes.
	//
	// A duty (a `worker:` resource) is honoured at any TTL up to
	// [MaxDutyTTL] whatever TTL the backend's seat leases run on, and
	// refused beyond it with an error wrapping [ErrTTLTooLong].
	TryAcquire(ctx context.Context, resource string, opts AcquireOptions) (*Lease, error)

	// Renew extends a lease the caller already holds at this epoch.
	// Reports false when the lease is definitively no longer theirs. A
	// duty's TTL is bounded exactly as TryAcquire's is.
	Renew(ctx context.Context, resource, owner string, epoch int64, ttl time.Duration) (bool, error)

	// Release gives up a lease the caller holds.
	//
	// It must EXPIRE THE RECORD IN PLACE, never delete it: a deleted
	// record restarts the epoch counter, which is the one token a zombie
	// from the released tenure is still fencing with.
	Release(ctx context.Context, resource, owner string, epoch int64) (bool, error)

	// Get reads a resource's current lease, or nil when unheld.
	Get(ctx context.Context, resource string) (*Lease, error)

	// ListOwned returns the live leases this owner holds.
	ListOwned(ctx context.Context, owner string) ([]Lease, error)

	// ListLive returns the live leases of one resource class. The
	// membership read — ListLive(ClassNode) — is built on this.
	ListLive(ctx context.Context, class Class) ([]Lease, error)

	// PreferredResources returns resources of this class whose stickiness
	// hint names this node, INCLUDING lapsed ones: the hint's whole
	// purpose is to bring a restarted node's own seats back to it.
	PreferredResources(ctx context.Context, class Class, nodeID string) (map[string]struct{}, error)

	// FleetProtocolFloor returns the lowest protocol among live leases,
	// and whether there were any. The observability half of the gate: it
	// tells a node whether it is blocked by an older peer or simply lost
	// a race.
	FleetProtocolFloor(ctx context.Context) (int, bool, error)
}

// --- resource naming ------------------------------------------------------

// ResourceSeparator joins the segments of a resource name.
//
// A COLON, and the choice is load-bearing rather than cosmetic: a key in the
// lease and epoch buckets is built from a resource by [DocumentKey], one
// segment per part, so the leading segment — the CLASS — becomes a subject
// token of its own and a whole class is a wildcard the broker can match.
// Every listing that wants one kind of thing is that filter; before the
// resource was segmented there was no token boundary to filter on and each of
// those reads walked the bucket and discarded the rest.
const ResourceSeparator = ":"

// Class is the leading segment of a resource name — what kind of thing the
// lease is for.
//
// A named type rather than a bare string because it is the one part of a
// resource the store treats structurally, and a class that is not a single
// subject token silently selects NOTHING: the filter built from it matches no
// key, and a listing that returns nothing is indistinguishable from a class
// with no members at every caller. [Class.Valid] is what refuses one.
//
// Classes are deliberately NOT enumerated here. This package owns the three
// the fleet itself leases; the tracker's claims are leases in the same bucket
// under classes of their own, and an enumeration here would either be wrong
// or drag every caller's vocabulary into this package.
type Class string

// The classes the fleet leases directly.
const (
	ClassSeat   Class = "seat"
	ClassWorker Class = "worker"
	ClassNode   Class = "node"
)

// Valid reports whether this class can address a key.
//
// SHAPE, not membership — see the type doc for why there is no enumeration.
// A class must be non-empty and must survive the key grammar's escaping
// UNCHANGED, because a class that escapes is a class whose filter no longer
// spells the same token as its own keys: the filter is built from the class
// as written and the keys are built from it escaped, so the two stop matching
// and the listing goes quietly empty.
//
// That one test is also what refuses a class carrying the separator, which is
// why there is no second check for it: the separator is not in the literal
// set, so it always escapes, and a class containing one can never equal its
// own encoding.
func (c Class) Valid() bool {
	return c != "" && DocumentKey(string(c)) == string(c)
}

// Resource names the lease for one member of this class.
//
// Variadic because a claim is sometimes addressed by more than one part — a
// sprint rollover names a project AND a number — and every part is a segment,
// so such a claim is still filterable by its class and by its project.
func (c Class) Resource(parts ...string) string {
	return string(c) + ResourceSeparator + strings.Join(parts, ResourceSeparator)
}

// Prefix is what every resource in this class starts with.
func (c Class) Prefix() string { return string(c) + ResourceSeparator }

// Holds reports whether a resource belongs to this class.
func (c Class) Holds(resource string) bool {
	return strings.HasPrefix(resource, c.Prefix())
}

// Name recovers everything after the class, reporting false for a resource of
// another class.
//
// The whole remainder, unsplit: a caller that wants one part of a multi-part
// name knows how many there are, and a helper that guessed would turn a
// handle containing a colon into a name nobody wrote.
func (c Class) Name(resource string) (string, bool) {
	if !c.Holds(resource) {
		return "", false
	}
	return strings.TrimPrefix(resource, c.Prefix()), true
}

// CheckResource refuses a resource name that cannot address a key.
//
// EVERY SEGMENT NAMES SOMETHING — a class, a handle, a node id, a project —
// so an empty one is a caller that lost a value on the way here, and the key
// it would build is one [DocumentSegments] refuses. That refusal is why this
// exists at the surface rather than at the encoder: a lease written under a
// key nothing can decode is a lease no listing ever returns, so the seat it
// covers looks free to every node while the record sits in the bucket. A
// refusal names the value to fix; the silent version hands out one seat twice.
func CheckResource(resource string) error {
	if resource == "" {
		return fmt.Errorf("coord: a resource name is required")
	}
	for i, seg := range ResourceSegments(resource) {
		if seg == "" {
			return fmt.Errorf("coord: resource %q has an empty segment at position %d: "+
				"every %q-separated part names something, so an empty one is a "+
				"value lost on the way here", resource, i, ResourceSeparator)
		}
	}
	return nil
}

// ResourceSegments splits a resource into the segments a key is built from.
//
// The inverse of [Class.Resource]: the leading segment is the class and the
// rest is its name, however many parts that took.
func ResourceSegments(resource string) []string {
	return strings.Split(resource, ResourceSeparator)
}

// SeatResource names the lease for an agent seat.
func SeatResource(handle string) string { return ClassSeat.Resource(handle) }

// WorkerResource names the lease for a per-company singleton duty.
func WorkerResource(duty string) string { return ClassWorker.Resource(duty) }

// NodeResource names a node's own presence lease.
//
// Every node holds one, renewed on the same heartbeat as its seats.
// Counting the live ones is how a node learns the fleet size it must divide
// the seats by. There is no membership service, and inferring the count from
// SEAT ownership cannot work: a fleet where nobody has claimed anything yet
// reads as zero nodes, and every node then believes it should take every
// seat.
func NodeResource(nodeID string) string { return ClassNode.Resource(nodeID) }

// IsSeatResource reports whether a resource names a seat.
func IsSeatResource(resource string) bool { return ClassSeat.Holds(resource) }

// IsWorkerResource reports whether a resource names a fleet duty.
func IsWorkerResource(resource string) bool { return ClassWorker.Holds(resource) }

// IsNodeResource reports whether a resource names a node's presence.
func IsNodeResource(resource string) bool { return ClassNode.Holds(resource) }

// SeatHandle recovers the handle from a seat resource name.
func SeatHandle(resource string) (string, bool) { return ClassSeat.Name(resource) }

// NodeID recovers the node id from a presence resource name.
func NodeID(resource string) (string, bool) { return ClassNode.Name(resource) }
