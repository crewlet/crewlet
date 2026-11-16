package kv

import (
	"encoding/json"
	"sync"
	"time"
)

// knownRecords is the last record this store wrote of each resource whose
// record is live for as long as it can be read, and the revision it wrote it
// at: what lets a renewal of a lease this store holds be ONE ROUND TRIP, its
// conditional write at that revision, where reading the record first made it
// two (see the package doc's "A renewal of a record this store last wrote").
//
// # Why a write at the known revision is the whole decision
//
// A renewal reads the record to learn three things: that it is still there,
// that it still names this owner at this epoch, and the revision to write at.
// Every revision of a key is a stream sequence, which the broker never hands
// out twice. So an update conditioned on the revision this store's own last
// write was given LANDS ONLY IF THE RECORD IS STILL EXACTLY THAT WRITE: no
// peer's claim, no release, no reap and no sibling's renewal has touched it
// since. That answers the first two questions by construction, and it is the
// revision. A write that is refused on the comparison learns nothing else, and
// the renewal reads the record and decides as it always did.
//
// # Only a record that cannot lapse while it can be read
//
// "Still there" means "still held" only for a record whose disappearance IS
// its expiry: on the seat lease bucket, one taken at the bucket's whole age,
// which every seat and presence lease is in production ([lane.reapsAtMax];
// [Store.held] consults no clock for one). A record judged by a deadline
// against the store's clock — every record in the duty bucket, and a seat
// lease asked for less than its bucket's age — can lapse and still be there,
// and a lapsed lease is deliberately NOT renewable: re-acquiring mints the
// epoch that fences the gap. A write at its revision would renew it. So such a
// record is never known here ([liveWhilePresent]), and its renewal always
// reads the store's clock first.
//
// # Forgotten when it can no longer be right, and never trusted past its age
//
// An entry goes on a release of the tenure it names, on a renewal answered
// false, and on a write at its revision that is refused or that nobody
// answered — each a moment the record may no longer be the one known. It is
// also not used once it is older, on this node's clock, than the bucket's age:
// by then the broker has reaped the record unless something renewed it, and a
// write at its revision could only be refused. That clock decides nothing
// about ownership — the compare-and-set does — it only spares a write that
// cannot land. An entry is stamped when its write is answered, after the
// broker stamped the record, so the age read here never undercounts the
// broker's; at worst the write is sent and refused.
//
// Bounded by the resources the seat lease bucket holds — this node's seats and
// its own presence — so an entry left by a lease this node stopped renewing
// without a release (a seat dropped as unrenewable) is one per seat and is
// replaced or forgotten the next time the seat is held here.
//
// # The bytes, never the value handed out
//
// Each entry keeps the record as it was encoded and decodes a fresh copy on
// every lookup, because the value a claim answers carries its Meta map to the
// caller as the lease's own: kept by reference, a caller that changed the map
// it was handed would change what the next renewal writes.
//
// The zero value is ready: the map is made on the first write, and the clock
// is the machine's.
type knownRecords struct {
	mu   sync.Mutex
	last map[string]knownRecord

	// now is this node's clock, which only an entry's age is read on. Nil
	// means time.Now; a test sets one it moves.
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
// store just wrote or read it. A record that can lapse while it is still there
// is not known, and forgets what was known of the resource before it; a write
// older than the one already known changes nothing, since two of this store's
// own calls on one resource may answer out of order.
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
		// The value was just written from these same fields, so this is
		// unreachable; unknown is the safe answer either way, since the
		// renewal then reads the record.
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
	// No store timestamp: a renewal's answer carries only the revision. It
	// is never read for a known record, which [Store.held] judges without a
	// clock, and nothing hands a known record out as a lease.
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
