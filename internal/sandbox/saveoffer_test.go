package sandbox

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// offerOf offers turnID's box for a save as a poll does, under the case's own
// context as its offerer, and waits for the task the offer started to end: true where one
// started.
func offerOf(t *testing.T, rig *coordRig, turnID string) bool {
	t.Helper()
	running := rig.coordinator.offerSave(t.Context(), rig.get(turnID), rig.runner)
	if running == nil {
		return false
	}
	<-running.ended
	return true
}

// savingWaiter is a waiter of rig's runs that offers each box its poll finds
// running to save.
func savingWaiter(t *testing.T, rig *waiterRig, save SaveFunc) *Waiter {
	t.Helper()
	waiter, err := NewWaiter(WaiterOptions{Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
		Now: func() time.Time { return rig.now }, Save: save})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	return waiter
}

// offerLog is every offer a waiter made, in order.
type offerLog struct {
	mu      sync.Mutex
	turns   []string
	runners []Runner
}

func (o *offerLog) save(_ context.Context, run PendingRun, runner Runner) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.turns = append(o.turns, run.TurnID)
	o.runners = append(o.runners, runner)
}

func (o *offerLog) offered() ([]string, []Runner) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.turns), slices.Clone(o.runners)
}

// spawning is a waiter's offer to rig's coordinator that hands every task it
// started to spawned.
func spawning(rig *coordRig, spawned chan<- *saveRun) SaveFunc {
	return func(offerer context.Context, run PendingRun, runner Runner) {
		if running := rig.coordinator.offerSave(offerer, run, runner); running != nil {
			spawned <- running
		}
	}
}

// landedSave stamps on turnID's row, landed, a save of its job taken at at,
// its object of size bytes.
func landedSave(t *testing.T, rig *coordRig, turnID string, at time.Time, size int64) {
	t.Helper()
	run := rig.get(turnID)
	save := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: nextCaptureSeq(run), Launch: run.LaunchID,
		State: CaptureLanded, Interrupted: true, Through: new(0), At: at, Harness: "claude-code",
		HarnessVersion: "2.1.310", SessionID: "s-1", ArchiveFormat: FakeArchiveFormat,
		Object: objstore.KeyAt(at).String(), ObjectHash: strings.Repeat("a", 64), ObjectSize: size,
		ContentKey: "k1"}
	if took, err := rig.pending.StampCapture(t.Context(), turnID, save); err != nil || !took {
		t.Fatalf("stamping the landed save = %v, %v", took, err)
	}
}

// A SAVE IS DUE ONLY ONCE ITS INTERVAL PASSED AND ITS SESSION MOVED: a box is
// not packed before its interval has passed since the launch, nor at its next
// interval while its job's stream has not moved since its last landed save,
// and is once the stream grows.
//
// Mutation: drop the due check, or the stream's.
func TestASaveIsDueOnlyOnceItsIntervalPassedAndItsSessionMoved(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		run := rig.launchKept(t, "t1")
		launched := rig.now
		for _, at := range []time.Duration{0, rigSaveInterval - time.Second} {
			rig.now = launched.Add(at)
			if offerOf(t, rig, "t1") {
				t.Errorf("a save was started %v after the launch, before its interval", at)
			}
		}
		rig.now = launched.Add(rigSaveInterval)
		if !offerOf(t, rig, "t1") || len(rig.runner.Takes()) != 1 {
			t.Fatalf("at its interval the save started nothing or took %d sessions, want one",
				len(rig.runner.Takes()))
		}
		rig.now = rig.now.Add(rigSaveInterval)
		offerOf(t, rig, "t1")
		if takes, left := rig.runner.Takes(), rig.runner.Released(); len(takes) != 1 || len(left) != 0 {
			t.Errorf("with the job's stream where its last save found it, took %d sessions and released %v, "+
				"want no pack", len(takes), left)
		}
		rig.runner.Stream(rig.provider.Box(run.SandboxID), 64)
		if !offerOf(t, rig, "t1") || len(rig.runner.Takes()) != 2 {
			t.Errorf("once the job's stream grew, took %d sessions, want a second", len(rig.runner.Takes()))
		}
	})
}

