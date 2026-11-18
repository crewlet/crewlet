package memsync

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"
)

// A HYDRATION BOUNDED BY ITS PROGRESS.
//
// The cases here run on a bubble's clock over the model changelog
// (copyfake_test.go): a copy that delivers slowly, one that stops answering,
// one that never catches up, and a count that runs low or high, each played
// exactly where a real broker would leave it to a race.

// diaryContent is the content the store holds for the note id.
func diaryContent(t *testing.T, s *Syncer, id string) string {
	t.Helper()
	var content string
	if err := s.db.SQL().QueryRowContext(t.Context(),
		"SELECT content FROM agent_diary WHERE id = ?", id).Scan(&content); err != nil {
		t.Fatalf("the diary holds no note %s: %v", id, err)
	}
	return content
}

// A HYDRATION WHOSE MEMORY TAKES LONGER THAN FIFTEEN SECONDS TO CROSS
// COMPLETES.
//
// A hydration is bounded by its progress, not a flat time, so a seat whose
// rows cross a slow link steadily is never refused for taking long: forty
// notes a second and a half apart, a minute in all, each past the second after
// which the replay probes the consumer (which says the delivery is still
// crossing), all arrive.
//
// Mutation: bound the hydration by a flat fifteen seconds, or measure the
// silence from the table's start rather than from the last word the replay
// heard, and this goes red.
func TestAHydrationWhoseMemoryTakesLongerThanFifteenSecondsToCrossCompletes(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		ids := notesOnto(t, log, "slow", 40, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{every: 1500 * time.Millisecond}}
		s := bubbleSyncer(t, db, copies, false)
		start := time.Now()
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(ids) {
			t.Fatalf("Hydrate = (%d, %v) after %v, want all %d notes: they kept arriving",
				carried, err, time.Since(start), len(ids))
		}
		if took := time.Since(start); took < 40*1500*time.Millisecond {
			t.Fatalf("the premise: the hydration took %v, want at least a minute", took)
		}
	})
}

// A COPY THAT STOPS ANSWERING REFUSES THE SEAT AFTER ITS STALL.
//
// Three notes arrive and then nothing does: no delivery, no heartbeat, and no
// answer to the consumer's info. The hydration is still running a nanosecond
// before the stall of its pull, the request term and what the pull's bytes take
// to arrive, and refused just past it, naming the bound, the rows carried, the
// barrier and the consumers made.
//
// Mutation: never arm the silence clock, and this goes red: the hydration
// waits for ever.
func TestACopyThatStopsAnsweringRefusesTheSeatAfterItsStall(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		notesOnto(t, log, "n", 5, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{silentAfter: 3}}
		s := bubbleSyncer(t, db, copies, false)
		stall := stallOf(s, pullBytesOf(s))
		h := hydrating(t.Context(), s)
		time.Sleep(stall - time.Nanosecond)
		synctest.Wait()
		if h.returned() {
			t.Fatalf("the hydration returned (%d, %v) a nanosecond before its stall of %v", h.carried,
				h.err, stall)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !h.returned() {
			t.Fatalf("the hydration was still running a second past its stall of %v", stall)
		}
		if !says(h.err, fmt.Sprint(stall), "(3 rows carried)", "the barrier this replay reads to is "+
			"at sequence 6", "1 consumer made", "the next placement sweep") {
			t.Errorf("the refusal = %v, want it to name the stall of %v, the 3 rows carried, the "+
				"barrier at sequence 6, the one consumer made and what happens next", h.err, stall)
		}
	})
}

// A REPLACEMENT PUBLISHED AROUND THE BARRIER IS CARRIED.
//
// A note held before the hydration's barrier is replaced before the replay
// reaches it: the replacement lies past the barrier, and the copy's running
// count has not caught up as the barrier is delivered, so the barrier says
// nothing follows; ending there carries neither version. The replay reads on
// to an end barrier, published once a barrier at or past the hydration's
// arrives, which lies past the replacement, so the replacement arrives first.
//
// Mutation: end at the first barrier at or past the hydration's, or at a
// count of nothing more, or publish the end barrier before that barrier
// arrives, and this goes red.
func TestAReplacementPublishedAroundTheBarrierIsCarried(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		spec := registered(t, "agent_diary")
		notesOnto(t, log, "n", 3, 512)
		newer := noteOfSize(t, "n00000", 600)
		body, err := encode(newer)
		if err != nil {
			t.Fatal(err)
		}
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{placed: func() {
			log.lagged(log.publish(spec.subject(seat.AgentID, newer.Values), body))
		}}}
		s := bubbleSyncer(t, db, copies, false)
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != 3 {
			t.Fatalf("Hydrate = (%d, %v), want the 3 notes, the replaced one as its replacement",
				carried, err)
		}
		if got := diaryContent(t, s, "n00000"); got != newer.Values["content"] {
			t.Errorf("the replaced note holds %d bytes of content, want its replacement's %d", len(got),
				len(newer.Values["content"].(string)))
		}
	})
}

