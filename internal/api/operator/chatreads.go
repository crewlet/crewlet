package operator

import (
	"context"
	"errors"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/coord"
)

// ChatReadStore is the coordination store's half of a person's chat read
// state this surface reaches: one read and one merge, and nothing else in the
// fleet's shared state, so a seam that serves a person's cursors is never given
// a reason to reach the activation pointer or the sealed secrets.
type ChatReadStore interface {
	ChatRead(ctx context.Context, handle string) (coord.ChatReadState, error)
	AdvanceChatRead(ctx context.Context, handle string, delta coord.ChatReadDelta) (
		coord.ChatReadState, error)
}

// ChatReads is a person's chat read state as this surface serves it: how far
// they have read in each room, what they muted and whether they are in
// do-not-disturb, read for `list_channels` ([builtin.ChatDeps.ReadState],
// through [ChatReads.ReadState]), and written by `mark_chat_read`
// ([builtin.ChatDeps.Cursors]).
//
// # Why it lives on this surface and no other
//
// A read cursor is a PERSON's attention. A seat is woken rather than browsing
// and has none, so no seat's surface is given either half — and the dashboard,
// which writes a person's every gesture through this catalogue (ADR-0024),
// flushes its cursors through the same tool a person's assistant would.
//
// # And why the write tells the person's other screens
//
// A cursor is not on the log, so no applier will ever announce it, and the one
// reader that wants it is the same person's other tabs: a badge cleared in one
// window has to clear in the other. So a flush that may have moved the record
// is handed to [ChatReads.Moved], beside the write it follows, because the
// tool knows no socket.
type ChatReads struct {
	// Store is where the state lives. Required.
	Store ChatReadStore

	// Moved tells the person's own sockets their read state moved, with the
	// record as it stands. Nil tells nobody, which is a node serving no live
	// channel.
	//
	// CALLED FOR EVERY FLUSH THAT MAY HAVE MOVED THE RECORD, and only for
	// those. One that landed is told with the state it resulted in. One
	// whose answer was lost ([coord.ErrWriteUnknown]) may have landed, so
	// the record is READ BACK and that is told: whether or not this flush
	// is in it, it is what every screen should show, and the person's other
	// tabs would otherwise keep a badge the record no longer owes until they
	// next read the rail. One the merge refused, or that failed before any
	// write, changed nothing and is told to nobody — a frame for it would
	// be news of nothing.
	//
	// IT IS HANDED NO CONTEXT. The flush's own request carries the seat the
	// dispatch pinned for its caller ([withSeat]), and the live channel's
	// push decides whose every other socket is: run under that request, it
	// once resolved every person's socket on the node to the caller and
	// closed the rest as rebound. What it needs is the person and the
	// record, and that is all it is given.
	Moved func(handle string, state coord.ChatReadState)
}

// Both seams it fills, held by the compiler: [ChatReads.ReadState] is
// [builtin.ChatDeps.ReadState]'s shape, and the value is a
// [builtin.ChatCursors].
var (
	_ builtin.ChatCursors                                        = ChatReads{}
	_ func(context.Context, string) (coord.ChatReadState, error) = ChatReads{}.ReadState
)

// ReadState is one person's read state — how far they have read in each room,
// what they muted, when their do-not-disturb ends — as the
// [builtin.ChatDeps.ReadState] seam reads it.
//
// THE WHOLE STATE, so `list_channels` marks what the person muted as the rail
// does, rather than handing a silenced room's backlog to their assistant as
// ordinary unread. AND THE PERSON'S ALONE: the reconcile's absence stamp is
// its machinery, so it is not answered.
//
// THE STORE'S ERROR IS RETURNED, never a zero state in its place: a zero state
// is somebody who has opened nothing, to whom every room's tail is unread, and
// a store that could not be read is not that person.
func (r ChatReads) ReadState(ctx context.Context, handle string) (coord.ChatReadState, error) {
	state, err := r.Store.ChatRead(ctx, handle)
	if err != nil {
		return coord.ChatReadState{}, err
	}
	state.AbsentSince = time.Time{}
	return state, nil
}

// AdvanceChatRead merges one flush into a person's read state, and tells their
// other screens — the [builtin.ChatCursors] seam.
//
// ITS ERROR IS THE STORE'S, AS IT IS — neither re-worded nor wrapped. The tool
// tells a flush the merge refused ([coord.ErrInvalidReadState], the caller's
// to change) from a store that could not take it (worth asking again) by the
// sentinel alone, and says the first in the merge's own words
// ([coord.ReadStateSentence]): re-worded, the sentinel is lost and a refusal
// that can never land reads "try again"; wrapped, the wrap's words reach the
// person ahead of the merge's.
//
// THE PERSON'S STATE AND NOTHING OF THE RECONCILE'S: the absence stamp is the
// maintenance sweep's machinery, so it is neither answered nor pushed.
//
// A FLUSH WHOSE ANSWER WAS LOST is still answered with that error — the tool
// says `unknown` from it — and the record is read back for [ChatReads.Moved]:
// see that field. The read-back is the caller's context, so a request that was
// itself abandoned pushes nothing, and the next read of the rail is what
// repairs the other tabs.
func (r ChatReads) AdvanceChatRead(ctx context.Context, handle string,
	delta coord.ChatReadDelta) (coord.ChatReadState, error) {

	state, err := r.Store.AdvanceChatRead(ctx, handle, delta)
	if errors.Is(err, coord.ErrWriteUnknown) {
		if stood, readErr := r.Store.ChatRead(ctx, handle); readErr == nil {
			r.tell(handle, stood)
		}
		return coord.ChatReadState{}, err
	}
	if err != nil {
		return coord.ChatReadState{}, err
	}
	return r.tell(handle, state), nil
}

// tell hands one state of a person's record to [ChatReads.Moved], stripped of
// the reconcile's absence stamp, and returns what it told.
func (r ChatReads) tell(handle string, state coord.ChatReadState) coord.ChatReadState {
	state.AbsentSince = time.Time{}
	if r.Moved != nil {
		r.Moved(handle, state)
	}
	return state
}
