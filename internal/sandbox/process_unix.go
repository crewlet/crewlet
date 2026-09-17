//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// localSupported gates [NewLocal]: this platform has the primitives.
const localSupported = true

// unsupportedReason is empty where the backend is supported.
const unsupportedReason = ""

// runHost runs one command on the engine host and captures it.
//
// The command gets its own session for the same reason a coding job does: on
// a timeout the whole tree is killed, not just the process the engine holds.
// `docker run` spawning a stuck child is precisely the case — killing the CLI
// alone would leave it behind.
func runHost(ctx context.Context, cmd hostCommand) (ExecResult, error) {
	if len(cmd.argv) == 0 {
		return ExecResult{}, errors.New("sandbox: runHost needs a command")
	}
	timeout := cmd.timeout
	if timeout <= 0 {
		timeout = controlTimeout
	}
	// Not exec.CommandContext: its Cancel kills the immediate child only,
	// and the whole reason for the session is that the tree outlives it.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	proc := exec.Command(cmd.argv[0], cmd.argv[1:]...) //nolint:noctx // the timeout is enforced below, on the GROUP
	proc.Dir = cmd.cwd
	proc.Env = flattenEnv(cmd.env)
	procgroup.Detach(proc)

	var stdout, stderr capture
	proc.Stdout = &stdout
	proc.Stderr = &stderr

	if err := proc.Start(); err != nil {
		return ExecResult{}, fmt.Errorf("sandbox: starting %q: %w", cmd.argv[0], err)
	}
	done := make(chan error, 1)
	go func() { done <- proc.Wait() }()

	select {
	case err := <-done:
		return ExecResult{
			ExitCode: proc.ProcessState.ExitCode(),
			Stdout:   stdout.String(),
			Stderr:   stderr.String(),
		}, exitOnly(err)
	case <-ctx.Done():
		logSignal("kill", proc.Process.Pid, procgroup.Kill(proc.Process.Pid))
		// Reap it, so the pid does not linger as a zombie that the liveness
		// probe would read as alive.
		<-done
		// THE NOTICE, THEN WHAT IT SAID. Replacing the captured stderr with
		// the timeout line threw away the only account of what the command
		// was doing when it hung — a registry auth prompt, a pull stalling,
		// an image that does not exist — and left the operator "timed out
		// after 30s" and nothing to act on. stdout was kept on this path all
		// along; stderr is where a stuck child explains itself.
		detail := fmt.Sprintf("timed out after %s", timeout)
		if said := strings.TrimSpace(stderr.String()); said != "" {
			detail += "\n" + said
		}
		return ExecResult{
			ExitCode: 124,
			Stdout:   stdout.String(),
			Stderr:   detail,
		}, nil
	}
}

// exitOnly swallows a non-zero exit, which every caller reads off ExitCode,
// and surfaces only a genuine failure to run the command.
func exitOnly(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return nil
	}
	return err
}
