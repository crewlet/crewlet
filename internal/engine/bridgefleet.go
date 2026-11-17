package engine

import (
	"context"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
)

// The executor's tool bridge (internal/api/mcpbridge) is served fleet-wide.
// EVERY NODE WITH A BRIDGE JOINS ITS RELAY: a box dials any node serving the
// bridge's route (a direct box, its launcher's loopback), while a run's session
// lives on whichever node holds the seat now, which may serve no API; so the
// node a call lands on reads the seat's lease and relays the call over the
// broker to the holder. A node with no broker is the fleet by itself.
//
// EXCEPT AN INGRESS NODE WITH api.port 0: it is one of the nodes boxes dial and
// serves nothing, so it joins nothing and an executor launch is refused naming
// the setting ([mcpbridge.Bridge.Reachable]) rather than started into calls
// that all fail. A non-ingress node joins whatever its port, since its boxes
// reach it through the ingress nodes: a `-api-port 0` satellite runs seats.

// bridgeHolderWaitLeases is how many seat-lease lifetimes a box's call waits
// for a node that can answer it: one for a dead holder's lease to run out, one
// for a peer to claim the seat and reopen its running jobs' sessions; after
// that the call is answered that the bridge is unavailable.
const bridgeHolderWaitLeases = 2

// joinBridgeFleet joins this node's bridge to its fleet; it runs after the
// node starts, whose incarnation a seat's lease names.
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

// stopBridgeFleet withdraws this node from the bridge's relay before its seats
// are released and the broker closes, so a call arriving mid-teardown finds
// another holder rather than a closing session. It first releases every notes
// poll this node holds ([mcpbridge.Bridge.Shutdown]): one relayed from another
// node has no listener here to free it before its hold runs out, and the relay
// waits on it.
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
