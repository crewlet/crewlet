package memsync

import (
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats-server/v2/server"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore"
)

// WHAT PLACES A REPLAY AGAIN, AND WHAT BOUNDS IT.
//
// The cases here run on a bubble's clock over the model changelog
// (copyfake_test.go), but one, which runs on the embedded broker.

// A PULL WHOSE ROWS THE STORE REFUSES FAILS THE HYDRATION, THOUGH ITS REPLAY
// WOULD BE PLACED AGAIN.
//
// The diary's first copy holds the changelog through a note the store
// refuses (one carrying nothing but its id, which breaks the table's NOT NULL
// columns) and never applies the barrier, so the replay is placed again past
// it once its copy has held nothing for a second. The pull's rows are written
// first, and the transaction fails. A replay that went on to its new
// placement had moved past rows that never committed, and the hydration
// completed with none of them.
//
// Mutation: place the replay again whatever the pull's transaction answered,
// and this goes red.
func TestAPullWhoseRowsTheStoreRefusesFailsTheHydrationThoughItsReplayWouldBePlacedAgain(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		notesOnto(t, log, "n", 2, 512)
		refused := Row{Table: "agent_diary", Values: map[string]any{"id": "n9"}}
		body, err := encode(refused)
		if err != nil {
			t.Fatal(err)
		}
		log.publish(registered(t, "agent_diary").subject(seat.AgentID, refused.Values), body)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{behind: true, holdsTo: 3}, {}}
		s := bubbleSyncer(t, db, copies, false)
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err == nil {
			t.Fatalf("Hydrate = (%d, nil) over a pull whose transaction the store refused, want the "+
				"seat refused", carried)
		}
		if held := countRows(t, db, "agent_diary"); held != 0 || carried != 0 {
			t.Errorf("the refused hydration reported %d rows and left %d notes, want none", carried, held)
		}
		if opened := len(copies.openedFor("agent_diary")); opened != 1 {
			t.Errorf("the diary's replay was placed %d times, want once: it was placed again past "+
				"rows that never committed", opened)
		}
	})
}

// A REPLAY EVERY PULL OF WHICH IS REFUSED IS REFUSED AFTER A MINUTE, ASKING
// FOR A CONSUMER A SECOND.
//
// Every consumer the diary's replay is made on refuses its pulls, as a
// consumer the broker deletes under them or a request limit does, and none
// delivers anything. Each refusal places the replay again, which nothing
// bounded: the replay asked for a consumer, and its deletion, as fast as the
// broker answered, for ever. Nothing delivered for a minute refuses the seat,
// naming the status the last placement ended with, and a placement that
// delivered nothing is followed by the next a second after it.
//
// Mutation: start the behind clock only where a copy says it holds nothing,
// and this goes red: the replay is placed again for ever.
func TestAReplayEveryPullOfWhichIsRefusedIsRefusedAfterAMinuteAskingForAConsumerASecond(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		notesOnto(t, log, "n", 3, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{refuses: "Consumer Deleted"}}
		h := hydrating(t.Context(), bubbleSyncer(t, db, copies, false))
		time.Sleep(behindWait - time.Nanosecond)
		synctest.Wait()
		if h.returned() {
			t.Fatalf("the hydration returned (%d, %v) before nothing had been delivered for a minute",
				h.carried, h.err)
		}
		time.Sleep(copyBehindWait)
		synctest.Wait()
		if !h.returned() {
			t.Fatal("the hydration was still running a second past a minute in which nothing was delivered")
		}
		made := len(copies.openedFor("agent_diary"))
		if most := int(behindWait/copyBehindWait) + 1; made < 2 || made > most {
			t.Errorf("the diary's replay was placed %d times in its minute, want between 2 and %d, "+
				"one a second", made, most)
		}
		if !says(h.err, "no copy delivered anything past sequence 0 for 1m0s", "409 Consumer Deleted",
			fmt.Sprintf("%d consumers made", made)) {
			t.Errorf("the refusal = %v, want it to name the minute, the 409 the last placement ended "+
				"with and the %d consumers made", h.err, made)
		}
	})
}

