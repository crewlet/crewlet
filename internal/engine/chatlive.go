package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/notify"
)

// ChatLive is the engine's own chat's live half: where a committed chat batch
// is announced to this node's sockets, and the poster a Chatlet seat's working
// indicator is raised on. The api's chat hub implements it.
//
// ONE VALUE FOR BOTH HALVES, because both are one hub: its presence answer is
// how a screen on another node sees an indicator raised here, so two hubs
// would record an indicator in one no presence answer reads. ATTACHED ON EVERY
// NODE, sockets or none, because any node runs turns; so an attached hub is
// not by itself screens to tell, which is [ChatLive.ServesSockets].
type ChatLive interface {
	// Applied hears every committed chat batch this node's sockets are shown
	// ([chatlet.Observer]): its own applier's while its copy serves, else a
	// sound peer's, relayed (liverelay.go). It runs on that applier's or the
	// relay follower's goroutine with the next batch behind it: never block.
	chatlet.Observer

	// Reread tells every chat screen on this node that what Applied hears has
	// a GAP (its source switched between its own applier and a relayed peer,
	// or between peers) and nothing in between is replayed: a screen rereads
	// its rooms. It must not block.
	Reread()

	// ServesSockets reports whether this node shows anybody what Applied
	// hears: true where the API's socket surface is built over this hub, open
	// screens or not; false on a node that attached it for the working
	// indicator alone. A FACT ABOUT THE NODE, not a count of open screens,
	// which would let the first screen of a node out of service hear its own
	// applier until the relay's follower next looked. [Engine.liveListening] reads it: on a node
	// answering false, following a peer's batches is a loop handing them to
	// nobody, reported as relaying. It must not block.
	ServesSockets() bool

	// StatusPoster is where a Chatlet seat's working indicator is raised —
	// in the mode `chat.chatlet.typing_status` names, by the driver
	// [Engine.applyChatStatus] builds over it.
	notify.StatusPoster
}

// SetChatLive attaches the chat live half (the api's hub); nil detaches, and
// it is safe to call while running or to leave unset.
//
// A SETTER, NOT AN OPTION, for [Engine.SetOnInboxMoved]'s reason: the API is
// built after the engine. The applier's hook ([applyHooks].chatApplied) reads
// what is attached at CALL time, so nothing is rebuilt. It does rebuild the
// working indicator, which no hook reaches: [New] ran
// [Engine.startNotifications] before the hub existed, and a replaced or
// detached hub takes its driver with it.
//
// START THE HUB BEFORE ATTACHING IT: it is called on the applier's goroutine,
// and a hub whose loop is not running is a buffer that fills during a catch-up.
func (e *Engine) SetChatLive(ctx context.Context, live ChatLive) {
	e.chatLive.Store(&live)
	e.applyChatStatus(ctx, e.Company())
}

// chatApplied is what the chat applier calls after a committed batch: announced
// to this node's sockets and kept for its relay followers, one source at a time
// ([Engine.announce]).
func (e *Engine) chatApplied(batch []chatlet.Applied) {
	e.announce(liveBatch{Chat: batch})
}

// attachedChat is the chat live half attached to this engine, or nil.
func (e *Engine) attachedChat() ChatLive {
	live := e.chatLive.Load()
	if live == nil {
		return nil
	}
	return *live
}

// chatListening reports whether a chat live half is attached on this node and
// shows what it hears to sockets here — see [ChatLive.ServesSockets].
func (e *Engine) chatListening() bool {
	live := e.attachedChat()
	return live != nil && live.ServesSockets()
}

// deliverChat hands one committed chat batch to the attached live half.
func (e *Engine) deliverChat(batch []chatlet.Applied) {
	if live := e.attachedChat(); live != nil {
		live.Applied(batch)
	}
}

// chatGap tells the attached live half that what it hears has a gap.
func (e *Engine) chatGap() {
	if live := e.attachedChat(); live != nil {
		live.Reread()
	}
}

// chatObserver is the applier's [chatlet.Observer] over the engine's post-commit
// hook, or nil for a nil hook — a log assembled with no engine behind it, which
// announces to nobody.
func chatObserver(applied func([]chatlet.Applied)) chatlet.Observer {
	if applied == nil {
		return nil
	}
	return chatObserverFunc(applied)
}

// chatObserverFunc is a function as a [chatlet.Observer].
type chatObserverFunc func([]chatlet.Applied)

// Applied implements [chatlet.Observer].
func (f chatObserverFunc) Applied(batch []chatlet.Applied) { f(batch) }
