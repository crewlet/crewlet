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
	cons   jetstream.Consumer
	stream string
	name   string
}

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
		Durable:   name,
		AckPolicy: jetstream.AckExplicitPolicy,
		AckWait:   domainConsumerAckWait,
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
	if errors.Is(err, jetstream.ErrConsumerNotFound) {
		cons, err = q.js.CreateConsumer(ctx, stream, config)
	}
	if err != nil {
		return nil, fmt.Errorf("jetstream: open the domain consumer %s on %s: %w",
			name, stream, err)
	}
	return &DomainConsumer{cons: cons, stream: stream, name: name}, nil
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
func (c *DomainConsumer) Fetch(ctx context.Context, maxMessages, maxBytes int,
	wait time.Duration) ([]statelog.Message, error) {

	if maxMessages <= 0 {
		return nil, nil
	}
	opts := []jetstream.FetchOpt{jetstream.FetchMaxWait(wait)}
	var batch jetstream.MessageBatch
	var err error
	if maxBytes > 0 {
		batch, err = c.cons.FetchBytes(maxBytes, opts...)
	} else {
		batch, err = c.cons.Fetch(maxMessages, opts...)
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
		if len(out) >= maxMessages {
			break
		}
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
	info, err := c.cons.Info(ctx)
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
		if ctx.Err() != nil {
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

// StreamBudget is what the broker will actually let this account store, and
// how much of it is already used.
//
// # Why a ceiling is MEASURED here rather than modelled
//
// A stream's byte ceiling is a RESERVATION: the broker refuses to create one it
// could not honour, with `insufficient storage resources available` and nothing
// naming the number it compared against. So a ceiling derived from the disk —
// a share of free space, a fixed default — can be refused on a machine that has
// the space, because the account's own limit is what decides and it is not the
// disk.
//
// This is that number. A limit of -1 means unlimited, which an in-memory or
// explicitly unbounded server reports; the caller reads it as "no cap to apply"
// rather than as zero, which would refuse every stream.
func (q *Queue) StreamBudget(ctx context.Context) (limit, used int64, err error) {
	info, err := q.js.AccountInfo(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("jetstream: read the account's storage limits: %w", err)
	}
	return info.Limits.MaxStore, int64(info.Store), nil
}

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