// A DELIVERY THE LINK LOST PLACES THE REPLAY AGAIN PAST THE LAST IT RECEIVED.
//
// The link loses the second note's delivery: the consumer counts it, and the
// third arrives counted after it. The third is discarded, never written, and
// the replay is placed again one past the first note, the last it received
// with no gap before it, where the lost note is delivered: every note is
// carried, once.
//
// Mutation: place the replay again past the third, or write it, and this goes
// red.
func TestADeliveryTheLinkLostPlacesTheReplayAgainPastTheLastItReceived(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		ids := notesOnto(t, log, "n", 3, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{loses: 2}, {}}
		s := bubbleSyncer(t, db, copies, false)
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(ids) {
			t.Fatalf("Hydrate = (%d, %v), want the %d notes, each once", carried, err, len(ids))
		}
		opened := copies.openedFor("agent_diary")
		if len(opened) != 2 || opened[1].cfg.OptStartSeq != 2 {
			var at []uint64
			for _, o := range opened {
				at = append(at, o.cfg.OptStartSeq)
			}
			t.Errorf("the diary's replay was placed at %v, want twice, the second at sequence 2", at)
		}
	})
}

// A PULL ASKS NO MORE MESSAGES THAN ITS CONSUMER TAKES.
//
// A broker whose JetStream limits set max_request_batch gives every consumer
// made without one that limit, and refuses a pull asking more. A replay
// asking its own 1024 was refused at every pull, on every consumer, and the
// seat with it. It asks what the consumer it was given takes, as the reply
// that made it says: here a hundred, so 250 notes take three pulls.
//
// Mutation: ask fetchBatch whatever the consumer takes, and this goes red.
func TestAPullAsksNoMoreMessagesThanItsConsumerTakes(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		ids := notesOnto(t, log, "n", 250, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{maxBatch: 100}}
		s := bubbleSyncer(t, db, copies, false)
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(ids) {
			t.Fatalf("Hydrate = (%d, %v), want the %d notes", carried, err, len(ids))
		}
		pulls := copies.pullsOf("agent_diary")
		for i, p := range pulls {
			if p.n > 100 {
				t.Errorf("pull %d asked %d messages of a consumer that takes 100", i, p.n)
			}
		}
		if len(pulls) != 3 {
			t.Errorf("the notes took %d pulls, want 3 of a hundred", len(pulls))
		}
	})
}

// A BROKER THAT LIMITS A PULL'S BATCH HYDRATES THE SEAT.
//
// The same, on the embedded broker this build runs, its JetStream limits
// setting max_request_batch to a hundred: the consumer a replay is made on
// carries that limit in the reply that made it (nats-server consumer.go,
// setConsumerConfigDefaults), and the 150 notes are carried.
func TestABrokerThatLimitsAPullsBatchHydratesTheSeat(t *testing.T) {
	t.Parallel()
	_, conn := brokerServerWith(t, func(opts *server.Options) { opts.JetStreamLimits.MaxRequestBatch = 100 })
	diary := registered(t, "agent_diary")
	for i := range 150 {
		note := noteOfSize(t, fmt.Sprintf("n%05d", i), 512)
		publishOn(t, conn, diary.subject(seat.AgentID, note.Values), note)
	}
	s := syncerOn(t, openStore(t), conn)
	spy := newSpyCopies(s)
	carried, err := s.Hydrate(t.Context(), seat.Handle)
	if err != nil || carried != 150 {
		t.Fatalf("Hydrate = (%d, %v), want the 150 notes", carried, err)
	}
	for i, p := range spy.of(tableFilter(seat.AgentID, diary.name)) {
		if p.n > 100 {
			t.Errorf("pull %d asked %d messages of a broker that takes 100", i, p.n)
		}
	}
}

