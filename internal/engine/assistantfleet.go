package engine

import (
	"context"
	"fmt"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/live"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/signedask"
)

// THE DASHBOARD ASSISTANT ACROSS THE FLEET: this node's half of a turn's
// stream (internal/assistant/live), over the queue's ephemeral verbs, the
// keyring every state-log record is signed under, the AEAD the conversation is
// sealed with at rest, and the fleet's roster; and the counter a person's
// turn starts are counted on.

// AssistantFleet builds this node's half of the assistant's fleet stream, or
// nil on an engine with no broker or no coordination store: a node with no
// peer serves its own turns. Its three channels are built here, over the
// fleet keyring, because the assistant imports no state log.
func AssistantFleet(e *Engine) (*live.Fleet, error) {
	if e.backends == nil || e.backends.Queue == nil || e.backends.Coord == nil {
		return nil, nil
	}
	ring := recordKeyring(e.boot)
	frames, err := signedask.NewOneWay[live.Sealed](live.FrameChannel, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the assistant's frame channel: %w", err)
	}
	replays, err := signedask.New[live.ReplayRequest, live.ReplayReply](live.ReplayChannel, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the assistant's replay channel: %w", err)
	}
	stops, err := signedask.New[live.StopRequest, live.StopReply](live.StopChannel, ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the assistant's stop channel: %w", err)
	}
	return live.NewFleet(live.FleetOptions{Node: e.Node().ID(), Broker: e.backends.Queue,
		Roster: nodeRoster{coord: e.backends.Coord}, Cipher: e.cipher, Frames: frames,
		Replays: replays, Stops: stops,
		// THE FLEET'S READ BUDGET, the one every scatter a person waits on
		// is bounded by: a resumed stream waits no longer for the running
		// node than a history read does.
		AskBudget: eventfan.FleetReadBudget})
}

// nodeRoster is the fleet's roster as the assistant asks it: whether a node
// still holds its presence lease (internal/coord's node class).
type nodeRoster struct{ coord coord.Backend }

// Present reports whether node holds its presence lease now. An unreachable
// store is an error, never an absent node.
func (r nodeRoster) Present(ctx context.Context, node string) (bool, error) {
	lease, err := r.coord.Get(ctx, coord.NodeResource(node))
	if err != nil {
		return false, fmt.Errorf("engine: read node %s's presence: %w", node, err)
	}
	return lease != nil, nil
}

// AssistantTurnRate is the fleet's counter a person's turn starts are counted
// on, or nil on a node with no fleet store, whose turns are counted on the
// node alone. A NIL INTERFACE there, never one holding a nil store.
func AssistantTurnRate(e *Engine) assistant.TurnRate {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return e.backends.Fleet
}
