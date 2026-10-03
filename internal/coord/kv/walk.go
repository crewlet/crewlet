// How this package reads a whole bucket, and why it is ONE ORDERED PASS
// closed by the stream leader rather than a listing plus a fetch per key.
//
// A KV bucket is a stream, and its keys are subjects under it. The obvious
// listing — ask for the key names, then Get each one — costs a round trip per
// key ON TOP of an ordered ephemeral consumer created and destroyed per call:
// two JetStream metadata proposals on a clustered bucket, for a question that
// is one pass over the stream. Several of a node's fifteen-second duty loops
// read a bucket on every tick and the state-log write fence lists the trim
// floors out of the positions register on every write at an expectation of
// zero, so that cost is paid continuously rather than at the edges.
//
// So the walk is ONE pass that carries each key AND its value together, and
// the filter goes to the BROKER rather than to a client-side test — followed by
// ONE question to the stream leader, because the pass is not served by it.
//
// # The batched direct read, and why it is not here
//
// `$JS.API.DIRECT.GET` with `multi_last` asks for the last message on every
// matching subject in ONE request/reply, with no consumer at all — which is
// exactly the shape of this question, and it was tried. It is not here, for
// two independent reasons, either of which alone is disqualifying:
//
//   - IT IS SERVED BY ANY REPLICA. That is the whole reason it is fast, and
//     it is why internal/queue/jetstream/stream.go sets allowDirect false on
//     every stream the framework reads a last-message answer from: a follower
//     may be behind an acknowledged write, so the read can miss a record the
//     quorum already has. This doc used to hold that most of this estate would
//     survive that, a liveness judgement deciding only what to attempt and a
//     compare-and-set deciding what happens. It does not survive it — see
//     bucket.go for what a stale replica did to a counter, a create race and
//     a run just written — and the two reads with no arbitration behind them
//     at all are worse still: the protocol gate lets a claim through a
//     mixed-version upgrade when it misses an older-protocol peer, and the
//     trim floor takes a MINIMUM across the positions rows, so a row it cannot
//     see RAISES the floor and deletes log records a node has not applied
//     yet. That one is data loss.
//
//   - IT ANSWERED WRONG ON ONE REPLICA, which is the reason that does not
//     depend on the first. A KV bucket keeps one message per subject, so
//     every Put deletes the one before it, and the server resolves multi_last
//     through a per-subject last-BLOCK index that this churn leaves stale
//     (filestore.go's multiLastSeqsByLastBlockLocked carries the unresolved
//     case explicitly). Measured against a single-node embedded broker, a
//     read of a populated key class came back EMPTY several times a run — and
//     an empty answer is the one failure this estate cannot survive, because
//     "no rows" is a legitimate answer everywhere it is asked.
//
// Do not reintroduce it without an answer to both. A benchmark is not one.
//
// # The pass is served by a replica too, so the leader closes it
//
// The ordered pass is an R1 consumer, and on a replicated bucket the server
// places it on a RANDOM member of the stream (server/jetstream_cluster.go,
// createGroupForConsumer), which delivers out of its own copy. A follower that
// has not yet applied an acknowledged write hands over the bucket as it was
// before that write — measured: up to 3 of every 100 passes hosted on a
// follower missed the newest write, and none hosted on the leader did. That is
// the first reason above arriving through the transport that replaced the
// batched read: the trim floor reads one row too few, and the listing a seat
// pause watch starts from misses the pause just taken.
//
// So a pass is CLOSED by the leader. Its messages arrive in stream order, so
// the highest sequence it delivered says how far its copy reached, and
// everything that copy had not applied lies beyond it. One read asks the
// leader for the first message in the class past that sequence: none, and the
// pass was as current as the leader — the ordinary answer, one round trip.
// Some, and the walk reads on from the leader, message by message, up to the
// class's newest message as the leader held it when the pass was found short,
// applying each over what the pass delivered. Every write acknowledged before
// the walk began is at or below that barrier, which is what makes the answer
// read-your-writes rather than "the bucket as some replica last saw it".
//
// The walk therefore MATERIALISES before it visits: an entry the pass handed
// over may be superseded or removed by one the leader adds, so nothing is
// visited until both halves are in. And the pass keeps the REMOVALS — a delete
// or purge marker is how the leader's half says that a key the pass still had
// is gone, and a marker dropped on the way in could never be told from a
// write that did not happen. A key is visited at its newest revision, in
// revision order, which is the order the pass alone used to deliver.
//
// A bucket's AGE is the one removal no message records: a record the leader
// has reaped and a lagging copy has not is still visited. That is a clock
// event rather than an acknowledged write, and every reader of an aged bucket
// already judges what it reads against the store's own clock.

package kv

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/nats-io/nats.go/jetstream"
)

// eachEntry walks the latest revision of every LIVE key in a bucket, handing
// each entry — its key AND its value together — to visit.
//
// A visit that returns an error stops the walk and that error is returned
// unwrapped, so a caller keeps its own vocabulary rather than having this
// function guess at it. Removals are filtered, so a removed key is never
// visited and no caller has to recognise one.
func eachEntry(ctx context.Context, b *leaderBucket,
	visit func(jetstream.KeyValueEntry) error) error {

	return eachEntryUnder(ctx, b, jetstream.AllKeys, b.Bucket(), visit)
}