// A SAVE AFTER A RESTART IS DUE WITHIN ONE INTERVAL WHATEVER THE CLOCKS: at
// first sight the row's newest save and the launch's start only shorten the
// wait, clamped to one interval. A save stamped an hour ahead, on another
// node's clock, leaves it at one interval; one long ago makes it due at once;
// and a seat whose allowance paces its next save hours out takes none at first
// sight after a save ten minutes ago.
//
// Mutation: read the newest instant unclamped, make first sight always due,
// or pace from anything but first sight's estimate.
func TestASaveAfterARestartIsDueWithinOneIntervalWhateverTheClocks(t *testing.T) {
	t.Parallel()
	t.Run("a save stamped an hour ahead", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.launchKept(t, "t1")
			sight := rig.now
			landedSave(t, rig, "t1", sight.Add(time.Hour), 1024)
			for _, at := range []time.Duration{0, rigSaveInterval - time.Second} {
				rig.now = sight.Add(at)
				if offerOf(t, rig, "t1") {
					t.Errorf("a save was started %v after first sight, before one interval", at)
				}
			}
			rig.now = sight.Add(rigSaveInterval)
			if !offerOf(t, rig, "t1") {
				t.Error("no save was started one interval after first sight")
			}
		})
	})
	t.Run("a save long ago", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.launchKept(t, "t1")
			landedSave(t, rig, "t1", rig.now, 1024)
			rig.now = rig.now.Add(3 * time.Hour)
			if !offerOf(t, rig, "t1") {
				t.Error("three hours after the job's last save, no save was started at first sight")
			}
		})
	})
	t.Run("a paced seat after a save ten minutes ago", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
			rig, _, keep := savingRig(t, memory.NewFleet(), newTakerStore())
			rig.launchKept(t, "t1")
			// AN EIGHTH OF THE SEAT'S ROOM, which paces the next save an eighth
			// of the hold, some three and a half hours, after the last.
			landedSave(t, rig, "t1", rig.now.Add(50*time.Minute), keep.policy.SeatBudget/8)
			rig.now = rig.now.Add(time.Hour)
			if !offerOf(t, rig, "t1") {
				t.Fatal("the premise: no save task was started an interval after the last save")
			}
			if takes, released := rig.runner.Takes(), rig.runner.Released(); len(takes) != 0 || len(released) != 0 {
				t.Errorf("took %d sessions and released %v, want no pack: the seat is paced", len(takes), released)
			}
			if row := rig.get("t1"); row.Capture == nil || row.Capture.CaptureSeq != 1 {
				t.Errorf("the run's newest capture is %+v, want the save ten minutes ago", row.Capture)
			}
		})
	})
}

// A SAVE THAT FAILED IS NOT TRIED AGAIN WITHIN THE INTERVAL: after a pack that
// failed and then two uploads that did, the next attempts come at twice and
// four times the interval, and then at the hour that caps it; a landing resets
// the wait to one interval, and a save the license refuses beside a capture in
// flight moves the clock without doubling.
//
// Mutation: no doubling, no cap, no reset on a landing, or doubling on what was
// not yet there.
func TestASaveThatFailedIsNotTriedAgainWithinTheInterval(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep := savingRig(t, memory.NewFleet(), newTakerStore())
		run := rig.launchKept(t, "t1")
		// dueAt holds the next save to wait out wait from the last attempt.
		last := rig.now.Add(rigSaveInterval)
		dueAt := func(wait time.Duration, before func()) {
			t.Helper()
			rig.now = last.Add(wait - time.Second)
			if offerOf(t, rig, "t1") {
				t.Fatalf("a save was started %v after the last attempt, before its wait of %v",
					wait-time.Second, wait)
			}
			rig.now = last.Add(wait)
			before()
			if !offerOf(t, rig, "t1") {
				t.Fatalf("no save was started %v after the last attempt", wait)
			}
			last = rig.now
		}
		uploads := func(err error) {
			keep.mu.Lock()
			defer keep.mu.Unlock()
			keep.putErr = err
		}
		rig.now = last
		rig.runner.PackSaveErr = errors.New("a file changed as it was packed")
		if !offerOf(t, rig, "t1") {
			t.Fatal("the premise: no save was started an interval after the launch")
		}
		dueAt(2*rigSaveInterval, func() {
			rig.runner.PackSaveErr = nil
			uploads(errors.New("the object store did not answer"))
		})
		dueAt(4*rigSaveInterval, func() {})
		dueAt(saveRetryCeiling, func() { uploads(nil) })
		if row := rig.get("t1"); row.Kept == nil || row.Kept.CaptureSeq != 3 {
			t.Fatalf("the run keeps %+v, want the save in place 3 landed", row.Kept)
		}
		dueAt(rigSaveInterval, func() {
			rig.runner.Stream(rig.provider.Box(run.SandboxID), 64)
			inFlight := SessionCapture{LaunchSeq: run.LaunchSeq, CaptureSeq: 4, Launch: run.LaunchID,
				State: CaptureBegun, Interrupted: true, Through: new(0), At: rig.now, Harness: "claude-code",
				HarnessVersion: "2.1.310", SessionID: "s-1", ArchiveFormat: FakeArchiveFormat}
			if took, err := rig.pending.ReserveSave(t.Context(), "t1", inFlight); err != nil || !took {
				t.Fatalf("reserving the capture in flight = %v, %v", took, err)
			}
		})
		dueAt(rigSaveInterval, func() {})
	})
}

