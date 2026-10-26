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

// BaseURLVar is where a sandbox reaches this engine's bridge.
//
// The SAME shape as the telemetry receiver's own variable, and for the same
// reason: what a box can dial is a property of the deployment's network, not
// of the company document, so it is Tier A environment rather than Tier B
// config. An engine behind a load balancer, in a private network, or on a
// laptop all answer this differently with the same company running.
const BaseURLVar = "CREWLET_MCP_BRIDGE_URL"

// Build is THE construction path, called by the engine.
//
// The engine OPENS a run's session and mints its endpoint, and the same process
// serves it: a session is a live tool surface, so no other process can (see
// [Bridge.session]). The token is still signed from the fleet's key material,
// and the reason is diagnosis rather than verification: a peer that a
// misrouted call reaches (a load balancer in front of several nodes) can then
// tell a token the fleet signed from a forged one, and log that the bridge URL
// addresses the wrong node rather than that the route is under attack. See
// [runtoken.Material] and [Bridge.resolve]. A keyring that cannot sign for the
// fleet is refused ([runtoken.ErrNoKeyring]) rather than given a per-process
// key: there is no node without one, so reaching here with one is a mistake to
// name, not a posture to degrade into.
//
// hostURL is where a box on this node's own host reaches it — the node's own
// listener on loopback ([Options.HostURL]), derived by whoever bound that
// listener, "" where the node binds none.
//
// NEITHER BUILDS NOTHING. The route is then absent, and every executor launch
// on this node is refused naming what is missing — which is a better failure
// than a run that starts and cannot call a tool. A node that runs no seat
// needs no bridge, so the absence is not an error here; the launch is where it
// is decided. One without the other builds a bridge that reaches the boxes it
// can: the host URL alone serves the `direct` cell, and the variable alone
// every cell.
func Build(env func(string) string, material runtoken.Material, hostURL string) (*Bridge, error) {
	hostURL = strings.TrimSpace(hostURL)
	if !Configured(env) && hostURL == "" {
		return nil, nil
	}
	var base string
	if Configured(env) {
		base = strings.TrimSpace(env(BaseURLVar))
	}
	return New(Options{Material: material, BaseURL: base, HostURL: hostURL})
}

// Configured reports whether env names a bridge base URL — the address every
// cell's boxes can dial, and the only one a container's or a remote box can.
//
// ITS OWN FUNCTION because a node asks it before there is an engine to build
// one: `crewlet run` binds the listener a seats node serves its bridge on the
// moment the process can, which is long before the engine reads the variable,
// and a second spelling of "is it set" there would be a node binding a
// listener for a bridge its engine never builds, or building one with no
// listener bound for it.
func Configured(env func(string) string) bool {
	return env != nil && strings.TrimSpace(env(BaseURLVar)) != ""
}
