// How this package reads a whole bucket, and why it is ONE ORDERED PASS
// rather than a listing plus a fetch per key.
//
// A KV bucket is a stream, and its keys are subjects under it. The obvious
// listing — ask for the key names, then Get each one — costs a round trip per
// key ON TOP of an ordered ephemeral consumer created and destroyed per call:
// two JetStream metadata proposals on a clustered bucket, for a question that
// is one pass over the stream. Several of a node's fifteen-second duty loops
// read a bucket on every tick and the state-log write fence reads the
// positions register on every first write to a subject, so that cost is paid
// continuously rather than at the edges.
//
// So the walk is ONE pass that carries each key AND its value together, and
// the filter goes to the BROKER rather than to a client-side test.
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
//     quorum already has. Most of this estate would survive that — a liveness
//     judgement decides what to ATTEMPT and the CAS at the revision it read
//     decides what happens — but two reads have no arbitration behind them.
//     The protocol gate lets a claim through a mixed-version upgrade when it
//     misses an older-protocol peer, and the trim floor takes a MINIMUM
//     across the positions rows, so a row it cannot see RAISES the floor and
//     deletes log records a node has not applied yet. That one is data loss.
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

package kv

import (
	"context"
	"errors"

	"github.com/nats-io/nats.go/jetstream"
)

// eachEntry walks the latest revision of every LIVE key in a bucket, handing
// each entry — its key AND its value together — to visit.
//
// A visit that returns an error stops the walk and that error is returned
// unwrapped, so a caller keeps its own vocabulary rather than having this
// function guess at it. Deletes are filtered, so a tombstoned key is never
// visited and no caller has to recognise one.
func eachEntry(ctx context.Context, kv jetstream.KeyValue,
	visit func(jetstream.KeyValueEntry) error) error {

	return eachEntryUnder(ctx, kv, jetstream.AllKeys, kv.Bucket(), visit)
}

// eachEntryUnder is [eachEntry] over the keys matching one filter.
//
// THE BROKER DOES THE FILTERING, which is the whole point. A key is a subject
// token path under its bucket (coord/keys.go), so a class of keys written by
// [coord.DocumentKey] is a subject wildcard the broker can match — and the
// shared positions register holds SEVEN classes, so a walk that read the whole
// bucket moved all seven to use one. That read is not an edge case: the
// state-log write fence takes it on every first write to a subject.
//
// The filter is in the KEY's vocabulary rather than the subject's — "floor.>"
// and not "$KV.crewlet_x.floor.>" — because a key is what the caller holds and
// what the watcher takes. Each walk composes its own, and there is
// deliberately no default: an empty filter read as "everything" would turn a
// caller that lost its class value into one that walks the whole register and
// decodes seven classes as one.
//
// `what` names the listing a failure could not complete — the bare name, which
// every message composes into "read <what>" — and a filtered walk names its
// LISTING rather than its bucket. Seven classes share the positions register,
// so "read crewlet_positions" is the same sentence for all of them: it names
// the file an operator would inspect and never the duty that stalled.
func eachEntryUnder(ctx context.Context, kv jetstream.KeyValue,
	keys, what string, visit func(jetstream.KeyValueEntry) error) error {

	return watchWalk(ctx, kv, keys, what, visit)
}

// eachKeyUnder is [eachEntryUnder] without the values: the broker sends each
// matching key's headers and no body.
//
// For a listing whose answer is the KEYS — which records exist, how many,
// what their numbers are — over records whose values can each be megabytes.
// Reading those values to throw them away would move the whole log to answer
// a question about its shape.
func eachKeyUnder(ctx context.Context, kv jetstream.KeyValue,
	keys, what string, visit func(key string) error) error {

	return watchWalk(ctx, kv, keys, what, func(kve jetstream.KeyValueEntry) error {
		return visit(kve.Key())
	}, jetstream.MetaOnly())
}

// watchWalk walks a bucket over an ordered ephemeral consumer.
//
// The one transport every listing in this package takes; the file doc says
// why the batched direct read is not another. It carries each key with its
// value (or, with [jetstream.MetaOnly] in opts, with its headers alone), so
// there is no Get per name here, and its honesty rule is that the nil entry —
// and ONLY the nil entry — ends the walk, and a CLOSED CHANNEL is a failure
// named as one.
//
// It also owns the watcher, so there is no early-return path that leaks one —
// the abandoned-listing case the client's blocking 256-entry handoff could
// park a goroutine and a server-side consumer on for ever.
func watchWalk(ctx context.Context, kv jetstream.KeyValue, keys, what string,
	visit func(jetstream.KeyValueEntry) error, opts ...jetstream.WatchOpt) error {

	// Watch rather than WatchAll, so this transport narrows server-side too.
	// A filter both walks honour is what keeps them interchangeable: one that
	// narrowed and one that did not would hand a caller a different bucket
	// depending on which answered.
	w, err := kv.Watch(ctx, keys, append([]jetstream.WatchOpt{jetstream.IgnoreDeletes()}, opts...)...)
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
