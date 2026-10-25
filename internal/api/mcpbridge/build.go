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
//
// ONE GRAMMAR WITH THE RECEIVER'S, too: the engine reads it through
// sandbox.ParseBoxURL, which refuses the boot on a value that is set and that
// no box could dial, and hands [Build] what that returns.
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
// base is [BaseURLVar] as the caller PARSED it — an absolute http or https URL
// with a host, its trailing '/' trimmed — never the raw variable. Every
// endpoint this bridge mints is base plus a path, handed to a CLI inside a box
// that never reports back, so a base no client can dial fails every tool call
// of every agent-mode run, each rescued as incomplete with no sign of why;
// that is refused at boot, by the engine, before anything opens. The parse is
// the caller's rather than this package's because its grammar is the one every
// URL a box dials shares (sandbox.ParseBoxURL), and the API's bridge has no
// other reason to depend on the sandbox runtime.
//
// An unset base URL builds NOTHING, and that is a real configuration rather
// than an error: most deployments run no agent mode. The route is then absent,
// and a seat that asks for agent mode is refused with the variable named —
// which is a better failure than a run that starts and cannot call a tool.
func Build(base string, material runtoken.Material) (*Bridge, error) {
	if base == "" {
		return nil, nil
	}
	return New(Options{Material: material, BaseURL: base})
}

// Configured reports whether env names a bridge base URL: whether the engine
// parses one and [Build] builds a bridge at all.
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
