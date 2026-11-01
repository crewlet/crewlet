package kv

import (
	"context"
	"maps"
	"slices"

	"github.com/nats-io/nats.go/jetstream"
)

// How a record family is WATCHED: every node keeps a copy of the family that
// a watch builds and then keeps current — the seat pauses, which every node
// holds a paused seat's inbox by, and the chat read state, which every node
// serving sockets pushes to that seat's tabs from.
//
// ONE implementation for every family, because what makes a watch correct is
// not the family's: it is how the first answer is certified and how a change
// the consumer delivers late is told from a change. Written once per family,
// the second copy is the one whose revision filter drifts.

// recordWatch is what one family hands [watchRecords].
type recordWatch[U any] struct {
	// what names the family in a failure: "the seat pauses".
	what string

	// ours reports whether a key is one this family writes. Any other key
	// the filter matched is skipped — a shape this build does not write
	// names no record of it — rather than failing the watch.
	ours func(key string) bool

	// update is the update one of this family's entries is: the record the
	// key now holds for a put, and its removal for a delete or a purge. An
	// error is a record this build cannot read.
	update func(kve jetstream.KeyValueEntry) (U, error)

	// undecodable reports a record this build cannot read that arrived as
	// a CHANGE, before the watch ends on it. One in the first answer fails
	// the call instead, and the caller says why.
	undecodable func(ctx context.Context, kve jetstream.KeyValueEntry, err error)

	// current is the marker that ends the first answer.
	current U
}

// watchRecords streams every record of a family that exists, the family's
// marker, then every change.
//
// # The first answer is a certified listing, and the call waits for it
//
// Everything before the marker is the answer a node REPLACES its copy with,
// so a record missing from it is one the node acts as if it were not there.
// The client's own end of the initial values cannot be that answer: it ends
// them on a count guess, while a record live throughout can still be ahead of
// the cursor, and it never ends them at all for a consumer whose pending
// messages were removed before it reached them — a removal's marker swept
// from the bucket, say — so that answer never came, and every node that met
// it waited until somebody next wrote the family. So the first answer is
// [watchUnder]'s, certified against the bucket's key index exactly as a
// listing is, and the client's marker is ignored whenever it arrives.
//
// The call returns once that answer is in hand, and a failure to certify it
// is the call's error rather than a closed channel: there is no answer for a
// consumer to have stopped hearing. A record in it this build cannot read
// fails the call the same way.
//
// # A change after the answer is the consumer's, and filtered by revision
//
// Every update after the answer comes off the same consumer, which is a
// replica's copy (walk.go): a change it delivers can be LATE, never wrong in
// order, because the consumer delivers a key's revisions in the order the
// stream holds them. The revision filter below is what keeps a late one from
// undoing a newer one the answer already holds, so the worst a lagging
// replica costs a node is the lag itself. Each family's own doc says why that
// lag is one it can carry rather than confirming every change on the leader.
func watchRecords[U any](ctx context.Context, f *FleetStore, kv jetstream.KeyValue,
	filter string, family recordWatch[U]) (<-chan U, error) {

	current, w, err := watchUnder(ctx, f.js, kv, filter, family.what)
	if err != nil {
		return nil, err
	}
	// held is the revision of each key the answer holds — a marker's too,
	// since a removal the answer saw is a revision of its key like any
	// other.
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
			// RAISED rather than skipped, as the listing raises it: a
			// record dropped from the answer is one every node acts as if
			// it were not there. The consumer watches again and says why.
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
					// The client ended the watch (a closed connection, a
					// deleted consumer). Closing is the contract's word for
					// "stopped hearing", which the consumer answers by
					// watching again — never by reading it as a removal.
					return
				}
				if kve == nil {
					// The client's end of the initial values, late or
					// never: the certified answer has already ended them.
					continue
				}
				if !family.ours(kve.Key()) {
					continue
				}
				if r, seen := held[kve.Key()]; seen && kve.Revision() <= r {
					// NOT A CHANGE: the revision the answer holds for this
					// key, or an older one — the consumer reaching a key its
					// pass was ended before, or one the leader's read was
					// ahead of it on. Sent, an older revision would undo the
					// newer one on every node until the consumer caught up.
					continue
				}
				held[kve.Key()] = kve.Revision()
				u, err := family.update(kve)
				if err != nil {
					// A record this build cannot read ends the watch rather
					// than being skipped: skipped, it would be a change every
					// node missed. The consumer watches again and the answer
					// raises the same failure.
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
