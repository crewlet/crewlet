package executor

import "time"

// jsTimerCeiling is the longest delay, in milliseconds, a JavaScript timer
// holds: 2147483647, a signed 32-bit count. Both harnesses that run in a box
// are JavaScript programs that time a call to an MCP server with one. Claude
// Code clamps the tool timeout it reads to it, and OpenCode hands
// `timeout.execution` to its timer as it is (by reading 2.1.310's and 2.1.4's
// bundles), so a wait past it is written as the ceiling, which still outlasts
// any run: almost twenty-five days.
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
