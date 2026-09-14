package jetstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/statelog"
)

// DomainConsumer is one node's own reader of one domain's log.
//
// # Why it is DURABLE and per NODE, not per fleet
//
// Every node applies every record — that is what makes N identical SQL copies
// — so this is not a work queue and there is no group to share. Each node
// keeps its own consumer, named after itself, and its own position within it.
// A shared consumer would deliver each record to exactly one node, which is
// the opposite of replication.
//
// # And why the CONSUMER's position is not the checkpoint
//
// The applier's checkpoint lives in the same transaction as the rows, in SQL.
// This consumer's acknowledgement floor is a second, weaker number: it moves
// after the commit, it can lag behind a crash, and on a rebuilt consumer it
// starts wherever the delivery policy says. So the loop RESUMES from the SQL
// checkpoint and the acknowledgement exists to let the broker release
// redelivery state — which is why a lost ack costs a redelivery and never a
// hole.
type DomainConsumer struct {
	q      *Queue
	stream string
	name   string

	// cons is the broker-side consumer, under a mutex because [Reset]
	// replaces it while the applier that holds this handle is between
	// runs — the handle stays, the consumer under it moves.
	//
	// NIL IS A REAL STATE: a reset deletes before it creates, so a create
	// that fails leaves the broker with no consumer at all, and a handle
	// that went on naming the deleted one would fail every fetch for the
	// life of the process with nothing able to repair it. Cleared instead,
	// and rebuilt from `want` by the next caller — see [DomainConsumer.consumerFor].
	mu   sync.Mutex
	cons jetstream.Consumer

	// want is the configuration this consumer should have, kept so that a
	// handle cleared by a failed reset can be rebuilt without the caller
	// knowing how one is configured.
	want jetstream.ConsumerConfig
}

// domainConsumerMaxAckPending is how many records the broker may hand this
// node's applier before one is acknowledged, and it is THE COUNT BOUND ON A
// PULL.
//
// The vendored client cannot bound one pull by both bytes and count, so the
// applier pulls by bytes and the count has to live somewhere the broker
// enforces it. This is that place: a pull hands over at most this many
// records however many bytes it asked for, and [DomainConsumer.Fetch] returns
// every one of them. The alternative — the applier taking a prefix of a
// byte-bounded batch — left the remainder delivered and unacknowledged, so
// the broker hit its own default cap of a thousand, handed over nothing more
// for a thirty-second ack window, and then redelivered the same prefix.
//
// [statelog.FetchMessages] is the number, and it is the applier's transaction
// budget: one pull is at most one transaction's worth of records in flight.
const domainConsumerMaxAckPending = statelog.FetchMessages

// domainConsumerAckWait is how long the broker waits for an acknowledgement
// before redelivering.
//
// THIRTY SECONDS, against an applier whose own transaction is bounded by a row
// budget and a time budget far below it. What a redelivery costs here is one
// re-apply, which is free by construction — the apply is idempotent at a
// position — so the bound exists to release the broker's redelivery state
// rather than to protect the applier from itself.
const domainConsumerAckWait = 30 * time.Second

