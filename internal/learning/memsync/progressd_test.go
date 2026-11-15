package memsync

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
)

// EVERY PLACEMENT BEHIND REFUSES THE SEAT AFTER A MINUTE.
//
// Every copy the diary's replay is placed on holds the changelog only to the
// note before its newest, and none ever applies the rest. The replay is placed
// again each second it hears its copy hold nothing for it, and the seat is
// still being taken a minute after the hydration began; past the minute its
// copies have stayed behind, it is refused, naming the sequence they held
// nothing past, the barrier they never applied and the consumers made.
//
// Mutation: never arm the behind clock, and this goes red: the replay is
// placed again for ever.
func TestEveryPlacementBehindRefusesTheSeatAfterAMinute(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		notesOnto(t, log, "n", 3, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{behind: true, holdsTo: 2}}
		h := hydrating(t.Context(), bubbleSyncer(t, db, copies, false))
		time.Sleep(behindWait)
		synctest.Wait()
		if h.returned() {
			t.Fatalf("the hydration returned (%d, %v) a minute after it began, before its copies "+
				"had stayed behind for a minute", h.carried, h.err)
		}
		time.Sleep(copyBehindWait + quietWait + pullExpiry + pullHeartbeat)
		synctest.Wait()
		if !h.returned() {
			t.Fatalf("the hydration was still running past the minute its copies stayed behind")
		}
		made := len(copies.openedFor("agent_diary"))
		if made < 2 || !says(h.err, "held nothing past sequence 2 for 1m0s",
			"had not applied the barrier at sequence 4", fmt.Sprintf("%d consumers made", made)) {
			t.Errorf("the refusal = %v, want it to name sequence 2, the minute, the barrier at "+
				"sequence 4 and the %d consumers made", h.err, made)
		}
	})
}

// A ROW LARGER THAN THE WINDOW CROSSES ALONE AT THE TRANSPORT'S BOUND.
//
// Across a link a pull carries a window. A note larger than it ends a pull
// that delivered nothing (`409 Message Size Exceeds MaxBytes`), and the next
// pull asks one message at the transport's bound, which carries it; the pull
// after it asks a window again.
//
// Mutation: ask the window again after such a pull, and this goes red: the
// seat is refused for a row that fits the transport.
func TestARowLargerThanTheWindowCrossesAloneAtTheTransportsBound(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		notesOnto(t, log, "a", 1, 512)
		notesOnto(t, log, "b", 1, 600<<10)
		notesOnto(t, log, "c", 1, 512)
		copies := newFakeCopies(log)
		s := bubbleSyncer(t, db, copies, true)
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != 3 {
			t.Fatalf("Hydrate = (%d, %v), want the 3 notes", carried, err)
		}
		window, transport := windowOf(s), pullBytesOf(s)
		var asked []string
		for _, p := range copies.pullsOf("agent_diary") {
			asked = append(asked, fmt.Sprintf("%d×%d→%d", p.n, p.maxBytes, p.delivered))
		}
		want := []string{
			fmt.Sprintf("%d×%d→1", fetchBatch, window),
			fmt.Sprintf("%d×%d→0", fetchBatch, window),
			fmt.Sprintf("1×%d→1", transport),
			fmt.Sprintf("%d×%d→3", fetchBatch, window),
		}
		if !slices.Equal(asked, want) {
			t.Errorf("the diary's pulls asked (messages×bytes→delivered) %v, want %v", asked, want)
		}
	})
}

// A PULL STILL CROSSING IS NEITHER A STALL NOR ASKED AGAIN.
//
// Each delivery here takes a second less than the pull's stall to arrive, far
// past the second after which the replay asks its consumer what it says of
// itself, which says the delivery is on its way. The replay waits for it on
// the same pull: it is neither refused, nor asked again, nor made again.
//
// Mutation: ask the pull again, or make the consumer again, when the consumer
// says a delivery is still crossing, and this goes red.
func TestAPullStillCrossingIsNeitherAStallNorAskedAgain(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		notesOnto(t, log, "n", 2, 512)
		copies := newFakeCopies(log)
		s := bubbleSyncer(t, db, copies, false)
		copies.scripts["agent_diary"] = []copyScript{{every: stallOf(s, pullBytesOf(s)) - time.Second}}
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != 2 {
			t.Fatalf("Hydrate = (%d, %v), want the 2 notes", carried, err)
		}
		if pulls, opened := copies.pullsOf("agent_diary"), copies.openedFor("agent_diary"); len(pulls) != 1 ||
			len(opened) != 1 {
			t.Errorf("the diary took %d pulls on %d consumers, want one pull on one", len(pulls), len(opened))
		}
	})
}

