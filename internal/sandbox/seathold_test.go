package sandbox

import (
	"context"
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
