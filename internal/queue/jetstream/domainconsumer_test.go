package jetstream

import (
	"context"
	"errors"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY NODE READS THE WHOLE LOG, which is what makes N identical copies.
//
// This is not a work queue: a shared consumer would deliver each record to
// exactly one node, and two nodes' tracker tables would quietly disagree.
func TestEveryNodeReadsEveryRecord(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_TEST_LOG", "crewlet.test.log")
	for i := range 5 {
		if _, _, err := log.Append(t.Context(), "crewlet.test.log.task."+id(i),
			"", nil, []byte(`{"n":`+itoa(i)+`}`)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	for _, node := range []string{"node-a", "node-b"} {
		cons, err := q.DomainConsumer(t.Context(), "CREWLET_TEST_LOG", node, 0)
		if err != nil {
			t.Fatalf("open %s's consumer: %v", node, err)
		}
		got := fetchAll(t, cons, 5)
		if len(got) != 5 {
			t.Fatalf("%s received %d of 5 records — a shared consumer would "+
				"deliver each record to exactly one node, which is the "+
				"opposite of replication", node, len(got))
		}
		for i, msg := range got {
			if msg.Seq != uint64(i+1) {
				t.Fatalf("%s received sequence %d at position %d",
					node, msg.Seq, i)
			}
			if msg.StoredAt.IsZero() {
				t.Fatal("a record arrived with no broker timestamp, which is " +
					"the instant every node must read identically")
			}
		}
	}
}

// A NEW CONSUMER RESUMES FROM THE ROWS, not from the head and not from zero.
//
// One created at the head skips every earlier record the node has not
// applied; one created at the beginning redelivers everything it has applied.
// The SQL checkpoint is the only durable statement of where the node is.
func TestAConsumerResumesFromTheCheckpoint(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_RESUME_LOG", "crewlet.resume.log")
	for i := range 6 {
		if _, _, err := log.Append(t.Context(), "crewlet.resume.log.task."+id(i),
			"", nil, []byte(`{}`)); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_RESUME_LOG", "node-a", 4)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	got := fetchAll(t, cons, 2)
	if len(got) != 2 || got[0].Seq != 5 {
		t.Fatalf("a consumer resuming after 4 received %d record(s) starting "+
			"at %d, want 2 starting at 5", len(got), seqOf(got))
	}
	if pending, err := cons.Pending(t.Context()); err != nil || pending != 0 {
		t.Fatalf("Pending = (%d, %v) after draining", pending, err)
	}
}

// A CONSUMER THAT AGREES WITH THE ROWS IS KEPT.
//
// The ordinary restart: the consumer's next delivery is already the record
// after the checkpoint, so a rebuild is a replicated delete and create on
// every boot of every node, for nothing. The delivery count says it was kept:
// a kept consumer has made the last run's four and the next record's, a
// rebuilt one only the next record's. The last run's acknowledgements are
// awaited at the BROKER first ([awaitAcknowledged] says why).
func TestAConsumerThatAgreesWithTheRowsIsKept(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_KEEP_LOG", "crewlet.keep.log")
	appendN(t, log, "crewlet.keep.log.task.a", 4)
	first, err := q.DomainConsumer(t.Context(), "CREWLET_KEEP_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got := fetchAll(t, first, 4); len(got) != 4 {
		t.Fatalf("the first run received %d of 4 records", len(got))
	}
	awaitAcknowledged(t, first, 4)

	again, err := q.DomainConsumer(t.Context(), "CREWLET_KEEP_LOG", "node-a", 4)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	appendN(t, log, "crewlet.keep.log.task.a", 1)
	if got := fetchAll(t, again, 1); len(got) != 1 || got[0].Seq != 5 {
		t.Fatalf("the kept consumer delivered %v, want the next record, 5", seqsOf(got))
	}
	info, err := consumerInfo(t, again)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Delivered.Consumer != 5 {
		t.Fatalf("the consumer's delivery count is %d after the last run's four and the "+
			"next record, want 5: a consumer that had delivered and acknowledged exactly "+
			"through the checkpoint was rebuilt — a replicated delete and create on every "+
			"ordinary restart", info.Delivered.Consumer)
	}
}

// A CONSUMER AHEAD OF THE ROWS IS REBUILT AT THEM, and the node replays what
// its reader had already given away.
//
// The node's replicated database went BACKWARDS under the same node id while
// the broker kept its consumer: acknowledged records are never handed over
// again (a node that never hydrates), delivered ones only after the ack
// window. Asserted from a checkpoint of nothing and from one partway, off the
// delivery count, which every delivery moves, a redelivery included: a
// rebuilt consumer has made exactly the replay's, a kept one five and more.
//
// Mutation: keep a consumer ahead of the rows, and the records are not handed
// over, or under a one-second ack window take ten deliveries for five.
func TestAConsumerAheadOfTheRowsIsRebuiltAtThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		ack   bool
		after uint64
	}{
		{"acknowledged, rows gone", true, 0},
		{"acknowledged, rows older", true, 2},
		{"delivered only, rows gone", false, 0},
		{"delivered only, rows older", false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stream := "CREWLET_AHEAD_" + strings.ToUpper(strings.NewReplacer(
				" ", "_", ",", "").Replace(tc.name))
			prefix := "crewlet.ahead." + strings.NewReplacer(" ", "", ",", "").Replace(tc.name)
			q, log := openDomain(t, stream, prefix)
			appendN(t, log, prefix+".task.a", 5)

			// THE LAST RUN: every record handed over and, in the
			// acknowledged cases, acknowledged as a committing applier does.
			first, err := q.DomainConsumer(t.Context(), stream, "node-a", 0)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			var handed []statelog.Message
			if tc.ack {
				handed = fetchAll(t, first, 5)
			} else {
				handed = fetchUnacked(t, first, 5)
			}
			if len(handed) != 5 {
				t.Fatalf("the last run was handed %d of 5 records", len(handed))
			}
			// THE STAGING IS CONFIRMED AT THE BROKER: acknowledgements
			// still queued there read as deliveries in flight, so an
			// acknowledged case reopened early would test the delivered one.
			if tc.ack {
				awaitAcknowledged(t, first, 5)
			} else if info, err := consumerInfo(t, first); err != nil {
				t.Fatalf("info: %v", err)
			} else if info.Delivered.Stream != 5 || info.AckFloor.Stream != 0 ||
				info.NumAckPending != 5 {
				t.Fatalf("the staging left the consumer delivered through %d, "+
					"acknowledged through %d with %d in flight, want 5, 0 and 5",
					info.Delivered.Stream, info.AckFloor.Stream, info.NumAckPending)
			}

			// THE ROWS WENT BACKWARDS: this run's checkpoint is below
			// what the consumer delivered.
			again, err := q.DomainConsumer(t.Context(), stream, "node-a", tc.after)
			if err != nil {
				t.Fatalf("reopen at %d: %v", tc.after, err)
			}
			want := 5 - int(tc.after)
			got := fetchAll(t, again, want)
			if len(got) != want || got[0].Seq != tc.after+1 {
				t.Fatalf("a node whose rows are at %d was handed %v by a consumer "+
					"that had already delivered through 5 — the records it is "+
					"missing are ones the broker will not hand over again "+
					"(acknowledged) or not before the %s ack window (delivered)",
					tc.after, seqsOf(got), domainConsumerAckWait)
			}
			// REBUILT, AND REPLAYED BY WHAT WAS REBUILT: its delivery count
			// is exactly the missing records, each once; a kept consumer
			// had already made five and counts on from there.
			info, err := consumerInfo(t, again)
			if err != nil {
				t.Fatalf("info: %v", err)
			}
			if info.Delivered.Consumer != uint64(want) {
				t.Fatalf("the replay of %d record(s) took %d deliveries of the consumer "+
					"the reopen left — a consumer that had delivered through 5 kept for "+
					"a node whose rows are at %d, a record handed over again once the "+
					"%s ack window ran out, rather than by a rebuild",
					want, info.Delivered.Consumer, tc.after, domainConsumerAckWait)
			}
		})
	}
}

// A CONSUMER HOLDING A CEILING OF DEAD DELIVERIES IS REBUILT.
//
// The last run committed its whole pull and died before acknowledging it: the
// checkpoint matches the delivered position, but every in-flight slot is held
// for a dead reader, so nothing new arrives until the ack window frees them.
func TestAConsumerHoldingACeilingOfDeadDeliveriesIsRebuilt(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_CEILING_LOG", "crewlet.ceiling.log")
	const ceiling = domainConsumerMaxAckPending
	body := make([]byte, 16)
	for i := range ceiling + 1 {
		if _, _, err := log.Append(t.Context(), "crewlet.ceiling.log.task.x", "", nil, body); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	first, err := q.DomainConsumer(t.Context(), "CREWLET_CEILING_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if handed := fetchUnacked(t, first, ceiling); len(handed) != ceiling {
		t.Fatalf("the last run was handed %d of %d records", len(handed), ceiling)
	}
	info, err := consumerInfo(t, first)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.NumAckPending != ceiling || info.Delivered.Stream != ceiling {
		t.Fatalf("the staging left %d in flight through %d, want the whole "+
			"ceiling of %d — without it this proves nothing about a full one",
			info.NumAckPending, info.Delivered.Stream, ceiling)
	}

	again, err := q.DomainConsumer(t.Context(), "CREWLET_CEILING_LOG", "node-a", ceiling)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := fetchAll(t, again, 1)
	if len(got) != 1 || got[0].Seq != ceiling+1 {
		t.Fatalf("a node whose rows are at %d was handed %v, want %d at once — a "+
			"consumer whose in-flight ceiling is full of a dead reader's "+
			"deliveries hands over nothing for the whole %s ack window",
			ceiling, seqsOf(got), ceiling+1, domainConsumerAckWait)
	}
}

// A CONSUMER BEHIND THE ROWS IS MOVED UP TO THEM.
//
// Correct if left, but it costs the redeliveries between for the applier to
// drop: after a node ADOPTS a snapshot at boot, the artefact's whole span,
// since each consumer is opened below a checkpoint that just moved.
func TestAConsumerBehindTheRowsIsMovedUpToThem(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_BEHIND_LOG", "crewlet.behind.log")
	appendN(t, log, "crewlet.behind.log.task.a", 4)
	if _, err := q.DomainConsumer(t.Context(), "CREWLET_BEHIND_LOG", "node-a", 0); err != nil {
		t.Fatalf("open: %v", err)
	}
	again, err := q.DomainConsumer(t.Context(), "CREWLET_BEHIND_LOG", "node-a", 3)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got := fetchAll(t, again, 1)
	if len(got) != 1 || got[0].Seq != 4 {
		t.Fatalf("reopening at 3 left the consumer below the rows: it delivered "+
			"%v, want 4 alone", seqsOf(got))
	}
}

// A CONSUMER THE BROKER PLACED AT THE FIRST SURVIVOR IS KEPT.
//
// A node below the trim floor with no donor resumes at its checkpoint plus
// one, which the stream no longer holds. The broker places the consumer at the
// first survivor, delivered AND acknowledged through its predecessor: past the
// checkpoint in raw numbers, yet handing over the same record next, so a
// rebuild would repeat on every boot below the floor. A consumer created after
// the purge is placed there; one that existed is moved there by the purge
// itself, the commoner case. Kept is read off the broker's create
// announcements ([consumerCreates]): no delivery preceded the reopen, so the
// delivery count cannot tell.
func TestAConsumerPlacedAtTheFirstSurvivorIsKept(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// openedBeforeThePurge stages the consumer that existed when the
		// trim ran, rather than one created after it.
		openedBeforeThePurge bool
	}{
		{"created after the purge", false},
		{"moved by the purge", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stream := "CREWLET_TRIMMED_" + strings.ToUpper(strings.ReplaceAll(tc.name, " ", "_"))
			prefix := "crewlet.trimmed." + strings.ReplaceAll(tc.name, " ", "")
			q, log := openDomain(t, stream, prefix)
			appendN(t, log, prefix+".task.a", 6)

			// A NODE WHOSE ROWS ARE AT 2, below the floor once the purge
			// has run, with nobody to donate.
			var first *DomainConsumer
			open := func() {
				var err error
				if first, err = q.DomainConsumer(t.Context(), stream, "node-a", 2); err != nil {
					t.Fatalf("open: %v", err)
				}
			}
			if tc.openedBeforeThePurge {
				open()
			}
			// Everything below 5 goes, so 5 is the first survivor.
			if err := log.Purge(t.Context(), 5); err != nil {
				t.Fatalf("purge: %v", err)
			}
			if !tc.openedBeforeThePurge {
				open()
			}
			info, err := consumerInfo(t, first)
			if err != nil {
				t.Fatalf("info: %v", err)
			}
			if info.Delivered.Stream != 4 || info.AckFloor.Stream != 4 ||
				info.NumAckPending != 0 {
				t.Fatalf("the broker reports the consumer delivered through %d and "+
					"acknowledged through %d with %d in flight, want 4, 4 and none "+
					"— without its placement at the first survivor this case stages "+
					"nothing", info.Delivered.Stream, info.AckFloor.Stream,
					info.NumAckPending)
			}

			// ITS NEXT BOOT, still below the floor.
			creates := consumerCreates(t, q, stream)
			again, err := q.DomainConsumer(t.Context(), stream, "node-a", 2)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			if n := creates(again.name); n != 0 {
				t.Fatalf("the broker announced %d create(s) of the consumer on the reopen, "+
					"want none: a consumer the broker placed at the stream's first survivor "+
					"was rebuilt — into the place it already was, and again on every boot "+
					"below the floor", n)
			}
			if got := fetchAll(t, again, 2); len(got) != 2 || got[0].Seq != 5 {
				t.Fatalf("the kept consumer delivered %v, want 5 and 6", seqsOf(got))
			}
		})
	}
}