// AN END BARRIER ACKNOWLEDGED PAST THE PULL'S STALL IS NO SILENCE OF THE COPY'S.
//
// Every end barrier's acknowledgement takes five seconds more than a pull's
// stall to come, as one across an election does, asked again meanwhile. The
// replay publishes it between two words of its pull and hears nothing while
// it waits, so a silence counted from the word before the publish was past
// its stall as the publish returned, and the seat was refused for a copy that
// had stayed silent only while the replay was not reading it. Each delivery
// takes a tenth of a second to cross, so the end barrier is still on its way
// as the publish returns, and nothing is on the inbox yet. The silence counts
// from the publish's return: the hydration completes.
//
// Mutation: count the silence from the barrier's delivery rather than from the
// end barrier's return, and this goes red.
func TestAnEndBarrierAcknowledgedPastThePullsStallIsNoSilenceOfTheCopys(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		ids := notesOnto(t, log, "n", 3, 512)
		copies := newFakeCopies(log)
		for _, spec := range tables {
			copies.scripts[spec.name] = []copyScript{{every: 100 * time.Millisecond}}
		}
		s := bubbleSyncer(t, db, copies, false)
		late := stallOf(s, pullBytesOf(s)) + 5*time.Second
		s.js.(*fakeJS).ackAfter = func(n int) time.Duration {
			if n == 1 {
				// THE HYDRATION'S OWN BARRIER, at once.
				return 0
			}
			return late
		}
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(ids) {
			t.Fatalf("Hydrate = (%d, %v), want the %d notes", carried, err, len(ids))
		}
	})
}

// WHAT ARRIVES WHILE A PROBE GOES UNANSWERED IS HEARD BEFORE THE PULL IS
// CALLED SILENT.
//
// Each note takes five seconds to arrive, and nobody answers the consumer's
// info, so each probe the replay sends once it has heard nothing for a second
// runs to the end of the pull's stall, and the note arrives during it. A
// replay that refused the seat as the probe ran out refused it with the note
// on its inbox. The inbox is read first: every note is carried.
//
// Mutation: refuse the seat for its silence without reading what the inbox
// holds, and this goes red.
func TestWhatArrivesWhileAProbeGoesUnansweredIsHeardBeforeThePullIsCalledSilent(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		ids := notesOnto(t, log, "n", 2, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{every: 5 * time.Second, probeUnanswered: true}}
		s := bubbleSyncer(t, db, copies, false)
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(ids) {
			t.Fatalf("Hydrate = (%d, %v), want the %d notes", carried, err, len(ids))
		}
	})
}

// A CONSUMER STILL WITHOUT ITS PULL AFTER THE REMAKES IS WAITED FOR, NEVER
// ASKED A SECOND PULL.
//
// Every consumer's first pull takes five seconds to reach it, as one held
// back by a slow link. The replay makes the consumer again each second it
// says it never got its pull, as many times as a cluster's dropped first pulls
// call for, and then waits for that consumer's pull, which arrives and
// delivers every note. A replay that asked a second pull of it on the same
// inbox had two out once the first arrived, and read every pull after them
// against the other's end.
//
// Mutation: ask the pull again once the remakes are spent, and this goes red.
func TestAConsumerStillWithoutItsPullAfterTheRemakesIsWaitedForNeverAskedASecondPull(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	synctest.Test(t, func(t *testing.T) {
		log := newFakeChangelog()
		ids := notesOnto(t, log, "n", 3, 512)
		copies := newFakeCopies(log)
		copies.scripts["agent_diary"] = []copyScript{{firstPullAfter: 5 * time.Second}}
		s := bubbleSyncer(t, db, copies, false)
		carried, err := s.Hydrate(t.Context(), seat.Handle)
		if err != nil || carried != len(ids) {
			t.Fatalf("Hydrate = (%d, %v), want the %d notes", carried, err, len(ids))
		}
		opened := copies.openedFor("agent_diary")
		if len(opened) != jsapi.FirstPullRemakes+1 {
			t.Fatalf("the diary's consumer was made %d times, want %d: once and a remake for each "+
				"pull it never got", len(opened), jsapi.FirstPullRemakes+1)
		}
		last := 0
		for _, p := range copies.pullsOf("agent_diary") {
			if p.placement == len(opened)-1 {
				last++
			}
		}
		if last != 1 {
			t.Errorf("the last consumer was asked %d pulls, want the one it was waited for", last)
		}
	})
}

