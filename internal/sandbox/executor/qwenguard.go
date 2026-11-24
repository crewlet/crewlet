package executor

import (
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A HOME OR A REPOSITORY CANNOT SWITCH CREWLET'S OWN SERVER OFF UNSEEN. Qwen
// Code 0.28.4 adds `mcp.excluded`, `mcp.allowed`, `permissions.deny`,
// `tools.exclude` and `tools.disabled` up across its settings files, and the
// run's system tier undid none of them (measured: an empty `mcp.excluded` and
// a `permissions.allow` of the bridge's), so the home's `~/.qwen/settings.json`,
// the working directory's `.qwen/settings.json` or the system defaults can
// take the bridge or its tools away, and the turn runs on without them. So the
// script's helper reads those files before Qwen Code starts and refuses the
// run naming the file, the key and the entry (qwen_helper.mjs's guard). What a
// tier does undo is set: `disableAllHooks: false` in the run's settings.

// qwenGuard is the guard's arguments for req, a flag and its value each: the
// run's own servers ([crewletServers]); "" for a run with none to guard.
func qwenGuard(req sandbox.RunRequest) string {
	var b strings.Builder
	for _, name := range crewletServers(req.MCPServers) {
		b.WriteString(" --server " + shellword.Quote(name))
	}
	return b.String()
}
