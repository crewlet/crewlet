package executor

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A HOME'S KIMI MCP SERVERS CANNOT SWITCH CREWLET'S SERVER OFF UNSEEN. The
// run's entry replaces the home's of its name in ~/.kimi-code/mcp.json, so a
// home entry with `enabled: false`, a `disabledTools` naming a tool of the
// bridge's or an `enabledTools` leaving one out, by the tool's own name,
// refuses the run, naming the key; the entry enabled, a qualified name (which
// Kimi matches nothing by), another tool, another server's switch and a run
// without the bridge write the file. Mutations, each red: drop the check; skip
// disabledTools or enabledTools.
func TestAKimiHomeCannotSwitchCrewletsServerOffUnseen(t *testing.T) {
	t.Parallel()
	bridge := map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: "http://127.0.0.1:7/mcp",
		AlwaysLoaded: true, Tools: []string{"submit_work", "steer_take"}}}
	for name, tc := range map[string]struct {
		home    string
		servers map[string]sandbox.MCPServer
		refused string
	}{
		"switched off":             {`{"mcpServers": {"crewlet": {"url": "http://x", "enabled": false}}}`, bridge, "mcpServers.crewlet.enabled is false"},
		"a tool disabled":          {`{"mcpServers": {"crewlet": {"disabledTools": ["submit_work"]}}}`, bridge, "disabledTools names submit_work"},
		"a tool left out":          {`{"mcpServers": {"crewlet": {"enabledTools": ["submit_work"]}}}`, bridge, "enabledTools leaves steer_take out"},
		"a qualified name":         {`{"mcpServers": {"crewlet": {"disabledTools": ["mcp__crewlet__steer_take"]}}}`, bridge, ""},
		"enabled":                  {`{"mcpServers": {"crewlet": {"enabled": true, "disabledTools": ["other"]}}}`, bridge, ""},
		"another server off":       {`{"mcpServers": {"other": {"url": "http://y", "enabled": false}}}`, bridge, ""},
		"a run without the bridge": {`{"mcpServers": {"crewlet": {"enabled": false}}}`, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := kimiMCPOver(tc.home, tc.servers)
			if (err != nil) != (tc.refused != "") {
				t.Fatalf("refused %v, want %q", err, tc.refused)
			}
			if err != nil && !strings.Contains(err.Error(), tc.refused) {
				t.Errorf("the refusal does not say %q: %v", tc.refused, err)
			}
		})
	}
}
