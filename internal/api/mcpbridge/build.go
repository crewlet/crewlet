package mcpbridge

import (
	"strings"

	"github.com/crewlet/crewlet/internal/secrets"
)

// KeyDomain separates the bridge's tokens from the telemetry receiver's.
//
// Without a domain, a token minted for one endpoint would validate at the
// other: both are signed under the same fleet keyring, and the subject is just
// a string. A telemetry token turning into a tool-call token is exactly the
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
// [Bridge.session]). The token is still signed under the fleet's keyring
// rather than a per-process key, and the reason is diagnosis rather than
// verification: a peer that a misrouted call reaches (a load balancer in front
// of several nodes) can then tell a token the fleet signed from a forged one,
// and log that the bridge URL addresses the wrong node rather than that the
// route is under attack. The WHOLE ring, so that diagnosis survives a rolling
// rotation: the active key mints and every key verifies. See internal/runtoken
// and [Bridge.resolve].
//
// An unset base URL builds NOTHING, and that is a real configuration rather
// than an error: most deployments run no agent mode. The route is then absent,
// and a seat that asks for agent mode is refused with the variable named —
// which is a better failure than a run that starts and cannot call a tool.
func Build(env func(string) string, keys secrets.Keyring) (*Bridge, error) {
	if env == nil {
		return nil, nil
	}
	base := strings.TrimSpace(env(BaseURLVar))
	if base == "" {
		return nil, nil
	}
	if len(keys.Keys) == 0 {
		// INFO, NOT A WARNING: a per-process key serves every session this
		// node opens, on one node or a fleet, because only the opening node
		// ever verifies a token. What it loses is the peer's diagnosis
		// above, which is worth a line an operator can find and not an
		// alarm on a configuration that works.
		log.Info("mcp_bridge_signing_key_ephemeral",
			"detail", "no Tier A secrets.keys, so bridge tokens are signed with "+
				"a per-process key: a peer that a misrouted bridge call reaches "+
				"cannot tell it from a forged token. `crewlet secrets keygen` "+
				"gives every node the same keyring")
	}
	return New(Options{Keys: keys, BaseURL: base})
}
