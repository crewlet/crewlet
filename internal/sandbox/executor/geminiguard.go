package executor

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A HOME OR A REPOSITORY CANNOT SWITCH CREWLET'S OWN SERVER, HOOK, SIGN-IN OR
// ROUND CAP OFF UNSEEN. The run's settings stand in the home's own file
// ([Paths.GeminiHomeSettings]); Gemini CLI 0.70.0 ranks the working
// directory's and the machine's settings above it, whose server by the
// bridge's name replaces it and whose `security.auth.selectedType` the
// sign-in; `hooksConfig.enabled` read as false where it ranks highest, the
// home's included, switches every hook off, never overruled; and
// `mcp.excluded`, `tools.exclude` and `hooksConfig.disabled` add up across
// files, every `mcp.allowed` intersecting (all measured). So the script checks
// those files and each active home extension's `excludeTools` once the run's
// settings are in place, refusing the run naming file, key and entry
// (gemini_helper.mjs's guard); [geminiAdminPolicy] answers a policy's deny.

// geminiGuard is the guard's arguments for req, each a flag and its value: the
// run's own servers ([crewletServers]), its hook's name where it is hooked,
// its skills' listed names, each tool those servers list
// ([sandbox.MCPServer.Tools]) and the bridge's
// the run's own machinery calls by name, by their own names, which a
// tools.exclude entry hides as well as by its qualified one; its sign-in
// ([geminiAuth]) and its round cap where it sets them.
func geminiGuard(req sandbox.RunRequest) string {
	var b strings.Builder
	var tools []string
	for _, name := range crewletServers(req.MCPServers) {
		b.WriteString(" --server " + shellword.Quote(name))
		tools = append(tools, req.MCPServers[name].Tools...)
	}
	if f := req.FreshWiring; f != nil {
		for _, name := range crewletServers(f.MCPServers) {
			tools = append(tools, f.MCPServers[name].Tools...)
		}
	}
	if geminiHooked(req) {
		b.WriteString(" --hook " + shellword.Quote(geminiHookName))
	}
	for _, s := range req.Skills {
		b.WriteString(" --skill " + shellword.Quote(geminiSkillName(s)))
	}
	if req.Steer.Set() {
		tools = append(tools, req.Steer.Tool)
	}
	if req.SkillLoads.Set() {
		tools = append(tools, req.SkillLoads.Tool)
	}
	for _, tool := range slices.Compact(slices.Sorted(slices.Values(tools))) {
		b.WriteString(" --tool " + shellword.Quote(tool))
	}
	if auth := geminiAuth(req); auth != "" {
		b.WriteString(" --auth " + auth)
	}
	if req.Limits.MaxTurns > 0 {
		b.WriteString(" --turns " + strconv.Itoa(req.Limits.MaxTurns))
	}
	return b.String()
}

// geminiAdminPolicy is the run's admin-tier policy (`--admin-policy`,
// [Paths.GeminiAdminPolicy]), allowing every tool of each of servers: the
// admin tier outranks the home's policies, so a rule of theirs asking before
// the bridge's tools, which a headless run denies, gives way (measured on
// 0.70.0). Their deny it would override too, so the guard refuses that run
// instead (policyDenies in gemini_helper.mjs). Gemini CLI ignores it, saying
// so, where the machine's own system policies directory holds a policy.
func geminiAdminPolicy(servers []string) []byte {
	var b strings.Builder
	b.WriteString("# Written by Crewlet: the tools of the run's own servers, above any home or repository rule.\n")
	for _, name := range servers {
		fmt.Fprintf(&b, "[[rule]]\ntoolName = \"*\"\nmcpName = %s\ndecision = \"allow\"\npriority = 999\n",
			tomlString(name))
	}
	return []byte(b.String())
}
