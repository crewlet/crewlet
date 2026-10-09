package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A LOST RUN IS ANNOUNCED EXACTLY ONCE, whichever node finishes its ending and
// however often. The ending is DECIDED on the row first ([RecordedEnding]), its
// announcement goes out before the record is deleted, and every attempt that
// finishes it publishes the same event — the same id and instant, which is the
// event store's own key — so a node that stops between any two steps leaves its
// successor the ending to finish rather than a reason of its own to announce.
// It used to be announced only after the delete: a delete that landed and
// reported a failure, or a node that stopped between the two, left the loss
// announced by nobody, and a seat's next holder announced the run it reaped as
// abandoned whatever it had been decided on.

// announcement is one publish of a lost run's announcement on the events
// topic, with the identity the event store keys it on.
type announcement struct {
	id     string
	at     time.Time
	failed types.SandboxRunFailed
}

// announcements is every publish of a lost run's announcement on the events
// topic, repeats included — the raw sequence, before any store collapses it.
func (r *coordRig) announcements() []announcement {
	r.queue.mu.Lock()
	defer r.queue.mu.Unlock()
	var out []announcement
	for _, p := range r.queue.published {
		payload, ok := p.event.Data.(*types.SandboxRunFailed)
		if !ok || p.topic != topics.Event(payload.EventType()) {
			continue
		}
		out = append(out, announcement{id: p.event.ID.String(), at: p.event.Timestamp, failed: *payload})
	}
	return out
}

// announcedOnce asserts every announcement published is ONE event — one
// (event_time, event_id), which is what the event store and the fleet's reads
// collapse repeats on — for the reason given, and that at least one went out.
func announcedOnce(t *testing.T, rig *coordRig, reason string) []announcement {
	t.Helper()
	got := rig.announcements()
	if len(got) == 0 {
		t.Fatalf("the run was never announced lost, want it announced once as %q", reason)
	}
	for _, a := range got {
		if a.id != got[0].id || !a.at.Equal(got[0].at) {
			t.Fatalf("announced as %d different events %+v, want one — every attempt publishes "+
				"the identity its ending recorded", len(got), got)
		}
		if a.failed.Reason != reason {
			t.Fatalf("announced %q, want %q: the reason the ending was decided for", a.failed.Reason, reason)
		}
	}
	return got
}

// deleteLandsThenFails deletes a run's record and then reports that it could
// not: the write landed and the store's answer was lost.
type deleteLandsThenFails struct{ PendingStore }

func (s deleteLandsThenFails) Finish(ctx context.Context, turnID, ending string) (PendingRun, bool, error) {
	if _, _, err := s.PendingStore.Finish(ctx, turnID, ending); err != nil {
		return PendingRun{}, false, err
	}
	return PendingRun{}, false, errRefusedCall
}

// A DELETE THAT LANDED AND REPORTED A FAILURE STILL LEAVES THE LOSS ANNOUNCED,
// once. The reap of a launch nobody finished deletes the record and is told the
// delete failed; the ending is kept, and its retry finds no record. Announced
// only after the delete, the run was announced by nobody: the attempt that would
// have made it found nothing left to end.
func TestAnEndingWhoseDeleteLandedAndFailedIsAnnouncedOnce(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launching("t1")
	next := reaper(t.Context(), t, rig, deleteLandsThenFails{rig.pending}, 2, nil)
	rig.finished("t1")
	next.fireRetries()
	announcedOnce(t, rig, types.SandboxFailureAbandoned)
	if got := rig.failures(); len(got) != 1 {
		t.Fatalf("announced %d losses, want one", len(got))
	}
}

// A RUN IS ANNOUNCED FOR THE REASON ITS ENDING WAS DECIDED ON, whichever node
// finishes it. A collect finds the box gone, so the node holding the seat
// decides to end the run as `collect_unreachable` — and the broker refuses the announcement, and
// the node stops before it can try again. The seat's next holder finds the
// ending decided on the row and finishes it: one announcement, for the reason it
// was decided on. It used to find a claim and reap it as an abandoned tail, so
// the run was announced lost for a reason it did not end for, and the reason it
// did end for was announced by nobody.
func TestAnEndingIsAnnouncedForItsOwnReasonByTheNodeThatFinishesIt(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launch("t1")
	rig.coordinator.countRun("swe", StatusRunning)
	rig.runner.Finish(Result{Success: true})
	// GONE, so the run is given up at once rather than its collection
	// retried ([Coordinator.collectFailed]).
	rig.runner.CollectErr = errBoxGoneMidRead
	rig.failPublishes(errors.New("the broker is unreachable"))
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if got := rig.get("t1"); got.Ending == nil || got.Ending.Reason != types.SandboxFailureCollect {
		t.Fatalf("the run's ending = %+v, want it decided as %q and kept for its retry",
			got.Ending, types.SandboxFailureCollect)
	}
	rig.coordinator.Stop()
	rig.failPublishes(nil)

	reaper(t.Context(), t, rig, rig.pending, 2, nil)
	rig.finished("t1")
	announcedOnce(t, rig, types.SandboxFailureCollect)
}

