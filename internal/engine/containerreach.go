package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// ContainerReachWarnings is the advisory a node gets when a container box on
// this host could not reach the engine at all: the company reaches the
// `container` cell, the listener a box dials binds loopback alone, and the
// host is Linux (goos, which callers pass as runtime.GOOS).
//
// THE TWO TIERS TOGETHER, which neither tier's own warnings can say: the bind
// is Tier A's and the cell is the company's. A container box dials the engine
// host by its runtime's name for it ([sandbox.EngineAddress]), and what that
// name reaches depends on the host. Docker Desktop and Podman's machine on
// macOS and Windows deliver it over the host's loopback, so a 127.0.0.1 bind
// answers. Docker Engine and Podman on Linux deliver it from the container's
// bridge network, which a 127.0.0.1 bind never hears: every bridged tool call,
// telemetry export and loopback MCP server would fail, each as a connection
// the box could not make, and nothing on the engine side would say why.
//
// A WARNING, not a refusal: a bind the operator fronts with their own proxy on
// the bridge address, or a host whose runtime forwards loopback after all
// (Docker Desktop for Linux), is a working configuration this cannot see.
//
// AT THE LISTENER A BOX DIALS: the public one when `api.public.port` is set,
// since the bridge and the receiver are served there alone, and `api` else. A
// node that binds no listener for them is told so by the launch that needs
// one, not here.
//
// ONLY ON A NODE WITH BOTH THE INGRESS AND THE SEATS ROLE, the one pair whose
// own bind a container box on this host dials. Ingress serves those routes to
// a box off the host at all: a node without it serves them on `api.port` to
// this host's loopback alone, for its own `direct` boxes (internal/api's
// Probes), and a container box launched on its host reaches the tool bridge
// through an ingress node's address (`CREWLET_MCP_BRIDGE_URL`), which this
// bind says nothing about. Seats is the role that launches a seat's box: a
// node without it starts none on this host, so every box dialling its routes
// comes from another machine, over the LAN or a proxy, and never through this
// host's runtime. Deep work is no reason to warn such a node either: on Linux
// a container deep-work run on a loopback bridge is held to loopback, and
// refused whatever the bind. A `direct` box is no concern of this either way:
// it shares the host's loopback, and dials the node's own listener there
// (cmd/crewlet's boxHostURL).
func ContainerReachWarnings(boot *config.Bootstrap, c *config.Company, goos string) []config.Warning {
	if boot == nil || c == nil || goos != "linux" {
		return nil
	}
	if profile := boot.Profile(""); !profile.RunsIngress() || !profile.RunsSeats() {
		return nil
	}
	if _, ok := c.SandboxPlacements()[config.PlacementContainer]; !ok {
		return nil
	}
	path, host := "api.host", boot.API.Host
	switch {
	case boot.API.Public.Port > 0:
		path, host = "api.public.host", boot.API.Public.Host
	case boot.API.Port <= 0:
		return nil
	}
	if !sandbox.LoopbackHost(host) {
		return nil
	}
	return []config.Warning{config.AdvisoryAt(path, fmt.Sprintf(
		"a seat's code work runs in a container on this Linux host, which reaches the "+
			"engine through the container runtime's bridge, and %s %q accepts connections "+
			"from this host's loopback alone, so a container box can reach neither its "+
			"tool bridge nor the telemetry receiver. Bind every interface (leave %s empty) "+
			"or the bridge's address (docker0's, 172.17.0.1 by default), and firewall the "+
			"port as you would any listener", path, host, path))}
}
