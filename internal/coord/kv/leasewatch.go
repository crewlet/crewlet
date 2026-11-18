package kv

import (
	"context"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// WatchLive streams every write to a lease of one class made after it returns.
// It watches only the class's keys ([Store.laneOf]) FROM NOW: a watch is a
// hint the caller re-lists on ([coord.LeaseView]), so it needs no certified
// first answer and is not a [watchRecords] family. The consumer exists before
// this returns, so every later write is delivered (a member behind the leader
// may also deliver a few earlier ones, harmless to a hint).
//
// Writes decode as a listing's do: a release's tombstone and a delete or purge
// marker are [coord.LeaseWrite]s with no owner; a CLAIMING record (no fencing
// token; its commit or give-back follows) is not delivered; an undecodable one
// is skipped, loudly. It costs the class's write rate on this connection.
func (s *Store) WatchLive(ctx context.Context, class coord.Class) (<-chan coord.LeaseWrite, error) {
	if err := checkClass(class); err != nil {
		return nil, err
	}
	l := s.laneOf(class)
	// STARTING IT IS BOUNDED by the store's request budget when ctx has no
	// deadline, or a silent broker would hold a lifetime-scoped caller. The
	// client reads this context only to create the consumer, so cancelling
	// it afterwards ends nothing the watch needs.
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
					// The client ended the watch (closed connection, lost
					// consumer). Closing is the contract's "stopped
					// hearing"; the consumer watches again.
					return
				}
				if kve == nil {
					// The end-of-initial-values marker, sent at once.
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
