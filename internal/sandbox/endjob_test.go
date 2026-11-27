package sandbox

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// stampSpy is a run's store that keeps every capture stamped on a row, so a case
// can read whether a take was final after the ending that took it has deleted
// the row.
type stampSpy struct {
	PendingStore
	mu     sync.Mutex
	stamps []SessionCapture
}

func (s *stampSpy) StampCapture(ctx context.Context, turnID string, capture SessionCapture) (bool, error) {
	s.mu.Lock()
	s.stamps = append(s.stamps, capture)
	s.mu.Unlock()
	return s.PendingStore.StampCapture(ctx, turnID, capture)
}

// begun is every capture stamped as begun: one per take.
func (s *stampSpy) begun() []SessionCapture {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []SessionCapture
	for _, c := range s.stamps {
		if c.State == CaptureBegun {
			out = append(out, c)
		}
	}
	return out
}

// spyStamps puts a [stampSpy] in front of the rig coordinator's store.
func (r *coordRig) spyStamps() *stampSpy {
	spy := &stampSpy{PendingStore: r.coordinator.pending}
	r.coordinator.pending = spy
	return spy
}

// steps is what a case watched happen, in order.
type steps struct {
	mu   sync.Mutex
	seen []string
}

func (s *steps) note(step string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, step)
}

func (s *steps) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.seen)
}

// first is where step first falls, or -1.
func (s *steps) first(step string) int { return slices.Index(s.all(), step) }

// watchEnd has the rig record, in order, the run's bridge session closed, its
// job asked to end — which a script then ends — and its job killed, with how
// many sessions had been taken by then.
func (r *coordRig) watchEnd(box *FakeSandbox) *steps {
	seen := &steps{}
	r.coordinator.ended = func(string) { seen.note("session closed") }
	r.runner.EndFunc = func(_ context.Context, box Sandbox, _ RunHandle) (EndAsk, error) {
		seen.note("asked")
		r.runner.EndJobIn(box)
		return EndAsked, nil
	}
	box.KillJobFunc = func(context.Context, string) error {
		seen.note(fmt.Sprintf("killed after %d takes", len(r.runner.Takes())))
		return nil
	}
	return seen
}

// A STOP ENDS ITS JOB BEFORE IT TAKES ITS SESSION. The frame that decides the
// stop closes the run's bridge session and goes; the ending's retry reads the
// box, and the read ENDS THE JOB first (asks its script, which ends it, then
// sweeps what is left), then takes the session final before the box is
// reclaimed, recording the capture before the turn's end. It is stamped
// INTERRUPTED though its script packed it after a clean exit: a harness told
// to stop was stopped mid-step.
//
// Mutation: take the session before the job's end, and it is packed as it
// stands, not final; read a job ended on request as a clean exit, and its
// capture is stamped whole.
func TestAStopEndsItsJobBeforeItTakesItsSession(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, _ := keepingRig(t)
		stamps := rig.spyStamps()
		launched := rig.launchKept(t, "t1")
		seen := rig.watchEnd(rig.provider.Box(launched.SandboxID))
		// WHAT THE ENDED JOB'S SCRIPT REPORTS: the archive it packed as its own
		// exit does, after a harness that went within its grace.
		rig.runner.EndsAs(Result{Text: "halfway through", SessionID: "s-1",
			Captured: &rig.runner.Session.Archive, JobEnd: JobEndAsked})
		if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
			t.Fatalf("StopSeat: %v", err)
		}
		if rig.runner.Asks() != 0 {
			t.Fatal("the frame that decided the stop asked the job to end itself")
		}
		rig.fireRetries()
		rig.awaitKeeps()
		rig.fireRetries()

		closed, asked := seen.first("session closed"), seen.first("asked")
		if closed < 0 || asked < 0 || closed > asked || seen.first("killed after 0 takes") < asked {
			t.Fatalf("the stop went %v, want its session closed, its job asked to end and swept, before anything "+
				"was taken", seen.all())
		}
		takes := rig.runner.Takes()
		if len(takes) != 1 || takes[0].Pack {
			t.Fatalf("takes %+v, want the session the job's end left", takes)
		}
		if begun := stamps.begun(); len(begun) != 1 || !begun[0].Final || !begun[0].Interrupted {
			t.Fatalf("the stop's capture was stamped %+v, want it final and interrupted", begun)
		}
		if killed := rig.provider.KilledIDs(); !slices.Contains(killed, launched.SandboxID) {
			t.Fatalf("killed %v, want the box reclaimed after its session was taken", killed)
		}
		if got := writes.calls(); !slices.Equal(got, []string{"capture", "end"}) {
			t.Fatalf("the log was written %v, want the capture before the turn's end", got)
		}
	})
}

