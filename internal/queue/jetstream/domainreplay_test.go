package jetstream

import (
	"context"
	"math"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A BYTE-BOUNDED REPLAY LEAVES NOTHING DELIVERED AND UNCONSUMED. The client
// cannot bound one pull by both bytes and count, and a record pulled but
// dropped holds a place under the consumer's ack-pending cap until its ack
// window returns it, wedging a node whose backlog outgrows one fetch. So the
// count bound is the consumer's in-flight ceiling, which the broker enforces
// per pull, and the fetch drains whatever a pull delivered. Driven as the
// applier drives it, over a backlog past that ceiling: the replay must be
// CONTIGUOUS and every record DELIVERED ONCE, by the broker's delivery count
// rather than the clock, since it counts a redelivery too.
//
// Mutation: acknowledge nothing and the ceiling withholds the rest until the
// window returns the first; hand a burst's last record back and it comes twice.
func TestAByteBoundedReplayLeavesNothingDeliveredUnconsumed(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_REPLAY_LOG", "crewlet.replay.log")
	// Past the in-flight ceiling by a margin, so the second pull has to be
	// served from records the broker withheld until the first was
	// acknowledged.
	const total = statelog.FetchMessages + 512
	body := make([]byte, 100)
	for i := range total {
		if _, _, err := log.Append(t.Context(), "crewlet.replay.log.task.x", "", nil, body); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_REPLAY_LOG", "node-replay", 0)
	if err != nil {
		t.Fatalf("open the consumer: %v", err)
	}

	// LIVENESS: four ack windows, which only a replay that never ends reaches;
	// one that waited out a window is caught by the delivery count below.
	ctx, cancel := context.WithTimeout(t.Context(), 4*domainConsumerAckWait)
	defer cancel()
	var next uint64 = 1
	pulls := 0
	for next <= total {
		batch, err := cons.Fetch(ctx, statelog.FetchMessages, statelog.FetchBytes, statelog.FetchWait)
		if err != nil {
			t.Fatalf("pull %d, at sequence %d of %d: %v — a replay still short of its "+
				"records four ack windows in", pulls, next-1, total, err)
		}
		pulls++
		for _, msg := range batch {
			if msg.Seq != next {
				t.Fatalf("pull %d delivered sequence %d where %d was next — a "+
					"hole in a strict log is a record the broker delivered and "+
					"nothing consumed", pulls, msg.Seq, next)
			}
			next++
			if err := msg.Ack(); err != nil {
				t.Fatalf("ack %d: %v", msg.Seq, err)
			}
		}
	}
	// AND THE FIRST PULL WAS BOUNDED BY THE CEILING rather than by the
	// client's own million: the broker handed over at most one
	// transaction's worth of records before an acknowledgement.
	info, err := consumerInfo(t, cons)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if info.Config.MaxAckPending != statelog.FetchMessages {
		t.Fatalf("the consumer's in-flight ceiling is %d, want %d — it is the "+
			"only count bound a byte-bounded pull has",
			info.Config.MaxAckPending, statelog.FetchMessages)
	}
	if pulls < 2 {
		t.Fatalf("%d records arrived in %d pull(s), so the ceiling did not bind "+
			"and this test proved nothing about the withheld remainder", total, pulls)
	}
	// AND EVERY RECORD DELIVERED ONCE: none of them waited out an ack window
	// to come back.
	if info.Delivered.Consumer != total {
		t.Fatalf("the replay of %d records took %d deliveries — a record left "+
			"delivered and unconsumed came back only once its %s ack window ran out",
			total, info.Delivered.Consumer, domainConsumerAckWait)
	}
}

// A DOMAIN CONSUMER IS RESET IN PLACE, and the applier's handle keeps working.
// An adoption moves the checkpoint and the broker will not move a consumer's
// start, so it is deleted and remade under the same handle; left in place it
// would deliver every record in between for the applier to drop.
func TestADomainConsumerIsResetToANewPosition(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_RESET_LOG", "crewlet.reset.log")
	for i := range 10 {
		if _, _, err := log.Append(t.Context(), "crewlet.reset.log.task.x", "", nil, []byte{byte(i)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_RESET_LOG", "node-reset", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	first := fetchAll(t, cons, 3)
	if len(first) != 3 || first[2].Seq != 3 {
		t.Fatalf("the first pull delivered %d record(s) ending at %d, want 1..3",
			len(first), first[len(first)-1].Seq)
	}
	if err := cons.Reset(t.Context(), 8); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	after := fetchAll(t, cons, 2)
	if len(after) != 2 || after[0].Seq != 9 || after[1].Seq != 10 {
		t.Fatalf("after a reset to 8 the consumer delivered %v, want 9 and 10 — "+
			"a consumer left at its old position delivers every record in "+
			"between for the applier to drop", seqsOf(after))
	}
	pending, err := cons.Pending(t.Context())
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending != 0 {
		t.Fatalf("%d record(s) pending after the reset drained, want 0", pending)
	}
}

func seqsOf(msgs []statelog.Message) []uint64 {
	out := make([]uint64, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Seq)
	}
	return out
}

// consumerInfo reads the broker's own view of a domain consumer, rebuilding
// the handle if a reset left none, as every production caller does.
func consumerInfo(t *testing.T, c *DomainConsumer) (*jetstream.ConsumerInfo, error) {
	t.Helper()
	cons, err := c.consumerFor(t.Context())
	if err != nil {
		return nil, err
	}
	return cons.Info(t.Context())
}

// A RESET THAT DELETES AND THEN CANNOT CREATE LEAVES A HANDLE THAT REPAIRS
// ITSELF. The broker can take the delete and refuse the create, and a handle
// still naming the deleted consumer would fail every fetch for the life of
// the process: nothing retries the reset, whose one caller only logs it.
func TestAResetThatCannotRecreateLeavesARepairableHandle(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_HALFRESET_LOG", "crewlet.halfreset.log")
	for i := range 6 {
		if _, _, err := log.Append(t.Context(), "crewlet.halfreset.log.task.x", "", nil, []byte{byte(i)}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_HALFRESET_LOG", "node-half", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	// A START SEQUENCE THE BROKER REFUSES: the delete lands and the create
	// does not, the half-done state a connection dropped between them leaves.
	if err := cons.Reset(t.Context(), math.MaxUint64); err == nil {
		t.Fatal("a reset whose create could not run reported success")
	}
	cons.mu.Lock()
	cleared := cons.cons == nil
	cons.mu.Unlock()
	if !cleared {
		t.Fatal("the handle still names the consumer the reset deleted — every " +
			"later fetch goes to something that does not exist, for the life " +
			"of the process, and its one caller only logs this error")
	}

	// AND THE NEXT FETCH REBUILDS IT, at the position held before the reset:
	// the applier resumes from its own checkpoint, so that costs
	// redeliveries, not correctness.
	got := fetchAll(t, cons, 2)
	if len(got) != 2 || got[0].Seq != 1 {
		t.Fatalf("after a half-done reset the handle delivered %v, want the log "+
			"from 1 — a handle naming a deleted consumer fails every fetch for "+
			"the life of the process", seqsOf(got))
	}
}