// WHICH CONSUMERS ARE KEPT, as arithmetic: every boundary of the judgement,
// including ones a broker would need a trim and a crash to stage. The order of
// the checks is pinned too: a consumer both ahead and holding deliveries is
// reported as the worse of the two.
func TestWhichConsumersAreKept(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                       string
		delivered, ackFloor, first uint64
		inFlight                   int
		after                      uint64
		want                       consumerDrift
	}{
		{"exactly at the checkpoint", 10, 10, 1, 0, 10, driftNone},
		{"an empty stream, a fresh consumer", 0, 0, 0, 0, 0, driftNone},
		{"acknowledged past, rows gone", 10, 10, 1, 0, 0, driftAcknowledged},
		{"acknowledged past, rows older", 10, 10, 1, 0, 7, driftAcknowledged},
		{"acknowledged past and still holding some", 12, 10, 1, 2, 7, driftAcknowledged},
		{"delivered past, acknowledged to the checkpoint", 12, 7, 1, 5, 7, driftDelivered},
		{"delivered past, nothing acknowledged", 5, 0, 1, 5, 0, driftDelivered},
		{"delivered past only what was trimmed, still held", 10, 3, 11, 7, 4, driftInFlight},
		{"in flight at the checkpoint", 10, 6, 1, 4, 10, driftInFlight},
		{"in flight below the checkpoint", 8, 6, 1, 2, 10, driftInFlight},
		{"behind", 3, 3, 1, 0, 10, driftBehind},
		{"behind a trimmed stream's survivor", 3, 3, 8, 0, 10, driftBehind},
		{"behind only what was trimmed", 3, 3, 11, 0, 10, driftNone},
		{"placed at the first survivor by the broker", 10, 10, 11, 0, 4, driftNone},
		{"ahead of a survivor the checkpoint is below", 12, 12, 11, 0, 4, driftAcknowledged},
		{"a purged-all stream, both before its first", 0, 0, 21, 0, 5, driftNone},
		{"unknown first, raw positions compared", 3, 3, 0, 0, 10, driftBehind},
	} {
		if got := driftOf(tc.delivered, tc.ackFloor, tc.inFlight, tc.first, tc.after); got != tc.want {
			t.Errorf("%s: delivered %d, ack floor %d, %d in flight, first %d, "+
				"checkpoint %d — judged %q, want %q", tc.name, tc.delivered,
				tc.ackFloor, tc.inFlight, tc.first, tc.after, got, tc.want)
		}
	}
}

