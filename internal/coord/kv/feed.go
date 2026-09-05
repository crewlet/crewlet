package kv

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// FeedDocuments opens a durable, fleet-wide feed over one key class.
//
// # The consumer, and every setting on it
//
// A KV bucket is a stream named KV_<bucket> whose subjects are
// $KV.<bucket>.<key>, so a class filter is one subject filter and needs no
// decoding: a consumer on `$KV.<bucket>.c.>` sees every change key and no
// heads, comments or counters. That is what makes the class the first token
// of the key grammar.
//
// PULL rather than push, because the consumer is shared by every node: a push
// consumer delivers to one subscriber, and the fleet needs whichever node
// asks next to get the work. DELIVERNEW at creation, because an upgrade that
// introduced a feed must not wake every seat for every change the company
// ever made — an existing consumer keeps the position the fleet left it at,
// which is what a rolling restart depends on.
func (f *FleetStore) FeedDocuments(ctx context.Context, family coord.Family, class, group string) (coord.Feed, error) {
	suffix, err := suffixFor(family)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(class) == "" {
		return nil, errors.New("coord/kv: a feed needs a key class to filter on")
	}
	if strings.TrimSpace(group) == "" {
		return nil, errors.New("coord/kv: a feed needs a durable name")
	}
	bucket := f.bucketPrefix + suffix
	stream := "KV_" + bucket
	filter := "$KV." + bucket + "." + class + ".>"

	consumer, err := f.js.CreateOrUpdateConsumer(ctx, stream, jetstream.ConsumerConfig{
		Durable:       group,
		FilterSubject: filter,
		AckPolicy:     jetstream.AckExplicitPolicy,

		// DELIVERNEW is honoured only at CREATION — an update ignores it,
		// which is exactly right: the first node to reach here sets the
		// starting point and every later one adopts the fleet's position.
		DeliverPolicy: jetstream.DeliverNewPolicy,

		// The ack window has to fit the whole handler: a claim on the
		// coordination store, a publish to the inbound topic, and the
		// publish's own ack. Thirty seconds is far past all three on any
		// healthy estate and short enough that a node that died mid-handle
		// hands the change to a peer within one window rather than
		// stalling the company's wakes.
		AckWait: feedAckWait,

		// MaxAckPending bounds how many changes are in flight across the
		// WHOLE FLEET, because the consumer is shared. Two hundred and
		// fifty-six is a burst of work larger than any single write
		// produces and small enough that a wedged node cannot hold the
		// company's entire backlog un-acked.
		MaxAckPending: feedMaxAckPending,

		// A change that has been redelivered this many times is not going
		// to succeed: it goes to the dead-letter path rather than
		// circling for ever. The broker's default is unlimited, which is
		// how one malformed record stops a feed permanently.
		MaxDeliver: feedMaxDeliver,
	})
	if err != nil {
		return nil, unavailable("open the "+string(family)+" feed", err)
	}

	// THE ITERATOR, NOT Fetch. Fetch blocks for its whole max-wait and
	// takes no context, so a shutdown landing a millisecond into a pull
	// waits the pull out — five seconds added to every node stop, on every
	// feed, measured. Messages() has a Stop that ends a blocked Next at
	// once, which is what makes the feed's lifetime the caller's context
	// rather than a timer's.
	iter, err := consumer.Messages(jetstream.PullMaxMessages(feedFetchBatch))
	if err != nil {
		return nil, unavailable("read the "+string(family)+" feed", err)
	}
	feed := &kvFeed{iter: iter, family: family, closed: make(chan struct{})}

	// The context is turned into a Stop by a goroutine, because the
	// iterator has no context of its own. It exits with the feed, so a
	// long-lived process holds one goroutine per feed rather than one per
	// read.
	go func() {
		select {
		case <-ctx.Done():
			feed.stop()
		case <-feed.closed:
		}
	}()
	return feed, nil
}

const (
	// feedAckWait bounds one change's handling. See the config above.
	feedAckWait = 30 * time.Second

	// feedMaxAckPending bounds in-flight changes across the fleet.
	feedMaxAckPending = 256

	// feedMaxDeliver bounds redelivery of one change.
	//
	// Five. A change that four peers could not handle is a record this
	// build cannot process, not a transient failure — and the broker's
	// unlimited default is how one malformed record stops a feed for ever.
	feedMaxDeliver = 5

	// feedFetchBatch is how many changes the iterator keeps in flight.
	//
	// Thirty-two. The iterator pulls ahead, so this is a buffer rather
	// than a request size: large enough that an idle-to-busy transition
	// does not cost a round trip per change, small enough that a node
	// going away naks a handful rather than a backlog. It is also bounded
	// from above by [feedMaxAckPending], which is the FLEET's budget —
	// this is one node's slice of it.
	feedFetchBatch = 32
)

