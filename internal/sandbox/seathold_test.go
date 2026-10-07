package sandbox

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
)

// A SEAT A RUN HOLDS HAS ITS INBOX HELD, AND ONLY FOR AS LONG AS THE RUN DOES.
//
// The spin this replaces: a held seat's mail was parked — republished onto the
// inbox it had just been fetched from — and nothing stopped the consumer, so
// every waiting message went round fetch, screen, publish, ack for the whole
// run. Held instead, the consumer fetches nothing until the run stops holding
// the seat, and the hold follows the coordinator's own transitions rather than
// waiting for a delivery to notice: taken when a run starts holding, lifted
// the moment it parks on a question (so the answer can arrive).
func TestASeatsInboxIsHeldExactlyWhileARunHoldsIt(t *testing.T) {
	rig := newCoordRig(t)
	holds := rig.withHold()
	run := rig.launch("t1")
	if err := rig.coordinator.OnStarted(t.Context(), types.SandboxRunStarted{
		AgentHandle: "swe", TurnID: run.TurnID, LaunchID: run.LaunchID,
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if !holds.holding("swe") {
		t.Fatal("a seat a running job holds has an open inbox: its mail would be fetched, " +
			"refused and republished for the length of the run")
	}

	// THE RUN PARKS ON A QUESTION, and gives the seat back: the answer
	// arrives on this inbox, so it must be open.
	rig.runner.Finish(Result{NeedsInput: true, Question: "which branch?", AskTo: "requester"})
	payload, ev := rig.completion("t1")
	if err := rig.coordinator.OnCompleted(t.Context(), payload, ev); err != nil {
		t.Fatalf("OnCompleted: %v", err)
	}
	if rig.get("t1").Status != StatusAwaiting {
		t.Fatal("the run did not park")
	}
	if holds.holding("swe") {
		t.Fatal("the seat's inbox is still held after its run parked on a question — the " +
			"answer to it could never arrive")
	}
	if got := holds.log; !slices.Equal(got, []string{"hold swe", "release swe"}) {
		t.Fatalf("holds = %v, want one hold and one release", got)
	}
}

// A SEAT RECOVERED MID-RUN IS HELD BEFORE ITS MAILBOX OPENS, and a seat this
// node releases is FORGOTTEN rather than released: the release's detach has
// already dropped the hold with the attachment, and a Release from here would
// lift the one this node takes again the moment it re-acquires the seat.
func TestARecoveredRunHoldsTheSeatAndAReleaseForgetsTheHold(t *testing.T) {
	rig := newCoordRig(t)
	holds := rig.withHold()
	rig.launch("t1")
	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-a", 1); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if !holds.holding("swe") {
		t.Fatal("a seat recovered with a running job was not held before its mailbox opened")
	}

	rig.coordinator.ReleaseSeat("swe")
	if got := holds.log; !slices.Equal(got, []string{"hold swe"}) {
		t.Fatalf("holds = %v: the release lifted a hold the detach had already dropped", got)
	}

	// RE-ACQUIRED: believed unheld, so the hold is taken again.
	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-a", 2); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if got := holds.log; !slices.Equal(got, []string{"hold swe", "hold swe"}) {
		t.Fatalf("holds = %v, want the hold taken again on the re-acquired seat", got)
	}
}

// A DELIVERY THAT RACED THE HOLD asks for it again, which is how a hold the
// queue refused is retried: at the spacing the delivery's own deferral sets.
func TestHoldSeatRetriesAHoldTheQueueRefused(t *testing.T) {
	rig := newCoordRig(t)
	refusing := &flakyHold{fail: true}
	rig.coordinator.hold = refusing
	rig.launch("t1")
	if err := rig.coordinator.RecoverSeat(t.Context(), "swe", "node-a", 1); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
	if refusing.held {
		t.Fatal("the refusing hold reports held")
	}
	refusing.fail = false
	rig.coordinator.HoldSeat(t.Context(), "swe")
	if !refusing.held {
		t.Fatal("a delivery reaching a held seat did not take the hold the queue refused before")
	}
}

// flakyHold is a [SeatHold] the queue refuses until a case lets it through.
type flakyHold struct {
	fail bool
	held bool
}

func (h *flakyHold) Hold(context.Context, string) error {
	if h.fail {
		return errRefusedCall
	}
	h.held = true
	return nil
}

func (h *flakyHold) Release(context.Context, string) error {
	h.held = false
	return nil
}

