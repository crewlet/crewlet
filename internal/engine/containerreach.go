package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/seat/placement"
)

// ContainerReachWarnings is the advisory for a Linux host (goos, as
// runtime.GOOS) whose container boxes cannot reach the engine: one starts on
// this node — a seat's, where it runs seats and a seat's box is placed in the
// `container` cell, or a check's, where it serves Gitlet and the company runs
// checks — and the listener a box dials binds loopback. Neither tier can say
// it alone (the bind is Tier A's, the cells the company's). On Linux a box
// reaches [sandbox.EngineAddress] from the bridge network, which a 127.0.0.1
// bind never hears. A warning, not a refusal: a proxy on the bridge, or a
// runtime forwarding loopback (Docker Desktop for Linux), works unseen. It
// judges the public listener when `api.public.port` is set, else `api` (no
// listener at all is the launch's to report), on an ingress node alone, the
// one serving what a box dials. A `direct` box shares the host's loopback.
func ContainerReachWarnings(boot *config.Bootstrap, c *config.Company, goos string) []config.Warning {
	if boot == nil || c == nil || goos != "linux" {
		return nil
	}
	profile := boot.Profile("")
	_, contained := c.SeatPlacements()[config.PlacementContainer]
	seats := profile.RunsSeats() && contained
	checks := profile.Roles.Has(placement.RoleGitlet) && c.ChecksRefusal() == ""
	if !profile.RunsIngress() || (!seats && !checks) {
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
		"a seat's code work or a check runs in a container on this Linux host, which reaches the "+
			"engine through the container runtime's bridge, and %s %q accepts connections "+
			"from this host's loopback alone, so a container box can reach neither its "+
			"tool bridge, the telemetry receiver nor Gitlet. Bind every interface (leave %s empty) "+
			"or the bridge's address (docker0's, 172.17.0.1 by default), and firewall the "+
			"port as you would any listener", path, host, path))}
}
