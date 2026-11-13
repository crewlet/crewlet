package engine

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	gitlethome "github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/worklet"
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
		as   gitlet.Principal
		want builtin.Actor
	}{
		// A SEAT'S CLOSE IS MADE FOR NOBODY ELSE: the queue lands the change
		// outside any turn, and the close is handed nobody the enqueue was
		// made for until crewlet#798's piece 4c-ii.
		{"a seat", gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"},
			builtin.Actor{Handle: "dev", Kind: worklet.AuthorAgent, WrittenFor: types.ForNobody()}},
		{"a person", gitlet.Principal{Kind: gitlet.PrincipalOperator, Seat: "founder", OperatorID: "pat:ada"},
			builtin.Actor{Handle: "founder", Kind: worklet.AuthorHuman, OperatorID: "pat:ada"}},
	} {
		got, err := enqueuerActor(c.as)
		if err != nil || got.Handle != c.want.Handle || got.Kind != c.want.Kind ||
			got.OperatorID != c.want.OperatorID || !reflect.DeepEqual(got.WrittenFor, c.want.WrittenFor) {
			t.Errorf("%s's close is written as %+v (%v), want %+v", c.name, got, err, c.want)
		}
	}
	for _, unbound := range []gitlet.Principal{
		{Kind: gitlet.PrincipalOperator, OperatorID: "token:founder"},
		{Kind: gitlet.PrincipalSystem},
	} {
		if got, err := enqueuerActor(unbound); err == nil {
			t.Errorf("%+v, bound to no seat, closes as %+v, want refused", unbound, got)
		}
	}

	err := queueCloser{}.CloseWorkItem(t.Context(), gitlethome.TaskClose{
		As:       gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: "dev"},
		WorkItem: types.WorkItem{Backend: types.WorkJira, ID: "10042", Key: "ENG-42"},
	})
	var refused *gitlethome.NotClosed
	if !errors.Is(err, errQueueCloseNotWorklet) || !errors.As(err, &refused) ||
		!strings.Contains(refused.Why, "ENG-42") {
		t.Fatalf("closing a Jira issue answered %v, want it settled not closed, naming ENG-42, as not Worklet's",
			err)
	}
	// A CLOSE NO ASKING AGAIN CHANGES is final; an unbound enqueuer is one.
	err = queueCloser{}.CloseWorkItem(t.Context(), gitlethome.TaskClose{
		As:       gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: "token:founder"},
		WorkItem: types.WorkItem{Backend: types.WorkWorklet, ID: "task-1", Key: "ENG-1"},
	})
	if !errors.As(err, &refused) {
		t.Fatalf("closing as a credential bound to no seat answered %v, want it settled not closed", err)
	}
}

// A CLOSE WORKLET REFUSES FOR GOOD IS NOT ASKED AGAIN (M5-D27): a queued
// change's close that Worklet answers with a refusal it MARKS as final — the
// item gone, not the enqueuer's to write, its content or its state refusing
// the close — is answered NotClosed, in the engine's own sentence naming the
// item, so the queue settles it and tells the change's people once; a
// failure of the node's own stays a plain error, which leaves the close owed
// for the next turn.
//
// Mutations: let a gone item through as the node's failure → it is asked
// again every turn, red; let a refused write through → red; settle the
// node's own failure → it is never asked again, red.
func TestACloseWorkletRefusesForGoodIsNotAskedAgain(t *testing.T) {
	t.Parallel()
	item := types.WorkItem{Backend: types.WorkWorklet, ID: "task-42", Key: "ENG-42"}
	for _, c := range []struct {
		name  string
		err   error
		final bool
	}{
		{"an item gone", fmt.Errorf("%w: task-42", worklet.ErrNoTask), true},
		{"a write not the enqueuer's", &worklet.Refusal{Mark: worklet.ErrForbidden,
			Err: errors.New("not dev's to write")}, true},
		{"a close its content refuses", &worklet.Refusal{Mark: worklet.ErrInvalid,
			Err: errors.New("a required field is empty")}, true},
		{"a close its state refuses", &worklet.Refusal{Mark: worklet.ErrState,
			Err: errors.New("it is in the trash")}, true},
		{"the node's own failure", errors.New("the data node did not answer"), false},
	} {
		got := closeFailed(item, "dev", "close", c.err)
		var refused *gitlethome.NotClosed
		switch {
		case !errors.Is(got, c.err):
			t.Errorf("%s: answered %v, which lost the cause %v", c.name, got, c.err)
		case c.final && (!errors.As(got, &refused) || !strings.Contains(refused.Why, "ENG-42")):
			t.Errorf("%s: answered %v, want it settled not closed naming ENG-42", c.name, got)
		case !c.final && errors.As(got, &refused):
			t.Errorf("%s: settled not closed (%q), want it left owed and asked again", c.name, refused.Why)
		}
	}
}
