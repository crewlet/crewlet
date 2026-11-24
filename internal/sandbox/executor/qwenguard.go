package executor

import (
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A HOME OR A REPOSITORY CANNOT SWITCH CREWLET'S OWN SERVER OR HOOK OFF
// UNSEEN. Qwen Code 0.28.4 adds `mcp.excluded`, `mcp.allowed`,
// `permissions.deny`, `tools.exclude` and `tools.disabled` up across its
// settings files, which the run's system tier does not undo, and its
// `disableAllHooks` and safe mode (QWEN_CODE_SAFE_MODE, from the environment,
// a `.env` or a settings `env`) switch every hook off, the skill hook among
// them, which the run never overrules (all measured). So the script's helper
// reads them before Qwen Code starts and refuses the run naming the file, the
// key and the entry (qwen_helper.mjs's guard).

// qwenGuard is the guard's arguments for req, a flag and its value each: the
// run's own servers ([crewletServers]) and, for a run handed skills, the hook
// that reports their loads; "" for a run with none to guard.
func qwenGuard(req sandbox.RunRequest) string {
	var b strings.Builder
	for _, name := range crewletServers(req.MCPServers) {
		b.WriteString(" --server " + shellword.Quote(name))
	}
	if len(req.Skills) > 0 {
		b.WriteString(" --hook " + shellword.Quote(qwenHookName))
	}
	return b.String()
}

// qwenHookName is how the guard names the run's skill hook: the event and the
// tool it fires after ([qwenSettings]).
const qwenHookName = "PostToolUse(skill)"
