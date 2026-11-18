package kv

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// claimingEpoch marks a record whose owner has won the exclusivity CAS but has
// not yet committed a fencing token. Zero, because it is never a valid token:
// a write predicated on epoch 0 matches an unset column. Such a record blocks a
// peer's claim and is never returned as a coord.Lease.
const claimingEpoch int64 = 0

// leaseValue is the JSON body of a key in the leases or the duties bucket, the
// EPHEMERAL half of a resource's state: its deadline (on the seat bucket, the
// bucket's MaxAge) ends it, which makes a dead node's seat or duty reclaimable
// and drops its presence from the listings. Its schema changes additively only,
// for the reason coord.ProtocolVersion states.
type leaseValue struct {
	// Resource repeats the escaped key for an operator reading the bucket
	// with `nats kv get`; the backend trusts only the decoded KEY.
	Resource string `json:"resource"`

	// Owner is empty in a TOMBSTONE, the record Release leaves so the lease
	// expires in place rather than being deleted. A reader treats it as
	// unheld; MaxAge reaps it, and the epochs bucket keeps its epoch.
	Owner string `json:"owner"`

	// Epoch is claimingEpoch while a claimant holds the record but has not
	// yet committed its fencing token. See TryAcquire.
	Epoch int64 `json:"epoch"`

	// Claim names the ONE CALL that wrote a record in the claiming state,
	// and is empty on every other record. The owner cannot say it: one
	// owner runs several claims of a resource at once (heartbeat, sweep,
	// recovery), so a claiming record may be a sibling's. A call that fails
	// part way gives back only a record carrying its own claim
	// ([Store.abandon]), never tearing down a sibling's; only that call
	// writes it.
	Claim string `json:"claim,omitempty"`

	// TTLNanos is the deadline the claimant asked for: on the seat bucket it
	// equals the bucket's TTL in production, on the duty bucket it is judged
	// against the store's clock (package doc, "Two lease buckets, because
	// seats and everything else want opposite TTLs"). Nanoseconds, so a
	// time.Duration round-trips exactly: a TTL truncated to zero would mint
	// an already-lapsed lease.
	TTLNanos int64 `json:"ttl_ns"`

	// AcquiredAt is the store's timestamp of the claiming write that WON
	// this tenure, carried unchanged by every renewal (coord.Lease.AcquiredAt).
	// A claiming record has none until the commit stamps it; omitzero keeps
	// the zero instant off the wire.
	AcquiredAt time.Time `json:"acquired_at,omitzero"`

	Preferred string         `json:"preferred,omitempty"`
	Protocol  int            `json:"protocol"`
	Meta      map[string]any `json:"meta,omitempty"`
}

func (v leaseValue) ttl() time.Duration { return time.Duration(v.TTLNanos) }

// resourceValue is the JSON body of a key in the epochs bucket, the
// PERSISTENT half (no TTL, never deleted). It holds what must outlive a
// tenure: the epoch, since a restarted counter would reissue a token a zombie
// still fences with, and the placement hint, which exists to bring a RESTARTED
// node's seats back and would be reaped with the lease record otherwise.
type resourceValue struct {
	Resource  string `json:"resource"`
	Epoch     int64  `json:"epoch"`
	Preferred string `json:"preferred,omitempty"`
}

// entry is one decoded lease record plus the revision a CAS is predicated on
// and the server timestamp the deadline is computed from.
type entry struct {
	resource string
	// lane is the bucket read from; it decides how expiry is judged.
	lane     *lane
	revision uint64
	// created is the store's clock at this revision. ExpiresAt derives from
	// it, never time.Now, so nodes with skewed clocks agree on expiry.
	created time.Time
	value   leaseValue
}

// lease renders the record as the contract's Lease.
func (e entry) lease() *coord.Lease {
	return &coord.Lease{
		Resource:   e.resource,
		Owner:      e.value.Owner,
		Epoch:      e.value.Epoch,
		ExpiresAt:  e.created.Add(e.value.ttl()).UTC(),
		AcquiredAt: e.value.AcquiredAt,
		Preferred:  e.value.Preferred,
		Protocol:   e.value.Protocol,
		Meta:       e.value.Meta,
	}
}

// encodeValue encodes a lease or epochs record with [coord.EncodeLeaseRecord],
// the encoding the Meta bound in [Store.TryAcquire] is measured by.
func encodeValue(v any) ([]byte, error) {
	data, err := coord.EncodeLeaseRecord(v)
	if err != nil {
		// Only caller-supplied Meta can fail here: a bad argument, not a
		// refusal.
		return nil, fmt.Errorf("coord/kv: encode record: %w", err)
	}
	return data, nil
}
