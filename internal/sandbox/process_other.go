//go:build !unix

package sandbox

import (
	"context"
	"errors"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// The local backend is POSIX-only on purpose: each guarantee it gives is a
// POSIX primitive — process groups and killpg to reach a harness's whole tree,
// SIGSTOP/SIGCONT for the clarification pause, the kernel's start time for the
// pid-reuse guard. A partial port would be worse than none: an unpausable box
// would silently hold a run open, and a teardown reaching only the top process
// would leave the agent running. So this platform compiles, and its provider
// refuses at construction, naming the reason.

const localSupported = false

const unsupportedReason = "the local sandbox backend needs POSIX process groups and " +
	"signals, which this platform does not provide — run the engine on Linux or macOS, " +
	"or configure a remote providers.sandbox backend"

func runHost(context.Context, hostCommand) (ExecResult, error) {
	return ExecResult{}, localErrorf("%s", unsupportedReason)
}

// askEnd has no signal to send on a platform without them.
func askEnd(procgroup.Leader, int) error { return errors.ErrUnsupported }