// A LOSS ENDS ITS JOB BEFORE IT TAKES ITS SESSION. A launch whose engine
// restarted before its turn could park is reaped as its seat is recovered, its
// job perhaps still working: the ending first closes its bridge session, so
// nothing it does after acts as the seat, then the read ends the job and takes
// the session final, and the box is reclaimed after.
//
// Mutations: drop the loss's end, and the job is never asked and its session is
// packed as it stands; drop the early close of the session, and the job acts as
// the seat through its whole take, until the reclaim closes it.
func TestALossEndsItsJobBeforeItTakesItsSession(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		stamps := rig.spyStamps()
		launched := rig.launching("t1")
		rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
		seen := rig.watchEnd(rig.provider.Box(launched.SandboxID))
		if err := rig.recoverSeat(t.Context(), "node-b:1", 9); err != nil {
			t.Fatalf("RecoverSeat: %v", err)
		}
		rig.fireRetries()
		rig.awaitKeeps()
		rig.fireRetries()

		order := seen.all()
		closed, asked := seen.first("session closed"), seen.first("asked")
		if closed < 0 || asked < 0 || closed > asked || seen.first("killed after 0 takes") < asked {
			t.Fatalf("the loss went %v, want its session closed, then its job asked to end and swept, before "+
				"anything was taken", order)
		}
		takes := rig.runner.Takes()
		if len(takes) != 1 || takes[0].Pack {
			t.Fatalf("takes %+v, want the session the lost job's end left", takes)
		}
		if begun := stamps.begun(); len(begun) != 1 || !begun[0].Final {
			t.Fatalf("the loss's capture was stamped %+v, want it final", begun)
		}
		if killed := rig.provider.KilledIDs(); !slices.Contains(killed, launched.SandboxID) {
			t.Fatalf("killed %v, want the lost job's box reclaimed after", killed)
		}
	})
}

