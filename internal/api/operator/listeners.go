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
// of the node's catalogues answered it.
//
// # Why the node keeps them, rather than each catalogue
//
// The transport keeps no session (see [guardedMCP]), so the one channel to a
// client between its requests is that stream, and it stays on the [Server]
// that answered it. A node builds a NEW Server whenever what it serves changes
// (the api package's EngineOperators), so an assistant connected at boot is
// parked on the boot catalogue through every later swap — and a node also
// hears of its PEERS' swaps, after which its listeners have to be told
// whatever catalogue they are parked on. So every catalogue a node builds
// shares ONE registry ([Options.Listeners]), and an announcement reaches every
// stream on the node: never one catalogue's listeners, and with no list of
// replaced catalogues to keep, prune and race.
//
// # Told one by one, rather than through the SDK's own notifier
//
// The SDK announces a tool-list change only when its server's tool registry
// changes, to every stream subscribed on that one server at once, 10 ms after
// the change. A catalogue here never changes once built, and the two moments
// a node must tell somebody are not changes to any registry: a peer rebuilt,
// or ONE stream has just been opened. So each stream is told directly, with
// its own subscription named in the notification's `_meta` as the SDK names
// it, through the sending handler of the server it is on.
//
// # Told the moment its stream opens
//
// A client lists its tools whenever it likes — the SDK's client opens its
// stream fire-and-forget at connect and returns, so its first listing usually
// races the stream — and a listing may be answered by any node. So a listing
// can predate a change some node then announced before this client's stream
// existed to hear it: the stream resolved its catalogue an instant before a
// swap, or a peer's rebuild was relayed to this node's listeners an instant
// before the stream subscribed. Nothing would ever tell it again. Telling
// every stream once as it opens closes that whole window for the price of
// one listing per connection: whatever a client listed before its stream
// existed, it lists again once the stream can hear every later change.
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
// the request that found its facts moved, under the lock its rebuilds take,
// and from its peers' announcements as they arrive. The streams are told one
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
