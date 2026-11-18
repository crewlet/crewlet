package kv

import (
	"encoding/json"
	"sync"
	"time"
)

// knownRecords is, per resource, the record this store last wrote and the
// revision the broker gave it: a renewal of a lease it holds is then one
// conditional write at that revision (package doc, "A renewal of a record this
// store last wrote is one round trip"). Only a record live while it can be
// read is kept ([liveWhilePresent]), so entries are bounded by the seat lease
// bucket's resources on this node. An entry goes on a release or false renewal
// of its tenure and on a refused or unanswered write at its revision, and is
// unused once older than the bucket's age on this node's clock, which only
// spares a write that cannot land; stamped after the broker's, it never
// overcounts. Entries hold encoded bytes, decoded per lookup: a claim hands its
// Meta map to the caller, who could otherwise change the next renewal's write.
// The zero value is ready.
type knownRecords struct {
	mu   sync.Mutex
	last map[string]knownRecord

	// now is this node's clock, read only for an entry's age; nil means
	// time.Now.
	now func() time.Time
}

// knownRecord is one resource's last write by this store.
type knownRecord struct {
	revision uint64
	// data is the record as it was encoded ([encodeValue]).
	data []byte
	// at is when the write was answered, on this node's clock.
	at time.Time
}

// liveWhilePresent reports whether a record of value on lane is live for as
// long as it can be read: the seat lease bucket's age reaps it at its deadline,
// so nothing has to compare a clock with it ([Store.held]).
func liveWhilePresent(l *lane, value leaseValue) bool {
	return l.reapsAtMax && value.ttl() >= l.maxTTL
}

func (k *knownRecords) clock() time.Time {
	if k.now != nil {
		return k.now()
	}
	return time.Now()
}

// remember records that resource's record on l is value, at revision, as this
// store just wrote or read it. A record that can lapse while still there is
// not kept, and forgets what was known before; an older revision changes
// nothing, since this store's own calls on a resource may answer out of order.
func (k *knownRecords) remember(l *lane, resource string, revision uint64, value leaseValue) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if have, ok := k.last[resource]; ok && have.revision > revision {
		return
	}
	if !liveWhilePresent(l, value) {
		delete(k.last, resource)
		return
	}
	data, err := encodeValue(value)
	if err != nil {
		// Unreachable (just written from these fields); unknown is safe,
		// since the renewal then reads the record.
		delete(k.last, resource)
		return
	}
	if k.last == nil {
		k.last = map[string]knownRecord{}
	}
	k.last[resource] = knownRecord{revision: revision, data: data, at: k.clock()}
}

// lookup is resource's record on l as this store last wrote it, while it names
// owner and is younger than the bucket's age; an entry past that age is
// forgotten.
func (k *knownRecords) lookup(l *lane, resource, owner string) (entry, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	have, ok := k.last[resource]
	if !ok {
		return entry{}, false
	}
	if k.clock().Sub(have.at) >= l.maxTTL {
		delete(k.last, resource)
		return entry{}, false
	}
	var value leaseValue
	if err := json.Unmarshal(have.data, &value); err != nil || value.Owner != owner {
		return entry{}, false
	}
	// No store timestamp (a renewal's answer carries only the revision):
	// [Store.held] judges a known record without a clock, and nothing hands
	// one out as a lease.
	return entry{resource: resource, lane: l, revision: have.revision, value: value}, true
}

// forgetAt forgets resource's record if what is known of it is still the
// write at revision: a write at that revision was refused, or nobody answered
// it. A newer write a concurrent call made is kept.
func (k *knownRecords) forgetAt(resource string, revision uint64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if have, ok := k.last[resource]; ok && have.revision == revision {
		delete(k.last, resource)
	}
}

// forgetTenure forgets resource's record if it names owner at epoch: that
// tenure was released, or a renewal of it was answered false. A later tenure
// of the same owner is kept.
func (k *knownRecords) forgetTenure(resource, owner string, epoch int64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	have, ok := k.last[resource]
	if !ok {
		return
	}
	var value leaseValue
	if err := json.Unmarshal(have.data, &value); err != nil ||
		(value.Owner == owner && value.Epoch == epoch) {
		delete(k.last, resource)
	}
}