// A COUNT ONE TOO HIGH ENDS THE TABLE AT ITS END BARRIER.
//
// Every delivery's count says one more message follows than the copy holds,
// as the server's can for a moment after its delivery skipped a removed row
// the count has not yet seen. Ending on a count of nothing more would wait out
// the budget for that message; the table ends at its end barrier's delivery,
// whatever the count says.
//
// Mutation: end a table on a count of nothing more, and this goes red.
func TestACountOneTooHighEndsTheTableAtItsEndBarrier(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		ids := notesOnto(t, log, "n", 3, 512)
		copies := newFakeCopies(log)
		for _, spec := range tables {
			copies.scripts[spec.name] = []copyScript{{overcounts: 1}}
		}
		s := bubbleSyncer(t, db, copies, false)
		start := time.Now()
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(ids) {
			t.Fatalf("Hydrate = (%d, %v), want the %d notes", carried, err, len(ids))
		}
		if took := time.Since(start); took > 0 {
			t.Errorf("the hydration took %v of a copy that delivered everything at once, want "+
				"none: it waited for a message the count promised", took)
		}
	})
}

// A REPLAY ON A COPY THAT NEVER CATCHES UP IS PLACED AGAIN AT REACHED + 1.
//
// The diary's first copy holds the changelog only to the note before its
// newest and never applies the rest, as a member still catching up after a
// restart. Waiting on it would hold the hydration until its budget ran out.
// The replay is placed again one past the last sequence it received, on a
// copy that has applied the barrier: every note is carried once.
//
// Mutation: refuse at once rather than place the replay again, or place it
// again from the start of the stream, and this goes red.
func TestAReplayOnACopyThatNeverCatchesUpIsPlacedAgainAtReachedPlusOne(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		ids := notesOnto(t, log, "n", 3, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{behind: true, holdsTo: 2}, {}}
		s := bubbleSyncer(t, db, copies, false)
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(ids) {
			t.Fatalf("Hydrate = (%d, %v), want the %d notes, each counted once", carried, err, len(ids))
		}
		opened := copies.openedFor("agent_diary")
		if len(opened) != 2 {
			t.Fatalf("the diary's replay was placed %d times, want twice", len(opened))
		}
		if again := opened[1].cfg; again.OptStartSeq != 3 {
			t.Errorf("the replay was placed again at sequence %d (%s), want 3, one past the "+
				"last it received", again.OptStartSeq, again.DeliverPolicy)
		}
	})
}

// A WRITER THAT NEVER STOPS CANNOT HOLD A HYDRATION OPEN.
//
// A writer of the seat that keeps writing during its hydration keeps messages,
// which are progress, arriving for ever, so the replay must end on its own: at
// its end barrier, published once. Every note held before the hydration's
// barrier is carried, the one replaced around it as its replacement, and the
// hydration returns while the writer goes on.
//
// Mutation: end a table only once its copy says it holds nothing more, and
// this goes red: the copy never does.
func TestAWriterThatNeverStopsCannotHoldAHydrationOpen(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		spec := registered(t, "agent_diary")
		ids := notesOnto(t, log, "n", 10, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{every: 20 * time.Millisecond}}
		s := bubbleSyncer(t, db, copies, false)
		last := len(ids) - 1
		newer := noteOfSize(t, ids[last], 700)
		stop := make(chan struct{})
		wrote := make(chan struct{})
		go func() {
			defer close(wrote)
			for i := 0; ; i++ {
				timer := time.NewTimer(10 * time.Millisecond)
				select {
				case <-stop:
					timer.Stop()
					return
				case <-timer.C:
				}
				row := newer
				if i > 0 {
					row = noteOfSize(t, fmt.Sprintf("w%05d", i), 512)
				}
				body, err := encode(row)
				if err != nil {
					t.Error(err)
					return
				}
				log.publish(spec.subject(seat.AgentID, row.Values), body)
			}
		}()
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		close(stop)
		<-wrote
		if err != nil {
			t.Fatalf("Hydrate = (%d, %v), want it ended at its end barrier", carried, err)
		}
		for _, id := range ids[:last] {
			diaryContent(t, s, id)
		}
		if got := diaryContent(t, s, ids[last]); got != newer.Values["content"] {
			t.Errorf("the note the writer replaced holds %d bytes of content, want its "+
				"replacement's %d", len(got), len(newer.Values["content"].(string)))
		}
	})
}

