package mcpbridge

import (
	"strings"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// KeyDomain separates the bridge's tokens from the telemetry receiver's: both
// are HMACs over the same fleet key, so without it a telemetry token would
// validate as a tool-call token, the escalation the per-run credential bounds.
const KeyDomain = "crewlet.mcp.v1"

// BaseURLVar is where a sandbox reaches the fleet's bridge: any node serving
// the route, or a load balancer before several. The node a call reaches relays
// it to the seat's holder ([Bridge.Join]), so every node gets the SAME value
// (package doc, "One address for the fleet"). It is Tier A environment, as the
// receiver's is, because what a box can dial is the network's property, not
// the company's; the engine parses it with sandbox.ParseBoxURL, which refuses
// the boot on a value no box could dial, and hands [Build] the result.
const BaseURLVar = "CREWLET_MCP_BRIDGE_URL"

// Build is THE construction path, called by the engine. The token is signed
// from the FLEET's material ([runtoken.Material]), so the session outlives its
// minting process and any node can spot a forgery before relaying; a keyring
// that cannot sign for the fleet is refused ([runtoken.ErrNoKeyring]), never
// given a per-process key. base is [BaseURLVar] as sandbox.ParseBoxURL returned
// it (absolute http(s) with a host, no trailing '/'; the caller parses it so
// the bridge needs no sandbox dependency); hostURL is this node's loopback
// listener for a box on its host ([Options.HostURL]), "" elsewhere.
//
// NEITHER BUILDS NOTHING, a real configuration: the route is absent and every
// executor launch here is refused naming what is missing. hostURL alone serves
// this node's `direct` cell; base alone, every cell.
func Build(base string, material runtoken.Material, hostURL string) (*Bridge, error) {
	hostURL = strings.TrimSpace(hostURL)
	if base == "" && hostURL == "" {
		return nil, nil
	}
	return New(Options{Material: material, BaseURL: base, HostURL: hostURL})
}