// A SAVE NEVER HOLDS THE POLL OR READS THE BOX IN IT: the waiter's offer
// returns before the save's pack does, the poll's next pass polls the box while
// the pack is still held, and the offer reads nothing of the box itself.
//
// Mutation: run the task inline, or read the job's stream in the offer.
func TestASaveNeverHoldsThePollOrReadsTheBoxInIt(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		rig.launchKept(t, "t1")
		rig.now = rig.now.Add(rigSaveInterval)
		spawned := make(chan *saveRun, 2)
		rig.waiter = savingWaiter(t, rig.waiterRig, spawning(rig, spawned))
		polls := 0
		rig.runner.PollFunc = func(context.Context, Sandbox) (JobState, error) {
			polls++
			return JobRunning, nil
		}
		packing, release := make(chan struct{}, 2), make(chan struct{})
		rig.runner.Packing = func(context.Context, Sandbox) {
			packing <- struct{}{}
			<-release
		}
		rig.tick()
		<-packing
		if reads := rig.runner.StreamedReads(); reads != 1 {
			t.Errorf("the job's stream was read %d times, want once, by the save's own task", reads)
		}
		rig.tick()
		if polls != 2 {
			t.Errorf("the box was polled %d times with its save's pack held, want twice", polls)
		}
		close(release)
		<-(<-spawned).ended
		if takes := rig.runner.Takes(); len(takes) != 1 || len(spawned) != 0 {
			t.Errorf("took %d sessions in %d more tasks, want the one save", len(takes), len(spawned))
		}
	})
}

// EVERY RUNNING BOX THE POLL FINDS IS OFFERED FOR A SAVE, in either cell, with
// the runner the poll asked; none past its turn's cap, none whose poll failed,
// and none a collection read back.
//
// Mutation: drop the hook from the poll, or put it ahead of the cap's check.
func TestEveryRunningBoxThePollFindsIsOfferedForASave(t *testing.T) {
	t.Parallel()
	for _, cell := range Placements {
		t.Run(string(cell), func(t *testing.T) {
			t.Parallel()
			rig := newWaiterRigIn(t, cell)
			offers := &offerLog{}
			rig.waiter = savingWaiter(t, rig, offers.save)
			rig.launch("running")
			rig.launchUntil("capped", rig.now.Add(time.Minute))
			failing := rig.launch("failing")
			collected := rig.launch("collected")
			if err := rig.provider.Box(collected.SandboxID).Pause(t.Context()); err != nil {
				t.Fatalf("pausing the collected job's box: %v", err)
			}
			if _, _, err := rig.pending.mutateAny(t.Context(), "collected", func(row *PendingRun) bool {
				row.Launch.Collected = true
				return true
			}); err != nil {
				t.Fatalf("marking the job read back: %v", err)
			}
			rig.runner.PollFunc = func(_ context.Context, box Sandbox) (JobState, error) {
				if box.ID() == failing.SandboxID {
					return "", errors.New("the box's job could not be asked")
				}
				return JobRunning, nil
			}
			rig.now = rig.now.Add(2 * time.Minute)
			rig.tick()
			turns, runners := offers.offered()
			if !slices.Equal(turns, []string{"running"}) {
				t.Errorf("offered %v for a save, want the running job's box alone", turns)
			}
			for _, runner := range runners {
				if runner != rig.runner {
					t.Errorf("offered the runner %v, want the one the poll asked", runner)
				}
			}
		})
	}
}

