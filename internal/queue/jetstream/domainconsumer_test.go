package jetstream

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// EVERY NODE READS THE WHOLE LOG, which is what makes N identical copies.
//
// This is not a work queue. A shared consumer would deliver each record to
// exactly one node — the opposite of replication, and a failure whose symptom
// is two nodes' tracker tables quietly disagreeing about different halves of
// the company.
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
// A consumer created at the head silently skips everything published before it
// existed, which on a state log is every record the node has not applied. One
// created at the beginning on a node that has applied a million records is a
// million redeliveries the applier drops one at a time. The SQL checkpoint is
// the only durable statement of where this node actually is.
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
// The ordinary restart: the last run applied and acknowledged everything it
// was handed, so the consumer's next delivery is already the record after the
// checkpoint. Rebuilding it would be harmless and is still wrong — it is a
// replicated delete and create on every boot of every node, for nothing — and
// it is the direction a judgement that rebuilt whenever in doubt would go.
// The consumer's creation instant is what says it was not rebuilt.
//
// The last run's acknowledgements are awaited at the BROKER before the
// reopen, because that is the staging this case claims: see
// [awaitAcknowledged] for why sending them is not the same thing.
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
	before := createdAt(t, first)

	again, err := q.DomainConsumer(t.Context(), "CREWLET_KEEP_LOG", "node-a", 4)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if after := createdAt(t, again); !after.Equal(before) {
		t.Fatalf("a consumer that had delivered and acknowledged exactly through "+
			"the checkpoint was rebuilt (created %s, now %s) — a replicated "+
			"delete and create on every ordinary restart", before, after)
	}
	appendN(t, log, "crewlet.keep.log.task.a", 1)
	if got := fetchAll(t, again, 1); len(got) != 1 || got[0].Seq != 5 {
		t.Fatalf("the kept consumer delivered %v, want the next record, 5", seqsOf(got))
	}
}

// A CONSUMER AHEAD OF THE ROWS IS REBUILT AT THEM, and the node replays what
// its reader had already given away.
//
// # The node this is
//
// One whose replicated database went BACKWARDS under the same node id —
// deleted and rebuilt, or restored from a copy older than the broker estate
// beside it — while the broker kept its consumer. The applier resumes at the
// checkpoint and waits for the next record; the consumer has already
// delivered it, so either it was acknowledged and the broker will never hand
// it over again, or it was not and the broker hands it over only when its ack
// window expires. The first is a node that never hydrates. The second is one
// that sits unhydrated for thirty seconds on every such restart.
//
// Both are asserted inside a bound far below that window, from a checkpoint
// of nothing (the rows are gone) and from one partway (an older copy).
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

			// THE LAST RUN: every record handed over, and — in the
			// acknowledged cases — every one of them acknowledged, which
			// is what an applier that committed them does.
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
			// THE STAGING IS CONFIRMED AT THE BROKER, because both halves
			// reopen into a rebuild whatever it holds: acknowledgements
			// still queued there read as deliveries in flight, so an
			// acknowledged case that reopened early would pass as the
			// delivered case under a name it did not test.
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
			started := time.Now()
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
			if took := time.Since(started); took >= domainConsumerAckWait/3 {
				t.Fatalf("the replay took %s, which is the ack window rather "+
					"than a rebuild", took)
			}
		})
	}
}

// A CONSUMER HOLDING A CEILING OF DEAD DELIVERIES IS REBUILT.
//
// The last run committed its whole pull and died before acknowledging any of
// it: the checkpoint is exactly where the consumer's delivered position is, so
// nothing past the rows has been given away — and still nothing new arrives,
// because every slot of the in-flight ceiling is held for a reader that will
// never acknowledge, and the broker hands over no more until the ack window
// frees them. The records are harmless; the thirty seconds are not.
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
// Correct if left, and costs the redeliveries between for the applier to drop
// under the in-flight ceiling. That is small after a crash, and it is the whole
// span an artefact covers after a node ADOPTS a snapshot at boot: the join
// runs before the consumers are opened, so each one is opened below a
// checkpoint that just moved — and the runtime adoption's reset was the only
// thing that ever moved a consumer for that cost.
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
// A node below the trim floor with no donor comes up on what it has, and its
// consumer resumes at its checkpoint plus one — a sequence the stream no
// longer holds. The broker places it at the first survivor instead and reports
// it as having delivered AND acknowledged through the survivor's predecessor,
// which is PAST the checkpoint. Comparing the raw numbers calls that
// acknowledged-past and rebuilds it, on every boot for as long as the node
// stays below the floor, into exactly the place it already was: the broker
// skips what the stream no longer holds, so the two positions hand over the
// same record next.
//
// # Both ways the broker places it
//
// A consumer CREATED after the purge is placed there outright. One that
// ALREADY EXISTED is moved there by the purge itself — its start and its ack
// floor are advanced to the first survivor — so it reports exactly the same
// position, and is the more common of the two: the trim purges under the
// consumers of nodes that are running. Neither agrees with the checkpoint in
// raw numbers; both are kept only through the broker's skip.
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
			before := info.Created

			// ITS NEXT BOOT, still below the floor.
			again, err := q.DomainConsumer(t.Context(), stream, "node-a", 2)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			if after := createdAt(t, again); !after.Equal(before) {
				t.Fatalf("a consumer the broker placed at the stream's first "+
					"survivor was rebuilt (created %s, now %s) — into the place it "+
					"already was, and again on every boot below the floor",
					before, after)
			}
			if got := fetchAll(t, again, 2); len(got) != 2 || got[0].Seq != 5 {
				t.Fatalf("the kept consumer delivered %v, want 5 and 6", seqsOf(got))
			}
		})
	}
}

