package jetstream

import (
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
	info, err := cons.cons.Info(t.Context())
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

// AN EXISTING CONSUMER'S IN-FLIGHT CEILING IS BROUGHT UP TO THIS BUILD'S.
//
// A consumer created by an earlier build carries the broker's default, and
// the ceiling is the count bound on every pull — so a node that kept the old
// one would replay a backlog a thousand records at a time behind a thirty
// second wait each. The start sequence is deliberately left alone, which is
// the one thing the broker refuses to move anyway.
func TestAnExistingDomainConsumerHasItsCeilingRealigned(t *testing.T) {
	t.Parallel()
	q, _ := openDomain(t, "CREWLET_ALIGN_LOG", "crewlet.align.log")
	name := domainConsumerName("CREWLET_ALIGN_LOG", "node-align")
	stale, err := q.js.CreateConsumer(t.Context(), "CREWLET_ALIGN_LOG", jetstream.ConsumerConfig{
		Durable:       name,
		AckPolicy:     jetstream.AckExplicitPolicy,
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   7,
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
	info, err := cons.cons.Info(t.Context())
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
	if info.Config.OptStartSeq != 7 {
		t.Fatalf("the realignment moved the start sequence to %d — the position "+
			"is the checkpoint's to decide and the broker's to keep",
			info.Config.OptStartSeq)
	}
}
