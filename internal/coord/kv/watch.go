package kv

import (
	"context"
	"maps"
	"slices"

	"github.com/nats-io/nats.go/jetstream"
)

// How a record family is WATCHED: every node keeps a copy of the family that a
// watch builds and keeps current (the seat pauses, the chat read state). ONE
// implementation for every family, because a watch's correctness is how the
// first answer is certified and a late change told from a new one, not the
// family's: a second copy is the one whose revision filter drifts.

// recordWatch is what one family hands [watchRecords].
type recordWatch[U any] struct {
	// what names the family in a failure: "the seat pauses".
	what string

	// ours reports whether a key is one this family writes. Any other key
	// the filter matched (a shape this build does not write) is skipped,
	// not fatal.
	ours func(key string) bool

	// update is an entry's update: the key's record for a put, its removal
	// for a delete or purge; an error is a record this build cannot read.
	update func(kve jetstream.KeyValueEntry) (U, error)

	// undecodable reports an unreadable record that arrived as a CHANGE,
	// before the watch ends on it; one in the first answer fails the call.
	undecodable func(ctx context.Context, kve jetstream.KeyValueEntry, err error)

	// current is the marker that ends the first answer.
	current U
}

// watchRecords streams every record of a family that exists, the family's
// marker, then every change. Everything before the marker is the answer a node
// REPLACES its copy with (a record missing from it is gone to that node), so it
// is [watchUnder]'s certified listing, not the client's marker: that ends on a
// count guess, or never once a consumer's pending messages are removed, leaving
// nodes waiting. The call returns once that answer is in hand; failing to
// certify it, or an unreadable record in it, is the call's error, not a closed
// channel. Later changes come off the same consumer, a replica's copy (package
// doc, "A tombstone the pass delivered is not an answer"): LATE but in order per
// key, so the revision filter keeps a late one from undoing a newer one the
// answer holds. Each family's doc says why it can carry that lag.
func watchRecords[U any](ctx context.Context, f *FleetStore, kv jetstream.KeyValue,
	filter string, family recordWatch[U]) (<-chan U, error) {

	current, w, err := watchUnder(ctx, f.js, kv, filter, family.what)
	if err != nil {
		return nil, err
	}
	// held is each key's revision in the answer, a removal marker's too.
	held := make(map[string]uint64, len(current))
	answer := make([]U, 0, len(current)+1)
	for _, key := range slices.Sorted(maps.Keys(current)) {
		kve := current[key]
		held[key] = kve.Revision()
		if !family.ours(key) || kve.Operation() != jetstream.KeyValuePut {
			continue
		}
		u, err := family.update(kve)
		if err != nil {
			// RAISED, as the listing raises it: a record dropped from the
			// answer is one every node acts as if it were not there.
			_ = w.Stop()
			return nil, err
		}
		answer = append(answer, u)
	}
	answer = append(answer, family.current)

	out := make(chan U)
	go func() {
		defer close(out)
		defer func() { _ = w.Stop() }()
		send := func(u U) bool {
			select {
			case out <- u:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, u := range answer {
			if !send(u) {
				return
			}
		}
		for {
			select {
			case <-ctx.Done():
				return
			case kve, ok := <-w.Updates():
				if !ok {
					// The client ended the watch. Closing means "stopped
					// hearing": the consumer watches again, never reads it
					// as a removal.
					return
				}
				if kve == nil {
					// The client's end of initial values: the certified
					// answer already ended them.
					continue
				}
				if !family.ours(kve.Key()) {
					continue
				}
				if r, seen := held[kve.Key()]; seen && kve.Revision() <= r {
					// NOT A CHANGE: a revision the answer already holds or
					// passed (a key the pass ended before, or the leader's
					// read ran ahead of the consumer).
					// Sent, it would undo the newer one on every node until
					// the consumer caught up.
					continue
				}
				held[kve.Key()] = kve.Revision()
				u, err := family.update(kve)
				if err != nil {
					// An unreadable record ends the watch: skipped, it would
					// be a change every node missed. Watched again, the
					// answer raises the same failure.
					family.undecodable(ctx, kve, err)
					return
				}
				if !send(u) {
					return
				}
			}
		}
	}()
	return out, nil
}

// removed reports an entry that is a key's removal rather than a record.
func removed(kve jetstream.KeyValueEntry) bool {
	op := kve.Operation()
	return op == jetstream.KeyValueDelete || op == jetstream.KeyValuePurge
}
