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
	"github.com/crewlet/crewlet/internal/jsprovision"
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
		if made < 2 || !says(h.err, "no copy delivered anything past sequence 2 for 1m0s",
			"the barrier at sequence 4 it reads to never arrived", fmt.Sprintf("%d consumers made", made),
			"the last ending because its copy held nothing for it", "the next placement sweep") {
			t.Errorf("the refusal = %v, want it to name sequence 2, the minute, the barrier at "+
				"sequence 4, the %d consumers made, why the last ended and what happens next", h.err, made)
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

// A ROW REPLACED AHEAD OF A REPLAY IS DELIVERED BEFORE ITS END BARRIER, ON THE
// BROKER THIS BUILD RUNS.
//
// The end barrier's proof rests on the server delivering a consumer's copy in
// sequence order from its store, reading the store at each delivery, so a row
// replaced after the replay's barrier was stored and before the replay
// reached the row is delivered at its NEW sequence, past that barrier and
// below an end barrier published after it (nats-server consumer.go,
// getNextMsg). Held here on the embedded server, as a replay reads, one
// message a pull: two notes and a barrier; a pull takes the first note; the
// second, not yet delivered, is replaced, which removes its first version
// ahead of the consumer and stores the replacement past the barrier; the next
// pull takes the barrier, its first version skipped; the end barrier is
// published once the barrier has arrived, as a replay publishes it; and
// reading on delivers the replacement and then the end barrier. The first
// version is never delivered, and the replacement is not missed. A
// nats-server bump that changes that order turns this red.
func TestARowReplacedAheadOfAReplayIsDeliveredBeforeTheEndBarrierOnTheBrokerThisBuildRuns(t *testing.T) {
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
	// read asks one pull of n messages, with no byte bound, so it ends at
	// its count's last delivery and nothing follows it, and answers the
	// subject and stream sequence of each delivery.
	read := func(n int) []string {
		t.Helper()
		if err := pulls.Ask(n, 0, time.Minute, 0); err != nil {
			t.Fatalf("ask: %v", err)
		}
		var got []string
		for len(got) < n {
			ev, err := pulls.Next(t.Context())
			if err != nil {
				t.Fatalf("read the consumer: %v", err)
			}
			if ev.Kind != jsapi.Delivered {
				t.Fatalf("the pull of %d ended with %s %s after %v", n, ev.Code, ev.Description, got)
			}
			got = append(got, fmt.Sprintf("%s@%d", ev.Msg.Subject, ev.Meta.Sequence.Stream))
		}
		return got
	}
	at := func(subject string) string {
		return fmt.Sprintf("%s@%d", subject, sequenceOf(t, conn, subject))
	}
	firstSubject := diary.subject(seat.AgentID, first.Values)
	secondSubject := diary.subject(seat.AgentID, second.Values)
	if got, want := read(1), at(firstSubject); got[0] != want {
		t.Fatalf("the first pull delivered %v, want the first note, %s", got, want)
	}
	replaced := noteOfSize(t, "second", 600)
	publishOn(t, conn, secondSubject, replaced)
	stored := sequenceOf(t, conn, secondSubject)
	if stored <= barrier {
		t.Fatalf("the premise: the replacement was stored at %d, not past the barrier at %d", stored, barrier)
	}
	b := barrierSubject(seat.AgentID)
	if got, want := read(1), fmt.Sprintf("%s@%d", b, barrier); got[0] != want {
		t.Fatalf("the pull after the first note delivered %v, want the barrier, %s, the replaced "+
			"note's first version skipped", got, want)
	}
	end, err := s.barrier(t.Context(), seat.AgentID, "the end barrier", 0)
	if err != nil {
		t.Fatalf("publish the end barrier: %v", err)
	}
	want := []string{fmt.Sprintf("%s@%d", secondSubject, stored), fmt.Sprintf("%s@%d", b, end)}
	if got := read(2); !slices.Equal(got, want) {
		t.Errorf("reading on delivered %v, want the replacement and then the end barrier, %v", got, want)
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
// the quiet wait; the behind bound the object store's stall; and on either
// topology a replay's consumer kept by the broker past the last bytes
// of a pull at the transport's bound and an end barrier asked to its ceiling
// behind them, the longest a live replay goes between two pulls.
func TestTheReplaysKnobsHoldTheirReasons(t *testing.T) {
	t.Parallel()
	agentID := func(string) string { return seat.AgentID }
	for _, s := range []*Syncer{newSyncer(openStore(t), &fakeJS{}, agentID),
		newSyncerAcross(openStore(t), &fakeJS{}, agentID)} {
		s.copies = newFakeCopies(newFakeChangelog())
		n := s.pullBytes()
		between := jsapi.Delivering(n, s.pace) + s.timing.Arriving(s.endBarrierArrival(n)).Lookup
		if reap := s.reapAfter(); reap < between {
			t.Errorf("a replay's consumer is reaped %v after its last word, before the %v a live "+
				"replay may take to ask its next pull (pace %v)", reap, between, s.pace)
		}
	}
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
}

// A PULL ACROSS A LINK CROSSES WITHIN A REQUEST'S TERM.
//
// A replay's pull rides the coordination connection, and across a link the
// requests sent after it, a lease renewal's among them wherever the renewals
// share that link, wait behind whatever of its reply is still crossing. Such
// a request is answered within its term ([jsapi.RequestTerm]) only if the
// pull crosses the floor ([objstore.MiBPace]) in what the term leaves after
// the request's own round trip, one [jsprovision.ReadTerm]: four seconds,
// 139 810 bytes, whatever lease the node holds. So a pull across a link is
// bounded by the widest window that crosses within that, and one in a
// member's own process by no window at all.
//
// Mutation: size the window by the whole five-second term, leaving the
// request no round trip of its own, and this goes red.
func TestAPullAcrossALinkCrossesWithinARequestsTerm(t *testing.T) {
	t.Parallel()
	agentID := func(string) string { return seat.AgentID }
	budget := jsapi.RequestTerm - jsprovision.ReadTerm
	crossing := func(bytes int) time.Duration {
		return time.Duration(bytes) * objstore.MiBPace / objstore.MiB
	}
	window := windowOf(newSyncerAcross(openStore(t), &fakeJS{}, agentID))
	if c := crossing(window); c > budget {
		t.Errorf("a pull across a link carries %d bytes, which cross the floor in %v: a request sent "+
			"behind it is answered past the %v its %v term leaves after its own round trip", window, c,
			budget, jsapi.RequestTerm)
	}
	if crossing(window+1) <= budget {
		t.Errorf("a pull across a link carries %d bytes, and a byte more still crosses the floor "+
			"within %v: round trips thrown away", window, budget)
	}
	if window != 139810 {
		t.Errorf("a pull across a link carries %d bytes, want the 139 810 the floor carries in %v",
			window, budget)
	}
	if got := windowOf(newSyncer(openStore(t), &fakeJS{}, agentID)); got != 0 {
		t.Errorf("a pull in a member's own process is windowed at %d bytes, want no window", got)
	}
}