// A REPLACEMENT STORED BEFORE THE BARRIER'S DELIVERY COMES BEFORE THE END
// BARRIER, ON THE BROKER THIS BUILD RUNS.
//
// The end barrier's proof rests on the server delivering a consumer's copy in
// sequence order from its store, so a row replaced after the consumer passed
// its old version is delivered at its new sequence, below a barrier published
// after it (nats-server consumer.go, getNextMsg). Held here on the embedded
// server: rows and a barrier, a consumer delivering to the barrier, a
// replacement of an earlier row, then the end barrier; reading on delivers the
// replacement, then the end barrier. A nats-server bump that changes the order
// turns this red.
func TestAReplacementStoredBeforeTheBarriersDeliveryComesBeforeTheEndBarrierOnTheBrokerThisBuildRuns(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	s := syncerOn(t, openStore(t), conn)
	diary := registered(t, "agent_diary")
	first, second := noteOfSize(t, "first", 512), noteOfSize(t, "second", 512)
	publishOn(t, conn, diary.subject(seat.AgentID, first.Values), first)
	publishOn(t, conn, diary.subject(seat.AgentID, second.Values), second)
	barrier, err := s.barrier(t.Context(), seat.AgentID, "the barrier", 0)
	if err != nil {
		t.Fatalf("publish the barrier: %v", err)
	}

	pulls, err := s.replays().open(t.Context(), jetstream.ConsumerConfig{
		Name:           "end-barrier-order",
		FilterSubjects: []string{tableFilter(seat.AgentID, diary.name), barrierSubject(seat.AgentID)},
		DeliverPolicy:  jetstream.DeliverAllPolicy,
		AckPolicy:      jetstream.AckNonePolicy,
	})
	if err != nil {
		t.Fatalf("open the consumer: %v", err)
	}
	t.Cleanup(func() { pulls.Stop(); s.replays().drop(t.Context(), pulls.Name()) })
	if err := pulls.Ask(fetchBatch, s.pullBytes(), time.Minute, 0); err != nil {
		t.Fatalf("ask: %v", err)
	}
	read := func(want int) []uint64 {
		t.Helper()
		var seqs []uint64
		for len(seqs) < want {
			ev, err := pulls.Next(t.Context())
			if err != nil {
				t.Fatalf("read the consumer: %v", err)
			}
			if ev.Kind == jsapi.Delivered {
				seqs = append(seqs, ev.Meta.Sequence.Stream)
			}
		}
		return seqs
	}
	if got := read(3); got[2] != barrier {
		t.Fatalf("the consumer delivered %v, want the two notes and then the barrier at %d", got, barrier)
	}
	replaced := noteOfSize(t, "first", 600)
	publishOn(t, conn, diary.subject(seat.AgentID, replaced.Values), replaced)
	stored := sequenceOf(t, conn, diary.subject(seat.AgentID, replaced.Values))
	end, err := s.barrier(t.Context(), seat.AgentID, "the end barrier", 0)
	if err != nil {
		t.Fatalf("publish the end barrier: %v", err)
	}
	if got := read(2); !slices.Equal(got, []uint64{stored, end}) || stored > end {
		t.Errorf("reading on delivered %v, want the replacement at %d and then the end barrier at %d",
			got, stored, end)
	}
}