// suffixFor is a family's bucket suffix.
func suffixFor(family coord.Family) (string, error) {
	switch family {
	case coord.FamilyWork:
		return workSuffix, nil
	case coord.FamilyPages:
		return pagesSuffix, nil
	case coord.FamilyKBVectors:
		return kbVectorsSuffix, nil
	}
	return "", coord.ErrUnknownFamily(family)
}

// kvFeed is one durable feed.
type kvFeed struct {
	iter   jetstream.MessagesContext
	family coord.Family

	// closed ends the context watcher, and once guards it so a Stop from
	// the caller and one from a cancelled context are the same Stop. The
	// iterator's own Stop is idempotent; the channel close is not.
	closed chan struct{}
	once   sync.Once
}

// Next returns the next change, blocking until one arrives.
//
// It answers (nil, nil) when the feed has ENDED — stopped by the caller or by
// its context — which is how the consumer above tells a shutdown from a
// failure. Every other error is the broker being unreachable, and is the
// caller's to back off on.
func (f *kvFeed) Next(ctx context.Context) (*coord.Delivery, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil
		}
		msg, err := f.iter.Next()
		if err != nil {
			if errors.Is(err, jetstream.ErrMsgIteratorClosed) {
				return nil, nil
			}
			return nil, unavailable("read from the "+string(f.family)+" feed", err)
		}
		delivery, err := f.deliveryOf(msg)
		if errors.Is(err, errSkip) {
			// Handled here — acked and dropped. Take the next rather than
			// handing the caller an error it has no rule for.
			continue
		}
		return delivery, err
	}
}

// deliveryOf turns a broker message into a change with its outcome.
//
// THE KEY IS RECOVERED FROM THE SUBJECT, because a KV message carries the key
// only there: $KV.<bucket>.<key>. A message whose subject does not have that
// shape is ACKED and skipped rather than naked — it is not a record this
// grammar wrote, so redelivering it would circle until MaxDeliver and
// dead-letter something that was never ours.
func (f *kvFeed) deliveryOf(msg jetstream.Msg) (*coord.Delivery, error) {
	key, ok := keyFromSubject(msg.Subject())
	if !ok {
		_ = msg.Ack()
		log.Warn("coord_feed_foreign_subject", "family", string(f.family),
			"subject", msg.Subject(),
			"detail", "acked and skipped: not a subject this key grammar writes")
		return nil, errSkip
	}
	meta, err := msg.Metadata()
	revision := uint64(0)
	if err == nil {
		revision = meta.Sequence.Stream
	}
	op := coord.OpPut
	if isPurgeMarker(msg) {
		op = coord.OpPurge
	}
	return &coord.Delivery{
		Change: coord.Change{
			Key: key, Value: msg.Data(), Op: op, Revision: revision,
		},
		Ack: msg.Ack,
		Nak: func(delay time.Duration) error { return msg.NakWithDelay(delay) },
	}, nil
}

// errSkip is the internal signal that a message was handled here.
var errSkip = errors.New("coord/kv: message skipped")

// keyFromSubject recovers a KV key from its subject.
func keyFromSubject(subject string) (string, bool) {
	const prefix = "$KV."
	if !strings.HasPrefix(subject, prefix) {
		return "", false
	}
	rest := subject[len(prefix):]
	at := strings.IndexByte(rest, '.')
	if at < 0 || at == len(rest)-1 {
		return "", false
	}
	return rest[at+1:], true
}

// isPurgeMarker reports a delete or purge, which the KV layer marks with a
// header rather than an empty payload — an empty VALUE is a legitimate
// document, and reading one as a removal would delete live records.
func isPurgeMarker(msg jetstream.Msg) bool {
	switch msg.Headers().Get("KV-Operation") {
	case "DEL", "PURGE":
		return true
	}
	return false
}

// Stop ends this process's use of the feed.
//
// THE DURABLE CONSUMER SURVIVES, deliberately: its position is the fleet's,
// so a restart resumes rather than replays, and a node leaving must not reset
// where its peers are reading from.
func (f *kvFeed) Stop() error {
	f.stop()
	return nil
}

// stop ends the iterator once, however it was reached.
//
// DRAIN RATHER THAN Stop on the iterator: a drain naks what it has pulled
// ahead and not yet handed over, so a peer gets those changes at once rather
// than after the full ack window. This node is going away and has done
// nothing with them.
func (f *kvFeed) stop() {
	f.once.Do(func() {
		close(f.closed)
		f.iter.Drain()
	})
}

var _ coord.Feeder = (*FleetStore)(nil)
