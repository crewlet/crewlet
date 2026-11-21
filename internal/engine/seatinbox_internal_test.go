package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
)

// A HOLD ASKED AFTER ITS SEAT'S RELEASE GATES NO LATER ATTACHMENT. The engine's
// holds on a seat's inbox name the attachment the seat held here: the sandbox
// coordinator's, for the attachment its decision was made for, and a person's
// pause and the no-model pause, for the one the seat holds as each is asked. A node's release
// detaches the inbox before the engine hears of it, so either can be asked
// after the detach; keyed by the subscription alone, it was carried onto the
// attachment this node made when it took the seat back, which consumed nothing
// with nothing left that knew of the hold to lift it.
//
// Mutation: take any of them for the inbox's current attachment rather than
// the one named, and the seat comes back held.
func TestAHoldAskedAfterItsSeatsReleaseGatesNoLaterAttachment(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	e, q := engineOn(t)
	id, err := e.seatID("ceo")
	if err != nil {
		t.Fatalf("seatID: %v", err)
	}
	inbox, group := seatInbox(e, "ceo"), seatInboxGroup(e, "ceo")
	held := e.inboxOf(id)

	// THE RELEASE: the inbox detached, then the engine told.
	if _, err := q.Detach(ctx, inbox, group); err != nil {
		t.Fatalf("Detach: %v", err)
	}
	e.detachInbox(id)

	if err := (seatHold{engine: e}).Hold(ctx, "ceo", held); !errors.Is(err, queue.ErrAttachmentGone) {
		t.Fatalf("the sandbox hold decided for the attachment the seat held = %v, want it refused", err)
	}
	if err := e.pause(ctx, "ceo", "no turn engine"); !errors.Is(err, queue.ErrAttachmentGone) {
		t.Fatalf("a no-model pause of a released seat = %v, want it refused", err)
	}
	if err := e.holdPausedInbox(ctx, "ceo"); !errors.Is(err, queue.ErrAttachmentGone) {
		t.Fatalf("a person's pause held on a released seat = %v, want it refused", err)
	}

	// THE SEAT COMES BACK, with nothing to hold it for.
	takeInbox(t, e, q, id)
	got := make(chan string, 1)
	if err := q.Subscribe(ctx, inbox, group, func(_ context.Context, ev *events.Event) queue.Result {
		got <- ev.Type
		return queue.Ack()
	}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := q.Publish(ctx, inbox, &events.Event{ID: uuid.New(), Type: "after-retake"}); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	select {
	case <-got:
	default:
		t.Fatalf("the seat came back held %v by a hold asked after its release",
			q.(*memory.Queue).PauseHolds(inbox, group))
	}
}