// THE HYDRATION'S LINES SAY WHAT IT TOOK.
//
// A hydration of a minute and a quarter says once, a minute in, that it is
// still running and how fast its bytes arrive (memory_hydration_slow), so an
// operator sees a slow link at work rather than a hung acquisition; a replay
// placed again says why (memory_replay_placed_again); and the hydration says
// as it completes how long it took, the pulls it asked and the consumers it
// made (memory_hydrated).
func TestTheHydrationLinesSayWhatItTook(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		notesOnto(t, log, "n", 50, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{behind: true, holdsTo: 2},
			{every: 1500 * time.Millisecond}}
		s := bubbleSyncer(t, db, copies, false)
		var mu sync.Mutex
		lines := map[string][]map[string]any{}
		s.said = func(msg string, args ...any) {
			mu.Lock()
			defer mu.Unlock()
			fields := map[string]any{}
			for i := 0; i+1 < len(args); i += 2 {
				fields[args[i].(string)] = args[i+1]
			}
			lines[msg] = append(lines[msg], fields)
		}
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != 50 {
			t.Fatalf("Hydrate = (%d, %v), want the 50 notes", carried, err)
		}
		mu.Lock()
		defer mu.Unlock()
		if slow := lines["memory_hydration_slow"]; len(slow) != 1 || slow[0]["seat"] != seat.Handle ||
			slow[0]["table"] != "agent_diary" || slow[0]["elapsed"].(time.Duration) < objstore.Stall ||
			slow[0]["elapsed"].(time.Duration) > objstore.Stall+2*time.Second ||
			slow[0]["rows_carried"] == nil || slow[0]["bytes_per_second"] == nil {
			t.Errorf("memory_hydration_slow said %v, want once, a minute in, for the diary", slow)
		}
		if again := lines["memory_replay_placed_again"]; len(again) != 1 || again[0]["why"] != "behind" ||
			again[0]["reached"] != uint64(2) || again[0]["placement"] != 2 {
			t.Errorf("memory_replay_placed_again said %v, want once, behind, at sequence 2, the "+
				"diary's second placement", again)
		}
		done := lines["memory_hydrated"]
		if len(done) != 1 || done[0]["rows"] != 50 || done[0]["placements"] != len(tables)+1 ||
			done[0]["pulls"] == nil || done[0]["took"].(time.Duration) < 50*1500*time.Millisecond {
			t.Errorf("memory_hydrated said %v, want the 50 rows, the %d consumers made, the pulls "+
				"and the minute and more it took", done, len(tables)+1)
		}
	})
}

// THE REPLAY'S KNOBS HOLD THE RELATIONS THEIR REASONS STATE.
//
// A pull's heartbeat at most half its expiry, which the server requires; the
// expiry the moment the replay calls its copy behind; four heartbeats inside
// the quiet wait; the behind bound the object store's stall; and across a
// link the window what the floor carries within the lease's heartbeat.
func TestTheReplaysKnobsHoldTheirReasons(t *testing.T) {
	t.Parallel()
	if 2*pullHeartbeat > pullExpiry {
		t.Errorf("a pull's heartbeat of %v is more than half its expiry of %v", pullHeartbeat, pullExpiry)
	}
	if pullExpiry != copyBehindWait || quietWait != 4*pullHeartbeat {
		t.Errorf("the expiry %v is not the behind wait %v, or the quiet wait %v is not four "+
			"heartbeats of %v", pullExpiry, copyBehindWait, quietWait, pullHeartbeat)
	}
	if behindWait != objstore.Stall {
		t.Errorf("the behind bound is %v, want the object store's stall of %v", behindWait, objstore.Stall)
	}
	for heartbeat, want := range map[time.Duration]int{15 * time.Second: 524288, 5 * time.Second: 174762} {
		if got := windowFor(heartbeat); got != want {
			t.Errorf("the window at a heartbeat of %v is %d bytes, want %d", heartbeat, got, want)
		}
	}
}

// A NODE ACROSS A LINK WITH NO LEASE HEARTBEAT IS REFUSED.
//
// Its replays' window is what a link at the floor carries within the node's
// lease heartbeat, so a node across a link that names none has no window to
// hold its pulls to: it is refused by name rather than given some default.
func TestANodeAcrossALinkWithNoLeaseHeartbeatIsRefused(t *testing.T) {
	t.Parallel()
	_, err := New(openStore(t), &fakeJS{}, func(string) string { return seat.AgentID },
		Config{AcrossLink: true})
	if !says(err, "Config.LeaseHeartbeat") {
		t.Errorf("New across a link with no lease heartbeat answered %v, want a refusal naming "+
			"Config.LeaseHeartbeat", err)
	}
}
