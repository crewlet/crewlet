package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// ContainerReachWarnings is the advisory for a Linux host (goos, as
// runtime.GOOS) whose container boxes cannot reach the engine: the company
// reaches the `container` cell and the listener a box dials binds loopback.
// Neither tier can say it alone (the bind is Tier A's, the cell the company's).
// On Linux a box reaches [sandbox.EngineAddress] from the bridge network, which
// a 127.0.0.1 bind never hears. A warning, not a refusal: a proxy on the bridge,
// or a runtime forwarding loopback (Docker Desktop for Linux), works unseen.
// It judges the public listener when `api.public.port` is set, else `api`
// (no listener at all is the launch's to report), and only on an
// ingress-and-seats node: elsewhere no box on this host dials it. A `direct`
// box shares the host's loopback (cmd/crewlet's boxHostURL), and container
// deep work on a loopback bridge is refused whatever the bind.
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