// THE FIRST SEQUENCE IS READ EXACTLY WHEN IT CAN CHANGE THE ANSWER.
//
// An open reads the stream's first sequence only for the drifts
// [consumerDrift.firstMatters] names, held against [driftOf] both ways over
// every consumer state of a small stream:
//
//   - a drift the gate skips is one NO first sequence changes, or the open
//     acts on an answer the read would have overturned;
//   - a drift the gate reads for is one SOME first sequence changes, or the
//     open spends a metadata request on a fleet's boot path for nothing.
func TestTheFirstSequenceIsReadOnlyWhenItCanChangeTheAnswer(t *testing.T) {
	t.Parallel()
	const top = 7
	changes := map[consumerDrift]bool{}
	seen := map[consumerDrift]bool{}
	for delivered := uint64(0); delivered <= top; delivered++ {
		for ackFloor := uint64(0); ackFloor <= delivered; ackFloor++ {
			// A delivery above the floor is either in flight or was
			// acknowledged on its own, so at most that many are held.
			for inFlight := 0; inFlight <= int(delivered-ackFloor); inFlight++ {
				for after := uint64(0); after <= top; after++ {
					raw := driftOf(delivered, ackFloor, inFlight, 0, after)
					seen[raw] = true
					for first := uint64(1); first <= top+2; first++ {
						judged := driftOf(delivered, ackFloor, inFlight, first, after)
						if judged == raw {
							continue
						}
						changes[raw] = true
						if !raw.firstMatters() {
							t.Errorf("delivered %d, ack floor %d, %d in flight, "+
								"checkpoint %d: judged %q on raw positions and %q "+
								"with a first sequence of %d, but the open does not "+
								"read the first sequence for %q", delivered, ackFloor,
								inFlight, after, raw, judged, first, raw)
						}
					}
				}
			}
		}
	}
	for _, d := range []consumerDrift{driftNone, driftAcknowledged,
		driftDelivered, driftInFlight, driftBehind} {
		if !seen[d] {
			t.Fatalf("no state judged %q on raw positions, so this sweep says "+
				"nothing about it — widen the stream", d)
		}
		if d.firstMatters() && !changes[d] {
			t.Errorf("the open reads the first sequence for %q, which no first "+
				"sequence changes — a metadata request on the boot path for "+
				"nothing", d)
		}
	}
}

// ---- the fixtures ----------------------------------------------------- //

func openDomain(t *testing.T, stream, prefix string) (*Queue, *DomainLog) {
	t.Helper()
	q := domainQueue(t, DomainStream{
		Name: stream, Subjects: []string{prefix + ".>"},
		MaxBytes: 16 << 20, Duplicates: time.Minute,
	})
	log, err := q.DomainLog(t.Context(), stream)
	if err != nil {
		t.Fatalf("open %s: %v", stream, err)
	}
	return q, log
}

