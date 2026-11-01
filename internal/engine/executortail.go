package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A running coding run's live output, read from the node that reads its box
// (`executor_tail`, internal/sandbox/livetail.go).
//
// # Every node serves, whether or not it has a sandbox backend
//
// A run is owned by whichever node holds its seat — and a box on an engine host
// is read by the node that launched it, which alone can reach it — and that
// node may serve no API, so the answer cannot be the API's to build: every node
// answers for the runs it owns or hosts, from their boxes, and every node that
// serves the API asks. A
// node with no sandbox backend owns no run, so it is never addressed — but it
// still ASKS, because the person watching may be looking at a run another node
// drives. The reader is therefore built wherever a fleet record exists, not
// only where a manager does.

// armExecutorTails builds this node's reader and makes the node an answerer.
//
// After the node, whose incarnation is what a run's record names as its owner.
func (e *Engine) armExecutorTails(ctx context.Context) error {
	if e.backends == nil || e.backends.Fleet == nil || e.node == nil {
		// No fleet record, no run: a detached run cannot exist without one
		// (buildSandboxRuntime refuses it), so there is nothing to watch.
		return nil
	}
	pending := sandbox.NewCoordStore(e.backends.Fleet)
	owner := e.node.Owner()
	// THE READINGS OUTLIVE THE REQUESTS THAT START THEM, and are ended here
	// rather than by whichever context built the engine: a read of a box is
	// shared by everybody watching the run and runs under its own budget.
	e.sandboxFeeds = sandbox.NewLiveFeeds(context.WithoutCancel(ctx), sandbox.LiveFeedsOptions{
		Manager: e.sandboxManager,
	})
	var q sandboxTailQueue
	if e.backends.Queue != nil {
		q = e.backends.Queue
	}
	reader, stop, err := serveExecutorTails(ctx, owner, e.id, pending, e.sandboxFeeds, q)
	if err != nil {
		return err
	}
	e.executorTails, e.stopTailServe = reader, stop
	return nil
}

// sandboxTailQueue is the broker a node asks a peer and answers one through.
type sandboxTailQueue interface {
	sandbox.TailAsker
	sandbox.TailServer
}

// serveExecutorTails builds a node's tail reader and makes it an answerer for
// the runs it owns and the boxes it launched on its own host, both classing a
// failure by [tailCondition]. owner is the node's incarnation, which a run's
// record names as its owner, and node its id, which a run's record names as
// the node a box on an engine host was launched on. A nil q is a node with no
// broker, which is the whole fleet: it owns every run there is, answers for
// itself, and serves nobody.
func serveExecutorTails(ctx context.Context, owner, node string, pending sandbox.PendingReader,
	feeds *sandbox.LiveFeeds, q sandboxTailQueue) (*sandbox.TailReader, queue.Unsubscribe, error) {

	reader := &sandbox.TailReader{Owner: owner, Node: node, Pending: pending, Feeds: feeds,
		Condition: tailCondition}
	if q == nil {
		return reader, nil, nil
	}
	reader.Queue = q
	stop, err := sandbox.ServeTail(ctx, q, owner, node, pending, feeds, tailCondition)
	if err != nil {
		return nil, nil, err
	}
	return reader, stop, nil
}

// tailCondition is the one rule for a condition ([builtin.Condition]), so a
// failure this node meets answering a run's output, for a peer or for itself,
// is classed exactly as the same failure is on the query surface beside it.
func tailCondition(err error) bool {
	_, ok := builtin.Condition(err)
	return ok
}

// stopExecutorTails withdraws this node as an answerer and ends its readings,
// before the seats are released and the boxes stop being this node's to read.
func (e *Engine) stopExecutorTails(ctx context.Context) {
	if e.stopTailServe != nil {
		if err := e.stopTailServe(context.WithoutCancel(ctx)); err != nil {
			log.WarnContext(ctx, "sandbox_tail_answerer_not_withdrawn", "error", err)
		}
		e.stopTailServe = nil
	}
	if e.sandboxFeeds != nil {
		e.sandboxFeeds.Stop()
	}
}

// ExecutorTails answers `executor_tail`: a running coding run's live output,
// from the node that owns it. Nil on a node with no fleet record.
func (e *Engine) ExecutorTails() *sandbox.TailReader { return e.executorTails }
