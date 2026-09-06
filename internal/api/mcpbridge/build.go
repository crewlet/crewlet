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

// Build is THE construction path, called by the engine and by a standalone API
// alike.
//
// Both need one and for different halves: the engine OPENS a run's session and
// mints its endpoint, and whichever process is externally reachable VERIFIES
// the token. That is why the token is signed from shared key material rather
// than stored — see [runtoken.KeyFrom].
//
// An unset base URL builds NOTHING, and that is a real configuration rather
// than an error: most deployments run no agent mode. The route is then absent,
// and a seat that asks for agent mode is refused with the variable named —
// which is a better failure than a run that starts and cannot call a tool.
func Build(env func(string) string, keyMaterial []string) *Bridge {
	if env == nil {
		return nil
	}
	base := strings.TrimSpace(env(BaseURLVar))
	if base == "" {
		return nil
	}
	key := runtoken.KeyFrom(KeyDomain, keyMaterial)
	if len(key) == 0 {
		// A PER-PROCESS KEY CANNOT WORK ACROSS TWO. Logged rather than
		// refused, because a merged deployment — the default — is
		// perfectly correct with one, and refusing would take agent mode
		// away from the topology it works on.
		log.Warn("mcp_bridge_signing_key_ephemeral",
			"detail", "no Tier A secrets.keys, so bridge tokens are signed with "+
				"a per-process key — a split API cannot verify tokens the "+
				"engine minted. `crewlet secrets keygen` fixes it")
	}
	return New(Options{Key: key, BaseURL: base})
}
