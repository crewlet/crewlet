package api

import (
	"context"
	"sync"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/engine"
)

// EngineChatLive is the process's chat hub attached to the engine beside it:
// the live half of the company's own chat — committed frames for the sockets
// this node serves, a chat room's presence on the engine's presence hub, a
// person's read state pushed to their own tabs, and the poster a Chatlet seat's
// working indicator is raised on.
//
// # One per process, attached on every node, after the engine exists
//
// A seat raises its working indicator on whichever node its turn runs on, and
// a node with the `seats` role alone — no listener, no `ingress` — runs turns
// too: the indicator is the seat's member of the room's presence, which that
// node's presence hub tells the node a person has the room open on.
// So the hub is built and attached on EVERY node, whether or not it serves an
// API, and its lifetime is the process's rather than an App's. [New] mounts
// the socket over it where one is served ([Options.ChatLive]), which is what
// tells the hub that what it hears is shown to somebody here.
//
// AFTER [engine.New], through the engine's own setter ([engine.Engine.SetChatLive])
// rather than an option it was built with: the chat applier is handed the
// engine's own post-commit hook at construction and that hook reads what is
// attached when it is CALLED, so nothing has to exist before the engine does —
// and the boot company's Chatlet indicator driver is rebuilt over the hub as it
// arrives.
//
// ONE CONSTRUCTOR for `crewlet run` and the end-to-end suite, for
// [NewHumanSurfaces]' reason: a copy of the wiring in either would be a
// harness agreeing with itself.
type EngineChatLive struct {
	hub    *stream.ChatHub
	engine *engine.Engine
	once   sync.Once
}

// NewEngineChatLive builds the process's chat hub over e, starts it, and
// attaches it to e.
//
// STARTED BEFORE IT IS ATTACHED: what is attached is called on the applier's
// goroutine, and a hub whose own loop is not running is a buffer that fills
// during a catch-up and drops what it was handed.
//
// UNDER ctx WITHOUT ITS CANCELLATION, as every loop the node runs is: a hub
// bound to the signal context would stop at SIGTERM while the drain is still
// finishing turns whose indicators and messages people are watching. It ends
// at [EngineChatLive.Stop], which the process calls after the drain.
func NewEngineChatLive(ctx context.Context, e *engine.Engine) *EngineChatLive {
	backends := e.Backends()
	opts := stream.ChatHubOptions{
		// RESOLVED PER CALL, never captured: the Chatlet half comes up
		// with the node's first company, which on a node that booted with
		// none is after this hub exists — and a company whose chat is not
		// the engine's own answers nil, which pushes nothing.
		Rooms: func() stream.ChatRooms {
			if rooms, ok := engine.OperatorChat(e); ok {
				return rooms
			}
			return nil
		},
		// THE DURABLE RECORD a person's read state is pushed from, which
		// every node holds whatever its company runs; the hub follows it
		// only once a socket surface is mounted over it.
		Reads: backends.Fleet,
	}
	// THE ENGINE'S OWN PRESENCE HUB, which an artifact's room rides too:
	// one implementation, one sealed wire. A NIL HUB STAYS A NIL
	// INTERFACE — an engine with no broker runs none — because a nil
	// pointer inside the interface would be a hub every join panics on.
	if p := e.Presence(); p != nil {
		opts.Presence = p
	}
	hub := stream.NewChatHub(opts)
	hub.Start(context.WithoutCancel(ctx))
	e.SetChatLive(ctx, hub)
	return &EngineChatLive{hub: hub, engine: e}
}

// Hub is the chat hub, for [Options.ChatLive].
func (l *EngineChatLive) Hub() *stream.ChatHub { return l.hub }

// Stop detaches the hub from the engine and stops it, once.
//
// AFTER THE DRAIN AND THE LISTENERS, BEFORE THE ENGINE STOPS: the drain is
// what the people watching are watching, and the engine's appliers are still
// running until it stops — detached first, so a batch committed in between is
// announced to nobody rather than to a hub that no longer drains its buffer,
// and the Chatlet indicator driver built over the hub goes with it. A stopped
// hub is never attached again, which is why this is the last word for it: a
// later attachment would rebuild a driver over a stopped engine.
func (l *EngineChatLive) Stop(ctx context.Context) {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.engine.SetChatLive(ctx, nil)
		l.hub.Stop()
	})
}
