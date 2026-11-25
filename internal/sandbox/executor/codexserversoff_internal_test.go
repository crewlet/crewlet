package executor

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A HOME'S CODEX CONFIGURATION CANNOT SWITCH CREWLET'S SERVER OFF UNSEEN. The
// run's entry replaces the home's of its name, stated enabled with no tool
// disabled, so a home entry with `enabled = false`, a `disabled_tools` naming
// a tool of the bridge's or an `enabled_tools` leaving one out refuses the run,
// naming the key; the entry enabled, another tool, another server's switch
// and a run without the bridge start. Mutations, each red: drop the enabled
// check; skip disabled_tools; skip enabled_tools; check every server.
func TestACodexHomeCannotSwitchCrewletsServerOffUnseen(t *testing.T) {
	t.Parallel()
	bridge := map[string]sandbox.MCPServer{"crewlet": {AlwaysLoaded: true, Tools: []string{"submit_work", "steer_take"}},
		"other": {}}
	for name, tc := range map[string]struct {
		home    string
		servers map[string]sandbox.MCPServer
		refused string
	}{
		"switched off":             {"[mcp_servers.crewlet]\nenabled = false\n", bridge, "mcp_servers.crewlet.enabled = false"},
		"a tool disabled":          {"[mcp_servers.crewlet]\ndisabled_tools = [\"submit_work\"]\n", bridge, "disabled_tools names submit_work"},
		"a tool left out":          {"[mcp_servers.crewlet]\nenabled_tools = [\"submit_work\"]\n", bridge, "enabled_tools leaves steer_take out"},
		"enabled":                  {"[mcp_servers.crewlet]\nenabled = true\nenabled_tools = [\"submit_work\", \"steer_take\"]\n", bridge, ""},
		"another tool disabled":    {"[mcp_servers.crewlet]\ndisabled_tools = [\"other_tool\"]\n", bridge, ""},
		"another server off":       {"[mcp_servers.other]\nenabled = false\n", bridge, ""},
		"a run without the bridge": {"[mcp_servers.crewlet]\nenabled = false\n", nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := codexServersOff(tc.home, tc.servers)
			if (err != nil) != (tc.refused != "") {
				t.Fatalf("refused %v, want %q", err, tc.refused)
			}
			if err != nil && !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("the refusal does not say %q: %v", tc.refused, err)
			}
		})
	}
}
