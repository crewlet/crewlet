package executor

import (
	"encoding/json"
	"fmt"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// oneShotTake is the note helper's take config, which every hook of a run that
// runs the helper once per firing reads (internal/notehelper's takeConfig):
// the bridge's endpoint and headers, the tool a take calls, the skill route
// and, for a harness whose skill load is a read of the skill's file, the
// directory the run's skills are written in. A file, since a hook's
// environment is the harness's to scrub ([Paths.MuseTakeConfig],
// [Paths.GrokTakeConfig]).
type oneShotTake struct {
	Endpoint  string            `json:"endpoint,omitempty"`
	Headers   map[string]string `json:"headers,omitempty"`
	Tool      string            `json:"tool,omitempty"`
	Skills    string            `json:"skills,omitempty"`
	SkillsDir string            `json:"skillsDir,omitempty"`
}

// hookTakeConfig renders a run's take config for its wiring, skillsDir the
// directory its skills are read from where a read is their load ("" for a
// harness whose load is a tool of its own): refused where the notes name a
// server the run is not handed over HTTP, which would leave every take
// without an endpoint.
func hookTakeConfig(servers map[string]sandbox.MCPServer, steer sandbox.SteerWiring,
	skills sandbox.SkillWiring, skillsDir string) ([]byte, error) {

	var take oneShotTake
	if skills.Set() {
		take.Skills, take.SkillsDir = skills.URL, skillsDir
	}
	if steer.Set() {
		server, ok := servers[steer.Server]
		if !ok || server.Transport != sandbox.TransportHTTP {
			return nil, fmt.Errorf("executor: the run's notes name the %q server, which the run is not "+
				"handed over HTTP, so no take could reach it", steer.Server)
		}
		take.Endpoint, take.Headers, take.Tool = server.URL, server.Headers, steer.Tool
	}
	return json.Marshal(take)
}
