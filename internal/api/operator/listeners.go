package operator

import (
	"context"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP methods this file acts on, spelled as the protocol spells them: the
// SDK keeps its own names for them unexported.
const (
	// methodListen is a client of the 2026-07-28 protocol opening the stream
	// it hears notifications on: one POST, held open until it ends.
	methodListen = "subscriptions/listen"
	// methodAcknowledged is the first message the server sends on that
	// stream, once the subscription it asked for is registered.
	methodAcknowledged = "notifications/subscriptions/acknowledged"
	// methodToolsChanged tells a client to list the tools again.
	methodToolsChanged = "notifications/tools/list_changed"
)

// listenerTellBudget bounds one notification to one listener.
//
// TEN SECONDS, the bound the MCP SDK gives every notification it sends a
// subscriber itself: a notification is one small server-sent event written to
// a stream a client is reading, so a healthy one takes microseconds and the
// bound only stops a context-honouring step from waiting on a client that has
// gone. A write the client has stopped reading is ended by its connection
// ending, as the SDK's own are.
const listenerTellBudget = 10 * time.Second

// Listeners is every MCP client listening on one node: a client of the
// 2026-07-28 protocol holding its `subscriptions/listen` stream open, whichever
// call's catalogue answered it.
//
// The transport keeps no session ([Server.MCPHandler]), so a stream is the only
// channel to a client between requests. What a node serves moves (a company, a
// backend, a tool server's tools, a PEER's move), so every call's server keeps
// its streams in ONE registry ([Options.Listeners]).
//
// Each stream is told directly, not through the SDK's notifier (which fires
// only when a server's tool registry changes, never here): named in `_meta`,
// and once as it opens, since a client's first listing races its stream.
type Listeners struct {
	mu      sync.Mutex
	streams map[*mcp.ServerSession]*listening
	// ended is set by [Listeners.End]: every stream held then has ended,
	// and one opened later ends as soon as it is acknowledged.
	ended bool
}

// listening is one held stream.
type listening struct {
	// end ends the stream: the listen's handler returns its result, so the
	// POST holding it completes.
	end context.CancelFunc
	// send is the sending handler of the server the stream is on, beneath
	// this package's middleware — the path the SDK's own notifications take
	// — and nil until the stream is acknowledged with the tool list among
	// what it hears.
	send mcp.MethodHandler
	// subscription is the stream's own request id, as its acknowledgement
	// named it, which every notification on the stream carries.
	subscription any
}

// NewListeners is an empty registry, for every catalogue one node builds.
func NewListeners() *Listeners {
	return &Listeners{streams: map[*mcp.ServerSession]*listening{}}
}

// Listening is how many streams hear a tool-list change now.
func (l *Listeners) Listening() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.streams {
		if s.send != nil {
			n++
		}
	}
	return n
}

// Announce tells every stream that hears a tool-list change that the tools
// changed, so its client lists them again — and is answered by whichever
// catalogue the node it reaches serves then.
//
// IN THE BACKGROUND, so no caller waits on a client: the node announces from
// the apply or the tool server that moved what it serves, and from its peers'
// announcements as they arrive. The streams are told one
// after another, as the SDK tells its own subscribers, each bounded by
// [listenerTellBudget]. ctx lends its values and never its cancellation: the
// announcement is owed to every listener whatever becomes of the request or
// the apply that caused it.
func (l *Listeners) Announce(ctx context.Context) {
	l.mu.Lock()
	targets := make([]target, 0, len(l.streams))
	for session, s := range l.streams {
		if s.send != nil {
			targets = append(targets, target{session: session, send: s.send,
				subscription: s.subscription})
		}
	}
	l.mu.Unlock()
	if len(targets) == 0 {
		return
	}
	detached := context.WithoutCancel(ctx)
	go func() {
		for _, t := range targets {
			ctx, cancel := context.WithTimeout(detached, listenerTellBudget)
			t.tell(ctx)
			cancel()
		}
	}()
}

// End ends every stream held now, and every one opened from now on as soon as
// it is acknowledged.
//
// It is what a node's API listener runs as it shuts down. A stream is ONE
// REQUEST HELD OPEN, so a listener's graceful shutdown waited on every
// assistant listening for its whole grace and then cut the connection with a
// warning that is meant to say a request was stuck. Ended here, each listen
// returns its result — the SDK's word for a subscription the server ended
// gracefully — and the request completes like any other. A tool call in
// flight is left alone: it is the shutdown's grace that bounds those.
func (l *Listeners) End() {
	l.mu.Lock()
	l.ended = true
	ends := make([]context.CancelFunc, 0, len(l.streams))
	for _, s := range l.streams {
		ends = append(ends, s.end)
	}
	l.mu.Unlock()
	for _, end := range ends {
		end()
	}
}

// receiving is the middleware every catalogue's MCP server receives through:
// a listen is held under a context [Listeners.End] can end, and leaves the
// registry when it returns. Every other method passes through untouched.
func (l *Listeners) receiving(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		session, ok := req.GetSession().(*mcp.ServerSession)
		if method != methodListen || !ok {
			return next(ctx, method, req)
		}
		ctx, end := context.WithCancel(ctx)
		defer end()
		l.mu.Lock()
		if l.ended {
			// A LISTEN OPENED AFTER THE END is acknowledged and ends at
			// once, rather than holding a listener that is closing.
			end()
		} else {
			l.streams[session] = &listening{end: end}
		}
		l.mu.Unlock()
		defer func() {
			l.mu.Lock()
			delete(l.streams, session)
			l.mu.Unlock()
		}()
		return next(ctx, method, req)
	}
}

// sending is the middleware every catalogue's MCP server sends through. It
// watches for one message: a stream's acknowledgement, which the SDK sends only
// once it has registered the subscription. From then on the stream hears every
// announcement, and it is told once at once — see [Listeners].
func (l *Listeners) sending(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		result, err := next(ctx, method, req)
		if method != methodAcknowledged || err != nil {
			return result, err
		}
		params, _ := req.GetParams().(*mcp.SubscriptionsAcknowledgedParams)
		session, _ := req.GetSession().(*mcp.ServerSession)
		if params == nil || session == nil || !params.Notifications.ToolsListChanged {
			return result, err
		}
		l.mu.Lock()
		s := l.streams[session]
		if s != nil {
			s.send, s.subscription = next, params.Meta[mcp.MetaKeySubscriptionID]
		}
		l.mu.Unlock()
		if s != nil {
			// On the listen's own context, which names the request the
			// notification belongs to, so it is written on this stream.
			target{session: session, send: next, subscription: s.subscription}.tell(ctx)
		}
		return result, err
	}
}

// target is one stream an announcement is written to.
type target struct {
	session      *mcp.ServerSession
	send         mcp.MethodHandler
	subscription any
}

// tell writes one tool-list change to the stream. A stream that ended in the
// meantime refuses it, which costs nothing: its client is no longer listening.
func (t target) tell(ctx context.Context) {
	params := &mcp.ToolListChangedParams{}
	if t.subscription != nil {
		params.Meta = mcp.Meta{mcp.MetaKeySubscriptionID: t.subscription}
	}
	if _, err := t.send(ctx, methodToolsChanged,
		&mcp.ServerRequest[*mcp.ToolListChangedParams]{Session: t.session, Params: params},
	); err != nil {
		log.DebugContext(ctx, "operator_listener_not_told", "error", err,
			"detail", "a stream that ended as it was told has no client to tell")
	}
}
