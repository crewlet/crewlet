package kv

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// claimingEpoch marks a record whose owner has won the exclusivity CAS but has
// not yet committed a fencing token.
//
// Zero is exactly the right marker because it is exactly the wrong token: a
// conditional write predicated on epoch 0 matches an unset column, so no caller
// can ever be handed one and no write can ever be fenced by one. A record in
// this state blocks a peer's claim (it names an owner) and is never returned as
// a coord.Lease.
const claimingEpoch int64 = 0

// layoutDutyLane is the storage layout of a build that keeps duty leases in the
// duty bucket rather than in the seat lease bucket, and every record this build
// writes carries it.
//
// A LAYOUT RATHER THAN A PROTOCOL BUMP. What moved is where a duty is written,
// not what holding any lease means, and coord.ProtocolVersion's gate refuses
// SEAT claims beside an older peer: bumping it would have stalled seat
// placement for a whole rolling upgrade to fix a duty. The layout gates only
// duty claims, and only while a record by an older build is live. A record
// written before the field existed reads as zero, which is the fail-closed
// reading. See the package doc's rolling-upgrade rule.
const layoutDutyLane = 1

// leaseValue is the JSON body of a key in the leases or the duties bucket, the
// EPHEMERAL half of a resource's state. Its deadline, or on the seat lease
// bucket the bucket's MaxAge, ends it, which is exactly what makes a dead
// node's seat or duty reclaimable.
//
// Schema evolution here is additive-only, for the reason coord.ProtocolVersion
// states: a rolling upgrade has two builds reading each other's records, and a
// field an older build requires is a crash rather than a missing feature.
type leaseValue struct {
	// Resource is redundant with the key, which is escaped and therefore
	// not immediately readable. It is here for the operator running
	// `nats kv get crewlet_leases seat=3Aalice` at 3am; the decoded KEY is
	// what the backend trusts.
	Resource string `json:"resource"`

	// Owner is empty in a TOMBSTONE — the record Release leaves behind so
	// the lease expires in place instead of being deleted. A reader treats
	// an empty owner as unheld; the bucket's MaxAge reaps the tombstone
	// later, and the epoch it was written at is untouched in the other
	// bucket.
	Owner string `json:"owner"`

	// Epoch is claimingEpoch while a claimant holds the record but has not
	// yet committed its fencing token. See TryAcquire.
	Epoch int64 `json:"epoch"`

	// TTLNanos is the deadline the claimant asked for. On the seat lease
	// bucket it equals the bucket's own TTL in every production path, in which
	// case the record's disappearance IS its expiry and nothing consults a
	// clock. On the duty bucket it is always judged against the store's clock.
	// See the package doc on the two lease buckets.
	//
	// Nanoseconds rather than the milliseconds an operator would rather
	// read: time.Duration IS nanoseconds, so this round-trips exactly, and
	// a TTL that truncated to zero on the way in would mint a lease that
	// was already lapsed — a seat nobody can hold.
	TTLNanos int64 `json:"ttl_ns"`

	Preferred string         `json:"preferred,omitempty"`
	Protocol  int            `json:"protocol"`
	Meta      map[string]any `json:"meta,omitempty"`

	// Layout is the storage layout of the build that wrote the record; see
	// layoutDutyLane. Zero on a record from a build that predates it.
	Layout int `json:"layout,omitempty"`
}

func (v leaseValue) ttl() time.Duration { return time.Duration(v.TTLNanos) }

// resourceValue is the JSON body of a key in the epochs bucket — the
// PERSISTENT half, which has no TTL and is never deleted.
//
// It holds the two facts that must outlive a tenure. The epoch, because a
// counter that restarts hands the next owner a token a zombie from the
// previous tenure is still fencing its writes with. And the placement hint,
// because the hint's whole purpose is to bring a RESTARTED node's own seats
// back to it — a hint that lived only in the lease record would be reaped
// with it, and would therefore answer nothing in exactly the case it exists
// for.
type resourceValue struct {
	Resource  string `json:"resource"`
	Epoch     int64  `json:"epoch"`
	Preferred string `json:"preferred,omitempty"`
}

// entry is one decoded lease record plus the store metadata every write needs:
// the revision a CAS is predicated on, and the server-assigned timestamp the
// deadline is computed from.
type entry struct {
	resource string
	// lane is the bucket the record was read from, which decides how its
	// expiry is judged and against whose clock.
	lane     *lane
	revision uint64
	// created is the store's own clock at the moment of this revision.
	// coord.Lease.ExpiresAt is derived from it and never from time.Now, so
	// two nodes with skewed clocks still agree on when a lease ends.
	created time.Time
	value   leaseValue
}

// lease renders the record as the contract's Lease.
func (e entry) lease() *coord.Lease {
	return &coord.Lease{
		Resource:  e.resource,
		Owner:     e.value.Owner,
		Epoch:     e.value.Epoch,
		ExpiresAt: e.created.Add(e.value.ttl()).UTC(),
		Preferred: e.value.Preferred,
		Protocol:  coord.StoredProtocol(e.value.Protocol),
		Meta:      e.value.Meta,
	}
}

func encodeValue(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		// Only reachable through Meta, which is caller-supplied and may
		// hold something JSON cannot express. That is an argument the
		// caller got wrong, so it must not read as a refusal.
		return nil, fmt.Errorf("coord/kv: encode record: %w", err)
	}
	return data, nil
}
