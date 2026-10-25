package api

import (
	"context"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// OperatorListens tells every MCP client listening on this node that its
// operator tools changed — when this node's catalogue moves, and when any
// other node's does.
//
// # Why there is anything to tell
//
// The operator surface reads its catalogue at the top of every call, so a
// client's next request is always served what the node serves then. What a
// connected assistant does NOT make is a request: a client of the 2026-07-28
// protocol holds one stream open (`subscriptions/listen`) and lists its tools
// again only when it is told they changed. The stream stays on the catalogue
// its listen was answered from, and nothing told it anything, so an assistant
// connected before a node's first company kept offering the tools it
// connected to — none of the tracker's — until it was reconnected. The node's
// one registry of streams is [operator.Listeners]; this decides WHEN to tell
// them.
//
// # When: what a call would be served has moved
//
// [OperatorListens.Moved] compares the tools a call is served now
// ([operator.Server.Tools]) with the ones it last compared, and tells the
// streams only where they differ: the node runs it after every company it
// publishes and every catalogue change a shared tool server makes, which is
// where each fact the catalogue is built from moves — the native halves at a
// node's first company, a knowledge backend added or retired, a tool server's
// tools. A revision that moves none of them tells nobody. The company's NAME
// is deliberately not compared: it titles the MCP server, which a client reads
// once, at connect, and a tool-list notification would not make it read the
// title again.
//
// # Told by the fleet, not by one node
//
// Every node applies a revision on its own clock — the activation reaches
// each by a nudge and a poll, and a company's first apply builds the native
// runtime, which takes seconds — while an assistant's next listing, with no
// session to pin it, reaches whichever ingress node a load balancer picks.
// Told only by the node holding its stream, an assistant listed again at once
// and was often answered by a peer that had not applied yet; that peer's own
// move later told only ITS listeners, so the assistant kept the old tools
// until the next change: after a knowledge backend was removed it went on
// offering `search_knowledge`, answered "unknown tool". So each move is
// announced to the fleet (`operator_tools_changed`), and every node that hears
// a peer's tells every stream it holds. The last thing an assistant hears
// therefore follows the last node's move, and the listing it makes then is the
// new catalogue wherever it lands — for as long as its stream is open and the
// broker carries the nudge, which is best effort like every nudge.
type OperatorListens struct {
	server    *operator.Server
	listeners *operator.Listeners

	// mu orders the comparisons, so two triggers arriving together tell the
	// streams once and never compare against a reading the other replaced.
	mu sync.Mutex
	// offered is the tool set the last comparison found, sorted; the zero
	// value until the first.
	offered []string
	// compared says offered holds a reading — an empty catalogue is one.
	compared bool

	fleet operatorFleet
}

// operatorFleet is how one node hears of, and tells, every other node's moves
// ([types.OperatorToolsChanged]).
type operatorFleet struct {
	// stream is where the nudge is published and heard; nil for a node
	// alone.
	stream announcements
	// node is this node's id — the origin its queue stamps on every event it
	// publishes ([events.Event.Node]) — so a node does not hear its own.
	node string
	// detach ends the subscription; nil where none was made.
	detach queue.Unsubscribe
	// telling is every publish of this node's nudge still in flight.
	telling sync.WaitGroup
}

// announcements is the event stream the fleet's moves travel on: the
// engine's queue.
type announcements interface {
	Publish(ctx context.Context, topic string, ev *events.Event) error
	SubscribeStream(ctx context.Context, pattern string, h queue.StreamHandler) (queue.Unsubscribe, error)
}

// operatorToolsTopic is where a node's move is announced to the fleet.
var operatorToolsTopic = topics.Event(types.OperatorToolsChanged{}.EventType())

// fleetTellBudget bounds the publish of one move's nudge.
//
// FIVE SECONDS, the bound the engine gives a lifecycle announcement on the
// same event stream: a publish a healthy broker acknowledges in milliseconds,
// and one a broker that will not acknowledge must not hold a goroutine for
// longer than a person would wait for the dashboard beside it. A nudge lost to
// it costs listeners elsewhere this one move, never a node's catalogue.
const fleetTellBudget = 5 * time.Second

// NewOperatorListens is the telling over server's catalogue and the registry
// its streams are kept in — the one [operator.Options.Listeners] it was built
// with — announcing to and hearing from the fleet on stream as the node named
// node once [OperatorListens.Join] runs. A nil stream is a node alone.
func NewOperatorListens(server *operator.Server, listeners *operator.Listeners,
	stream announcements, node string) *OperatorListens {
	return &OperatorListens{
		server: server, listeners: listeners,
		fleet: operatorFleet{stream: stream, node: node},
	}
}

// Join attaches this node to the fleet's announcements, and takes the first
// reading of what a call is served, so the first move it is told of is
// compared with what the node served before it.
//
// A SUBSCRIPTION THAT WILL NOT ATTACH IS NOT FATAL, and saying so is the
// design, as for every nudge between nodes: this node still tells its own
// listeners of its own moves and still tells its peers of them, and what it
// costs is that a listener here is not told of a peer's.
func (o *OperatorListens) Join(ctx context.Context) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.offered, o.compared = o.tools(), true
	o.mu.Unlock()
	if o.fleet.stream == nil {
		return
	}
	detach, err := o.fleet.stream.SubscribeStream(ctx, operatorToolsTopic, o.hear)
	if err != nil {
		log.WarnContext(ctx, "operator_tools_nudge_unavailable", "error", err,
			"detail", "an MCP client listening on this node is not told when another "+
				"node's operator tools change, only when this one's do")
		return
	}
	o.fleet.detach = detach
}