// DomainConsumer opens (or creates) this node's durable reader of a domain's
// log, resuming from `after`.
//
// # DeliverPolicy is derived from the SQL checkpoint, and that is the whole
// of the resume rule
//
// A consumer created fresh must not start at the stream's head — it would
// silently skip everything published before it existed, which on a state log
// is every record the node has not applied. It must not blindly start at the
// beginning either, on a node that has applied a million records: that is a
// million redeliveries the applier drops one at a time.
//
// So a node with a checkpoint starts at `after + 1` and a node with none
// starts at the beginning. Both are the same rule — resume from what the ROWS
// say — and the rows are the only durable statement of it.
func (q *Queue) DomainConsumer(ctx context.Context, stream, nodeID string,
	after uint64) (*DomainConsumer, error) {

	if nodeID == "" {
		return nil, fmt.Errorf("jetstream: a domain consumer has no node id — " +
			"every node reads the whole log, so the consumer is named after " +
			"the node and a shared name would deliver each record to one of them")
	}
	name := domainConsumerName(stream, nodeID)
	config := jetstream.ConsumerConfig{
		Durable:       name,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       domainConsumerAckWait,
		MaxAckPending: domainConsumerMaxAckPending,
		// NO MaxDeliver. A record this node cannot apply is not a
		// poison message to be given up on: it is either a transient
		// failure the loop retries or a version this build retains
		// deliberately, and a delivery budget that ran out would leave
		// the node silently past a record it never applied.
		MaxDeliver: -1,
	}
	if after > 0 {
		config.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		config.OptStartSeq = after + 1
	} else {
		config.DeliverPolicy = jetstream.DeliverAllPolicy
	}

	// CREATE-OR-UPDATE would silently REFUSE to move an existing
	// consumer's start sequence — the broker treats it as immutable — so
	// an existing consumer is taken as it is and only an absent one is
	// created. The applier resumes from its own checkpoint regardless, and
	// drops anything below it, so a consumer sitting lower costs
	// redeliveries rather than correctness.
	cons, err := q.js.Consumer(ctx, stream, name)
	switch {
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		cons, err = q.js.CreateConsumer(ctx, stream, config)
	case err == nil:
		// THE IN-FLIGHT CEILING IS BROUGHT UP TO DATE on a consumer
		// that already exists, because it is the count bound on every
		// pull and a consumer created by an earlier build carries the
		// broker's own default. Unlike the start sequence it IS
		// updatable, and the update carries the consumer's current
		// configuration with that one field changed — a config built
		// from this build's defaults would reset the start policy the
		// broker refuses to move.
		cons, err = q.alignDomainConsumer(ctx, stream, cons)
	}
	if err != nil {
		return nil, fmt.Errorf("jetstream: open the domain consumer %s on %s: %w",
			name, stream, err)
	}
	return &DomainConsumer{q: q, cons: cons, stream: stream, name: name, want: config}, nil
}