// A CAPPED JOB IS ENDED BEFORE ITS SESSION IS TAKEN. A job still working past
// its turn's wall-clock cap is announced as finished, and the collection that
// claims it ends it as a stop's read does (bridge session closed, script asked
// to end the job, the rest swept), then takes the session FINAL and
// INTERRUPTED (its harness stopped mid-step), never beside a harness still
// writing it. The read then PAUSES the box, as every collection's does, and
// reclaims nothing: only the turn's ending reclaims it.
//
// Mutation: read the job as it stands, and it is never asked to end and is
// packed not final; end it after the take, and the take precedes the request;
// skip the end where the poll answered running, and the job is taken working;
// pause nothing after the take, and the turn resumes with its box running.
func TestACappedJobIsEndedBeforeItsSessionIsTaken(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := keepingRig(t)
		stamps := rig.spyStamps()
		launched := rig.launchUntil("t1", rig.now.Add(-time.Minute))
		rig.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
		rig.coordinator.countRun("swe", StatusRunning)
		box := rig.provider.Box(launched.SandboxID)
		seen := rig.watchEnd(box)
		// WHAT THE ENDED JOB'S SCRIPT REPORTS: the archive it packed as its own
		// exit does, after a harness that went within its grace.
		rig.runner.EndsAs(Result{Text: "halfway through", SessionID: "s-1",
			Captured: &rig.runner.Session.Archive, JobEnd: JobEndAsked})
		// PAST ITS TURN'S CAP a resume that fails after the collection is the
		// turn's last: the run is ended at once, and its ending reclaims the box
		// as soon as the resume has returned.
		rig.resumer.failWith(errors.New("held for the case to read the box"))
		// THE BOX AS THE TURN RESUMES: after its collection, and before the
		// ending that reclaims it.
		var resumed int
		var pausedAtResume, reclaimedAtResume bool
		rig.resumer.during = func(context.Context, PendingRun) {
			resumed++
			pausedAtResume = box.Paused()
			reclaimedAtResume = slices.Contains(rig.provider.KilledIDs(), launched.SandboxID)
		}

		payload, ev := rig.completion("t1")
		if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
			t.Logf("OnCompleted: %v", err)
		}
		rig.awaitKeeps()

		closed, asked := seen.first("session closed"), seen.first("asked")
		if closed < 0 || asked < 0 || closed > asked || seen.first("killed after 0 takes") < asked {
			t.Fatalf("the capped job's collection went %v, want its session closed, its job asked to end and "+
				"swept, before anything was taken", seen.all())
		}
		takes := rig.runner.Takes()
		if len(takes) != 1 || takes[0].Pack {
			t.Fatalf("takes %+v, want the one session the job's end left", takes)
		}
		if begun := stamps.begun(); len(begun) != 1 || !begun[0].Final || !begun[0].Interrupted {
			t.Fatalf("the capped job's capture was stamped %+v, want it final and interrupted", begun)
		}
		if resumed != 1 || !pausedAtResume || reclaimedAtResume {
			t.Fatalf("the turn resumed %d times, its box paused %v and reclaimed %v as it did; want it resumed "+
				"once, its box paused by the collection, as every collection's is, and reclaimed only by the "+
				"turn's ending after", resumed, pausedAtResume, reclaimedAtResume)
		}
	})
}

// A JOB THAT OUTLIVES ITS END IS KILLED BEFORE ITS TAKE. A job not over by the
// end's wait (a pack stuck, a call wedged) is killed: a kill that ends it
// leaves the session taken final; one that does not leaves it taken as it
// stands, not final, and says so.
//
// Mutation: take the session when the wait runs out, killing nothing, and the
// job is never killed and is taken as it stands.
func TestAJobThatOutlivesItsEndIsKilledBeforeItsTake(t *testing.T) {
	for name, killEnds := range map[string]bool{"a kill that ends it": true, "a kill that does not": false} {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				logs := captureLogs(t)
				rig, _, _ := keepingRig(t)
				rig.coordinator.endWait = 50 * time.Millisecond
				stamps := rig.spyStamps()
				launched := rig.launchKept(t, "t1")
				box := rig.provider.Box(launched.SandboxID)
				rig.runner.EndFunc = func(context.Context, Sandbox, RunHandle) (EndAsk, error) { return EndAsked, nil }
				var kills atomic.Int32
				box.KillJobFunc = func(context.Context, string) error {
					kills.Add(1)
					if killEnds {
						rig.runner.EndJobIn(box)
					}
					return nil
				}
				if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
					t.Fatalf("StopSeat: %v", err)
				}
				rig.fireRetries()
				rig.awaitKeeps()
				rig.fireRetries()

				if n := kills.Load(); n != 1 {
					t.Fatalf("the job that outlived its end was killed %d times, want once", n)
				}
				takes, begun := rig.runner.Takes(), stamps.begun()
				if len(takes) != 1 || len(begun) != 1 {
					t.Fatalf("takes %+v, stamped %+v; want one take", takes, begun)
				}
				if takes[0].Pack == killEnds || begun[0].Final != killEnds {
					t.Fatalf("the take packed now %v and was stamped final %v, want final %v",
						takes[0].Pack, begun[0].Final, killEnds)
				}
				if !strings.Contains(logs.String(), "sandbox_job_killed") ||
					!strings.Contains(logs.String(), "outlived_its_end") {
					t.Fatalf("no kill past the end's wait was said:\n%s", logs.String())
				}
				if unended := strings.Contains(logs.String(), "sandbox_job_unended"); unended == killEnds {
					t.Fatalf("a job the kill ended %v was said unended %v:\n%s", killEnds, unended, logs.String())
				}
			})
		})
	}
}

