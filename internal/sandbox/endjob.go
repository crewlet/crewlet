package sandbox

import "time"

// JobStopGrace is how long one process of a run's job is given between
// SIGTERM and SIGKILL: a direct box's job group at its teardown, and a process
// a run's script stops itself — its harness, an OpenCode run's server and its
// login exporter (internal/sandbox/executor).
//
// ONE FIGURE, because it is one decision: how long a process told to go is
// given to go before it is made to. Five seconds is the grace the engine host
// gives a harness it terminates (internal/providers/llm/harness's termGrace),
// enough for a Node runtime — Claude Code is one, and so is OpenCode's — to run
// its exit handlers and close its sockets. Both go well inside it: Claude Code
// 2.1.310, told to stop mid-tool with a stdio MCP server connected, measured
// going in under a fifth of a second, its tool's processes and its MCP server
// with it; and OpenCode 2.0.32's server, the slowest process a run's script
// stops, in a tenth of a second, and in three with a stdio MCP server still
// connecting, which it stops itself. A process past it is wedged rather than
// slow, and a run's done marker waits on its script: a process that ignored
// SIGTERM would otherwise hold the run open for ever.
const JobStopGrace = 5 * time.Second