// Reset moves this node's reader to resume from `after`, by deleting the
// broker-side consumer and creating it again there.
//
// # Why a delete rather than an update
//
// The broker treats a consumer's start sequence as immutable, so the only way
// to move one is to make a new one. What needs moving it is an ADOPTION: a
// node that fell below the trim floor installs a peer's snapshot, and its
// checkpoint jumps to the artefact's position — thousands or millions of
// records past where its consumer stopped. Left where it was, the consumer
// would deliver every record in between for the applier to drop one at a
// time, under an in-flight ceiling, for as long as that took.
//
// The applier resumes from the checkpoint whatever the consumer says, so a
// reset that fails leaves correctness alone and costs only those
// redeliveries; it is reported so the operator knows which.
//
// # What a HALF-done reset leaves, and why the handle is cleared
//
// The delete and the create are two calls and the broker can take the first
// and refuse the second — a connection lost in between, a server that went
// away. That leaves no consumer on the stream at all, and a handle still
// naming the deleted one: every later fetch fails against something that does
// not exist, for the life of the process, and no later reset is ever attempted
// because the caller only logs this error. The handle is therefore CLEARED on
// that path, which makes the next fetch rebuild it — the repair happens where
// the damage is noticed rather than needing a second failure to trigger it.
func (c *DomainConsumer) Reset(ctx context.Context, after uint64) error {
	config := jetstream.ConsumerConfig{
		Durable:       c.name,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       domainConsumerAckWait,
		MaxAckPending: domainConsumerMaxAckPending,
		MaxDeliver:    -1,
	}
	if after > 0 {
		config.DeliverPolicy = jetstream.DeliverByStartSequencePolicy
		config.OptStartSeq = after + 1
	} else {
		config.DeliverPolicy = jetstream.DeliverAllPolicy
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.q.js.DeleteConsumer(ctx, c.stream, c.name); err != nil &&
		!errors.Is(err, jetstream.ErrConsumerNotFound) {
		return fmt.Errorf("jetstream: reset the domain consumer %s on %s: %w",
			c.name, c.stream, err)
	}
	// THE DELETE HAS LANDED, so from here the broker has no consumer and
	// the handle must not go on naming one.
	// THE DELETE HAS LANDED, so from here the broker has no consumer and
	// the handle must not go on naming one.
	c.cons = nil
	cons, err := c.q.js.CreateConsumer(ctx, c.stream, config)
	if err != nil {
		// `want` IS LEFT AS IT WAS, deliberately: the rebuild then
		// restores the consumer at its PREVIOUS position rather than at
		// one this broker has just refused. The applier resumes from its
		// own checkpoint whatever the consumer says, so that costs
		// redeliveries and not correctness — which is the same trade the
		// caller makes when it logs this error and carries on.
		return fmt.Errorf("jetstream: recreate the domain consumer %s on %s at "+
			"%d: %w", c.name, c.stream, after, err)
	}
	c.cons, c.want = cons, config
	return nil
}

// consumerFor is the current broker-side consumer, created if a failed reset
// left none.
//
// THE REBUILD IS HERE rather than at the call site because this is where the
// absence is discovered, and because every caller wants the same thing: a
// consumer at the position the last reset asked for. It is idempotent — a
// consumer the broker still has comes back from CreateConsumer unchanged.
func (c *DomainConsumer) consumerFor(ctx context.Context) (jetstream.Consumer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cons != nil {
		return c.cons, nil
	}
	cons, err := c.q.js.CreateConsumer(ctx, c.stream, c.want)
	if err != nil {
		return nil, fmt.Errorf("jetstream: rebuild the domain consumer %s on "+
			"%s after a reset that deleted it and could not create it again: %w",
			c.name, c.stream, err)
	}
	c.cons = cons
	return cons, nil
}

// alignDomainConsumer updates an existing consumer's updatable bounds to this
// build's, leaving its position alone.
func (q *Queue) alignDomainConsumer(ctx context.Context, stream string,
	cons jetstream.Consumer) (jetstream.Consumer, error) {

	info, err := cons.Info(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the consumer's configuration: %w", err)
	}
	config := info.Config
	if config.MaxAckPending == domainConsumerMaxAckPending &&
		config.AckWait == domainConsumerAckWait {
		return cons, nil
	}
	config.MaxAckPending = domainConsumerMaxAckPending
	config.AckWait = domainConsumerAckWait
	updated, err := q.js.UpdateConsumer(ctx, stream, config)
	if err != nil {
		return nil, fmt.Errorf("raise the consumer's in-flight ceiling to %d: %w",
			domainConsumerMaxAckPending, err)
	}
	return updated, nil
}

// domainConsumerName is what this node's reader is called on the broker.
//
// A node id can hold characters a consumer name may not, and two node ids can
// differ only in one of them — so the readable half is escaped and a digest
// over the exact pair is appended, exactly as the mailbox consumer's name is
// built. Truncation cannot reintroduce an alias, because the digest is taken
// over the full pair and appended after it.
func domainConsumerName(stream, nodeID string) string {
	safe := func(s string) string {
		return strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_").Replace(s)
	}
	sum := sha256.Sum256([]byte(nodeID + "\x00" + stream))
	id := hex.EncodeToString(sum[:6])

	readable := "statelog__" + safe(stream) + "__" + safe(nodeID)
	if max := consumerNameMax - len(id) - 2; len(readable) > max {
		readable = readable[:max]
	}
	return readable + "__" + id
}

// Fetch implements [statelog.Fetcher].
//
// A message that cannot report its own sequence is DROPPED with its
// acknowledgement withheld, rather than passed on with a zero: the sequence is
// the applier's checkpoint, its contiguity check and the next writer's
// expectation, so a record delivered as sequence zero would move the cursor
// backwards on every node that applied it.
//
// # Everything a pull delivered is returned
//
// A byte-bounded pull cannot also be count-bounded by this client, and the
// count is enforced by the consumer's own in-flight ceiling instead — see
// [domainConsumerMaxAckPending]. So maxMessages is passed to the broker only
// on the count-bounded path, and on the byte-bounded one the batch is drained
// whole: a message the broker delivered and this call did not return is one it
// holds against that ceiling and redelivers after the ack window, which is a
// hole in the applier's run for thirty seconds.
func (c *DomainConsumer) Fetch(ctx context.Context, maxMessages, maxBytes int,
	wait time.Duration) ([]statelog.Message, error) {

	if maxMessages <= 0 {
		return nil, nil
	}
	opts := []jetstream.FetchOpt{jetstream.FetchMaxWait(wait)}
	var batch jetstream.MessageBatch
	var err error
	cons, err := c.consumerFor(ctx)
	if err != nil {
		return nil, err
	}
	if maxBytes > 0 {
		batch, err = cons.FetchBytes(maxBytes, opts...)
	} else {
		batch, err = cons.Fetch(maxMessages, opts...)
	}
	if err != nil {
		return nil, fmt.Errorf("jetstream: fetch from %s: %w", c.name, err)
	}

	// THE CONTEXT ENDS THE DRAIN, and until it did this function took a
	// context and used it for nothing but its own return value.
	//
	// A batch closes when it is FULL or when `wait` expires — one record
	// arriving does not end it — so a caller that wanted the records
	// already in hand had no way to say so and paid the whole wait. The
	// applier's [statelog.Runner] is exactly that caller: a barrier
	// appended onto a quiet log sat in a batch nobody had finished
	// collecting for five seconds, against a two second read budget, so
	// every linearizable read on an idle company refused `behind`.
	//
	// WHAT IS COLLECTED IS RETURNED. A cancelled drain is this caller
	// deciding it has waited long enough, not a failure — the messages
	// already taken are real, and the ones still in the batch are
	// redelivered because they were never acknowledged.
	var out []statelog.Message
	msgs := batch.Messages()
	for {
		var msg jetstream.Msg
		var open bool
		select {
		case msg, open = <-msgs:
			if !open {
				msg = nil
			}
		case <-ctx.Done():
		}
		if msg == nil {
			break
		}
		meta, err := msg.Metadata()
		if err != nil {
			// NOT ACKNOWLEDGED. A message whose metadata is
			// unreadable is one this node cannot place in the log,
			// and acknowledging it would move the broker's floor
			// past a record nothing applied.
			continue
		}
		out = append(out, statelog.Message{
			Seq:      meta.Sequence.Stream,
			StoredAt: meta.Timestamp,
			Payload:  msg.Data(),
			Ack:      msg.Ack,
		})
	}
	if err := batch.Error(); err != nil && !errors.Is(err, context.Canceled) {
		return out, fmt.Errorf("jetstream: fetch from %s: %w", c.name, err)
	}
	return out, ctx.Err()
}

// Pending implements [statelog.Fetcher]: how many records this consumer has
// not delivered, which is what tells a partially filled batch whether waiting
// would buy anything.
func (c *DomainConsumer) Pending(ctx context.Context) (uint64, error) {
	cons, err := c.consumerFor(ctx)
	if err != nil {
		return 0, err
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return 0, fmt.Errorf("jetstream: read %s: %w", c.name, err)
	}
	return info.NumPending, nil
}

// Name is what this consumer is called on the broker, for an operator tracing
// a stalled applier back to the thing that is not delivering.
func (c *DomainConsumer) Name() string { return c.name }

// DomainGroup is a FLEET-WIDE pull consumer over one domain's log.
//
// # Why this exists beside the per-node one
//
// [DomainConsumer] is replication: every node reads every record, so each has
// its own. This is the opposite shape — one consumer SHARED by the fleet,
// where a record is handled by whichever node gets there first — and it is
// what a change feed needs.
//
// A GROUP RATHER THAN A DUTY, and the difference is a real outage: a duty is
// held by one node under a lease, so a lease flap stalls the whole company's
// notifications for work that is stateless. A group has no holder to lose.
type DomainGroup struct {
	cons     jetstream.Consumer
	messages jetstream.MessagesContext
	stream   string
	name     string

	// closed ends the context watcher, and once guards the iterator's
	// stop so a Stop from the caller and one from a cancelled context are
	// the same Stop.
	//
	// THE ITERATOR HAS NO CONTEXT OF ITS OWN. [jetstream.MessagesContext.Next]
	// blocks until a message arrives or the iterator is stopped, and a
	// quiet log means it blocks for ever — so a caller that only
	// cancelled a context would hang on the goroutine it was joining.
	// That is not a theoretical shutdown wart: the tracker's wake feed
	// sits in exactly this call, and it wedged every engine test that
	// stopped before its own context expired.
	closed chan struct{}
	once   sync.Once
}

// DomainDelivery is one record delivered to a group.
type DomainDelivery struct {
	// Subject is the wire subject, which is what a translator reads the
	// object's kind and id out of.
	Subject string

	// Seq is the broker's sequence and StoredAt its own timestamp — the
	// instant every node reads identically.
	Seq      uint64
	StoredAt time.Time

	Payload []byte

	// Ack marks the record handled, and Nak returns it for redelivery
	// after a delay. A handler that could not reach something it needs
	// naks; one that decided the record means nothing to it ACKS, because
	// a decision is handling.
	Ack func() error
	Nak func(time.Duration) error
}

// Group opens (or creates) a fleet-wide consumer over this log.
//
// DELIVER ALL, always. A group created at the head exists and still discards
// everything published before its first consumer — which for a wake feed is
// every notification the company owed while nothing was watching.
func (l *DomainLog) Group(ctx context.Context, name string) (*DomainGroup, error) {
	if name == "" {
		return nil, fmt.Errorf("jetstream: a domain group has no name — it is " +
			"the durable consumer's identity, and an unnamed one would be a " +
			"fresh consumer per process that replays the whole log on restart")
	}
	safe := domainGroupName(l.name, name)
	// THROUGH THE PACKAGE'S ONE DURABLE-CONSUMER PATH, so a group every
	// node of a fleet ensures at boot survives a peer winning the race —
	// see [Queue.ensureDurableConsumer].
	cons, err := l.q.ensureDurableConsumer(ctx, l.name, jetstream.ConsumerConfig{
		Durable:       safe,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       domainConsumerAckWait,
		DeliverPolicy: jetstream.DeliverAllPolicy,
		// NO MaxDeliver, for the reason the per-node consumer gives: a
		// record nobody could handle yet is a retry rather than a poison
		// message, and a budget that ran out would drop a wake silently.
		MaxDeliver: -1,
	})
	if err != nil {
		return nil, fmt.Errorf("jetstream: open the group %s on %s: %w",
			safe, l.name, err)
	}
	messages, err := cons.Messages()
	if err != nil {
		return nil, fmt.Errorf("jetstream: consume %s on %s: %w", safe, l.name, err)
	}
	group := &DomainGroup{
		cons: cons, messages: messages, stream: l.name, name: safe,
		closed: make(chan struct{}),
	}
	// The context is turned into a Stop by a goroutine, for the reason
	// [DomainGroup.closed] gives. It exits with the group, so a
	// long-lived process holds one goroutine per group rather than one
	// per read.
	go func() {
		select {
		case <-ctx.Done():
			group.stop()
		case <-group.closed:
		}
	}()
	return group, nil
}

// domainGroupName escapes a group's name the way every derived consumer name
// here is escaped, and appends a digest over the exact pair.
func domainGroupName(stream, group string) string {
	safe := func(s string) string {
		return strings.NewReplacer(".", "_", "*", "_", ">", "_", " ", "_").Replace(s)
	}
	sum := sha256.Sum256([]byte(group + "\x00" + stream))
	id := hex.EncodeToString(sum[:6])
	readable := safe(group) + "__" + safe(stream)
	if max := consumerNameMax - len(id) - 2; len(readable) > max {
		readable = readable[:max]
	}
	return readable + "__" + id
}

// Next blocks for the next delivery.
//
// A NIL DELIVERY WITH A NIL ERROR MEANS THE CONSUMER CLOSED, which is how a
// caller tells a shutdown from a failure — the two have opposite responses,
// and a shutdown reported as a failure is a log line on every clean stop.
func (g *DomainGroup) Next(ctx context.Context) (*DomainDelivery, error) {
	msg, err := g.messages.Next()
	switch {
	case errors.Is(err, jetstream.ErrMsgIteratorClosed):
		return nil, nil
	case err != nil:
		// A CANCELLED CALLER IS THE SAME ANSWER AS A CLOSED ITERATOR. The
		// goroutine this group started turns ctx.Done into a Drain, so a
		// cancellation reaches the iterator by two routes at once and
		// whichever lands first decides what Next returns — the tidy
		// ErrMsgIteratorClosed above, or the underlying read failing
		// mid-drain. Reporting the second as a failure would make a clean
		// shutdown log an error on the race's losing half.
		if ctx.Err() != nil {
			//nolint:nilerr // Swallowing it IS the contract: a nil delivery
			// with a nil error means the consumer closed, which is how the
			// feed above tells a shutdown from a failure.
			return nil, nil
		}
		return nil, fmt.Errorf("jetstream: read %s: %w", g.name, err)
	}
	meta, err := msg.Metadata()
	if err != nil {
		// NOT ACKNOWLEDGED, and not returned: a delivery this node
		// cannot place in the log is one it cannot derive a stable wake
		// id from, and acknowledging it would drop the wake outright.
		return nil, fmt.Errorf("jetstream: read %s's metadata: %w", g.name, err)
	}
	return &DomainDelivery{
		Subject: msg.Subject(), Seq: meta.Sequence.Stream,
		StoredAt: meta.Timestamp, Payload: msg.Data(),
		Ack: msg.Ack,
		Nak: func(delay time.Duration) error { return msg.NakWithDelay(delay) },
	}, nil
}

// Stop ends this process's consumption. THE DURABLE POSITION SURVIVES, which
// is what makes a restart resume rather than replay: it is the fleet's
// position, not this process's.
func (g *DomainGroup) Stop() error {
	g.stop()
	return nil
}

// stop ends the iterator once, however it was reached.
//
// DRAIN RATHER THAN Stop, which is the choice [internal/coord/kv]'s feed makes
// for the same reason: a drain naks what it has pulled ahead and not yet
// handed over, so a peer receives those records at once rather than after the
// full ack window. This node is going away and has done nothing with them.
func (g *DomainGroup) stop() {
	g.once.Do(func() {
		close(g.closed)
		g.messages.Drain()
	})
}

// Name is what this group is called on the broker.
func (g *DomainGroup) Name() string { return g.name }

// GroupAckFloor is how far a fleet-wide group has acknowledged, WITHOUT
// attaching to it.
//
// # Why the retention gate cannot just ask the group
//
// The trim's feed term is "a record the wake feed has not seen is one nobody
// has been told about", and the node evaluating it is whichever one holds the
// trim duty — which is not necessarily a node running the feed at all. So the
// question has to be answerable from the consumer's NAME, which is stable by
// construction ([DomainLog.Group] derives it from the stream and the group),
// rather than from a handle.
//
// It reports (0, false, nil) when no such consumer exists. That is an ordinary
// answer and a load-bearing one: a fleet that has never run the feed has not
// failed to read it, and the two must not look alike — an unreadable term
// blocks the trim, while an absent feed is a domain that does not have one.
func (l *DomainLog) GroupAckFloor(ctx context.Context, group string) (uint64, bool, error) {
	cons, err := l.stream.Consumer(ctx, domainGroupName(l.name, group))
	switch {
	case errors.Is(err, jetstream.ErrConsumerNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("jetstream: read the group %q on %s: %w",
			group, l.name, err)
	}
	info, err := cons.Info(ctx)
	if err != nil {
		return 0, false, fmt.Errorf("jetstream: read the group %q on %s: %w",
			group, l.name, err)
	}
	// THE ACK FLOOR RATHER THAN THE DELIVERED SEQUENCE. Delivered says a
	// record left the broker; the floor says every record below it was
	// handled. The trim needs the second, because a wake the feed fetched
	// and had not finished acting on is one the record still has to be
	// there for.
	return info.AckFloor.Stream, true, nil
}
