package executor

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A BOX CANNOT SWITCH A HERMES RUN'S OWN SERVER OR PLUGIN OFF UNSEEN. The
// run's overlay states every leaf of its own servers' entries it can
// ([hermesOwnLeaves]); the rest is judged before Hermes starts, by Hermes's
// own loader and gates under the run's managed scope (the carry script's
// `guard`), measured on 0.23.3: a sticky `active_profile`; HERMES_SAFE_MODE
// or HERMES_IGNORE_USER_CONFIG in the run's environment or a `.env`;
// `agent.disabled_toolsets` naming the server or `mcp-<server>`; a
// `platform_toolsets.cli` list leaving it out; a header beside the run's own
// ones; an `env` Hermes finds suspicious or a malformed `tool_costs` table;
// and the plugin gated off (`plugins.disabled`, or `plugins.enabled` leaving
// it out). Entries naming another server, toolset or plugin pass.

// hermesGuard is the part of a run's script that ends it before Hermes starts
// where the guard refuses it, saying why. A guard that cannot read the box's
// configuration is no verdict: Hermes's own load decides then, as it does on
// a newer release whose modules moved.
func hermesGuard(req sandbox.RunRequest) string {
	own := map[string][]string{}
	add := func(servers map[string]sandbox.MCPServer) {
		for _, name := range crewletServers(servers) {
			names := slices.Concat(own[name], slices.Collect(maps.Keys(servers[name].Headers)))
			slices.Sort(names)
			own[name] = slices.Compact(names)
		}
	}
	add(req.MCPServers)
	if f := req.FreshWiring; f != nil {
		add(f.MCPServers)
	}
	raw, _ := json.Marshal(own) // names and header names always marshal
	skills := []string{}
	for _, s := range req.Skills {
		skills = append(skills, s.Listed())
	}
	listed, _ := json.Marshal(skills) // names always marshal
	return fmt.Sprintf(`hg=$(hcarry guard %s %s %s); case "$hg" in 'crewlet-carry refused '*) printf 'crewlet: Hermes Agent would not start with the run'"'"'s own plugin and MCP servers as written: %%s. The run was refused before Hermes Agent started.\n' "${hg#crewlet-carry refused }" >&2; exit 1;; esac
`, hermesPluginName, shellword.Quote(string(raw)), shellword.Quote(string(listed)))
}
