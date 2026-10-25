package assistant_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// openedOn records a turn of r's person running on node, with no runner
// anywhere: what a node that restarted mid-turn leaves behind.
func openedOn(t *testing.T, r *assistanttest.Rig, node string) uuid.UUID {
	t.Helper()
	turn := evals.ID("turn/" + node)
	if _, err := r.Store.OpenTurn(t.Context(), assistant.Write{Owner: r.Person.ID,
		By: iam.ActorFor(r.Person), Key: evals.Key("opened-" + node)}, assistantdomain.NewTurn{
		Conversation: r.Conversation, Turn: turn, Node: node,
		PromptVersion: assistant.PromptVersion}); err != nil {
		t.Fatal(err)
	}
	return turn
}

// A TURN WHOSE RUNNER WAS THIS NODE AND IS GONE IS ANSWERED FROM THE
// TRANSCRIPT AND CLOSED INTERRUPTED, ONCE: the stream draws the snapshot, the
// interrupt's error and the end, and a second stream reads the end recorded.
//
// Mutation: answer the snapshot alone (as for a turn running elsewhere) and
// the conversation stays locked on a turn nobody runs.
func TestAStreamOfATurnThisNodeLostClosesItInterrupted(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{})
	turn := openedOn(t, f, "node-a")
	frames := f.Follow(turn, 0)
	if got := kinds(frames); len(got) != 3 || got[0] != wire.EventSnapshot ||
		got[1] != wire.EventError || got[2] != wire.EventDone {
		t.Fatalf("the stream drew %v, want the snapshot, the interrupt and the end", got)
	}
	if frames[1].Error.Code != wire.ErrorTurnInterrupted ||
		frames[2].Done.State != wire.TurnInterrupted {
		t.Errorf("the turn ended %s with %s", frames[2].Done.State, frames[1].Error.Code)
	}
	again := evals.Last(f.Follow(turn, 0), wire.EventDone)
	if again == nil || again.Done.State != wire.TurnInterrupted || f.Store.Closes(turn) != 1 {
		t.Errorf("a second stream ended %+v with %d closes, want interrupted once",
			again, f.Store.Closes(turn))
	}
}

// A TURN RUNNING ON ANOTHER NODE THIS ONE CANNOT REACH IS ONLY DESCRIBED: the
// stream answers the snapshot, no end, and the turn is left to its runner.
func TestAStreamOfATurnRunningElsewhereLeavesItAlone(t *testing.T) {
	t.Parallel()
	f := assistanttest.NewRig(t, evals.RigOptions{})
	turn := openedOn(t, f, "node-b")
	frames := f.Follow(turn, 0)
	if got := kinds(frames); len(got) != 1 || got[0] != wire.EventSnapshot {
		t.Fatalf("the stream drew %v, want the snapshot alone", got)
	}
	if row, _ := f.Store.Turn(t.Context(), f.Person.ID, turn); row.State != wire.TurnRunning {
		t.Errorf("a turn running elsewhere is %s after a stream of it, want running", row.State)
	}
}

// CLEAR STOPS WHAT RUNS IN THE CONVERSATION AND PURGES IT AT ONCE: a turn
// running here is stopped, every grant of it ended, and the conversation is
// gone for its owner.
//
// Mutation: purge without stopping the local turn, and its runner keeps
// spending on a conversation that no longer exists.
func TestClearStopsTheConversationsTurnAndPurgesIt(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	defer close(hold)
	f := heldRig(t, evals.RigOptions{}, hold)
	accepted, err := f.Ask("clearing", "Slow question", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Svc.Clear(t.Context(), f.Person, f.Conversation, evals.Key("clear")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for f.Svc.Running(accepted.TurnID) && ctx.Err() == nil {
		time.Sleep(5 * time.Millisecond)
	}
	if f.Svc.Running(accepted.TurnID) {
		t.Error("the cleared conversation's turn is still running")
	}
	if !f.Revoker.RevokedTurn(accepted.TurnID) {
		t.Error("the cleared conversation's turn kept its grants")
	}
	if _, err := f.Svc.Transcript(t.Context(), f.Person, f.Conversation); !errors.Is(err, assistant.ErrNotFound) {
		t.Errorf("the cleared conversation reads %v, want not found", err)
	}
}
