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
// overlay states its servers' leaves ([hermesOwnLeaves]); Hermes's own loader
// and gates judge the rest before it starts (the carry script's `guard`,
// measured on 0.23.3): a sticky `active_profile`; HERMES_SAFE_MODE or
// HERMES_IGNORE_USER_CONFIG set; a disabled toolset or a cli toolset list
// leaving the server out; a header beside the run's own; an `env` Hermes
// finds suspicious or a malformed `tool_costs`; the plugin gated off, which
// holds the run's record, notes and skill loads; a box file's own switch the
// overlay would state back (`enabled: false`, `disabled: true`, a `tools`
// list hiding its tools). What only cuts the nested hand short refuses
// nothing ([hermesGateCommand]). Entries naming another server, toolset or
// plugin pass.

// hermesGuarded is the servers the guard judges: Crewlet's own, the run's and
// a fresh session's ([crewletServers]), sorted.
func hermesGuarded(req sandbox.RunRequest) []string {
	names := crewletServers(req.MCPServers)
	if f := req.FreshWiring; f != nil {
		names = append(names, crewletServers(f.MCPServers)...)
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// hermesGuard is the part of a run's script that ends it before Hermes starts
// where the guard refuses it, saying why. A guard that cannot read the box's
// configuration is no verdict: Hermes's own load decides then, as it does on
// a newer release whose modules moved.
func hermesGuard(req sandbox.RunRequest) string {
	type entry struct {
		Headers []string `json:"headers"`
		Tools   []string `json:"tools"`
	}
	own := map[string]entry{}
	add := func(servers map[string]sandbox.MCPServer) {
		for _, name := range crewletServers(servers) {
			e := own[name]
			e.Headers = slices.Compact(slices.Sorted(slices.Values(slices.Concat(e.Headers,
				slices.Collect(maps.Keys(servers[name].Headers))))))
			e.Tools = slices.Compact(slices.Sorted(slices.Values(slices.Concat(e.Tools, servers[name].Tools))))
			own[name] = e
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
