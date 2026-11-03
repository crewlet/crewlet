package kv

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// WatchLive streams every write to a lease of one class made after it returns.
//
// A KV watcher over the class's own keys in the one bucket the class lives in
// ([Store.laneOf]), so the broker sends this class and no other: a presence
// watch on the seat lease bucket is never handed a seat's heartbeat. FROM NOW
// (jetstream.UpdatesOnly), because a watch is a hint about what changes next
// and the caller's listing is what says what is held — so it reads nothing of
// the bucket's past, which is every lease in the class, and needs neither the
// certified pass nor the client's end-of-initial-values guess a listing does.
//
// The consumer is created before this returns, at the stream's last sequence
// as the broker places it: a write made after the call is delivered. A member
// placing it behind the leader may hand over a few writes from before the
// call as well, which a hint can only be the safer for.
//
// What it delivers is the record each write left, decoded as a listing decodes
// it: a record naming no owner — the tombstone a release writes — and a
// delete or purge marker are both [coord.LeaseWrite]s with no owner. A record
// in the CLAIMING state is not delivered: it carries no fencing token, holds
// nothing a listing would count, and is followed by the claim's commit or by
// its give-back, which is. A record that does not decode is skipped, loudly,
// as every listing skips one.
//
// It costs the class's write rate on this node's connection for as long as it
// runs: for the presence class, one message per node per presence heartbeat.
//
// NOT A [watchRecords] FAMILY, though both are watches of a bucket. What that
// one function keeps in one place is a certified FIRST ANSWER and the
// revision filter that keeps a late change from undoing it, because a family
// watched there is a copy the node replaces its own with. This watch has no
// first answer — its caller lists for that ([coord.LeaseView]) — and is a
// HINT the caller lists again on, so a late or repeated write costs a listing
// and nothing else.
func (s *Store) WatchLive(ctx context.Context, class coord.Class) (<-chan coord.LeaseWrite, error) {
	if err := checkClass(class); err != nil {
		return nil, err
	}
	l := s.laneOf(class)
	// STARTING IT IS BOUNDED whatever ctx says, by the store's own request
	// budget where ctx has no deadline: the client creates the consumer
	// under the context it is handed, and a broker that never answers would
	// otherwise hold a caller whose context is its whole lifetime. The
	// client reads that context for the creation alone, so ending it once
	// the watch is up ends nothing the watch needs.
	start, cancelStart := ctx, context.CancelFunc(func() {})
	if _, bounded := ctx.Deadline(); !bounded {
		start, cancelStart = context.WithTimeout(ctx, s.js.Options().DefaultTimeout)
	}
	w, err := l.kv.Watch(start, coord.DocumentFilter(string(class)), jetstream.UpdatesOnly())
	cancelStart()
	if err != nil {
		return nil, unavailable("watch the "+string(class)+" leases", err)
	}
	out := make(chan coord.LeaseWrite)
	go func() {
		defer close(out)
		defer func() { _ = w.Stop() }()
		for {
			select {
			case <-ctx.Done():
				return
			case kve, ok := <-w.Updates():
				if !ok {
					// The client ended the watch — a closed connection, a
					// consumer lost for good. Closing is the contract's
					// word for "stopped hearing"; the consumer watches
					// again.
					return
				}
				if kve == nil {
					// The client's end-of-initial-values marker, which a
					// watch from now sends at once.
					continue
				}
				write, ok := leaseWriteOf(ctx, kve, l)
				if !ok {
					continue
				}
				select {
				case out <- write:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// leaseWriteOf is what one message of a lease bucket says about its lease,
// and false for one that says nothing a watch delivers.
func leaseWriteOf(ctx context.Context, kve jetstream.KeyValueEntry, l *lane) (coord.LeaseWrite, bool) {
	if op := kve.Operation(); op == jetstream.KeyValueDelete || op == jetstream.KeyValuePurge {
		resource, ok := decodeResource(kve.Key())
		if !ok {
			return coord.LeaseWrite{}, false
		}
		return coord.LeaseWrite{Resource: resource}, true
	}
	e, ok := decodeEntry(kve, l)
	if !ok {
		log.WarnContext(ctx, "coord_kv_undecodable_record", "bucket", l.kv.Bucket(), "key", kve.Key())
		return coord.LeaseWrite{}, false
	}
	if e.value.Epoch == claimingEpoch {
		return coord.LeaseWrite{}, false
	}
	return coord.LeaseWrite{Resource: e.resource, Owner: e.value.Owner, Epoch: e.value.Epoch}, true
}