// turnstileKeep is a keep whose every upload says it arrived and waits to be
// let through, one an admit.
type turnstileKeep struct {
	*sessionKeep
	arrived chan struct{}
	admit   chan struct{}
}

func (k turnstileKeep) Upload(ctx context.Context, spool executorkeep.SealedSpool,
	sealed io.Reader) (executorkeep.SealedObject, error) {

	k.arrived <- struct{}{}
	select {
	case <-k.admit:
	case <-ctx.Done():
		return executorkeep.SealedObject{}, ctx.Err()
	}
	return k.sessionKeep.Upload(ctx, spool, sealed)
}

// THE SAVE SLOTS BOUND THE TAKES AND THE KEEPS ON A NODE: with six runs due and
// every upload held, four saves are packed and taken and the rest are offered
// in vain; the fifth is taken only once one of the four has kept its save.
//
// Mutation: give the slot back once the save has spooled, before its keep.
func TestTheSaveSlotsBoundTheTakesAndTheKeepsOnANode(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep := savingRig(t, memory.NewFleet(), newTakerStore())
		turns := []string{"t1", "t2", "t3", "t4", "t5", "t6"}
		for _, turn := range turns {
			rig.launchKept(t, turn)
		}
		rig.now = rig.now.Add(rigSaveInterval)
		gate := turnstileKeep{sessionKeep: keep, arrived: make(chan struct{}, len(turns)),
			admit: make(chan struct{})}
		rig.coordinator.keep = gate
		ended := make(chan struct{}, len(turns))
		offer := func(turn string) bool {
			running := rig.coordinator.offerSave(t.Context(), rig.get(turn), rig.runner)
			if running == nil {
				return false
			}
			<-gate.arrived
			go func() {
				<-running.ended
				ended <- struct{}{}
			}()
			return true
		}
		for _, turn := range turns[:maxConcurrentSaves] {
			if !offer(turn) {
				t.Fatalf("the save of %s was not started with a slot free", turn)
			}
		}
		for _, turn := range turns[maxConcurrentSaves:] {
			if offer(turn) {
				t.Errorf("the save of %s was started beside %d keeps under way", turn, maxConcurrentSaves)
			}
		}
		if takes := rig.runner.Takes(); len(takes) != maxConcurrentSaves {
			t.Errorf("took %d sessions, want %d", len(takes), maxConcurrentSaves)
		}
		gate.admit <- struct{}{}
		<-ended
		if !offer("t5") {
			t.Error("the fifth save was not started once a keep had ended")
		}
		if offer("t6") {
			t.Error("the sixth save was started beside four keeps under way")
		}
		close(gate.admit)
		for range maxConcurrentSaves {
			<-ended
		}
	})
}

// A POLL THAT ENDS AFTER ITS WAITER STOPPED OFFERS NO SAVE: the offer is made
// under the waiter's life, and a box whose poll answered as the waiter was
// stopping is offered nothing, while one polled before the stop was.
//
// Mutation: offer the save whatever the waiter's life says.
func TestAPollThatEndsAfterItsWaiterStoppedOffersNoSave(t *testing.T) {
	t.Parallel()
	rig := newWaiterRig(t)
	offers := &offerLog{}
	waiter, err := NewWaiter(WaiterOptions{Queue: rig.queue, Pending: rig.pending, Manager: rig.managers,
		Interval: time.Hour, Now: func() time.Time { return rig.now }, Save: offers.save})
	if err != nil {
		t.Fatalf("NewWaiter: %v", err)
	}
	life, stop := context.WithCancel(t.Context())
	waiter.Start(life)
	t.Cleanup(waiter.Stop)
	rig.waiter = waiter
	rig.launch("t1")
	rig.tick()
	if turns, _ := offers.offered(); !slices.Equal(turns, []string{"t1"}) {
		t.Fatalf("the premise: a poll of the living waiter offered %v, want t1", turns)
	}
	rig.runner.PollFunc = func(context.Context, Sandbox) (JobState, error) {
		stop()
		return JobRunning, nil
	}
	rig.tick()
	if turns, _ := offers.offered(); !slices.Equal(turns, []string{"t1"}) {
		t.Errorf("a poll that ended after its waiter stopped offered %v, want nothing more", turns)
	}
}

