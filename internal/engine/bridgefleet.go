package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
)

// The executor's tool bridge, served to the whole fleet (internal/api/mcpbridge).
//
// # Every node joins, in every role
//
// A box dials a node that serves the bridge's route — the fleet's one address,
// any node that serves the API, or for a direct box its launching node's own
// listener on loopback, which a node without the ingress role serves its own
// host alone — and a run's session lives on the node holding the run's seat,
// which may serve no API at all, and may have handed the seat on since the box
// was launched. So every node with a bridge joins the fleet's relay: the node
// a call lands on reads the seat's lease and relays the call to the holder
// over the broker, and the holder answers it from its own session. A node that
// has no broker is the fleet, and answers from its own sessions alone.
//
// # Not an ingress node that serves no HTTP
//
// A node WITHOUT the ingress role joins whatever its own api.port: its remote
// boxes reach it through the nodes with the role, and a satellite started with
// `-api-port 0` serves nothing and still runs agent seats. A node WITH the
// role is one of the nodes a box dials, and api.port 0 — the hard off switch
// for every listener — leaves it serving nothing; in a fleet of one it is the
// whole fleet, and a box would reach nothing at all. So such a node joins
// nothing, its bridge is unreachable, and an executor launch is refused naming
// the setting ([mcpbridge.Bridge.Reachable]) rather than started into calls
// that all fail.

// bridgeHolderWaitLeases is how many seat-lease lifetimes a box's call waits
// for a node that can answer it: one for a dead holder's lease to run out, and
// one for a peer to claim the seat and open its running jobs' sessions again
// as it recovers it — after which nothing more is coming, and the call is
// answered that the bridge is unavailable.
const bridgeHolderWaitLeases = 2

// joinBridgeFleet joins this node's bridge to its fleet. After the node, whose
// incarnation a seat's lease names.
func (e *Engine) joinBridgeFleet(ctx context.Context, boot *config.Bootstrap) error {
	if e.bridge == nil || e.node == nil || e.backends == nil || e.backends.Queue == nil ||
		e.backends.Coord == nil || boot == nil {
		return nil
	}
	if boot.API.Port == 0 && boot.Profile(e.node.ID()).RunsIngress() {
		return nil
	}
	leave, err := e.bridge.Join(ctx, mcpbridge.FleetOptions{
		Fabric: e.backends.Queue, Leases: e.backends.Coord,
		Node: e.node.ID(), Owner: e.node.Owner(),
		Keyring:    recordKeyring(boot),
		HolderWait: bridgeHolderWaitLeases * leaseTTL(boot),
	})
	if err != nil {
		return err
	}
	e.leaveBridgeFleet = leave
	return nil
}

// stopBridgeFleet withdraws this node from the bridge's relay, before its seats
// are released and the broker closes, so a call arriving during the teardown is
// found another holder rather than answered from a session that is closing.
//
// AND RELEASES EVERY POLL FOR A RUN'S NOTES THIS NODE HOLDS FIRST
// ([mcpbridge.Bridge.Shutdown]): a poll relayed here from another node is held
// by no listener of this one, so nothing else lets it go before its hold runs
// out — and the relay waits on it.
func (e *Engine) stopBridgeFleet(ctx context.Context) {
	e.bridge.Shutdown()
	if e.leaveBridgeFleet == nil {
		return
	}
	if err := e.leaveBridgeFleet(context.WithoutCancel(ctx)); err != nil {
		log.WarnContext(ctx, "bridge_relay_not_withdrawn", "error", err)
	}
	e.leaveBridgeFleet = nil
}