// WHICH CONSUMERS ARE KEPT, as arithmetic.
//
// The broker cases above can each stage one shape; this is every boundary of
// the judgement, including the ones a stream would take a trim and a crash to
// stage. The order of the checks is part of what is pinned: a consumer that is
// both ahead and holding deliveries is reported as the worse of the two.
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
// An open judges the raw positions first and asks the broker for the stream's
// first sequence only for the drifts [consumerDrift.firstMatters] names. Both
// directions of that are a claim about [driftOf], so both are held against it
// over every consumer state a broker can report on a small stream:
//
//   - a drift the gate skips is one NO first sequence changes, or the open
//     keeps or reports a consumer on an answer the read would have
//     overturned;
//   - a drift the gate reads for is one SOME first sequence changes, or the
//     open spends a metadata request on a fleet's boot path that could not
//     have mattered — which is what it did for every consumer holding
//     deliveries in flight, the commonest state a busy restart leaves.
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
// Sending them is not the same thing. [statelog.Message.Ack] is the client's
// fire-and-forget acknowledgement, and the server only QUEUES each one for the
// consumer's own goroutine, while a consumer lookup is answered by a separate
// API worker — nothing orders the two. So a reopen straight after a pull can
// judge a state from before the acknowledgements landed, which reads as
// deliveries in flight and is rebuilt: the right decision about the wrong
// staging, and it failed the keep case about one run in a hundred under the
// race detector.
//
// A BOUNDED WAIT ON WHAT THE BROKER REPORTS, rather than on time: the state
// is read until it is the one the case claims, and the bound only decides how
// long a broker that never applies them is given before the case says so.
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
		if time.Now().After(deadline) {
			t.Fatalf("the broker never applied the last run's acknowledgements: "+
				"acknowledged through %d with %d in flight, want through %d with "+
				"none — without them this case is not the state it claims to "+
				"stage", info.AckFloor.Stream, info.NumAckPending, through)
		}
		time.Sleep(5 * time.Millisecond)
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

// createdAt is when the broker made the consumer this handle reads, which is
// what tells a kept consumer from a rebuilt one.
func createdAt(t *testing.T, c *DomainConsumer) time.Time {
	t.Helper()
	info, err := consumerInfo(t, c)
	if err != nil {
		t.Fatalf("consumer info: %v", err)
	}
	return info.Created
}

func seqOf(msgs []statelog.Message) uint64 {
	if len(msgs) == 0 {
		return 0
	}
	return msgs[0].Seq
}

func id(i int) string   { return string(rune('a' + i)) }
func itoa(i int) string { return string(rune('0' + i)) }

// A CANCELLED CONTEXT ENDS A GROUP'S BLOCKING READ.
//
// # Why this is a hang rather than a leak
//
// [jetstream.MessagesContext.Next] takes no context and blocks until a
// message arrives or the iterator is stopped. A quiet log means it blocks for
// ever — so a consumer that only cancelled a context and then joined its
// reader would wait on a goroutine that has no way to notice.
//
// It is not hypothetical. The tracker's wake feed sits in exactly this call
// for the life of a node, and shutdown joins it: without the watcher this
// asserts, every engine test that stopped before its own context expired
// wedged, and the only symptom was a suite that never finished.
//
// The assertion is the WALL CLOCK, because that is the symptom. A flag saying
// the watcher exists would go on being true while the block moved elsewhere.
//
// AND IT ENDS AS A STOP RATHER THAN AS A FAILURE, which is the second half and
// is not cosmetic: a cancellation reaches the iterator by two routes at once —
// the watcher's Drain and the read failing under it — so whichever lands first
// decides what Next returns, and the losing half must answer the same way. A
// nil delivery with a nil error is how the change feed above tells a shutdown
// from a failure, so an error here is an error line on every clean stop of
// every node.
func TestAGroupsBlockingReadEndsWithItsContext(t *testing.T) {
	t.Parallel()
	q, log := openDomain(t, "CREWLET_GROUP_CANCEL", "crewlet.groupcancel")
	defer func() { _ = q.Stop(context.Background()) }()

	ctx, cancel := context.WithCancel(t.Context())
	group, err := log.Group(ctx, "waker")
	if err != nil {
		t.Fatalf("open the group: %v", err)
	}
	// NOTHING IS PUBLISHED, deliberately: an empty log is the state a
	// company spends almost all of its time in, and it is the one where
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
	case <-time.After(10 * time.Second):
		t.Fatal("a group's blocking read outlived its cancelled context, so " +
			"anything joining it waits for ever")
	}
	if readErr != nil {
		t.Errorf("a cancelled read reported %v, want no error — the caller is "+
			"shutting down, and a shutdown reported as a failure is an error "+
			"line on every clean stop", readErr)
	}

	// AND AN EXPLICIT STOP IS THE SAME STOP, so a caller that does both —
	// which a shutdown ordering change makes ordinary — does not panic on
	// a second close.
	if err := group.Stop(); err != nil {
		t.Errorf("stopping an already-cancelled group: %v", err)
	}
	if err := group.Stop(); err != nil {
		t.Errorf("stopping twice: %v", err)
	}
}