// AN ENDING FINISHED BY TWO NODES IS ONE EVENT. The reaping holder announces the
// loss and stops before its delete lands; the holder after it finishes the same
// ending and announces it again — under the SAME id and instant, so the event
// store, whose key that pair is, keeps one row, and the fleet's reads merge the
// two copies into one.
func TestAnEndingFinishedByTwoNodesIsOneEvent(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launching("t1")
	first := reaper(t.Context(), t, rig, &finishUntil{refusingStore: &refusingStore{inner: rig.pending}}, 2, nil)
	if got := rig.get("t1"); got.Ending == nil {
		t.Fatal("the reap's ending is not on the row its delete could not take")
	}
	if n := len(rig.announcements()); n != 1 {
		t.Fatalf("the reaping holder published %d announcements, want one before its delete", n)
	}
	first.coordinator.Stop()

	reaper(t.Context(), t, rig, rig.pending, 3, nil)
	rig.finished("t1")
	if got := announcedOnce(t, rig, types.SandboxFailureAbandoned); len(got) != 2 {
		t.Fatalf("published %d copies, want the second holder's repeat of the first's", len(got))
	}
}

// AN ANNOUNCEMENT THE BROKER REFUSED KEEPS THE ENDING, and the retry makes it:
// the record is not deleted until the loss is on the record, because a deleted
// row is read by nobody and the announcement is the only account of how the run
// ended.
func TestAnAnnouncementTheBrokerRefusedIsMadeByTheRetry(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launching("t1")
	rig.failPublishes(errors.New("the broker is unreachable"))
	next := reaper(t.Context(), t, rig, rig.pending, 2, nil)
	if got := rig.get("t1"); got.Ending == nil {
		t.Fatal("the run's record was deleted before its loss was announced")
	}
	rig.failPublishes(nil)
	if next.fireRetries() != 1 {
		t.Fatal("nothing was scheduled to finish the ending its announcement held up")
	}
	rig.finished("t1")
	announcedOnce(t, rig, types.SandboxFailureAbandoned)
}

// THE ANNOUNCEMENT GOES OUT WHILE THE RECORD STILL EXISTS, which is what lets a
// node that stops between the two leave the ending to be finished — and the
// announcement made again, as the same event — rather than lose it.
func TestAnEndingIsAnnouncedBeforeItsRecordIsDeleted(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	rig.launching("t1")
	var recordAtAnnouncement []bool
	rig.queue.mu.Lock()
	rig.queue.before = func() {
		_, found, err := rig.pending.Get(t.Context(), "t1")
		recordAtAnnouncement = append(recordAtAnnouncement, err == nil && found)
	}
	rig.queue.mu.Unlock()
	reaper(t.Context(), t, rig, rig.pending, 2, nil)
	rig.finished("t1")
	announcedOnce(t, rig, types.SandboxFailureAbandoned)
	if len(recordAtAnnouncement) == 0 || !recordAtAnnouncement[0] {
		t.Fatalf("the record was gone when the loss was announced (%v): a node stopped between "+
			"the two leaves the loss announced by nobody", recordAtAnnouncement)
	}
}