// A PULL HOLDS AT MOST fetchBatch ROWS AND ONE WINDOW'S BYTES.
//
// A thousand ordinary notes take one pull in this node's own process;
// fetchBatch and 44 more take two; notes of 10 KiB fill a pull's bytes before
// its count; and across a link no pull asks for or carries more than the
// window that crosses the floor within what a request's term leaves after its
// own round trip.
//
// Mutation: set fetchBatch to 256, or drop the window across a link, and this
// goes red.
func TestAPullHoldsAtMostFetchBatchRowsAndOneWindowsBytes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		notes     int
		size      int
		link      bool
		wantPulls int
	}{
		{"a thousand ordinary notes", 1000, 512, false, 1},
		{"fetchBatch and 44 more", fetchBatch + 44, 512, false, 2},
		{"notes of 10 KiB", 1000, 10 << 10, false, 0},
		{"across a link", 300, 10 << 10, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openStore(t)
			synctest.Test(t, func(t *testing.T) {
				log := newFakeChangelog()
				notesOnto(t, log, "n", tc.notes, tc.size)
				copies := newFakeCopies(log)
				s := bubbleSyncer(t, db, copies, tc.link)
				carried, err := s.Hydrate(t.Context(), seat.Handle)
				if err != nil || carried != tc.notes {
					t.Fatalf("Hydrate = (%d, %v), want the %d notes", carried, err, tc.notes)
				}
				pulls := copies.pullsOf("agent_diary")
				if tc.wantPulls > 0 && len(pulls) != tc.wantPulls {
					t.Errorf("the notes took %d pulls, want %d", len(pulls), tc.wantPulls)
				}
				bound := pullBytesOf(s)
				if tc.link {
					bound = windowOf(s)
				}
				byBytes := false
				for i, p := range pulls {
					if p.delivered > fetchBatch {
						t.Errorf("pull %d delivered %d messages, past the %d a pull asks for", i,
							p.delivered, fetchBatch)
					}
					if p.maxBytes > bound || p.bytes > bound {
						t.Errorf("pull %d asked for %d bytes and carried %d, past the %d it is "+
							"bounded by", i, p.maxBytes, p.bytes, bound)
					}
					byBytes = byBytes || (p.delivered < fetchBatch && i < len(pulls)-1)
				}
				if tc.wantPulls == 0 && !byBytes {
					t.Errorf("no pull of %d notes of %d bytes ended on its bytes", tc.notes, tc.size)
				}
			})
		})
	}
}

// A DROPPED FIRST PULL REMAKES THE CONSUMER.
//
// A cluster drops a new consumer's first pull routinely. The consumer then
// holds the diary's notes and no pull, and has delivered nothing: the replay
// asks it what it says of itself once it has heard nothing for a second,
// makes it again, and the hydration carries every note, with never more than
// one pull out at once.
//
// Mutation: ask the pull again on the same consumer rather than make it
// again, and this goes red.
func TestADroppedFirstPullRemakesTheConsumer(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		ids := notesOnto(t, log, "n", 3, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{dropFirstPull: true}, {}}
		s := bubbleSyncer(t, db, copies, false)
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(ids) {
			t.Fatalf("Hydrate = (%d, %v), want the %d notes", carried, err, len(ids))
		}
		if opened := copies.openedFor("agent_diary"); len(opened) != 2 {
			t.Errorf("the diary's consumer was made %d times, want twice: once, and again for "+
				"the pull it never got", len(opened))
		}
		if copies.mostInFlight > 1 {
			t.Errorf("%d pulls were out at once, want one at most", copies.mostInFlight)
		}
	})
}

// A HYDRATION RUNS WITHOUT A DEADLINE OF ITS OWN.
//
// Nothing a hydration hands the changelog's copies carries a deadline it set:
// every request it makes keeps its own bound, and the hydration as a whole has
// none, so a seat that is merely large is never refused for its size.
//
// Mutation: give the hydration a deadline, as a flat fifteen seconds, and this
// goes red.
func TestAHydrationRunsWithoutADeadlineOfItsOwn(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		notesOnto(t, log, "n", 3, 512)
		copies := newFakeCopies(log)
		s := bubbleSyncer(t, db, copies, false)
		if _, err := s.Hydrate(t.Context(), seat.Handle); err != nil {
			t.Fatalf("Hydrate: %v", err)
		}
		for _, o := range copies.openedFor("agent_diary") {
			if o.deadline {
				t.Errorf("the diary's replay was opened on a context with a deadline the " +
					"hydration set")
			}
		}
	})
}
