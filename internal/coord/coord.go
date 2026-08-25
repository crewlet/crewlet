// Package coord is cross-process resource ownership: TTL leases with a
// fencing token, plus the shared counters and ledgers a fleet coordinates
// through.
//
// This is the primitive every multi-node duty is built on. A node claims a
// resource (seat:{handle}, worker:{duty}), renews it on a heartbeat, and
// loses it by crashing or releasing.
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
// conflating two of them is the single most incident-hardened lesson in the
// Python engine. Go's (value, error) expresses it natively:
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
package coord

import (
	"context"
	"errors"
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
// something merely gains a field. History carried from the Python engine:
// v2 = holding a seat means consulting the completion ledger; v3 = claiming
// a seat means this node satisfies the role's placement. Both were silent
// corruption in a mixed fleet, which is the bar.
const ProtocolVersion = 3

// ErrUnavailable is the canonical "store could not answer" error. Backends
// wrap their transport failures in it. Callers should not switch on it —
// ANY non-nil error means unknown — but it gives the common case one name
// in logs and lets tests assert the tri-state deliberately.
var ErrUnavailable = errors.New("coordination store unavailable")

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
	// fleet-wide stall, and Go moves it: Python's value was a keyword
	// default of 1, so leaving it out was harmless, while Go's is a
	// struct zero, so leaving it out is the DANGEROUS case. Read as
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
	// holding the resource.
	TryAcquire(ctx context.Context, resource string, opts AcquireOptions) (*Lease, error)

	// Renew extends a lease the caller already holds at this epoch.
	// Reports false when the lease is definitively no longer theirs.
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

	// ListLive returns live leases whose resource has this prefix. The
	// membership read — ListLive("node:") — is built on this.
	ListLive(ctx context.Context, prefix string) ([]Lease, error)

	// PreferredResources returns resources under prefix whose stickiness
	// hint names this node, INCLUDING lapsed ones: the hint's whole
	// purpose is to bring a restarted node's own seats back to it.
	PreferredResources(ctx context.Context, prefix, nodeID string) (map[string]struct{}, error)

	// FleetProtocolFloor returns the lowest protocol among live leases,
	// and whether there were any. The observability half of the gate: it
	// tells a node whether it is blocked by an older peer or simply lost
	// a race.
	FleetProtocolFloor(ctx context.Context) (int, bool, error)
}

// --- resource naming ------------------------------------------------------

const (
	seatPrefix   = "seat:"
	workerPrefix = "worker:"
	nodePrefix   = "node:"
)

// SeatResource names the lease for an agent seat.
func SeatResource(handle string) string { return seatPrefix + handle }

// WorkerResource names the lease for a per-company singleton duty.
func WorkerResource(duty string) string { return workerPrefix + duty }

// NodeResource names a node's own presence lease.
//
// Every node holds one, renewed on the same heartbeat as its seats.
// Counting the live ones is how a node learns the fleet size it must divide
// the seats by. There is no membership service, and inferring the count from
// SEAT ownership cannot work: a fleet where nobody has claimed anything yet
// reads as zero nodes, and every node then believes it should take every
// seat.
func NodeResource(nodeID string) string { return nodePrefix + nodeID }

// SeatPrefix, WorkerPrefix and NodePrefix are the ListLive arguments.
const (
	SeatPrefix   = seatPrefix
	WorkerPrefix = workerPrefix
	NodePrefix   = nodePrefix
)

// IsSeatResource reports whether a resource names a seat.
func IsSeatResource(resource string) bool { return strings.HasPrefix(resource, seatPrefix) }

// IsNodeResource reports whether a resource names a node's presence.
func IsNodeResource(resource string) bool { return strings.HasPrefix(resource, nodePrefix) }

// SeatHandle recovers the handle from a seat resource name.
func SeatHandle(resource string) (string, bool) {
	if !IsSeatResource(resource) {
		return "", false
	}
	return strings.TrimPrefix(resource, seatPrefix), true
}

// NodeID recovers the node id from a presence resource name.
func NodeID(resource string) (string, bool) {
	if !IsNodeResource(resource) {
		return "", false
	}
	return strings.TrimPrefix(resource, nodePrefix), true
}
