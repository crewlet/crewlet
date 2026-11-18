package executor

import "time"

// jsTimerCeiling is the longest delay, in milliseconds, a JavaScript timer
// holds (a signed 32-bit count, almost 25 days). Both harnesses time an MCP call
// with one: Claude Code clamps its tool timeout to it, OpenCode hands
// `timeout.execution` to its timer as is (2.1.310's and 2.1.4's bundles), so a
// longer wait is written as the ceiling, which still outlasts any run.
const jsTimerCeiling = 1<<31 - 1

// waitMillis is a server's call wait ([sandbox.MCPServer.CallTimeout]) as a
// harness's MCP settings take it, in milliseconds: zero where the engine set
// none, which renders nothing and leaves the harness its own.
func waitMillis(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return min(d.Milliseconds(), jsTimerCeiling)
}