// A JOB ASKED NOTHING IS KILLED, NOT WAITED FOR. A job whose script could not
// be asked to end (no end protocol recorded, as an older build's or one not yet
// at its first line; a recorded pid the box says is not the job's; a box that
// could not be asked) is killed at once, since nothing would end it within the
// wait. Each is polled once, whether it was over, before the kill, its session is taken final after
// it, and the kill says why, with the error of a box that could not be asked.
//
// Mutation: wait out the end's wait for any of the three — an ask's error or a
// pid that is not the job's taken for a request delivered — and the job is
// polled through the whole of it before it is killed.
func TestAJobAskedNothingIsKilledNotWaitedFor(t *testing.T) {
	for name, tc := range map[string]struct {
		unheard bool
		endErr  error
		why     string
	}{
		"a script that records no end protocol": {unheard: true, why: "script_unheard"},
		"a pid that is no process of the job":   {endErr: ErrNotJobProcess, why: "not_the_jobs"},
		"a box that could not be asked":         {endErr: errors.New("the runtime is down"), why: "ask_failed"},
	} {
		t.Run(name, func(t *testing.T) {
			inBubble(t, func(t *testing.T) {
				logs := captureLogs(t)
				rig, _, _ := keepingRig(t)
				launched := rig.launchKept(t, "t1")
				box := rig.provider.Box(launched.SandboxID)
				rig.runner.Unheard = tc.unheard
				if tc.endErr != nil {
					box.EndJobFunc = func(context.Context, string, int) error { return tc.endErr }
				}
				var mu sync.Mutex
				polls, killed, pollsAtKill := 0, false, -1
				rig.runner.PollFunc = func(context.Context, Sandbox) (JobState, error) {
					mu.Lock()
					defer mu.Unlock()
					polls++
					if killed {
						return JobEnded, nil
					}
					return JobRunning, nil
				}
				box.KillJobFunc = func(context.Context, string) error {
					mu.Lock()
					defer mu.Unlock()
					killed, pollsAtKill = true, polls
					return nil
				}
				if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
					t.Fatalf("StopSeat: %v", err)
				}
				rig.fireRetries()
				rig.awaitKeeps()
				rig.fireRetries()

				mu.Lock()
				defer mu.Unlock()
				if pollsAtKill != 1 {
					t.Fatalf("the job was polled %d times before it was killed, want once — whether it was over",
						pollsAtKill)
				}
				if rig.runner.Asks() != 1 {
					t.Fatalf("the job was asked to end %d times, want once", rig.runner.Asks())
				}
				if takes := rig.runner.Takes(); len(takes) != 1 || takes[0].Pack {
					t.Fatalf("takes %+v, want the killed job's session taken final", takes)
				}
				said := logs.String()
				if !strings.Contains(said, "msg=sandbox_job_killed") || !strings.Contains(said, "why="+tc.why) {
					t.Fatalf("the kill was not said as why=%s:\n%s", tc.why, said)
				}
				if unasked := strings.Contains(said, "sandbox_job_end_unasked"); unasked != (tc.why == "ask_failed") {
					t.Fatalf("the ask's failure was said %v, want %v:\n%s", unasked, tc.why == "ask_failed", said)
				}
			})
		})
	}
}