func fetchAll(t *testing.T, c *DomainConsumer, want int) []statelog.Message {
	t.Helper()
	var out []statelog.Message
	for range 10 {
		batch, err := c.Fetch(t.Context(), want-len(out), 0, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		for _, msg := range batch {
			if err := msg.Ack(); err != nil {
				t.Fatalf("ack: %v", err)
			}
		}
		out = append(out, batch...)
		if len(out) >= want {
			break
		}
	}
	return out
}

// fetchUnacked is what a process that died before acknowledging was handed:
// the records are DELIVERED, and the broker holds each one against the
// in-flight ceiling until its ack window expires.
func fetchUnacked(t *testing.T, c *DomainConsumer, want int) []statelog.Message {
	t.Helper()
	var out []statelog.Message
	for range 10 {
		batch, err := c.Fetch(t.Context(), want-len(out), 0, 500*time.Millisecond)
		if err != nil {
			t.Fatalf("fetch: %v", err)
		}
		out = append(out, batch...)
		if len(out) >= want {
			break
		}
	}
	return out
}

// awaitAcknowledged waits until the BROKER reports this consumer's
// acknowledgements applied through `through`, with nothing in flight, and
// fails the case if it never does.
//
// Sending them is not the same thing: [statelog.Message.Ack] is
// fire-and-forget, the server QUEUES each for the consumer's own goroutine,
// and a consumer lookup is answered by a separate API worker, so a reopen
// straight after a pull can judge the state from before they landed and
// rebuild. The deadline only bounds a broker that never applies them.
func awaitAcknowledged(t *testing.T, c *DomainConsumer, through uint64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		info, err := consumerInfo(t, c)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		if info.AckFloor.Stream == through && info.NumAckPending == 0 {
			return
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("the broker never applied the last run's acknowledgements: "+
				"acknowledged through %d with %d in flight, want through %d with "+
				"none — without them this case is not the state it claims to "+
				"stage", info.AckFloor.Stream, info.NumAckPending, through)
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
}

func appendN(t *testing.T, log *DomainLog, subject string, n int) {
	t.Helper()
	for i := range n {
		if _, _, err := log.Append(t.Context(), subject, "", nil, []byte(`{}`)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
}

// consumerCreates watches the broker announce the consumers it creates on
// stream: call it before the step that may rebuild one, and the returned
// function after it, for how often the consumer named name was created in
// between. A create is the broker's own RECORD of a rebuild; the creation
// instant is not, since two creates inside one clock tick stamp alike.
//
// The subscription is flushed before the step, as the broker announces only
// to a subject somebody listens on. One stream's announcements leave through
// one outbound queue in creation order, so the count ends at a fence consumer
// created after the step; the read's bound is liveness only.
func consumerCreates(t *testing.T, q *Queue, stream string) func(name string) int {
	t.Helper()
	prefix := server.JSAdvisoryConsumerCreatedPre + "." + stream + "."
	sub, err := q.nc.SubscribeSync(prefix + "*")
	if err != nil {
		t.Fatalf("subscribe to %s's consumer creates: %v", stream, err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	if err := q.nc.Flush(); err != nil {
		t.Fatalf("flush the subscription to %s's consumer creates: %v", stream, err)
	}
	return func(name string) int {
		t.Helper()
		fence := "fence-" + name
		if _, err := q.js.CreateConsumer(t.Context(), stream, jetstream.ConsumerConfig{Durable: fence}); err != nil {
			t.Fatalf("create the fence consumer on %s: %v", stream, err)
		}
		t.Cleanup(func() { _ = q.js.DeleteConsumer(context.WithoutCancel(t.Context()), stream, fence) })
		creates := 0
		for {
			msg, err := sub.NextMsg(10 * time.Second)
			if err != nil {
				t.Fatalf("the broker never announced the fence consumer it created on %s: %v", stream, err)
			}
			switch strings.TrimPrefix(msg.Subject, prefix) {
			case fence:
				return creates
			case name:
				creates++
			}
		}
	}
}

func seqOf(msgs []statelog.Message) uint64 {
	if len(msgs) == 0 {
		return 0
	}
	return msgs[0].Seq
}

func id(i int) string   { return string(rune('a' + i)) }
func itoa(i int) string { return string(rune('0' + i)) }

// A CANCELLED CONTEXT ENDS A GROUP'S BLOCKING READ, as a stop.
//
// [jetstream.MessagesContext.Next] takes no context and blocks until a message
// arrives or the iterator stops, so on a quiet log a reader that only
// cancelled waits for ever; the tracker's wake feed sits in this call for the
// node's life, and shutdown joins it. The wall clock is the assertion, since
// the hang is the symptom.
//
// The cancellation reaches the iterator two ways at once (the watcher's Drain
// and the read failing under it), and either must end in a nil delivery and a
// nil error: that is how the change feed tells a shutdown from a failure, and
// an error here is an error line on every clean stop.
func TestAGroupsBlockingReadEndsWithItsContext(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_GROUP_CANCEL", "crewlet.groupcancel")
	defer func() { _ = q.Stop(context.Background()) }()

	ctx, cancel := context.WithCancel(t.Context())
	group, _, err := log.Group(ctx, "waker", 0)
	if err != nil {
		t.Fatalf("open the group: %v", err)
	}
	// NOTHING IS PUBLISHED: an empty log, a company's usual state, is where
	// the blocking read never returns on its own.
	done := make(chan struct{})
	var readErr error
	go func() {
		defer close(done)
		for {
			delivery, err := group.Next(ctx)
			if err != nil || delivery == nil {
				readErr = err
				return
			}
		}
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("a group's blocking read outlived its cancelled context, so " +
			"anything joining it waits for ever")
	}
	if readErr != nil {
		t.Errorf("a cancelled read reported %v, want no error — the caller is "+
			"shutting down, and a shutdown reported as a failure is an error "+
			"line on every clean stop", readErr)
	}

	// AN EXPLICIT STOP IS THE SAME STOP, so a caller that also stops does
	// not panic on a second close.
	if err := group.Stop(); err != nil {
		t.Errorf("stopping an already-cancelled group: %v", err)
	}
	if err := group.Stop(); err != nil {
		t.Errorf("stopping twice: %v", err)
	}
}

// A RECORD THAT REACHES AN ABANDONED PULL GOES TO THE NEXT READER AT ONCE, not
// after the ack window.
//
// A fetch whose context ends leaves its pull request standing on the server
// for the rest of its wait, and a record appended then is delivered to it.
// Over a connection that stays open the next pull would wait out the ack
// window for it, and a read barrier appended then refuses every linearizable
// read as behind. The delivery lands in the handle's buffer, and the next
// fetch must hand it over waiting for nothing and asking for nothing.
//
// Mutation: drop a record that reaches no running fetch, and it never reaches
// the buffer.
func TestARecordReachingAnAbandonedPullIsHandedBack(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_ABANDON_LOG", "crewlet.abandon.log")
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_ABANDON_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// THE ABANDONED PULL: a long wait, given up on almost at once.
	drain, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if got, err := cons.Fetch(drain, 1, 0, 20*time.Second); len(got) != 0 {
		t.Fatalf("an empty log answered %d record(s) (%v)", len(got), err)
	}
	p, err := cons.standing()
	if err != nil {
		t.Fatalf("the standing pull: %v", err)
	}
	// A record the standing pull is still asking for.
	appendN(t, log, "crewlet.abandon.log.task", 1)

	// DELIVERED TO THE PULL NOBODY IS RUNNING, and held for the next reader.
	// A delivery dropped on its way there is in flight until the ack window,
	// past the abandoned request's twenty seconds, so it never arrives.
	waitUntil(t, "the record delivered to the abandoned pull to be held for the "+
		"next fetch", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.buffered) == 1
	})
	p.mu.Lock()
	sentBefore := p.sent
	p.mu.Unlock()
	got, err := cons.Fetch(t.Context(), 1, 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("a fetch that waits for nothing took %d record(s) from %d, want "+
			"sequence 1 — the record delivered to the abandoned pull was held, and "+
			"not handed to the next reader at once", len(got), seqOf(got))
	}
	p.mu.Lock()
	asked := p.sent - sentBefore
	p.mu.Unlock()
	if asked != 0 {
		t.Errorf("a fetch holding the record sent the broker %d request(s) before "+
			"handing it over — what it already held was asked for again", asked)
	}
}

// A CLOSED HANDLE TAKES NOTHING THE NEXT READER OF ITS CONSUMER NEEDS.
//
// A stopped reader's last pull stands at the broker for the rest of its wait,
// and the broker serves the oldest request first, so over a connection that
// outlives the reader a record appended then would sit in the stopped
// reader's inbox until the ack window. Closing the handle drops the inbox, and
// the broker, finding no interest, serves the next reader's request.
//
// Read off whose buffer the record reaches, never the clock. Mutation: close
// the handle without releasing its pull, and the closed reader's buffer takes
// the record.
func TestAClosedHandleTakesNothingTheNextReaderNeeds(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_CLOSED_LOG", "crewlet.closed.log")
	first, err := q.DomainConsumer(t.Context(), "CREWLET_CLOSED_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	// THE STOPPED READER'S PULL: a long wait, given up on almost at once.
	drain, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if got, err := first.Fetch(drain, 1, 0, 20*time.Second); len(got) != 0 {
		t.Fatalf("an empty log answered %d record(s) (%v)", len(got), err)
	}
	waitStanding(t, first)
	closed, err := first.standing()
	if err != nil {
		t.Fatalf("the stopped reader's pull: %v", err)
	}
	first.Close()

	// THE NEXT READER of the same consumer, which agrees with the rows and
	// is therefore kept rather than rebuilt, and its own request.
	second, err := q.DomainConsumer(t.Context(), "CREWLET_CLOSED_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(second.Close)
	next, err := second.standing()
	if err != nil {
		t.Fatalf("the next reader's pull: %v", err)
	}
	next.mu.Lock()
	err = next.request(1, 0, 20*time.Second)
	next.mu.Unlock()
	if err != nil {
		t.Fatalf("the next reader's request: %v", err)
	}
	appendN(t, log, "crewlet.closed.log.task", 1)

	held := func(p *puller) int {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.buffered)
	}
	waitUntil(t, "the record to reach one of the two readers", func() bool {
		return held(closed)+held(next) > 0
	})
	if n := held(closed); n != 0 {
		t.Fatalf("the closed reader's pull holds %d record(s): the broker delivered "+
			"the record to its standing request, where nobody reads it until the "+
			"ack window", n)
	}
	next.mu.Lock()
	sentBefore := next.sent
	next.mu.Unlock()
	got, err := second.Fetch(t.Context(), 1, 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("a fetch that waits for nothing took %d record(s) from %d, want "+
			"sequence 1 — the record delivered to the next reader's request was "+
			"not handed over at once", len(got), seqOf(got))
	}
	next.mu.Lock()
	asked := next.sent - sentBefore
	next.mu.Unlock()
	if asked != 0 {
		t.Errorf("a fetch holding the record sent the broker %d request(s) before "+
			"handing it over — what it already held was asked for again", asked)
	}
}

// A RECORD DELIVERED TO A REQUEST THE CLIENT GAVE UP ON IS TAKEN BY THE NEXT
// FETCH — at once, never after the ack window.
//
// Client and server each end a request on their own clock, so under load the
// server serves one the client counts as over; made here directly, as a server
// running later than [pullGrace] looks. The record is awaited in the handle's
// buffer, and the next fetch must hand it over without asking the broker,
// which would answer that the consumer has nothing.
//
// Mutation: drop a delivery that answers a request the client no longer
// counts, and it never reaches the buffer; hand over the buffer only under a
// request of the fetch's own, and it asks the broker for what it holds.
func TestARecordDeliveredToARequestTheClientGaveUpOnIsTakenByTheNext(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_LATE_LOG", "crewlet.late.log")
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_LATE_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got, err := cons.Fetch(t.Context(), 1, 0, 10*time.Millisecond); err != nil || len(got) != 0 {
		t.Fatalf("an empty log answered %d record(s), %v", len(got), err)
	}
	p, err := cons.standing()
	if err != nil {
		t.Fatalf("the standing pull: %v", err)
	}
	p.mu.Lock()
	if err := p.request(1, 0, 20*time.Second); err != nil {
		p.mu.Unlock()
		t.Fatalf("a long request: %v", err)
	}
	// THE CLIENT HAS STOPPED COUNTING IT; the server has not.
	p.requests[len(p.requests)-1].until = time.Now().Add(-time.Second)
	p.mu.Unlock()
	waitStanding(t, cons)
	appendN(t, log, "crewlet.late.log.task", 1)

	// DELIVERED BEFORE THE FETCH BEGINS, into the buffer it takes from. A
	// dropped delivery is in flight until the ack window, past the
	// request's twenty seconds, so it never arrives.
	waitUntil(t, "the record delivered to the request the client gave up on to be "+
		"held for the next fetch", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.buffered) == 1
	})
	p.mu.Lock()
	sentBefore := p.sent
	p.mu.Unlock()
	got, err := cons.Fetch(t.Context(), 1, 0, 0)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("a fetch that waits for nothing took %d record(s) from %d, want "+
			"sequence 1 — the record the server delivered to a request the client "+
			"had given up on was held, and not handed over at once",
			len(got), seqOf(got))
	}
	p.mu.Lock()
	asked := p.sent - sentBefore
	p.mu.Unlock()
	if asked != 0 {
		t.Errorf("a fetch holding the record sent the broker %d request(s) before "+
			"handing it over — what it already held was asked for again", asked)
	}
}

// A STATUS ENDS ONLY THE REQUEST IT NAMES, never the request standing after it.
//
// A finished request's status (`409 Batch Completed`, a `408`) can arrive
// after the client stopped counting it; counted against the oldest request it
// ends the next one, and the server serves one more than the client counts.
// Made here under the pull's lock: a no-wait request sent and counted out, the
// next sent, so the first's `404` comes once the second stands, which must
// still be counted and serve the next record. A fence behind it, once no
// longer counted, proves the first's status taken: the callback runs in order.
//
// Mutation: count a status against the oldest request, and the `404` ends the
// one standing; let a status end nothing, and the fence is never taken.
func TestAStatusEndsOnlyTheRequestItNames(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_NAMED_LOG", "crewlet.named.log")
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_NAMED_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got, err := cons.Fetch(t.Context(), 1, 0, 10*time.Millisecond); err != nil || len(got) != 0 {
		t.Fatalf("an empty log answered %d record(s), %v", len(got), err)
	}
	p, err := cons.standing()
	if err != nil {
		t.Fatalf("the standing pull: %v", err)
	}
	statusesBefore, err := p.sub.Delivered()
	if err != nil {
		t.Fatalf("the inbox's count: %v", err)
	}
	p.mu.Lock()
	// A request the server ends at once with a `404`, counted out here
	// before that status arrives...
	if err := p.request(1, 0, 0); err != nil {
		p.mu.Unlock()
		t.Fatalf("a no-wait request: %v", err)
	}
	p.requests = p.requests[:len(p.requests)-1]
	// ...and the request that follows it, standing when the status lands.
	if err := p.request(1, 0, 20*time.Second); err != nil {
		p.mu.Unlock()
		t.Fatalf("the next request: %v", err)
	}
	next := p.requests[len(p.requests)-1]
	// The `404` is at the inbox, its callback waiting on the lock held here.
	deadline := time.Now().Add(5 * time.Second)
	for {
		pending, _, err := p.sub.Pending()
		if err != nil {
			p.mu.Unlock()
			t.Fatalf("the inbox's pending count: %v", err)
		}
		delivered, err := p.sub.Delivered()
		if err != nil {
			p.mu.Unlock()
			t.Fatalf("the inbox's count: %v", err)
		}
		if pending > 0 || delivered > statusesBefore {
			break
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			p.mu.Unlock()
			t.Fatal("the server's 404 for a no-wait request never reached the inbox")
		}
		time.Sleep(time.Millisecond) //fixedwait:liveness
	}
	// THE FENCE, behind the first's `404` at the inbox.
	if err := p.request(1, 0, 0); err != nil {
		p.mu.Unlock()
		t.Fatalf("the fence: %v", err)
	}
	fence := p.requests[len(p.requests)-1]
	p.mu.Unlock()
	waitUntil(t, "the fence's own 404 to end it, behind the first's", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return !slices.Contains(p.requests, fence)
	})
	p.mu.Lock()
	standing := len(p.requests) == 1 && p.requests[0] == next
	counted := len(p.requests)
	p.mu.Unlock()
	if !standing {
		t.Fatalf("the client counts %d request(s) after a status for one it had "+
			"already counted out — the status ended the request standing after "+
			"it, so the next fetch sends another while the server still serves "+
			"this one", counted)
	}

	appendN(t, log, "crewlet.named.log.task", 2)
	got, err := cons.Fetch(t.Context(), 1, 0, 5*time.Second)
	if err != nil || len(got) != 1 || seqOf(got) != 1 {
		t.Fatalf("the next fetch returned %d record(s) from %d, %v, want sequence 1",
			len(got), seqOf(got), err)
	}
	info, err := consumerInfo(t, cons)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	if info.NumAckPending != 1 {
		t.Fatalf("the consumer holds %d records in flight after a fetch that "+
			"returned 1 — a second request was sent beside the one standing",
			info.NumAckPending)
	}
}

// standPhantom makes the client count a request the server never received, as
// after a dropped connection or a lost ending status: only the client's own
// presumption ever ends it.
func standPhantom(t *testing.T, cons *DomainConsumer, until time.Duration) {
	t.Helper()
	p, err := cons.standing()
	if err != nil {
		t.Fatalf("the standing pull: %v", err)
	}
	p.mu.Lock()
	p.requests = append(p.requests, &standingRequest{batch: 1, until: time.Now().Add(until)})
	p.mu.Unlock()
}

// A FETCH IS BOUNDED BY ITS WAIT AND THE GRACE even when the request it is
// waiting on never ends.
//
// The ending status is not guaranteed: a dropped connection loses the server's
// copy of a request, and a slow consumer's inbox drops a status like any
// message. A fetch that rechecks its deadline only when a delivery wakes it
// waits on nothing, and its applier applies nothing. The error discriminates:
// a fetch honouring the presumption returns nothing, cleanly, well inside the
// context's ten seconds.
func TestAFetchIsBoundedEvenWhenItsRequestNeverEnds(t *testing.T) {
	t.Parallel()
	q, _ := openDomain(t, "CREWLET_LOST_LOG", "crewlet.lost.log")
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_LOST_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	standPhantom(t, cons, 300*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	got, err := cons.Fetch(ctx, 1, 0, 50*time.Millisecond)
	if err != nil || len(got) != 0 {
		t.Fatalf("a fetch waiting on a request nothing serves answered %d record(s), "+
			"%v — want nothing, once the request is presumed gone", len(got), err)
	}
}

// A REQUEST LEFT STANDING DOES NOT KEEP THE NEXT FETCH FROM ASKING once it is
// presumed gone.
//
// A fetch sends a request only when none stands, so it waits on one an earlier
// fetch left behind; if that one never ends, only the presumption lets this
// fetch send its own. The fetch waits an hour, so one that asks only once its
// wait has passed never asks inside [waitStanding]'s or the case's bounds.
//
// Mutation: presume a request gone only on a wake, and the fetch does not ask
// until its hour has passed; never presume one gone, and it does not ask at
// all.
func TestARequestLeftStandingDoesNotKeepTheNextFetchFromAsking(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_LEFT_LOG", "crewlet.left.log")
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_LEFT_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	standPhantom(t, cons, 200*time.Millisecond)
	type fetched struct {
		got []statelog.Message
		err error
	}
	done := make(chan fetched, 1)
	go func() {
		got, err := cons.Fetch(t.Context(), 1, 0, time.Hour)
		done <- fetched{got, err}
	}()
	waitStanding(t, cons)
	appendN(t, log, "crewlet.left.log.task", 1)
	select {
	case f := <-done:
		if f.err != nil || len(f.got) != 1 || seqOf(f.got) != 1 {
			t.Fatalf("the fetch returned %d record(s) from %d, %v, want sequence 1",
				len(f.got), seqOf(f.got), f.err)
		}
	// LIVENESS: a passing fetch returns once the record arrives, a failing
	// one in an hour; this only ends that wait.
	case <-time.After(15 * time.Second): //fixedwait:liveness
		t.Fatal("a record appended into a fetch's own request was not handed over")
	}
}

// standLong sends a request the server serves for twenty seconds through a
// handle's standing pull, and waits until the server holds it — what an
// engine's last fetch leaves behind when the engine stops.
func standLong(t *testing.T, cons *DomainConsumer) *puller {
	t.Helper()
	p, err := cons.standing()
	if err != nil {
		t.Fatalf("the standing pull: %v", err)
	}
	p.mu.Lock()
	err = p.request(1, 0, 20*time.Second)
	p.mu.Unlock()
	if err != nil {
		t.Fatalf("a long request: %v", err)
	}
	waitWaiting(t, cons, 1)
	return p
}

// waitWaiting waits until the server holds n requests on the consumer.
func waitWaiting(t *testing.T, cons *DomainConsumer, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := consumerInfo(t, cons)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		if info.NumWaiting >= n {
			return
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("the server holds %d request(s) on %s, want %d — a request "+
				"did not reach it", info.NumWaiting, cons.name, n)
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
}

// A CLOSED HANDLE HANDS BACK WHAT IT HELD, to the next handle on the consumer
// at once.
//
// An engine stopped and another started over the same backends opens its own
// handle, and nobody reads the first one's buffer; the second fetch's twenty
// seconds end inside the thirty-second ack window, so a record left there is
// never handed over.
func TestAClosedHandleHandsBackWhatItHeld(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_HELD_LOG", "crewlet.held.log")
	first, err := q.DomainConsumer(t.Context(), "CREWLET_HELD_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p := standLong(t, first)
	// THE SUCCESSOR OPENS while nothing has been delivered, so the consumer
	// is kept: a rebuild would hand everything over again regardless.
	second, err := q.DomainConsumer(t.Context(), "CREWLET_HELD_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open the successor: %v", err)
	}
	appendN(t, log, "crewlet.held.log.task", 1)
	waitUntil(t, "the first handle to hold the record", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.buffered) == 1
	})
	first.Close()
	got, err := second.Fetch(t.Context(), 1, 0, 20*time.Second)
	if err != nil || len(got) != 1 || seqOf(got) != 1 {
		t.Fatalf("the successor's fetch returned %d record(s) from %d, %v, want "+
			"sequence 1 — the record the closed handle held was not handed back",
			len(got), seqOf(got), err)
	}
	if _, err := first.Fetch(t.Context(), 1, 0, time.Millisecond); !errors.Is(err, errHandleClosed) {
		t.Errorf("a fetch through the closed handle answered %v, want it refused", err)
	}
}

// A CLOSED HANDLE'S STANDING REQUEST DOES NOT TAKE THE SUCCESSOR'S NEXT
// RECORD.
//
// The server serves a consumer's requests oldest first, so the closed handle's
// still-standing request would be served before the successor's: a read
// barrier of the second engine delivered to the first one's inbox. Run on a
// member that SERVES LEAVES, as a data node does in a fleet with non-data
// nodes, since such a server finds a request's reply across a link too.
func TestAClosedHandlesStandingRequestDoesNotTakeTheNextRecord(t *testing.T) {
	t.Parallel()
	q := newQueueWith(t, Config{
		ServerName: "member", LeafHost: "127.0.0.1", LeafPort: AnyPort,
		StoreDir: brokertest.TempDir(t), StoreMaxBytes: brokertest.StoreMaxBytes, FleetAuth: testFleetAuth(),
	})
	if err := q.EnsureDomainStream(t.Context(), DomainStream{
		Name: "CREWLET_LATER_LOG", Subjects: []string{"crewlet.later.log.>"},
		MaxBytes: 16 << 20, Duplicates: time.Minute,
	}); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	log, err := q.DomainLog(t.Context(), "CREWLET_LATER_LOG")
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	first, err := q.DomainConsumer(t.Context(), "CREWLET_LATER_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	p := standLong(t, first)
	second, err := q.DomainConsumer(t.Context(), "CREWLET_LATER_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open the successor: %v", err)
	}
	first.Close()
	type fetched struct {
		got []statelog.Message
		err error
	}
	done := make(chan fetched, 1)
	go func() {
		got, err := second.Fetch(t.Context(), 1, 0, 20*time.Second)
		done <- fetched{got, err}
	}()
	waitWaiting(t, second, 2)
	appendN(t, log, "crewlet.later.log.task", 1)
	select {
	case f := <-done:
		if f.err != nil || len(f.got) != 1 || seqOf(f.got) != 1 {
			t.Fatalf("the successor's fetch returned %d record(s) from %d, %v, want "+
				"sequence 1 — the closed handle's request was served it",
				len(f.got), seqOf(f.got), f.err)
		}
	case <-time.After(25 * time.Second): //fixedwait:liveness
		t.Fatal("the successor's fetch did not return")
	}
	// AND THE INBOX IS GONE, so nothing the server still sends it can
	// reach it.
	waitUntil(t, "the closed handle's inbox to be drained", func() bool {
		return !p.sub.IsValid()
	})
}

// A RECORD IS HANDED OVER WITH ITS BURST, not at the end of the wait.
//
// One record does not end a pull request, so a fetch that read its request
// until it ended would hold a record for the rest of the wait.
// [statelog.Fetcher] waits for the FIRST record; the runner decides whether a
// partial run is worth another fetch. The fetch waits an hour, so one holding
// its records for its wait is still holding them when the case gives up.
//
// Mutation: hand the buffer over only once the request has ended or the wait
// has run out, never when the newest delivery says nothing follows it, and the
// fetch holds its records for the hour.
func TestARecordIsHandedOverWithItsBurst(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_BURST_LOG", "crewlet.burst.log")
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_BURST_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	type fetched struct {
		got []statelog.Message
		err error
	}
	done := make(chan fetched, 1)
	go func() {
		got, err := cons.Fetch(t.Context(), statelog.FetchMessages, statelog.FetchBytes,
			time.Hour)
		done <- fetched{got, err}
	}()
	waitStanding(t, cons)
	appendN(t, log, "crewlet.burst.log.task", 3)
	select {
	case f := <-done:
		if f.err != nil || len(f.got) == 0 || seqOf(f.got) != 1 {
			t.Fatalf("the fetch returned %d record(s) from %d, %v", len(f.got), seqOf(f.got), f.err)
		}
	// LIVENESS: a passing fetch returns once the records arrive, a failing
	// one in an hour; this only ends that wait.
	case <-time.After(20 * time.Second): //fixedwait:liveness
		t.Fatal("a fetch holding appended records was still holding them twenty " +
			"seconds in — it holds them for its wait of an hour")
	}
}

// waitStanding waits until the server holds a request for this consumer, so a
// record appended next is delivered to it rather than waiting for a request.
func waitStanding(t *testing.T, cons *DomainConsumer) {
	t.Helper()
	waitWaiting(t, cons, 1)
}

// waitUntil waits up to five seconds for cond, naming what it waited for.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("waited five seconds for %s", what)
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
}

