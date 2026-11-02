package api

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// heldReads is a read-state store holding one record, stamped absent by the
// reconcile — the state a seat's record is in for a day after the company
// lost the seat, while a person may still be reading as it.
type heldReads struct {
	state coord.ChatReadState
	err   error
}

func (h heldReads) ChatRead(context.Context, string) (coord.ChatReadState, error) {
	return h.state, h.err
}

func (h heldReads) AdvanceChatRead(context.Context, string, coord.ChatReadDelta) (
	coord.ChatReadState, error) {
	return h.state, h.err
}

// A PERSON IS HANDED THEIR OWN READ STATE AND NOTHING OF THE RECONCILE'S, on
// the read a listing counts its badges by and on the answer a flush returns:
// the absence stamp is the maintenance sweep's machinery, and handed to a
// person's assistant it is a date nobody chose sitting in their state.
func TestAPersonsReadStateCarriesNothingOfTheReconcile(t *testing.T) {
	t.Parallel()
	stamped := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	reads := chatReads{store: heldReads{state: coord.ChatReadState{
		Cursors: map[string]int64{"c1": 7}, Muted: []string{"c2"},
		Revision: 4, AbsentSince: stamped,
	}}}
	read, err := reads.ReadState(t.Context(), "jane-founder")
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	flushed, err := reads.AdvanceChatRead(t.Context(), "jane-founder",
		coord.ChatReadDelta{Cursors: map[string]int64{"c1": 8}})
	if err != nil {
		t.Fatalf("AdvanceChatRead: %v", err)
	}
	for name, state := range map[string]coord.ChatReadState{
		"the listing's read": read, "the flush's answer": flushed,
	} {
		if !state.AbsentSince.IsZero() {
			t.Errorf("%s carries the reconcile's absence stamp %s", name, state.AbsentSince)
		}
		if state.Cursors["c1"] != 7 || len(state.Muted) != 1 || state.Revision != 4 {
			t.Errorf("%s lost the person's own state: %+v", name, state)
		}
	}
}

// THE STORE'S ERROR REACHES THE TOOL AS IT IS, and never a zero state beside
// it: the tool tells a flush the merge refused from one whose answer was lost
// by the sentinel alone, and a zero state is somebody who has read nothing —
// every room's tail unread to them.
func TestAReadStateTheStoreCouldNotAnswerIsTheStoresAnswer(t *testing.T) {
	t.Parallel()
	for name, cause := range map[string]error{
		"a flush the merge refused": fmt.Errorf("%w: a cursor names no channel",
			coord.ErrInvalidReadState),
		"a flush whose answer was lost":  coord.ErrWriteUnknown,
		"a store that could not be read": errors.New("the bucket is unreachable"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			reads := chatReads{store: heldReads{
				state: coord.ChatReadState{Cursors: map[string]int64{"c1": 7}},
				err:   cause,
			}}
			read, err := reads.ReadState(t.Context(), "jane-founder")
			if !errors.Is(err, cause) || err.Error() != cause.Error() {
				t.Errorf("the read answered %v, want the store's own %v", err, cause)
			}
			if read.Cursors != nil {
				t.Errorf("the read answered a state %+v beside its error", read)
			}
			flushed, err := reads.AdvanceChatRead(t.Context(), "jane-founder",
				coord.ChatReadDelta{})
			if !errors.Is(err, cause) || err.Error() != cause.Error() {
				t.Errorf("the flush answered %v, want the store's own %v", err, cause)
			}
			if flushed.Cursors != nil {
				t.Errorf("the flush answered a state %+v beside its error", flushed)
			}
		})
	}
}