// A STOP OF A BOX ON ANOTHER HOST ENDS ITS JOB THERE. Only a box's host reaches
// a `direct` or `container` box, so the node that holds the stopped seat and not
// the box asks the host for the stop's read, and ends nothing itself; the host
// makes the read the run's row licenses, and its read ends the job.
//
// Mutation: end the job on the asker, and its own box backend — which holds no
// such box — is asked to.
func TestAStopOfABoxOnAnotherHostEndsItsJobThere(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		asker, _, _ := keepingRig(t)
		asks := &boxAsks{result: Result{}}
		asker.onNode(t, "node-b", asks, &nudges{})
		run := asker.launchedBy("t1", "node-a")
		asker.handOver(t, "t1", HandedSecrets{Values: []string{handedToken}})
		if err := asker.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
			t.Fatalf("StopSeat: %v", err)
		}
		asker.fireRetries()
		asker.awaitKeeps()
		asker.fireRetries()
		if got := asks.reads(); !slices.Contains(got, BoxReadStop) {
			t.Fatalf("asked the host %v, want the stop's read", got)
		}
		if asker.runner.Asks() != 0 || len(asker.provider.Box(run.SandboxID).JobCalls()) != 0 {
			t.Fatalf("the asker ended the job itself: %d asks, %v", asker.runner.Asks(),
				asker.provider.Box(run.SandboxID).JobCalls())
		}

		// AND THE HOST'S READ ENDS IT, licensed by the stop the row records.
		host, _, _ := keepingRig(t)
		host.onNode(t, "node-a", &boxAsks{}, &nudges{})
		hosted := host.launchedBy("t2", "node-a")
		host.handOver(t, "t2", HandedSecrets{Values: []string{handedToken}})
		if err := host.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
			t.Fatalf("StopSeat: %v", err)
		}
		if _, _, err := host.coordinator.ReadHostedBox(t.Context(), "t2", hosted.LaunchID, hosted.SandboxID,
			BoxReadStop); err != nil {
			t.Fatalf("ReadHostedBox of the stop: %v", err)
		}
		if host.runner.Asks() != 1 {
			t.Fatalf("the host asked the stopped job to end %d times, want once", host.runner.Asks())
		}
		if calls := host.provider.Box(hosted.SandboxID).JobCalls(); !slices.Contains(calls, "kill cmd-1") {
			t.Fatalf("the host's read made %v of the box, want its job swept", calls)
		}
	})
}

// A STOP WITH THE SWITCH OFF ENDS ITS JOB OFF THE FRAME. Ending a job is
// waiting for it, so the ending's retry reads every stop of a job started in a
// box, whatever `conversation_session.enabled` says: the switch decides what is
// kept of a session, not how a stop ends a job. The deciding frame touches
// nothing of the box; the retry ends the job, reads it and reclaims the box,
// keeping no session.
//
// Mutation: read a local stop with the switch off on the frame that decided it,
// and the pause watch waits for the job's end.
func TestAStopWithTheSwitchOffEndsItsJobOffTheFrame(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, writes, keep := keepingRig(t)
		keep.policy.Keep = false
		launched := rig.launchKept(t, "t1")
		box := rig.provider.Box(launched.SandboxID)
		if err := rig.coordinator.StopSeat(t.Context(), "swe", RunStop{By: "founder"}); err != nil {
			t.Fatalf("StopSeat: %v", err)
		}
		if rig.runner.Asks() != 0 || len(box.JobCalls()) != 0 || len(rig.provider.KilledIDs()) != 0 {
			t.Fatalf("the frame that decided the stop touched the box: %d asks, %v, killed %v",
				rig.runner.Asks(), box.JobCalls(), rig.provider.KilledIDs())
		}
		rig.fireRetries()
		rig.awaitKeeps()
		rig.fireRetries()

		if rig.runner.Asks() != 1 {
			t.Fatalf("the stop's retry asked the job to end %d times, want once", rig.runner.Asks())
		}
		if killed := rig.provider.KilledIDs(); !slices.Contains(killed, launched.SandboxID) {
			t.Fatalf("killed %v, want the stopped job's box reclaimed", killed)
		}
		if takes := rig.runner.Takes(); len(takes) != 0 || len(writes.captured()) != 0 {
			t.Fatalf("took %d sessions and recorded %d captures with the switch off", len(takes), len(writes.captured()))
		}
		if _, found, _ := rig.pending.Get(t.Context(), "t1"); found {
			t.Fatal("the stopped run's record was left behind")
		}
	})
}

