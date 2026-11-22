// How this package WATCHES a settled bucket: an ordered push consumer of its
// own, never the client's KV watch. The client fixes that consumer's heartbeat
// at five seconds and resets it when nothing arrived in two: no heartbeat
// passes a delivery still crossing, so across a link a run's value at its
// bound (three minutes raw) is cut and asked for again every ten seconds and
// never lands. Here the heartbeat is the quiet of the pass or watch reading
// it ([passQuiet]): a value lands whole before the consumer is
// presumed lost, at the cost of noticing a lost one after twice that. Entries
// are decoded as the client decodes them (nats.go jetstream/kv.go
// WatchFiltered), so a pass reads the same either way. ADR-0175 has the
// measurement.

package kv

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
)

// headerWatcher is a bucket that watches its keys' headers alone, never
// their values, beating at the quiet of the pass that reads them: what the
// marker sweep walks ([watchWalk]), and a pass of what writers stated
// ([listHeadersUnder]).
type headerWatcher interface {
	watchHeaders(ctx context.Context, keys string, beat time.Duration) (jetstream.KeyWatcher, error)
}

// Watch watches the keys under the filter keys with the bucket's own watcher
// ([settledBucket.watch]). It takes NO OPTION: none is read, and no caller
// on a settled handle passes one, so one passed is refused rather than
// dropped.
func (b settledBucket) Watch(ctx context.Context, keys string, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	if len(opts) > 0 {
		return nil, fmt.Errorf("coord/kv: a watch of %s takes no option: its own watcher reads none", b.Bucket())
	}
	return b.watch(ctx, keys, false, b.quiet(keys))
}

// WatchAll is [settledBucket.Watch] of every key.
func (b settledBucket) WatchAll(ctx context.Context, opts ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return b.Watch(ctx, jetstream.AllKeys, opts...)
}

// errClientWatch is a call the client's handle serves through its own KV
// watch, refused on a settled bucket: its heartbeat cuts a value still
// crossing a link, so a listing goes through [eachEntryUnder] and a watch
// through [settledBucket.Watch].
var errClientWatch = errors.New("served only through the client's watch, whose " +
	"five-second heartbeat cuts a value still crossing a link")

// refuseClientWatch is method of the bucket refused ([errClientWatch]).
func (b settledBucket) refuseClientWatch(method string) error {
	return fmt.Errorf("coord/kv: %s of %s: %w; list through eachEntryUnder, watch through Watch",
		method, b.Bucket(), errClientWatch)
}

// WatchFiltered is refused ([errClientWatch]): a watch takes one filter.
func (b settledBucket) WatchFiltered(context.Context, []string, ...jetstream.WatchOpt) (jetstream.KeyWatcher, error) {
	return nil, b.refuseClientWatch("WatchFiltered")
}

// Keys is refused ([errClientWatch]).
func (b settledBucket) Keys(context.Context, ...jetstream.WatchOpt) ([]string, error) {
	return nil, b.refuseClientWatch("Keys")
}

// ListKeys is refused ([errClientWatch]).
func (b settledBucket) ListKeys(context.Context, ...jetstream.WatchOpt) (jetstream.KeyLister, error) {
	return nil, b.refuseClientWatch("ListKeys")
}

// ListKeysFiltered is refused ([errClientWatch]).
func (b settledBucket) ListKeysFiltered(context.Context, ...string) (jetstream.KeyLister, error) {
	return nil, b.refuseClientWatch("ListKeysFiltered")
}

// History is refused ([errClientWatch]).
func (b settledBucket) History(context.Context, string, ...jetstream.WatchOpt) ([]jetstream.KeyValueEntry, error) {
	return nil, b.refuseClientWatch("History")
}

// watchHeaders is [settledBucket.Watch] delivering each key's headers alone,
// beating at beat: the quiet of the pass reading it, since a header waits
// behind whatever else its connection carries.
func (b settledBucket) watchHeaders(ctx context.Context, keys string, beat time.Duration) (jetstream.KeyWatcher, error) {
	return b.watch(ctx, keys, true, beat)
}

// watch subscribes an ordered push consumer to the newest message of each
// key under keys, as the client's KV watch does, with a heartbeat of beat.
// The consumer's first answer ends with a nil entry, as the client's does.
func (b settledBucket) watch(ctx context.Context, keys string, headersOnly bool,
	beat time.Duration) (jetstream.KeyWatcher, error) {

	api, err := jsapi.Of(b.js)
	if err != nil {
		return nil, err
	}
	push, err := api.Push(b.js.Conn())
	if err != nil {
		return nil, err
	}
	opts := []nats.SubOpt{nats.BindStream(bucketStream(b)), nats.OrderedConsumer(),
		nats.DeliverLastPerSubject(), nats.Context(ctx), nats.IdleHeartbeat(beat)}
	if headersOnly {
		opts = append(opts, nats.HeadersOnly())
	}
	w := &bucketWatcher{bucket: b.Bucket(), pre: bucketSubjects(b),
		updates: make(chan jetstream.KeyValueEntry, watchBuffer)}
	// Held until the first answer's size is known, so no delivery is
	// counted against it before it is.
	w.mu.Lock()
	defer w.mu.Unlock()
	sub, err := push.Subscribe(w.pre+keys, w.deliver, opts...)
	if err != nil {
		return nil, err
	}
	sub.SetClosedHandler(func(string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		close(w.updates)
	})
	if pending, known := sub.InitialConsumerPending(); known == nil {
		if pending == 0 {
			w.initDone = true
			w.updates <- nil
		} else {
			w.initPending = pending
		}
	}
	w.sub = sub
	return w, nil
}

// watchBuffer is the client's own handoff (nats.go jetstream/kv.go
// WatchFiltered): a watcher's reader that falls behind blocks its consumer.
const watchBuffer = 256

// bucketWatcher is a settled bucket's watcher ([settledBucket.watch]).
type bucketWatcher struct {
	bucket, pre string
	updates     chan jetstream.KeyValueEntry
	sub         *nats.Subscription

	mu          sync.Mutex
	initDone    bool
	initPending uint64
	received    uint64
}

// Updates is the watcher's entries; a nil one ends the first answer.
func (w *bucketWatcher) Updates() <-chan jetstream.KeyValueEntry { return w.updates }

// Stop ends the watcher's consumer, closing its updates.
func (w *bucketWatcher) Stop() error { return w.sub.Unsubscribe() }

// deliver hands one delivery on as the client's watcher would: its key off
// the subject, its revision, instant and how many follow off its metadata.
func (w *bucketWatcher) deliver(m *nats.Msg) {
	meta, err := m.Metadata()
	if err != nil {
		return
	}
	key, ok := strings.CutPrefix(m.Subject, w.pre)
	if !ok || key == "" {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.updates <- watchedEntry{leaderEntry: leaderEntry{bucket: w.bucket, key: key, value: m.Data,
		revision: meta.Sequence.Stream, created: meta.Timestamp, op: operationOfHeader(m.Header),
		header: m.Header}, delta: meta.NumPending}
	if !w.initDone {
		w.received++
		if w.received >= w.initPending || meta.NumPending == 0 {
			w.initDone = true
			w.updates <- nil
		}
	}
}

// watchedEntry is a key's message as a watch delivered it: how many newer
// messages follow it is its Delta.
type watchedEntry struct {
	leaderEntry
	delta uint64
}

func (e watchedEntry) Delta() uint64 { return e.delta }