// idlePullBudget is the most one pull of an EMPTY log may allocate, heap
// bytes, whatever the bounds it carries.
//
// 64 KiB: an idle applier pulls twice a second ([statelog.FetchWait]), so
// ≤ 128 KiB/s of garbage per idle domain. The pull needs about 6.5 KiB (the
// server's side included, in this process), so this is ten times that and
// still below BOTH client-library fetches: the byte-bounded one's 32 MiB and
// the count-only one's ≈ 172 KiB at the applier's four thousand. A figure that
// grows with the BATCH a pull may receive is the defect.
const idlePullBudget = 64 << 10

// AN IDLE PULL ALLOCATES FOR WHAT IT RECEIVED, never for the batch it was
// allowed.
//
// The client library's fetches size two channels by the batch on every call
// ([idlePullBudget] has the figures), and the applier pulls every half second
// per domain for the node's life. It is no leak, so only the allocation rate
// shows it, and for a few hundred consumers that rate is an OOM kill.
//
// NOT PARALLEL: the measurement is the process's own allocation counter, which
// a concurrent case would add to.
func TestAnIdlePullAllocatesForWhatItReceived(t *testing.T) {
	q, _ := openDomain(t, "CREWLET_IDLE_PULL", "crewlet.idlepull")
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_IDLE_PULL", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pull := func() {
		got, err := cons.Fetch(t.Context(), statelog.FetchMessages,
			statelog.FetchBytes, 20*time.Millisecond)
		if err != nil || len(got) != 0 {
			t.Fatalf("an idle pull answered %d record(s), %v", len(got), err)
		}
	}
	pull() // the connection's first request warms its own buffers
	const pulls = 20
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for range pulls {
		pull()
	}
	runtime.ReadMemStats(&after)
	per := (after.TotalAlloc - before.TotalAlloc) / pulls
	t.Logf("an idle pull allocated %d bytes", per)
	if per > idlePullBudget {
		t.Fatalf("an idle pull allocated %d KiB, and the budget is %d KiB — a "+
			"pull that sizes its buffers by the batch it MAY receive allocates "+
			"that on every call of an idle applier, twice a second per domain",
			per>>10, idlePullBudget>>10)
	}
}