// A LAUNCH HOLDS ITS SEAT FROM THE ROW IT WRITES, before the box exists and with
// no started event processed at all.
//
// The window this closes: the seat used to count as held only once the run's
// started event came round on the seat's control topic, a subscription the
// launching turn never waits for. Mail drained beside the launching delivery,
// and mail fetched the moment the turn returned, reached a seat the screening
// called free and ran a turn beside the job.
func TestALaunchHoldsItsSeatFromTheRowItWrites(t *testing.T) {
	rig := newCoordRig(t)
	holds := rig.withHold()
	witness := &seatWitness{FakeProvider: rig.provider, coordinator: rig.coordinator, holds: holds}
	manager, err := NewManager(ManagerOptions{
		Providers: map[Placement]Provider{Direct: witness},
		Runners:   map[string]Runner{"claude-code": rig.runner},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	if _, err := rig.coordinator.Launch(t.Context(), manager, launchReq("t1")); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !witness.checked {
		t.Fatal("the launch never provisioned a box, so the order was not observed")
	}
	if !witness.held || !witness.inboxHeld {
		t.Fatalf("while the box was being provisioned the seat was held=%v and its inbox held=%v: "+
			"mail reaching the seat in that window runs a turn beside the job",
			witness.held, witness.inboxHeld)
	}
	if held, _ := rig.coordinator.SeatRuns("swe"); !held || !holds.holding("swe") {
		t.Fatal("the launch returned with its seat free, waiting on a started event the " +
			"launching turn never waits for")
	}
	if got := holds.log; !slices.Equal(got, []string{"hold swe"}) {
		t.Fatalf("holds = %v, want the one hold the launch took", got)
	}

	// ITS ANNOUNCEMENT, when it does come round, recounts the same answer
	// and moves nothing.
	run := rig.get("t1")
	if err := rig.coordinator.OnStarted(t.Context(), types.SandboxRunStarted{
		AgentHandle: "swe", TurnID: run.TurnID, LaunchID: run.LaunchID,
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if got := holds.log; !slices.Equal(got, []string{"hold swe"}) {
		t.Fatalf("holds = %v: the started event moved a hold the launch already took", got)
	}
}

// seatWitness is a provider that records, as the launch provisions its box,
// whether the coordinator already holds the launching seat.
type seatWitness struct {
	*FakeProvider
	coordinator              *Coordinator
	holds                    *holdSpy
	checked, held, inboxHeld bool
}

func (w *seatWitness) Create(ctx context.Context, spec Spec) (Sandbox, error) {
	w.checked = true
	w.held, _ = w.coordinator.SeatRuns("swe")
	w.inboxHeld = w.holds.holding("swe")
	return w.FakeProvider.Create(ctx, spec)
}

// A RECOUNT NEVER WRITES BACK OVER A LAUNCH IT DID NOT SEE. Another run's start
// recounts the seat from the store; the listing is taken, a launch writes its
// row and holds the seat, and the recount lands. Written back as read, it
// would free the seat the launch just held — beside a job that is starting.
func TestARecountDoesNotFreeASeatALaunchHeldUnderIt(t *testing.T) {
	rig := newCoordRig(t)
	holds := rig.withHold()
	racing := &listRace{PendingStore: rig.pending}
	rig.coordinator.pending = racing
	racing.during = func() {
		if _, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1")); err != nil {
			t.Errorf("Launch: %v", err)
		}
	}

	if err := rig.coordinator.OnStarted(t.Context(), types.SandboxRunStarted{
		AgentHandle: "swe", TurnID: "t0",
	}); err != nil {
		t.Fatalf("OnStarted: %v", err)
	}
	if held, _ := rig.coordinator.SeatRuns("swe"); !held || !holds.holding("swe") {
		t.Fatalf("held = %v, holds = %v: a recount read before the launch freed the seat the "+
			"launch held", held, holds.log)
	}
}

// listRace is a store whose first seat listing is taken BEFORE something else
// happens and returned after it.
type listRace struct {
	PendingStore
	during func()
	once   bool
}

func (s *listRace) ListActiveForSeat(ctx context.Context, handle string) ([]PendingRun, error) {
	runs, err := s.PendingStore.ListActiveForSeat(ctx, handle)
	if !s.once {
		s.once = true
		s.during()
	}
	return runs, err
}

// A LAUNCH THAT FAILS GIVES ITS SEAT BACK — every way it can fail after the row
// it holds the seat from: at the box, at the job, and at the turn's own
// suspension into the row, which [Coordinator.FailRun] settles.
func TestAFailedLaunchGivesItsSeatBack(t *testing.T) {
	for _, tc := range []struct {
		name   string
		derail func(*coordRig)
		// suspendFails launches cleanly and then fails the turn's
		// suspension, the way the engine does when it cannot write the
		// conversation.
		suspendFails bool
	}{
		{name: "the box cannot be provisioned", derail: func(r *coordRig) {
			r.provider.CreateErr = errors.New("no capacity")
		}},
		{name: "the coding agent cannot be started", derail: func(r *coordRig) {
			r.runner.StartErr = errors.New("the coding CLI is not installed")
		}},
		{name: "the turn cannot suspend into the run", derail: func(*coordRig) {}, suspendFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newCoordRig(t)
			holds := rig.withHold()
			tc.derail(rig)
			_, err := rig.coordinator.Launch(t.Context(), rig.manager, launchReq("t1"))
			if tc.suspendFails {
				if err != nil {
					t.Fatalf("Launch: %v", err)
				}
				if err := rig.coordinator.FailRun(t.Context(), "t1",
					types.SandboxFailureSuspensionUnrecorded, "the conversation could not be written"); err != nil {
					t.Fatalf("FailRun: %v", err)
				}
			} else if err == nil {
				t.Fatal("a launch that could not finish reported success")
			}
			if held, _ := rig.coordinator.SeatRuns("swe"); held {
				t.Fatal("a launch that ended holds its seat: every later message waits behind a job " +
					"that is not running")
			}
			if holds.holding("swe") {
				t.Fatalf("holds = %v: the seat's inbox stays held after its launch ended", holds.log)
			}
		})
	}
}