// Moved compares what a call is served now with what the last comparison
// found and, where they differ, tells every stream on this node and the fleet.
// ctx lends what it carries to the telling and never its cancellation
// ([operator.Listeners.Announce]): the announcement is owed to every listener
// whatever becomes of the apply that caused it.
func (o *OperatorListens) Moved(ctx context.Context) {
	if o == nil {
		return
	}
	if o.compare() {
		o.listeners.Announce(ctx)
		o.tell(ctx)
	}
}

// compare takes a reading and reports whether it differs from the last.
func (o *OperatorListens) compare() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	now := o.tools()
	moved := o.compared && !slices.Equal(now, o.offered)
	o.offered, o.compared = now, true
	return moved
}

// tools is what a call is served now, sorted.
func (o *OperatorListens) tools() []string {
	names := o.server.Tools()
	slices.Sort(names)
	return names
}

// tell announces this node's move to every other node, in the background, on
// ctx's values and never its cancellation, for [OperatorListens.Moved]'s
// reason. Best effort, like every nudge: a publish that fails costs listeners
// elsewhere this one move.
func (o *OperatorListens) tell(ctx context.Context) {
	if o.fleet.stream == nil {
		return
	}
	detached := context.WithoutCancel(ctx)
	o.fleet.telling.Go(func() {
		ctx, cancel := context.WithTimeout(detached, fleetTellBudget)
		defer cancel()
		// The ORIGIN is the queue's stamp (events.Event.Node), which is what
		// a hearer compares with its own; Source says who, as every nudge's
		// does.
		ev := events.New(types.OperatorToolsChanged{}, events.TraceContext{})
		ev.Source = o.fleet.node
		if err := o.fleet.stream.Publish(ctx, operatorToolsTopic, ev); err != nil {
			log.WarnContext(ctx, "operator_tools_nudge_not_published", "error", err,
				"detail", "MCP clients listening on other nodes are not told of this "+
					"node's new operator tools until the next change anywhere")
		}
	})
}

// hear is a peer's move arriving: every stream on this node is told — whether
// or not this node's own catalogue moved too, since a client connected here may
// have listed from the peer before IT moved.
//
// THIS NODE'S OWN READING IS TAKEN FIRST, so a node that has applied the same
// revision and not yet run its hook records the move now, and its hook then
// finds nothing to tell twice; such a move is told to the fleet as well.
func (o *OperatorListens) hear(ctx context.Context, _ string, ev *events.Event) {
	if ev == nil || ev.Node == o.fleet.node {
		return
	}
	if o.compare() {
		o.tell(ctx)
	}
	o.listeners.Announce(ctx)
}

// End ends every stream a client listens on at this node
// ([operator.Listeners.End]). The node runs it as its API listener begins to
// shut down: a stream is one request held open, and the listener's graceful
// shutdown would otherwise wait for every assistant listening until its grace
// ran out and then cut them with a warning meant for a request that was stuck.
func (o *OperatorListens) End() {
	if o != nil {
		o.listeners.End()
	}
}

// Close detaches this node from the fleet's announcements and waits for the
// nudges it is still publishing, bounded by ctx. The node runs it as its API
// comes down, before the engine closes the queue both ride.
func (o *OperatorListens) Close(ctx context.Context) {
	if o == nil {
		return
	}
	if o.fleet.detach != nil {
		// WithoutCancel: a stop is routinely driven by the cancellation it is
		// cleaning up after, and a detach that inherited it would leave the
		// subscription behind on the broker.
		if err := o.fleet.detach(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "operator_tools_nudge_detach_failed", "error", err)
		}
	}
	done := make(chan struct{})
	go func() {
		o.fleet.telling.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
