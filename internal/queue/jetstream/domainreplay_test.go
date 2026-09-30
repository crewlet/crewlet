package jetstream

import (
	"math"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A BYTE-BOUNDED REPLAY LEAVES NOTHING DELIVERED AND UNCONSUMED.
//
// # The wedge this guards against
//
// The applier pulls by bytes because the vendored client cannot bound one pull
// by both bytes and count, and it used to apply the count itself: take the
// first [statelog.FetchMessages] of what came back and drop the rest. The rest
// had already been DELIVERED. The broker counted every dropped record against
// the consumer's ack-pending cap — a thousand by default — handed over nothing
// more until the thirty-second ack window expired, and then redelivered the
// same prefix. Measured: the first pull returned records 1 to 256, thirteen
// further pulls over the next six seconds returned nothing, and at thirty
// seconds 1 to 256 came back. Every node with a backlog of 257 records wedged.
//
// So the count bound is the consumer's own in-flight ceiling, which the broker
// enforces per pull, and the fetch drains whatever a pull delivered. This test
// drives the consumer exactly as the applier does — the framework's own pull
// constants, an acknowledgement after each batch — over a backlog larger than
// that ceiling, and requires the replay to be CONTIGUOUS and to finish well
// inside one ack window.
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

	started := time.Now()
	var next uint64 = 1
	pulls := 0
	for next <= total {
		if time.Since(started) > 15*time.Second {
			t.Fatalf("the replay reached sequence %d of %d after %d pull(s) and "+
				"15 seconds — a pull that leaves delivered records unconsumed "+
				"stalls the applier for the whole ack window", next-1, total, pulls)
		}
		batch, err := cons.Fetch(t.Context(), statelog.FetchMessages, statelog.FetchBytes, statelog.FetchWait)
		if err != nil {
			t.Fatalf("pull %d: %v", pulls, err)
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
}

// A KEPT CONSUMER'S IN-FLIGHT CEILING IS BROUGHT UP TO THIS BUILD'S.
//
// A consumer created by an earlier build carries the broker's default, and
// the ceiling is the count bound on every pull — so a node that kept the old
// one would replay a backlog a thousand records at a time behind a thirty
// second wait each. The consumer is otherwise one that AGREES with the
// checkpoint, so it is updated in place rather than rebuilt, and its start
// sequence is left alone: the broker refuses to move it, and it was right.
func TestAnExistingDomainConsumerHasItsCeilingRealigned(t *testing.T) {
	t.Parallel()
	q, _ := openDomain(t, "CREWLET_ALIGN_LOG", "crewlet.align.log")
	name := domainConsumerName("CREWLET_ALIGN_LOG", "node-align")
	stale, err := q.js.CreateConsumer(t.Context(), "CREWLET_ALIGN_LOG", jetstream.ConsumerConfig{
		Durable:       name,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   4,
		MaxAckPending: 1000,
		MaxDeliver:    -1,
	})
	if err != nil {
		t.Fatalf("create a stale consumer: %v", err)
	}
	if got := stale.CachedInfo().Config.MaxAckPending; got != 1000 {
		t.Fatalf("the stale consumer reports a ceiling of %d, want 1000", got)
	}

	cons, err := q.DomainConsumer(t.Context(), "CREWLET_ALIGN_LOG", "node-align", 3)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	info, err := consumerInfo(t, cons)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Config.MaxAckPending != statelog.FetchMessages {
		t.Fatalf("the reopened consumer's ceiling is %d, want %d",
			info.Config.MaxAckPending, statelog.FetchMessages)
	}
	if info.Config.AckWait != domainConsumerAckWait {
		t.Fatalf("the reopened consumer's ack wait is %s, want %s",
			info.Config.AckWait, domainConsumerAckWait)
	}
	if info.Config.OptStartSeq != 4 {
		t.Fatalf("the realignment moved the start sequence to %d — the position "+
			"is the checkpoint's to decide and the broker's to keep",
			info.Config.OptStartSeq)
	}
	if !info.Created.Equal(stale.CachedInfo().Created) {
		t.Fatalf("a consumer that agreed with the checkpoint was rebuilt to raise "+
			"its ceiling (created %s, now %s) — the ceiling is updatable in place",
			stale.CachedInfo().Created, info.Created)
	}
}

// A DOMAIN CONSUMER IS RESET IN PLACE, and the handle the applier holds keeps
// working.
//
// An adoption moves a node's checkpoint to the artefact's position, and the
// broker will not move a consumer's start sequence — so the consumer is
// deleted and remade at the new position under the same handle. Left where it
// was, it would deliver every record between the old position and the new one
// for the applier to drop.
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
// the handle if a reset left none — which is what every production caller
// does too.
func consumerInfo(t *testing.T, c *DomainConsumer) (*jetstream.ConsumerInfo, error) {
	t.Helper()
	cons, err := c.consumerFor(t.Context())
	if err != nil {
		return nil, err
	}
	return cons.Info(t.Context())
}

// A RESET THAT DELETES AND THEN CANNOT CREATE LEAVES A HANDLE THAT REPAIRS
// ITSELF.
//
// The two calls are separate and the broker can take the first and refuse the
// second — a connection lost in between, a server that went away. What that
// used to leave was a handle still naming the deleted consumer: every later
// fetch failed against something that did not exist, for the life of the
// process, and nothing retried the reset because its one caller only logs the
// error and starts the appliers anyway.
//
// The interleaving is staged by taking the STREAM away between the two calls:
// the delete is then a tolerated not-found and the create has nowhere to go,
// which is the same half-done state a dropped connection leaves.
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

	// A START SEQUENCE THE BROKER REFUSES: the delete lands, the create
	// does not. Any refusal reaches the same half-done state — a dropped
	// connection between the two calls is the one an operator will see.
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

	// AND THE NEXT FETCH REBUILDS IT, at the position the consumer held
	// before the reset — the applier resumes from its own checkpoint
	// whatever the consumer says, so restoring the previous position
	// costs redeliveries and not correctness.
	got := fetchAll(t, cons, 2)
	if len(got) != 2 || got[0].Seq != 1 {
		t.Fatalf("after a half-done reset the handle delivered %v, want the log "+
			"from 1 — a handle naming a deleted consumer fails every fetch for "+
			"the life of the process", seqsOf(got))
	}
}
