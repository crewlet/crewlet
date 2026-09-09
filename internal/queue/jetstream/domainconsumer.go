package jetstream

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
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

	var out []statelog.Message
	for msg := range batch.Messages() {
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
