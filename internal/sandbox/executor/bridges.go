package executor

import (
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// bridgeServers is the run's servers its harness must serve as the engine
// hands them, sorted: the bridge's ([sandbox.MCPServer.AlwaysLoaded]), over
// HTTP as every launch hands it. A runner's guard holds every other server
// off these names (code-sandbox.md, each harness's section).
func bridgeServers(servers map[string]sandbox.MCPServer) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(servers)) {
		if s := servers[name]; s.AlwaysLoaded && s.Transport == sandbox.TransportHTTP {
			out = append(out, name)
		}
	}
	return out
}