// ONE SAVE OF A RUN IS UNDER WAY ON A NODE: while a save of the turn is taken,
// another offer of its box, or of a later launch of the turn, starts nothing,
// and a drain beside it waits for that save rather than start its own.
//
// Mutation: drop the check for a task of the turn under way, from the offer or
// from the drain.
func TestOneSaveOfARunIsUnderWayOnANode(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		run := rig.launchKept(t, "t1")
		rig.now = rig.now.Add(rigSaveInterval)
		packing, release := make(chan struct{}, 3), make(chan struct{})
		rig.runner.Packing = func(context.Context, Sandbox) {
			packing <- struct{}{}
			<-release
		}
		first := rig.coordinator.offerSave(t.Context(), run, rig.runner)
		if first == nil {
			t.Fatal("the premise: the first offer started no save")
		}
		<-packing
		relaunched := run
		relaunched.LaunchID, relaunched.LaunchSeq = "a-later-job", run.LaunchSeq+1
		for name, again := range map[string]PendingRun{"the same job": run, "a later launch": relaunched} {
			if running := rig.coordinator.offerSave(t.Context(), again, rig.runner); running != nil {
				t.Errorf("an offer of %s started a second save of the turn", name)
				defer func() { <-running.ended }()
			}
		}
		drained := make(chan []*saveRun, 1)
		go func() { drained <- rig.coordinator.saveHosted(t.Context()) }()
		// THE DRAIN HAS LISTED THE RUN once every goroutine waits, and only
		// then does the save under way go on.
		synctest.Wait()
		close(release)
		if picked := <-drained; !slices.Equal(picked, []*saveRun{first}) || len(packing) != 0 {
			t.Errorf("the drain beside the save under way picked %v and packed %d more, want that save alone",
				picked, len(packing))
		}
		<-first.ended
	})
}

// A STOPPING COORDINATOR TAKES NO SAVE AND STAMPS THE ONE IT CUT: a save whose
// spool the stop cut is stamped refused within the stop's grace, and an offer
// after the stop starts nothing, not even an attach.
//
// Mutation: start a task without asking whether the coordinator stopped, or
// count it with the keeps from its start, which the stop's grace does not wait
// for.
func TestAStoppingCoordinatorTakesNoSaveAndStampsTheOneItCut(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		// THE FLEET'S RECORD BUDGET: the keeping rig's 200 ms is shorter than
		// the second this store takes to answer, so it would cut any stamp.
		rig.coordinator.pending = stampsAcrossTheWire{PendingStore: rig.coordinator.pending}
		rig.coordinator.recordBudget = sessionRecordBudget
		rig.launchKept(t, "t1")
		rig.now = rig.now.Add(rigSaveInterval)
		streaming := make(chan struct{})
		rig.runner.Session.Streaming = func(ctx context.Context) {
			close(streaming)
			<-ctx.Done()
		}
		running := rig.coordinator.offerSave(t.Context(), rig.get("t1"), rig.runner)
		if running == nil {
			t.Fatal("the premise: the offer started no save")
		}
		<-streaming
		start := time.Now()
		rig.coordinator.Stop()
		if took := time.Since(start); took > discardGrace {
			t.Errorf("the stop took %v, past its grace of %v", took, discardGrace)
		}
		<-running.ended
		if row := rig.get("t1"); row.Capture == nil || row.Capture.CaptureSeq != 1 ||
			row.Capture.State != CaptureRefused {
			t.Errorf("the run's newest capture is %+v, want the save the stop cut refused", row.Capture)
		}
		attaches := 0
		rig.provider.AttachFunc = func(context.Context, string) error {
			attaches++
			return nil
		}
		if again := rig.coordinator.offerSave(t.Context(), rig.get("t1"), rig.runner); again != nil || attaches != 0 {
			t.Errorf("an offer after the stop started a save (%t) and attached %d boxes, want neither",
				again != nil, attaches)
		}
	})
}

