package engine

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code"
	codehome "github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A QUEUED CLOSE IS WRITTEN AS ITS ENQUEUER (M5-D27): the work item of a
// change its queue landed is closed in the enqueuer's own name — a seat as
// itself, a person as the human seat they hold with the credential they
// enqueued through beside it, the inverse of builtin.CodeAskerOf — and never
// in a name of the queue's: a principal bound to no seat closes nothing, and
// an item on a vendor's tracker is refused before anything is read or
// written, the engine holding no credential of the enqueuer's there.
//
// Mutation: write a person's close as an operator (or drop its credential) →
// red; give an unbound credential a name of its own → red; close a vendor's
// item through the engine's tracker → it reaches the nil engine, red.
func TestAQueuedCloseIsWrittenAsItsEnqueuer(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		as   code.Principal
		want builtin.Actor
	}{
		{"a seat", code.Principal{Kind: code.PrincipalAgent, Seat: "dev"},
			builtin.Actor{Handle: "dev", Kind: tracker.AuthorAgent}},
		{"a person", code.Principal{Kind: code.PrincipalOperator, Seat: "founder", OperatorID: "pat:ada"},
			builtin.Actor{Handle: "founder", Kind: tracker.AuthorHuman, OperatorID: "pat:ada"}},
	} {
		got, err := enqueuerActor(c.as)
		if err != nil || got.Handle != c.want.Handle || got.Kind != c.want.Kind ||
			got.OperatorID != c.want.OperatorID {
			t.Errorf("%s's close is written as %+v (%v), want %+v", c.name, got, err, c.want)
		}
	}
	for _, unbound := range []code.Principal{
		{Kind: code.PrincipalOperator, OperatorID: "token:founder"},
		{Kind: code.PrincipalSystem},
	} {
		if got, err := enqueuerActor(unbound); err == nil {
			t.Errorf("%+v, bound to no seat, closes as %+v, want refused", unbound, got)
		}
	}

	err := queueCloser{}.CloseWorkItem(t.Context(), codehome.TaskClose{
		As:       code.Principal{Kind: code.PrincipalAgent, Seat: "dev"},
		WorkItem: types.WorkItem{Backend: types.WorkJira, ID: "10042", Key: "ENG-42"},
	})
	if !errors.Is(err, errQueueCloseNotNative) {
		t.Fatalf("closing a Jira issue answered %v, want it refused as not the engine's own tracker's", err)
	}
}
