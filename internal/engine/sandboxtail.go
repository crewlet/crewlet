package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A running coding run's live output, read from the node that owns the run
// (`sandbox_tail`, internal/sandbox/livetail.go).
//
// # Every node serves, whether or not it has a sandbox backend
//
// A run is owned by whichever node holds its seat, and that node may serve no
// API, so the answer cannot be the API's to build: every node answers for the
// runs it owns, from their boxes, and every node that serves the API asks. A
// node with no sandbox backend owns no run, so it is never addressed — but it
// still ASKS, because the person watching may be looking at a run another node
// drives. The reader is therefore built wherever a fleet record exists, not
// only where a manager does.

// armSandboxTails builds this node's reader and makes the node an answerer.
//
// After the node, whose incarnation is what a run's record names as its owner.
func (e *Engine) armSandboxTails(ctx context.Context) error {
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
	e.sandboxTails = &sandbox.TailReader{Owner: owner, Pending: pending, Feeds: e.sandboxFeeds}
	if e.backends.Queue == nil {
		// A NODE WITH NO BROKER IS THE FLEET: it owns every run there is.
		return nil
	}
	e.sandboxTails.Queue = e.backends.Queue
	stop, err := sandbox.ServeTail(ctx, e.backends.Queue, owner, pending, e.sandboxFeeds)
	if err != nil {
		return err
	}
	e.stopTailServe = stop
	return nil
}

// stopSandboxTails withdraws this node as an answerer and ends its readings,
// before the seats are released and the boxes stop being this node's to read.
func (e *Engine) stopSandboxTails(ctx context.Context) {
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

// SandboxTails answers `sandbox_tail`: a running coding run's live output,
// from the node that owns it. Nil on a node with no fleet record.
func (e *Engine) SandboxTails() *sandbox.TailReader { return e.sandboxTails }
