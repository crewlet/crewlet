package toolloop

import (
	"context"
	"errors"
	"testing"
)

type servedKey struct{}
type phaseKey struct{}

// A CALL CARRIES WHAT ITS RUN WAS SERVED WITH, AND ENDS WITH THE PHASE ALONE.
// The backend puts on the context it serves a run under what that run's calls
// need — the CLI backend lends its slot there to a model call made inside one
// of them, which otherwise deadlocks an entry with one slot — while a call is
// the phase's: the run's context ending (its CLI stopped, a fallback taking
// over) cancels nothing a tool is half-way through, and the phase's does.
//
// Mutation: run calls under the phase's context alone, and the served value is
// gone; under the served context, and its cancellation reaches the call.
func TestACallCarriesItsRunsValuesAndThePhasesCancellation(t *testing.T) {
	t.Parallel()
	phase, cancelPhase := context.WithCancelCause(context.WithValue(context.Background(), phaseKey{}, "turn"))
	served, cancelRun := context.WithCancel(context.WithValue(phase, servedKey{}, "slot"))
	call := newCallContext(served, phase)

	if call.Value(servedKey{}) != "slot" || call.Value(phaseKey{}) != "turn" {
		t.Fatalf("the call carries %v and %v, want the run's and the phase's values",
			call.Value(servedKey{}), call.Value(phaseKey{}))
	}
	cancelRun()
	if call.Err() != nil {
		t.Fatalf("the run ending cancelled the call: %v", call.Err())
	}
	stop := errors.New("the fence moved")
	cancelPhase(stop)
	<-call.Done()
	if !errors.Is(context.Cause(call), stop) {
		t.Errorf("the call's cause is %v, want the phase's", context.Cause(call))
	}
}