// AN ENDING DECIDED ON A RUN IN ANY STATUS IS FINISHED BY THE SEAT'S NEXT
// HOLDER, not only a claim's. A retirement decides to end a run parked on its
// question, and the broker refuses the announcement; the retirement's node stops.
// The seat's next holder finds the ending on a row that still reads as a parked
// question and finishes it — announced once, for the retirement's reason —
// rather than counting it as a question waiting on a person, which nothing would
// ever end again.
func TestAnEndingDecidedOnAParkedRunIsFinishedByTheNextHolder(t *testing.T) {
	t.Parallel()
	rig := newCoordRig(t)
	parkOnAQuestion(t, rig)
	rig.failPublishes(errors.New("the broker is unreachable"))
	if err := rig.coordinator.RetireSeat(t.Context(), "swe", "retirement:1", 9); err == nil {
		t.Fatal("a retirement whose announcement was refused reported its runs ended")
	}
	if got := rig.get("t1"); got.Status != StatusAwaiting || got.Ending == nil {
		t.Fatalf("run %q with ending %+v, want a parked row with its ending decided", got.Status, got.Ending)
	}
	rig.coordinator.Stop()
	rig.failPublishes(nil)

	next := reaper(t.Context(), t, rig, rig.pending, 10, nil)
	rig.finished("t1")
	announcedOnce(t, rig, types.SandboxFailureSeatRemoved)
	if _, awaits := next.coordinator.SeatRuns("swe"); awaits {
		t.Fatal("the next holder counted a run whose ending is decided as a question waiting on a person")
	}
}

// A RESUME THAT BROKE BEFORE ITS TURN BEGAN IS ANNOUNCED, ONCE, like every other
// run lost without its turn. Nothing of the turn ran, so no completion of its own
// says what became of it, and the run is ended rather than retried — it used to
// leave a guard breach and nothing else, so the turn parked on the run read as
// parked for good. On every route: a completion's resume, a recorded answer's
// inline attempt and its retry — the last two saying the person's reply went
// back to the seat.
func TestAResumeBrokenBeforeItsTurnIsAnnouncedOnce(t *testing.T) {
	t.Parallel()
	brokenBeforeTurn := func(rig *coordRig) {
		rig.resumer.failWith(ErrResumeAbandoned)
		rig.resumer.beforeTurn = true
	}
	t.Run("a completion", func(t *testing.T) {
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.runner.Finish(Result{Success: true, Text: "done"})
		brokenBeforeTurn(rig)
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
		rig.finished("t1")
		got := announcedOnce(t, rig, types.SandboxFailureResumeBroken)
		if got[0].failed.Detail != resumeBrokenDetail {
			t.Errorf("detail %q, want nothing said of a reply: none drove this resume", got[0].failed.Detail)
		}
	})
	t.Run("a completion whose record could not be read again", func(t *testing.T) {
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.runner.Finish(Result{Success: true, Text: "done"})
		brokenBeforeTurn(rig)
		rig.coordinator.pending = &refusingStore{inner: rig.pending, refuse: []string{"Get"}}
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
		rig.finished("t1")
		announcedOnce(t, rig, types.SandboxFailureResumeBroken)
	})
	t.Run("a recorded answer's inline attempt", func(t *testing.T) {
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		brokenBeforeTurn(rig)
		r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
		if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
			chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
			t.Fatalf("R1 = %q, want it recorded", d)
		}
		rig.finished("t1")
		got := announcedOnce(t, rig, types.SandboxFailureResumeBroken)
		if got[0].failed.Detail != resumeBrokenDetail+replyReturnedDetail {
			t.Errorf("detail %q, want it saying the reply went back to the seat", got[0].failed.Detail)
		}
	})
	t.Run("a recorded answer's retry", func(t *testing.T) {
		rig := newCoordRig(t)
		parkOnAQuestion(t, rig)
		rig.resumer.failWith(errors.New("transient"))
		r1 := replyAt("use main", rig.get("t1").AskedAt.Add(time.Minute))
		if d, _ := rig.coordinator.TryResumeFromAnswer(t.Context(), "swe",
			chatReply(answerOnTheDM, "use main", r1)); d != AnswerConsumed {
			t.Fatalf("R1 = %q, want it recorded", d)
		}
		brokenBeforeTurn(rig)
		rig.fireRetries()
		rig.finished("t1")
		announcedOnce(t, rig, types.SandboxFailureResumeBroken)
	})
	t.Run("but not one whose turn ran", func(t *testing.T) {
		rig := newCoordRig(t)
		rig.launch("t1")
		rig.runner.Finish(Result{Success: true, Text: "done"})
		rig.resumer.failWith(ErrResumeAbandoned)
		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Fatalf("OnCompleted: %v", err)
		}
		rig.finished("t1")
		if got := rig.announcements(); len(got) != 0 {
			t.Fatalf("announced %+v for a resume whose turn ran and published its own completion", got)
		}
	})
}