// eachEntryUnder is [eachEntry] over the keys matching one filter.
//
// THE BROKER DOES THE FILTERING, which is the whole point. A key is a subject
// token path under its bucket (coord/keys.go), so a class of keys written by
// [coord.DocumentKey] is a subject wildcard the broker can match — and the
// shared positions register holds EIGHT classes, so a walk that read the whole
// bucket moved all eight to use one. That read is not an edge case: the
// state-log write fence takes it, for the floors, on every write at an
// expectation of zero.
//
// The filter is in the KEY's vocabulary rather than the subject's — "floor.>"
// and not "$KV.crewlet_x.floor.>" — because a key is what the caller holds and
// what the watcher takes. Each walk composes its own, and there is
// deliberately no default: an empty filter read as "everything" would turn a
// caller that lost its class value into one that walks the whole register and
// decodes eight classes as one.
//
// `what` names the listing a failure could not complete — the bare name, which
// every message composes into "read <what>" — and a filtered walk names its
// LISTING rather than its bucket. Eight classes share the positions register,
// so "read crewlet_positions" is the same sentence for all of them: it names
// the file an operator would inspect and never the duty that stalled.
func eachEntryUnder(ctx context.Context, b *leaderBucket,
	keys, what string, visit func(jetstream.KeyValueEntry) error) error {

	if !validFilter(keys) {
		return fmt.Errorf("coord/kv: read %s: %q is not a key filter", what, keys)
	}
	v := newView()
	if err := orderedPass(ctx, b.client, keys, what, func(e jetstream.KeyValueEntry) error {
		v.apply(e)
		return nil
	}); err != nil {
		return err
	}
	if err := b.closeOnLeader(ctx, keys, what, v); err != nil {
		return err
	}
	for _, e := range v.entries() {
		if err := visit(e); err != nil {
			return err
		}
	}
	return nil
}

// orderedPass walks a bucket over an ordered ephemeral consumer, handing every
// message it delivers — removals included — to visit.
//
// The first half of a walk: the file doc says why the batched read is not the
// transport, and why this half alone is not the answer. It carries the key AND
// the value together, so there is still no Get per name here, and its honesty
// rule is the whole of it: the nil entry — and ONLY the nil entry — ends the
// pass, and a CLOSED CHANNEL is a failure named as one.
//
// It also owns the watcher, so there is no early-return path that leaks one —
// the abandoned-listing case the client's blocking 256-entry handoff could
// park a goroutine and a server-side consumer on for ever.
func orderedPass(ctx context.Context, kv clientBucket, keys, what string,
	visit func(jetstream.KeyValueEntry) error) error {

	// Watch rather than WatchAll, so the pass narrows server-side too. And
	// WITHOUT IgnoreDeletes, which this used to ask for: a removal is a
	// message the leader's half may need to set against what the pass
	// delivered, and [view] is where removals are filtered now.
	w, err := kv.Watch(ctx, keys)
	if err != nil {
		return unavailable("read "+what, err)
	}
	defer func() { _ = w.Stop() }()

	for {
		select {
		case <-ctx.Done():
			return unavailable("read "+what, ctx.Err())
		case kve, ok := <-w.Updates():
			if !ok {
				return unavailable("read "+what, errors.New("listing ended early"))
			}
			// nil marks the end of the initial values.
			if kve == nil {
				return nil
			}
			if err := visit(kve); err != nil {
				return err
			}
		}
	}
}

// closeOnLeader brings a pass up to what the stream leader holds: the second
// half of a walk, and the reason a walk can be trusted on a replicated bucket.
// See the file doc.
func (b *leaderBucket) closeOnLeader(ctx context.Context, keys, what string, v *view) error {
	filter := b.prefix + keys
	next, found, err := b.ask(ctx, msgGetRequest{Seq: v.high + 1, NextFor: filter})
	if err != nil {
		return unavailable("read "+what, err)
	}
	if !found {
		// The ordinary answer: nothing in this class past what the pass
		// delivered, so the pass was as current as the leader.
		return nil
	}
	// SHORT. The pass's copy had not applied everything the leader holds,
	// so the leader is read from here on — up to the class's newest message
	// as it stands NOW, which is what bounds the read: a class written
	// faster than it is read would otherwise never let the walk end.
	newest, found, err := b.ask(ctx, msgGetRequest{LastFor: filter})
	if err != nil {
		return unavailable("read "+what, err)
	}
	barrier := next.Sequence
	if found {
		barrier = max(barrier, newest.Sequence)
	}
	for {
		e, err := b.entryOf(next)
		if err != nil {
			return unavailable("read "+what, err)
		}
		v.apply(e)
		if next.Sequence >= barrier {
			return nil
		}
		next, found, err = b.ask(ctx, msgGetRequest{Seq: next.Sequence + 1, NextFor: filter})
		if err != nil {
			return unavailable("read "+what, err)
		}
		if !found || next.Sequence > barrier {
			return nil
		}
	}
}

// view is a walk's picture of a bucket while it is being assembled: the live
// value of every key, and the newest revision applied to each, a removal's
// included.
type view struct {
	live map[string]jetstream.KeyValueEntry
	last map[string]uint64
	// high is the highest sequence applied, which is how far the copy the
	// pass read had reached.
	high uint64
}

func newView() *view {
	return &view{live: map[string]jetstream.KeyValueEntry{}, last: map[string]uint64{}}
}

// apply folds one message into the view. A revision no newer than one already
// applied to its key changes nothing, so the order the two halves arrive in
// cannot put an older value back.
func (v *view) apply(e jetstream.KeyValueEntry) {
	v.high = max(v.high, e.Revision())
	key := e.Key()
	if e.Revision() <= v.last[key] {
		return
	}
	v.last[key] = e.Revision()
	if e.Operation() == jetstream.KeyValuePut {
		v.live[key] = e
	} else {
		delete(v.live, key)
	}
}

// entries is every live key at its newest revision, in revision order.
func (v *view) entries() []jetstream.KeyValueEntry {
	out := make([]jetstream.KeyValueEntry, 0, len(v.live))
	for _, e := range v.live {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b jetstream.KeyValueEntry) int {
		return cmp.Compare(a.Revision(), b.Revision())
	})
	return out
}
