//go:build !unix

package sandbox

import (
	"context"
	"time"
)

// The local backend is POSIX-only, and deliberately so: every containment
// property it offers is a POSIX primitive with no equivalent elsewhere —
// process groups and killpg for reaching a coding agent's whole tree,
// SIGSTOP/SIGCONT for the clarification pause, /proc start times for the
// pid-reuse guard. A partial port would offer the same interface with none of
// the guarantees, which is worse than not offering it: a box that cannot be
// paused would silently hold a run open, and one whose teardown reaches only
// the top process would leave the agent running.
//
// So this platform gets a build that compiles and a provider that refuses at
// construction, naming the reason.

const localSupported = false

const unsupportedReason = "the local sandbox backend needs POSIX process groups and " +
	"signals, which this platform does not provide — run the engine on Linux or macOS, " +
	"or configure a remote providers.sandbox backend"

func processGroupAlive(int, time.Time) bool { return false }

func runHost(context.Context, hostCommand) (ExecResult, error) {
	return ExecResult{}, localErrorf("%s", unsupportedReason)
}
