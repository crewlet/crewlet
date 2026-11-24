package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A running executor's live output, read from the node that reads its box
// (`executor_tail`, internal/sandbox/livetail.go).
//
// # Every node serves, whether or not it has a sandbox backend
//
// A run is read by the node that launched its box, which alone can reach it,
// and that node may serve no API: so every node answers for the boxes it
// launched, and every node serving the API asks. A node with no sandbox
// backend is never addressed but still ASKS, since the person watching may be
// looking at a run another node drives: the reader is built wherever a fleet
// record exists, not only where a manager does.

// armExecutorTails builds this node's reader and makes the node an answerer for
// e.id, the launch node a run's record names. It is armed with the node's other
// answerers, before its host claims a seat, so no box launched here is asked
// about before this node answers for it.
func (e *Engine) armExecutorTails(ctx context.Context) error {
	if e.backends == nil || e.backends.Fleet == nil {
		// No fleet record, no run: a detached run cannot exist without one
		// (buildSandboxRuntime refuses it), so there is nothing to watch.
		return nil
	}
	pending := sandbox.NewCoordStore(e.backends.Fleet)
	// THE READINGS OUTLIVE THE REQUESTS THAT START THEM and end in
	// stopExecutorTails: a read of a box is shared by everybody watching the
	// run and runs under its own budget.
	e.sandboxFeeds = sandbox.NewLiveFeeds(context.WithoutCancel(ctx), sandbox.LiveFeedsOptions{
		Manager: e.sandboxManager,
	})
	var q sandboxTailQueue
	if e.backends.Queue != nil {
		q = e.backends.Queue
	}
	reader, stop, err := serveExecutorTails(ctx, e.id, pending, e.sandboxFeeds, q, e.answerBudget)
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
// the boxes it launched, both classing a failure by [tailCondition]. node is
// its id, what a run's record names as its launch node. A nil q is a
// broker-less node, the whole fleet: every box is its own, it answers for
// itself, and serves nobody.
func serveExecutorTails(ctx context.Context, node string, pending sandbox.PendingReader,
	feeds *sandbox.LiveFeeds, q sandboxTailQueue, answers *queue.AnswerBudget) (*sandbox.TailReader,
	queue.Unsubscribe, error) {

	reader := &sandbox.TailReader{Node: node, Pending: pending, Feeds: feeds, Condition: tailCondition}
	if q == nil {
		return reader, nil, nil
	}
	reader.Queue = q
	stop, err := sandbox.ServeTail(ctx, q, node, pending, feeds, tailCondition, answers)
	if err != nil {
		return nil, nil, err
	}
	return reader, stop, nil
}

// tailCondition is the one rule for a condition ([builtin.Condition]), so a
// failure met answering a run's output, for a peer or itself, is classed as
// the query surface beside it classes the same failure.
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

// ExecutorTails answers `executor_tail`: a running executor's live output,
// from the node that launched its box. Nil on a node with no fleet record.
func (e *Engine) ExecutorTails() *sandbox.TailReader { return e.executorTails }
