package executor

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A BOX CANNOT SWITCH A HERMES RUN'S OWN SERVER OR PLUGIN OFF UNSEEN. The
// run's overlay states every leaf of its own servers' entries a home or /etc
// could switch them off with ([hermesOwnLeaves]). What it cannot state
// without overriding the box's own entries is judged before Hermes starts, by
// Hermes's own loader and gates under the run's managed scope (the carry
// script's `guard`), measured on 0.23.3: a sticky `active_profile`;
// HERMES_SAFE_MODE or HERMES_IGNORE_USER_CONFIG in the run's environment, the
// home's `.env` or the box's managed one; `agent.disabled_toolsets` naming the
// server or `mcp-<server>`; a `platform_toolsets.cli` list leaving it out; an
// entry Hermes skips (an `env` it finds suspicious, a malformed `tool_costs`
// table); and the plugin gated off (`plugins.disabled`, or a `plugins.enabled`
// leaving it out). Entries naming another server, toolset or plugin pass.

// hermesGuard is the part of a run's script that ends it before Hermes starts
// where the guard refuses it, saying why. A guard that cannot read the box's
// configuration is no verdict: Hermes's own load decides then, as it does on
// a newer release whose modules moved.
func hermesGuard(req sandbox.RunRequest) string {
	names := crewletServers(req.MCPServers)
	if f := req.FreshWiring; f != nil {
		names = append(names, crewletServers(f.MCPServers)...)
	}
	slices.Sort(names)
	names = slices.Compact(names)
	if names == nil {
		names = []string{}
	}
	raw, _ := json.Marshal(names) // a list of strings always marshals
	return fmt.Sprintf(`hg=$(hcarry guard %s %s); case "$hg" in 'crewlet-carry refused '*) printf 'crewlet: Hermes Agent would start without the run'"'"'s own plugin or MCP server: %%s. The run was refused before Hermes Agent started.\n' "${hg#crewlet-carry refused }" >&2; exit 1;; esac
`, hermesPluginName, shellword.Quote(string(raw)))
}