// ONE PULL HONOURS BOTH OF ITS BOUNDS, and nothing past either is delivered.
//
// The applier asks for a count AND a byte total on every pull, one
// transaction's worth. Asserted at the BROKER, not on the returned slice: a
// record delivered and not returned is held in flight until the ack window,
// so the ack-pending count must equal what came back.
func TestAPullHonoursBothOfItsBounds(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_BOUNDS_LOG", "crewlet.bounds.log")
	body := []byte(`{"pad":"` + strings.Repeat("x", 1000) + `"}`)
	for i := range 6 {
		if _, _, err := log.Append(t.Context(), "crewlet.bounds.log.task", "", nil, body); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_BOUNDS_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	inFlight := func() int {
		t.Helper()
		info, err := consumerInfo(t, cons)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		return info.NumAckPending
	}

	// THE COUNT BINDS, under a byte bound that would admit all six.
	got, err := cons.Fetch(t.Context(), 3, 1<<20, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 3 || seqOf(got) != 1 {
		t.Fatalf("a pull for 3 records under a 1 MiB bound returned %d from %d, "+
			"want 3 from 1", len(got), seqOf(got))
	}
	if n := inFlight(); n != 3 {
		t.Fatalf("the consumer holds %d records in flight after a pull that "+
			"returned 3 — the count was not the request's, so the broker "+
			"delivered past it", n)
	}

	// THE BYTES BIND, under a count that would admit all the rest: a
	// record here is a kilobyte and its envelope, so fifteen hundred bytes
	// holds one and not two.
	got, err = cons.Fetch(t.Context(), 100, 1500, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if len(got) != 1 || seqOf(got) != 4 {
		t.Fatalf("a pull bounded at 1500 bytes returned %d record(s) from %d, "+
			"want 1 from 4", len(got), seqOf(got))
	}
	if n := inFlight(); n != 4 {
		t.Fatalf("the consumer holds %d records in flight after pulls that "+
			"returned 4", n)
	}
}

// A PULL THE SERVER ENDS AS A FAILURE IS AN ERROR, never an empty batch.
//
// A wait running out or a bound met says nothing is there; a consumer deleted
// under a standing request says nothing CAN be, and an applier told the first
// would sit on a log it no longer reads and report itself caught up. It is
// reported when it comes: the fetch waits an hour, so one not woken by the
// failure is still waiting when the case gives up.
//
// Mutation: record a refusing status as an ordinary end, and the fetch asks
// the deleted consumer again rather than reporting anything; record it
// without waking the fetch, and the fetch reports it an hour later.
func TestAPullEndedAsAFailureReportsIt(t *testing.T) {
	t.Parallel()
	q, _ := openDomain(t, "CREWLET_REFUSED_LOG", "crewlet.refused.log")
	cons, err := q.DomainConsumer(t.Context(), "CREWLET_REFUSED_LOG", "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := cons.Fetch(t.Context(), 10, 1<<20, time.Hour)
		done <- err
	}()
	// THE REQUEST MUST BE STANDING before the consumer goes, or the
	// deletion is a pull addressed to nothing rather than one cut off.
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := consumerInfo(t, cons)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		if info.NumWaiting > 0 {
			break
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatal("the pull never reached the server")
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
	if err := q.JetStream().DeleteConsumer(t.Context(), "CREWLET_REFUSED_LOG", cons.Name()); err != nil {
		t.Fatalf("delete the consumer: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, jsapi.ErrPullRefused) {
			t.Fatalf("a pull whose consumer was deleted under it returned %v, "+
				"want the refusal — an empty answer reads as a quiet log", err)
		}
	// LIVENESS: a passing fetch returns once the failure arrives, a failing
	// one in an hour; this only ends that wait.
	case <-time.After(20 * time.Second): //fixedwait:liveness
		t.Fatal("a pull whose consumer was deleted under it went on waiting " +
			"for its whole expiry")
	}
}

// A PULL THAT FAILS HANDS BACK WHAT IT HELD, never to the fetch after it.
//
// The applier reads a failed fetch as the point after which the log may be
// another history (a broker restored under a running node ends its pulls) and
// gives back every record from before it; one the pull's buffer kept across
// the failure would reach it as though delivered after. Staged with a record
// buffered for a request outliving its fetch, the consumer recreated from the
// start: the fetch reports the failure, then the new consumer's first record.
//
// Mutations: leave what the failed fetch held for the next, and that hands
// over the record from before; record no failure from a refusing status, and
// the pull is never told.
func TestAFailedPullHandsBackWhatItHeld(t *testing.T) {
	t.Parallel()
	const stream = "CREWLET_HANDBACK_LOG"
	q, log := openDomain(t, stream, "crewlet.handback.log")
	cons, err := q.DomainConsumer(t.Context(), stream, "node-a", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	appendOne := func(i int) {
		t.Helper()
		if _, _, err := log.Append(t.Context(), "crewlet.handback.log.task."+id(i),
			"", nil, []byte(`{"n":`+itoa(i)+`}`)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// THE FIRST RECORD, taken by a fetch whose request stands on after it
	// returns, with room for nine more.
	appendOne(1)
	got, err := cons.Fetch(t.Context(), 10, 1<<20, 10*time.Second)
	if err != nil || len(got) != 1 || seqOf(got) != 1 {
		t.Fatalf("the first fetch returned %d record(s) from %d (%v), want 1 from 1",
			len(got), seqOf(got), err)
	}
	// THE SECOND, delivered to that standing request with no fetch running:
	// the pull's buffer holds it.
	appendOne(2)
	deadline := time.Now().Add(5 * time.Second)
	for {
		info, err := consumerInfo(t, cons)
		if err != nil {
			t.Fatalf("consumer info: %v", err)
		}
		if info.NumAckPending == 2 && info.NumWaiting > 0 {
			break
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatalf("the second record never reached the standing request: %d "+
				"in flight, %d waiting", info.NumAckPending, info.NumWaiting)
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}

	// THE FAILURE: the case waits for the PULL to be told, not for the
	// delete to return. The server answers the delete on the pull's inbox
	// (`409`) and on the delete's reply, dispatched on two goroutines nothing
	// orders, so a fetch as the delete returns could rightly hand the record
	// over before any failure. In the pull's inbox the record precedes the
	// status, so a pull told of its failure still holds the record.
	if err := q.JetStream().DeleteConsumer(t.Context(), stream, cons.Name()); err != nil {
		t.Fatalf("delete the consumer: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		failed, held := pullFailure(cons)
		if failed {
			if held != 1 {
				t.Fatalf("the pull was told of its failure holding %d record(s), "+
					"want the one delivered before it", held)
			}
			break
		}
		if time.Until(deadline) <= 0 { //fixedwait:measure a poll's own deadline
			t.Fatal("the pull was never told its consumer went")
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
	if _, err := cons.Fetch(t.Context(), 10, 1<<20, 5*time.Second); !errors.Is(err, jsapi.ErrPullRefused) {
		t.Fatalf("the fetch after the consumer went returned %v, want the refusal", err)
	}

	// THE BROKER AS IT NOW IS: the consumer again, from the start.
	if _, err := q.JetStream().CreateConsumer(t.Context(), stream,
		domainConsumerConfig(cons.Name(), 0)); err != nil {
		t.Fatalf("create the consumer again: %v", err)
	}
	got, err = cons.Fetch(t.Context(), 10, 1<<20, 5*time.Second)
	if err != nil {
		t.Fatalf("the fetch after the consumer came back: %v", err)
	}
	if seqOf(got) != 1 {
		t.Fatalf("the first record handed over after the failure is %d, want the new "+
			"consumer's first delivery, 1 — a record the pull held across its failure "+
			"reached the reader as though delivered after it", seqOf(got))
	}
}

// pullFailure is whether c's standing pull has been told of a failure no fetch
// has reported yet, and how many records its buffer holds.
func pullFailure(c *DomainConsumer) (failed bool, held int) {
	c.mu.Lock()
	p := c.pull
	c.mu.Unlock()
	if p == nil {
		return false, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.failure != nil, len(p.buffered)
}

// BenchmarkAnIdlePull is what one pull of an empty log costs, B/op being the
// figure [TestAnIdlePullAllocatesForWhatItReceived] bounds: ≈ 6.5 KiB here,
// against 32 MiB through the client library's byte-bounded fetch and
// ≈ 136 KiB through its count-only one.
func BenchmarkAnIdlePull(b *testing.B) {
	q, err := Open(b.Context(), Config{})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { _ = q.Stop(context.WithoutCancel(b.Context())) })
	if err := q.EnsureDomainStream(b.Context(), DomainStream{
		Name: "CREWLET_IDLE_BENCH", Subjects: []string{"crewlet.idlebench.>"},
		MaxBytes: 16 << 20, Duplicates: time.Minute,
	}); err != nil {
		b.Fatalf("stream: %v", err)
	}
	cons, err := q.DomainConsumer(b.Context(), "CREWLET_IDLE_BENCH", "node-a", 0)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := cons.Fetch(b.Context(), statelog.FetchMessages,
			statelog.FetchBytes, time.Millisecond); err != nil {
			b.Fatalf("fetch: %v", err)
		}
	}
}

// A GROUP CREATED FROM A START BEGINS THERE, AND ONE THAT EXISTS WHERE IT
// STANDS. The start is the caller's record of where the group had got to, for
// the one case the durable position cannot answer: the consumer is gone.
// Created at the log's first record instead, a wake feed's group replays the
// whole retention as wakes. An existing group is the fleet's position, which a
// start never moves.
func TestAGroupCreatedFromAStartBeginsThereAndAnExistingOneWhereItStands(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_GROUP_START", "crewlet.groupstart")
	defer func() { _ = q.Stop(context.Background()) }()
	for i := range 8 {
		if _, _, err := log.Append(t.Context(), "crewlet.groupstart.task."+id(i), "", nil,
			[]byte(`{"n":`+itoa(i)+`}`)); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	next := func(g *DomainGroup) *DomainDelivery {
		t.Helper()
		d, err := g.Next(t.Context())
		if err != nil || d == nil {
			t.Fatalf("read the group: %v, %v", d, err)
		}
		return d
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started, created, err := log.Group(ctx, "waker", 6)
	if err != nil || !created {
		t.Fatalf("create the group from 6 = created %v, %v", created, err)
	}
	for want := uint64(6); want <= 8; want++ {
		d := next(started)
		if d.Seq != want {
			t.Fatalf("a group created from 6 delivered %d, want %d", d.Seq, want)
		}
		if err := d.Ack(); err != nil {
			t.Fatalf("ack: %v", err)
		}
	}
	if err := started.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// ONE RECORD AFTER THE STOP, so the reopened group has exactly one to
	// hand over if it stands where it was, and the log's first if it was
	// moved back.
	if _, _, err := log.Append(t.Context(), "crewlet.groupstart.task.late", "", nil,
		[]byte(`{"n":9}`)); err != nil {
		t.Fatalf("append: %v", err)
	}

	again, created, err := log.Group(ctx, "waker", 1)
	if err != nil || created {
		t.Fatalf("open the existing group = created %v, %v; want it opened as it stands", created, err)
	}
	if d := next(again); d.Seq != 9 {
		t.Fatalf("an existing group asked to start at 1 delivered %d, want 9 where it stood", d.Seq)
	}
	_ = again.Stop()

	fresh, created, err := log.Group(ctx, "other", 0)
	if err != nil || !created {
		t.Fatalf("create a group with no start = created %v, %v", created, err)
	}
	defer func() { _ = fresh.Stop() }()
	if d := next(fresh); d.Seq != 1 {
		t.Fatalf("a group created with no start delivered %d first, want the log's first record", d.Seq)
	}
}