// A READ THAT MAY END ITS JOB IS GIVEN THE END ON TOP OF THE TAKE: a stop's and
// a loss's always, and every collection of a capped turn, since its host judges
// the cap on its own clock. Each waits the end's budget beyond the take's, at
// the host's share, so a take after an end at its bound keeps its whole budget;
// a capped collection also waits the teardown's grace and slowest crossing of
// its host's record, made before the end's wait, that the cap ended the job. An uncapped collection waits the take
// alone. The take's budget is read in a bubble, whose clock stands still, so
// it is held exactly, on a host whose store is as slow as any node's.
//
// Mutation: one budget for every read, the budget off the read alone, or no
// change or no grace counted for the end's record or the take's reads before
// it, and a take after an end (or those) at its bound is cut short of its own.
func TestAReadThatMayEndItsJobIsGivenTheEndOnTopOfTheTake(t *testing.T) {
	t.Parallel()
	uncapped := PendingRun{TurnID: "t1"}
	held := SessionTakeBudget + 2*sessionRecordBudget + coord.MaxValueReadCrossing + MediaReadersRoom +
		3*discardGrace + boxReadChanges*coord.MaxValueCrossing
	if got, want := BoxReadBudget(uncapped, BoxReadCollect, ThreadTraits{}),
		pollBound+time.Duration(float64(held)/BoxReadAnswerShare); got != want {
		t.Errorf("a collection's read of a turn with no cap waits %v, want the take alone, %v", got, want)
	}
	// A VARIABLE, so the quotient is no constant: the end's budget at the share
	// is no whole number of nanoseconds, which a constant conversion refuses.
	endBudget := JobEndBudget
	end := time.Duration(float64(endBudget) / BoxReadAnswerShare)
	endRecorded := time.Duration(float64(endBudget+discardGrace+coord.MaxValueCrossing) / BoxReadAnswerShare)
	// A CAP STILL AHEAD on the asker's clock, and one already passed: the
	// host reads the deadline against its own.
	ahead := PendingRun{TurnID: "t1", Deadline: time.Now().Add(time.Hour).UTC()}
	passed := PendingRun{TurnID: "t1", Deadline: time.Now().Add(-time.Hour).UTC()}
	for name, ask := range map[string]struct {
		run  PendingRun
		read BoxRead
		want time.Duration
	}{
		"a stop's":                            {uncapped, BoxReadStop, end},
		"a loss's":                            {uncapped, BoxReadLoss, end},
		"a collection's, its cap still ahead": {ahead, BoxReadCollect, endRecorded},
		"a collection's, its cap passed":      {passed, BoxReadCollect, endRecorded},
		"a stop's, its turn capped":           {ahead, BoxReadStop, end},
		"a loss's, its turn's cap passed":     {passed, BoxReadLoss, end},
	} {
		over := BoxReadBudget(ask.run, ask.read, ThreadTraits{}) - BoxReadBudget(uncapped, BoxReadCollect, ThreadTraits{})
		if diff := over - ask.want; diff < -time.Nanosecond || diff > time.Nanosecond {
			t.Errorf("%s read waits %v beyond an uncapped collection's, want %v at the host's share",
				name, over, ask.want)
		}
	}

	// THE HOST'S READ, with its job's end, the cap's record (its grace and the
	// slowest crossing, which this host's store takes), its own poll, the
	// take's read of its row ([Coordinator.placingRow]) and of who reads its
	// media ([Coordinator.placingPolicy]) all spent at their bounds, still
	// hands its take the take's whole budget.
	inBubble(t, func(t *testing.T) {
		rig := newCoordRigWith(t, func(o *CoordinatorOptions) {
			o.Pending = &crossingWrites{PendingStore: o.Pending, crossing: coord.MaxValueCrossing,
				readCrossing: coord.MaxValueReadCrossing, given: map[string][]handed{}}
		})
		share := time.Duration(float64(BoxReadBudget(passed, BoxReadCollect, ThreadTraits{})) * BoxReadAnswerShare)
		spent := JobEndBudget + discardGrace + coord.MaxValueCrossing +
			time.Duration(float64(pollBound)*BoxReadAnswerShare) +
			RowReadBudget(rig.coordinator.pending, rig.coordinator.recordBudget) + MediaReadersRoom
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(share-spent))
		defer cancel()
		take, cancelTake, _ := rig.coordinator.takeContext(ctx, atKeptBound)
		defer cancelTake()
		deadline, _ := take.Deadline()
		// Less the nanosecond the share's float rounding takes off the read's
		// own deadline, as above.
		whole := SessionTakeBudget + rig.coordinator.reserveRoom()
		if left := time.Until(deadline); left < whole-time.Nanosecond {
			t.Fatalf("a take after an end at its bound is given %v, want its whole %v and its "+
				"reservation's room", left, whole)
		}
	})
}

