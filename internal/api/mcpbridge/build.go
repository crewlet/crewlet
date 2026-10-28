package mcpbridge

import (
	"strings"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// KeyDomain separates the bridge's tokens from the telemetry receiver's.
//
// Without a domain, a token minted for one endpoint would validate at the
// other: both are HMACs over the same fleet key, and the subject is just a
// string. A telemetry token turning into a tool-call token is exactly the
// escalation the per-run credential exists to bound.
const KeyDomain = "crewlet.mcp.v1"

// BaseURLVar is where a sandbox reaches the fleet's bridge: any node that
// serves the route, a load balancer in front of several included. The node a
// call reaches relays it to the one holding the run's seat ([Bridge.Join]), so
// every node of a fleet is given the SAME value.
//
// The same shape as the telemetry receiver's own variable, and for the same
// reason: what a box can dial is a property of the deployment's network, not
// of the company document, so it is Tier A environment rather than Tier B
// config. An engine behind a load balancer, in a private network, or on a
// laptop all answer this differently with the same company running.
const BaseURLVar = "CREWLET_MCP_BRIDGE_URL"

// Build is THE construction path, called by the engine.
//
// The engine OPENS a run's session and mints its endpoint, and the node holding
// the run's seat serves it: a session is a live tool surface, so no other node
// can (see [Bridge.verdict]). The token is signed from the FLEET's key
// material, and that is what lets the session outlive the process that minted
// it: a node that restarted, or the peer that took the run's seat over, opens
// the job's session again and verifies the very token the box holds — and any
// node a call lands on can tell a token the fleet signed from a forged one
// before it relays the call to the holder. See [runtoken.Material]. A keyring
// that cannot sign for the fleet is refused ([runtoken.ErrNoKeyring]) rather
// than given a per-process key: there is no node without one, so reaching
// here with one is a mistake to name, not a posture to degrade into.
//
// hostURL is where a box on this node's own host reaches it — the node's own
// listener on loopback ([Options.HostURL]), derived by whoever bound that
// listener on a node that runs seats, "" anywhere else.
//
// NEITHER BUILDS NOTHING, and that is a real configuration rather than an
// error. The route is then absent, and every executor launch on this node is
// refused naming what is missing — which is a better failure than a run that
// starts and cannot call a tool. A node that runs no seat needs no bridge, so
// the absence is not an error here; the launch is where it is decided. One
// without the other builds a bridge that reaches the boxes it can: the host
// URL alone serves the `direct` cell of this node, and the variable alone
// every cell.
func Build(env func(string) string, material runtoken.Material, hostURL string) (*Bridge, error) {
	base := ""
	if env != nil {
		base = strings.TrimSpace(env(BaseURLVar))
	}
	hostURL = strings.TrimSpace(hostURL)
	if base == "" && hostURL == "" {
		return nil, nil
	}
	return New(Options{Material: material, BaseURL: base, HostURL: hostURL})
}
