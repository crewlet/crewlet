package executor

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A HOME OR A REPOSITORY CANNOT SWITCH CREWLET'S OWN SERVER OR HOOK OFF
// UNSEEN. Gemini CLI 0.70.0 adds `mcp.excluded`, `tools.exclude` and
// `hooksConfig.disabled` up across its settings files and intersects every
// `mcp.allowed` (measured: an empty or wider list in the run's system tier
// undid none of them), so the home's `~/.gemini/settings.json`, the working
// directory's `.gemini/settings.json` or the system defaults can switch the
// bridge or the run's hook off, and the stream says nothing of it. So the
// script checks those files before Gemini CLI first starts and refuses the
// run naming the file (gemini_helper.mjs's guard). What a tier does undo is
// set: `hooksConfig.enabled` in the run's settings, and a policy denying the
// bridge's tools gives way to the run's admin-tier one ([geminiAdminPolicy]).

// geminiGuard is the guard's arguments for req, each a flag and its value: the
// run's own servers ([crewletServers]), its hook's name where it is hooked,
// and the tools of the bridge's the run's own machinery calls by name.
func geminiGuard(req sandbox.RunRequest) string {
	var b strings.Builder
	for _, name := range crewletServers(req) {
		b.WriteString(" --server " + shellword.Quote(name))
	}
	if geminiHooked(req) {
		b.WriteString(" --hook " + shellword.Quote(geminiHookName))
	}
	var tools []string
	if req.Steer.Set() {
		tools = append(tools, req.Steer.Tool)
	}
	if req.SkillLoads.Set() {
		tools = append(tools, req.SkillLoads.Tool)
	}
	for _, tool := range slices.Compact(slices.Sorted(slices.Values(tools))) {
		b.WriteString(" --tool " + shellword.Quote(tool))
	}
	return b.String()
}

// geminiAdminPolicy is the run's admin-tier policy (`--admin-policy`,
// [Paths.GeminiAdminPolicy]), allowing every tool of each of servers: the
// admin tier outranks the home's and the working directory's policies, so a
// rule of theirs denying the bridge's tools gives way (measured on 0.70.0).
// Gemini CLI ignores it, saying so, where the machine's own system policies
// directory holds a policy, which is that machine's operator's to set.
func geminiAdminPolicy(servers []string) []byte {
	var b strings.Builder
	b.WriteString("# Written by Crewlet: the tools of the run's own servers, above any home or repository rule.\n")
	for _, name := range servers {
		fmt.Fprintf(&b, "[[rule]]\ntoolName = \"*\"\nmcpName = %s\ndecision = \"allow\"\npriority = 999\n",
			tomlString(name))
	}
	return []byte(b.String())
}
