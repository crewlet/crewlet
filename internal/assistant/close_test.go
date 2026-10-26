package assistant_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/assistanttest"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// CLOSE DRAINS WHAT IT WAS HANDED, AND WHAT RACED IT: a start admitted just
// before Close (held here between its admission and its runner) is waited
// for, its runner meets the draining node and ends interrupted, and its row
// is closed before Close returns, never left running for a node that is gone.
//
// Mutation: gather the runners before waiting for the admitted starts (as
// before) and Close returns while the start is still on its way, leaving its
// turn to run on a node that has shut down.
func TestCloseWaitsForAStartThatRacedIt(t *testing.T) {
	t.Parallel()
	g := newGate()
	f := assistanttest.NewRig(t, evals.RigOptions{Rate: g,
		Rounds: []evals.Round{evals.Say("Hi.")}})
	started := make(chan error, 1)
	go func() { _, err := f.Ask("racing", "Hello?", nil); started <- err }()
	<-g.entered
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		closed <- f.Svc.Close(ctx)
	}()
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while a start it admitted was still on its way", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(g.release)
	<-started
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	row, err := f.Store.Turn(t.Context(), f.Person.ID, assistant.TurnID(evals.Key("racing")))
	if err == nil && !row.State.Final() {
		t.Errorf("after Close the raced turn is %s, want it closed", row.State)
	}
}

// CLOSE ENDS EVERY TURN IT RUNS INTERRUPTED and waits for each end to be
// recorded.
func TestCloseDrainsTheTurnsItRuns(t *testing.T) {
	t.Parallel()
	hold := make(chan struct{})
	defer close(hold)
	f := heldRig(t, evals.RigOptions{}, hold)
	accepted, err := f.Ask("draining", "Slow question", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := f.Svc.Close(ctx); err != nil {
		t.Fatal(err)
	}
	row, _ := f.Store.Turn(t.Context(), f.Person.ID, accepted.TurnID)
	if row.State != wire.TurnInterrupted {
		t.Errorf("a turn running at Close is %s, want interrupted", row.State)
	}
	if _, err := f.Ask("after", "Another", nil); err == nil {
		t.Error("a closed service took a new question")
	}
}