// A READ CUT SHORT IS NO JOB THAT OUTLIVED ITS END. A read whose context ends
// while it waits for its job (the node draining, the asker gone) says nothing
// of the job. The kill still comes, under its own context, since the stop or
// loss decided the job's end, but it is logged at info as the read cut short,
// never as the outlived-end warning an operator is told to look into, and
// nothing is read after it.
//
// Mutation: take the context's end for the wait run out, and the read cut
// short is said as a job that outlived its end.
func TestAReadCutShortIsNoJobThatOutlivedItsEnd(t *testing.T) {
	logs := captureLogs(t)
	box := NewFakeSandbox("box1")
	runner := NewFakeRunner("claude-code")
	ctx, cut := context.WithCancel(t.Context())
	defer cut()
	runner.EndFunc = func(context.Context, Sandbox, RunHandle) (EndAsk, error) { return EndAsked, nil }
	var polls atomic.Int32
	var killed, readAfterKill atomic.Bool
	runner.PollFunc = func(context.Context, Sandbox) (JobState, error) {
		// THE READ ENDS while it waits, its job still working.
		if polls.Add(1) == 3 {
			cut()
		}
		if killed.Load() {
			readAfterKill.Store(true)
		}
		return JobRunning, nil
	}
	killedLive := make(chan bool, 1)
	box.KillJobFunc = func(ctx context.Context, _ string) error {
		killed.Store(true)
		killedLive <- ctx.Err() == nil
		return nil
	}
	coord := &Coordinator{endWait: time.Hour, endPoll: 10 * time.Millisecond}
	over := coord.endJob(ctx, PendingRun{TurnID: "t1", SandboxID: "box1", CommandID: "cmd-1"}, box, runner, nil)

	if over {
		t.Fatal("a read cut short reads its job over")
	}
	select {
	case live := <-killedLive:
		if !live {
			t.Fatal("the job of a read cut short was killed under the read's dead context")
		}
	default:
		t.Fatal("the job of a read cut short was not killed")
	}
	if readAfterKill.Load() {
		t.Fatal("a read cut short read its job again after the kill")
	}
	said := logs.String()
	if !strings.Contains(said, "why=read_cut_short") || strings.Contains(said, "outlived_its_end") {
		t.Fatalf("the kill of a read cut short was said as:\n%s", said)
	}
	if strings.Contains(said, "level=WARN") {
		t.Fatalf("a read cut short was said at warning:\n%s", said)
	}
}
