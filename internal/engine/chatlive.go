package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/notify"
)

// ChatLive is what the process attaches for the engine's own chat's live half:
// where a committed chat batch is announced to the sockets this node serves,
// and the poster a Chatlet seat's working indicator is raised on.
//
// ONE VALUE FOR BOTH HALVES, because both are one hub: the room a person is
// watching is where a committed message is pushed AND where "is thinking…"
// appears, and the hub's presence answer is how a screen on another node sees
// an indicator raised here. Two attachments could be handed two hubs, and then
// a seat's indicator is recorded in one that no presence answer reads.
//
// ATTACHED ON EVERY NODE, sockets or none: a seat raises its indicator on the
// node its turn runs on, and a node without the `ingress` role, or with no
// listener, runs turns too. So an attached hub is not by itself a node with
// screens to tell, which is why [ChatLive.ServesSockets] is asked.
//
// DECLARED HERE, by the consumer, and kept to what the engine calls. The api's
// chat hub implements it.
type ChatLive interface {
	// Applied hears every committed chat batch this node's sockets are to be
	// shown ([chatlet.Observer]) — its own applier's while its copy serves, a
	// sound peer's, relayed, while it does not (liverelay.go). It is called
	// on that applier's goroutine, or the relay follower's, with the next
	// batch waiting behind it, so it must not block.
	chatlet.Observer

	// Reread tells every chat screen on this node that the batches Applied
	// hears have a GAP — its sockets switched from this node's own applier
	// to a relayed peer's, back, or from one peer to another — and what was
	// committed in between is not replayed: a screen reads its rooms again.
	// It must not block either.
	Reread()

	// ServesSockets reports whether this node shows anybody what Applied
	// hears: true where the API's socket surface is built over this hub,
	// whether or not a screen is open right now, and false on a node that
	// serves no API and attached its hub for the working indicator alone.
	// A FACT ABOUT THE NODE rather than about who is looking, as the inbox's
	// listener is one the API registers wherever it serves: a count of open
	// screens would let the first screen of a node out of service hear its
	// own applier until the relay's follower next looked.
	//
	// The live relay's follower reads it ([Engine.liveListening]): on a node
	// that answers false, a peer's batches have nowhere to go, and following
	// them would be a loop asking a peer every few seconds for batches it
	// hands to nobody, reported as relaying. It must not block.
	ServesSockets() bool

	// StatusPoster is where a Chatlet seat's working indicator is raised —
	// in the mode `chat.chatlet.typing_status` names, by the driver
	// [Engine.applyChatStatus] builds over it.
	notify.StatusPoster
}

// SetChatLive attaches the chat live half — the api's hub — to this engine.
//
// A SETTER AND NOT AN OPTION, for [Engine.SetOnInboxMoved]'s reason: the API
// half is built after the engine. The chat applier is handed the engine's own
// hook at construction ([applyHooks].chatApplied) and the hook reads what is
// attached at CALL time, so the applier may already be applying when the hub
// arrives and nothing has to be rebuilt; before it, a committed batch is
// announced to nobody, which is what a screen nobody has opened needs. Safe to
// call while the engine is running, and safe to leave unset; nil detaches.
//
// AND IT REBUILDS THE WORKING INDICATOR, which is the half no hook reaches:
// [New] ran [Engine.startNotifications] before the hub existed, so the boot
// company has no Chatlet driver until one is built over what arrives here —
// and a hub replaced or detached takes its driver with it.
//
// START THE HUB BEFORE ATTACHING IT: what is attached is called on the
// applier's goroutine, and a hub whose own loop is not running is a buffer that
// fills during a catch-up.
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
