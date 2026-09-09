package jetstream

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// The facts in this file belong to the VENDORED CLIENT and the broker it
// talks to, not to this package. They are asserted here because the design
// above them is built on them and because neither is visible from a checkout:
// a client bump that changed one would be a silently different engine, and
// the first symptom would be a trim that removed the wrong records or a
// catch-up replay that fetched a hundred times what it was sized for.

// A PURGE BY SEQUENCE REMOVES EXACTLY WHAT IS BELOW IT.
//
// This is the trim's whole mechanism: the fleet computes a floor — the lowest
// position any counted node still needs — and removes everything below it.
// The boundary is INCLUSIVE OF NOTHING AT THE FLOOR: WithPurgeSequence(n)
// keeps n, and a client that removed it too would delete the record a node is
// about to replay, with no error and no way to get it back.
func TestAPurgeBySequenceKeepsTheFloor(t *testing.T) {
	t.Parallel()
	spec := probeDomain()
	q := domainQueue(t, spec)
	ctx := t.Context()

	const total = 10
	for i := range total {
		// PUBLISHED THROUGH THE CLIENT, not through Queue.Publish: that
		// one derives a NAMESPACE stream from the subject's first token,
		// which is the seat-mailbox topology and not a domain's. A
		// domain publishes to its own stream, which is step 3's
		// publisher; here the client is enough.
		if _, err := q.js.Publish(ctx, "crewlet.probe.log.item", []byte("record")); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}
	stream, err := q.js.Stream(ctx, spec.Name)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	info, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if got := info.State.Msgs; got != total {
		t.Fatalf("the stream holds %d messages, want %d", got, total)
	}
	first := info.State.FirstSeq

	// Keep everything from the fifth record on.
	floor := first + 4
	if err := stream.Purge(ctx, jetstream.WithPurgeSequence(floor)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	after, err := stream.Info(ctx)
	if err != nil {
		t.Fatalf("info after the purge: %v", err)
	}
	if got, want := after.State.Msgs, uint64(total-4); got != want {
		t.Errorf("the stream holds %d messages after purging below %d, want %d",
			got, floor, want)
	}
	if after.State.FirstSeq != floor {
		t.Errorf("first_seq = %d after purging below %d: the record AT the floor "+
			"is the one the slowest node is about to replay, and a purge that "+
			"took it would remove it with no error and nothing to recover from",
			after.State.FirstSeq, floor)
	}
	if after.State.LastSeq != info.State.LastSeq {
		t.Errorf("last_seq moved from %d to %d: a purge below a floor must not "+
			"touch the head", info.State.LastSeq, after.State.LastSeq)
	}
}

// A FETCH IS BOUNDED BY BYTES, and this is what the vendored client actually
// offers rather than what the design asked for.
//
// # The finding
//
// A replay's fetch has to be bounded by SIZE, not by message count: a log
// whose modal record is a 140-byte barrier and whose largest is over a
// megabyte turns any message count into a request that is either pointless or
// enormous. The design asked for ONE pull carrying both bounds — a message
// count and a byte ceiling.
//
// nats.go v1.53.1 does not offer that, and this test is the record of it:
//
//   - Consumer.Fetch(batch, opts...) takes no byte option at all. Every
//     FetchOpt it accepts is about waiting (MaxWait, Heartbeat, Context) or
//     about priority groups.
//   - Consumer.FetchBytes(maxBytes, opts...) is the byte-bounded pull, and it
//     fixes the message count itself at one million — effectively unbounded.
//   - PullMaxBytes exists, but only for Consume and Messages, and its own
//     documentation says it is EXCLUSIVE with PullMaxMessages.
//
// So the replay pulls with FetchBytes and the message count is bounded
// elsewhere: by the consumer's MaxAckPending, which is a consumer property
// and not a per-pull one. That is a better place for it — it bounds what is
// in flight across every pull rather than within one.
func TestTheVendoredClientBoundsAFetchByBytesAndNotByBoth(t *testing.T) {
	t.Parallel()
	spec := probeDomain()
	q := domainQueue(t, spec)
	ctx := t.Context()

	// Records large enough that a byte bound binds before a message bound
	// would: ten of these are ~640 KiB.
	const records = 10
	body := make([]byte, 64<<10)
	for i := range records {
		if _, err := q.js.Publish(ctx, "crewlet.probe.log.item", body); err != nil {
			t.Fatalf("publish %d: %v", i, err)
		}
	}

	stream, err := q.js.Stream(ctx, spec.Name)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	consumer, err := stream.CreateOrUpdateConsumer(ctx, jetstream.ConsumerConfig{
		Durable:       "probe-replay",
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	})
	if err != nil {
		t.Fatalf("consumer: %v", err)
	}

	// A ceiling that holds two of these records and not three.
	batch, err := consumer.FetchBytes(150<<10, jetstream.FetchMaxWait(2*time.Second))
	if err != nil {
		t.Fatalf("FetchBytes: %v", err)
	}
	var got, bytes int
	for msg := range batch.Messages() {
		got++
		bytes += len(msg.Data())
		if err := msg.Ack(); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}
	if err := batch.Error(); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got == 0 {
		t.Fatal("a byte-bounded fetch returned nothing")
	}
	if got >= records {
		t.Errorf("a 150 KiB fetch returned all %d records (%d bytes): the byte "+
			"bound did not bind, so a replay of a log whose largest record is "+
			"over a megabyte would pull the whole backlog into one transaction",
			got, bytes)
	}
}