// stampsAcrossTheWire answers a capture's stamp a second after it is asked,
// and refuses one whose context ended meanwhile, as the fleet's store over the
// wire does and the in-memory twin, which applies a first attempt whatever its
// context, does not: a stamp the stop's grace cut leaves the place begun.
type stampsAcrossTheWire struct{ PendingStore }

func (s stampsAcrossTheWire) StampCapture(ctx context.Context, turnID string, stamp SessionCapture) (bool, error) {
	time.Sleep(time.Second) //fixedwait:simulated a stamp crossing to the store, on the case's bubble clock
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.PendingStore.StampCapture(ctx, turnID, stamp)
}

// A RUNNER THAT TAKES NO SAVE IS OFFERED NONE: a runner whose harness takes no
// save of a running job costs its offer nothing: no task, no slot, no memory
// and no read of its box.
//
// Mutation: drop the runner's check from the offer.
func TestARunnerThatTakesNoSaveIsOfferedNone(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, _ := savingRig(t, memory.NewFleet(), newTakerStore())
		rig.runner.SavesSessions = false
		rig.launchKept(t, "t1")
		rig.now = rig.now.Add(rigSaveInterval)
		if running := rig.coordinator.offerSave(t.Context(), rig.get("t1"), rig.runner); running != nil {
			<-running.ended
			t.Error("a runner that takes no save was offered one")
		}
		rig.coordinator.saves.mu.Lock()
		_, remembered := rig.coordinator.saves.memos["t1"]
		slots := len(rig.coordinator.saves.slots)
		rig.coordinator.saves.mu.Unlock()
		if remembered || slots != 0 || rig.runner.StreamedReads() != 0 {
			t.Errorf("remembered the turn %t, held %d slots, read the stream %d times; want none of them",
				remembered, slots, rig.runner.StreamedReads())
		}
	})
}

// A LOST JOB WITH ITS BOX GONE CONTINUES ITS NEWEST SAVE: a save the poll
// offered lands, the job's box goes with the job, and the relaunch continues
// that save, a capture its job went on past (cut off and not final), which its
// brief tells as a session saved while the run worked.
//
// Mutation: stamp the save as not cut off, which the store refuses, and no save
// is kept to continue.
func TestALostJobWithItsBoxGoneContinuesItsNewestSave(t *testing.T) {
	t.Parallel()
	inBubble(t, func(t *testing.T) {
		rig, _, keep := savingRig(t, memory.NewFleet(), newTakerStore())
		rig.runner.Carries = true
		rig.coordinator.relaunchWait = 2 * time.Second
		run := rig.launchKept(t, "t1")
		rig.now = rig.now.Add(rigSaveInterval)
		spawned := make(chan *saveRun, 1)
		rig.waiter = savingWaiter(t, rig.waiterRig, spawning(rig, spawned))
		rig.tick()
		<-(<-spawned).ended
		saved := rig.get("t1").Kept
		if saved == nil || saved.State != CaptureLanded || saved.Final || !saved.Interrupted {
			t.Fatalf("the run keeps %+v, want the save landed, cut off and not final", saved)
		}
		uploads := keep.uploads()
		if len(uploads) != 1 {
			t.Fatalf("the premise: uploaded %d sessions, want the save's", len(uploads))
		}
		rig.coordinator.restore = &keptRestore{rows: map[string]executorkeep.TurnRow{"t1": keptRow(t, *saved)},
			archives: map[string][]byte{saved.Object: uploads[0]}}
		lostWithItsBox(t, rig, run)
		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t1", RestorerUnder(rig.runner, ""))
		if why != "" || !continued.Capture.samePlace(*saved) || continued.Capture.Final ||
			!continued.Capture.Interrupted {
			t.Errorf("the relaunch continues %+v (%q), want the save, cut off and not final", continued.Capture, why)
		}
	})
}