// EVERY REQUEST A HYDRATION MAKES KEEPS ITS OWN BOUND.
//
// A hydration has no deadline of its own, so each of its requests is held to
// its own: the changelog's identity and the hydration's barrier are asked for
// no longer than the longest silence the server keeps and their crossing, a
// count's consumer is asked to be made for its create's budget, and a
// consumer's deletion is given its cleanup bound and its crossing. Each
// request here is never answered (the deletion, at once), and the hydration
// is still running a nanosecond before its bound and done past it.
//
// Mutation: hold the barrier or the identity read to the client's five
// seconds, or to nothing, or ask the count's create without its budget, or
// delete without the cleanup bound, and a row goes red.
func TestEveryRequestAHydrationMakesKeepsItsOwnBound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		cut   func(*fakeJS)
		bound func(*Syncer) time.Duration
		says  string
		fails bool
	}{
		{"the changelog's identity", func(j *fakeJS) { j.identityUnanswered = true },
			func(s *Syncer) time.Duration { return s.timing.Arriving(jsapi.Crossing(apiBytes, s.pace)).Lookup },
			"read the memory changelog's identity", true},
		{"the hydration's barrier", func(j *fakeJS) { j.ackAfter = func(int) time.Duration { return -1 } },
			func(s *Syncer) time.Duration { return s.timing.Arriving(jsapi.Sending(0, s.pace)).Lookup },
			"no member of the changelog acknowledged it", true},
		{"a retired table's count", func(j *fakeJS) { j.countUnanswered = true },
			func(s *Syncer) time.Duration { return s.createBudget() }, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openStore(t)
			synctest.Test(t, func(t *testing.T) {
				log := newFakeChangelog()
				notesOnto(t, log, "n", 3, 512)
				s := bubbleSyncer(t, db, newFakeCopies(log), true)
				tc.cut(s.js.(*fakeJS))
				bound := tc.bound(s)
				h := hydrating(t.Context(), s)
				time.Sleep(bound - time.Nanosecond)
				synctest.Wait()
				if h.returned() {
					t.Fatalf("the hydration returned (%d, %v) a nanosecond before the %v its request "+
						"is held to", h.carried, h.err, bound)
				}
				time.Sleep(time.Second)
				synctest.Wait()
				if !h.returned() {
					t.Fatalf("the hydration was still running a second past the %v its request is "+
						"held to", bound)
				}
				if tc.fails != (h.err != nil) || (tc.fails && !says(h.err, tc.says)) {
					t.Errorf("the hydration answered %v, want a refusal %t saying %q", h.err, tc.fails,
						tc.says)
				}
			})
		})
	}
	t.Run("a consumer's deletion", func(t *testing.T) {
		t.Parallel()
		db := openStore(t)
		synctest.Test(t, func(t *testing.T) {
			log := newFakeChangelog()
			notesOnto(t, log, "n", 3, 512)
			s := bubbleSyncer(t, db, newFakeCopies(log), true)
			if _, err := s.Hydrate(t.Context(), seat.Handle); err != nil {
				t.Fatalf("Hydrate: %v", err)
			}
			js := s.js.(*fakeJS)
			want := consumerCleanupTimeout + jsapi.Sending(apiBytes, objstore.MiBPace)
			if len(js.deleteBounds) == 0 {
				t.Fatal("the hydration deleted no consumer of the changelog's own")
			}
			for _, got := range js.deleteBounds {
				if got != want {
					t.Errorf("a consumer's deletion was given %v, want the cleanup bound and its "+
						"crossing, %v", got, want)
				}
			}
		})
	})
}
