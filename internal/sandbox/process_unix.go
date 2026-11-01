//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/procgroup"
)

// localSupported gates [NewLocal]: this platform has the primitives.
const localSupported = true

// unsupportedReason is empty where the backend is supported.
const unsupportedReason = ""

// runHost runs one command on the engine host and captures it.
//
// The command gets its own session for the same reason a launched job does: on
// a timeout the whole tree is killed, not just the process the engine holds.
// `docker run` spawning a stuck child is precisely the case — killing the CLI
// alone would leave it behind.
//
// ITS OWN TIMEOUT IS AN EXIT, 124, and A CALLER THAT CANCELLED IS AN ERROR
// carrying that cancellation: the tree is killed either way, but a command
// the caller gave up on did not run out of anything, and answered as 124 it
// was told as the box timing out, or as a container image the runtime would
// not start, about a launch this node had cancelled. A deadline of the
// caller's is the command not finishing in the time it had, so it is the 124
// too.
func runHost(ctx context.Context, cmd hostCommand) (ExecResult, error) {
	if len(cmd.argv) == 0 {
		return ExecResult{}, errors.New("sandbox: runHost needs a command")
	}
	// A MISSING ENVIRONMENT IS NOT AN EMPTY ONE: os/exec hands a child whose
	// Env is nil the ENGINE's — its keyring, its Tier A tokens and every
	// credential it was started with. Every container-runtime call here once
	// reached the child that way; refused, the next one to forget cannot.
	if cmd.env == nil {
		return ExecResult{}, fmt.Errorf("sandbox: %s was about to run with no "+
			"environment of its own, which os/exec would fill with the engine's; "+
			"it was not started", cmd.argv[0])
	}
	timeout := cmd.timeout
	if timeout <= 0 {
		timeout = ControlTimeout
	}
	// Not exec.CommandContext: its Cancel kills the immediate child only,
	// and the whole reason for the session is that the tree outlives it.
	caller := ctx
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	proc := exec.Command(cmd.argv[0], cmd.argv[1:]...) //nolint:noctx // the timeout is enforced below, on the GROUP
	proc.Dir = cmd.cwd
	proc.Env = hostbox.Environ(cmd.env)
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
		code, said := hostExit(proc.ProcessState, stderr.String())
		return ExecResult{
			ExitCode: code,
			Stdout:   stdout.String(),
			Stderr:   said,
		}, exitOnly(err)
	case <-ctx.Done():
		logSignal("kill", proc.Process.Pid, procgroup.Kill(proc.Process.Pid))
		// Reap it, so the pid does not linger as a zombie that the liveness
		// probe would read as alive. Wait answers the kill's own exit here,
		// which the timeout below already reports; anything else — the
		// output copy failing — is not the timeout, and is logged rather
		// than lost behind it.
		if err := exitOnly(<-done); err != nil {
			localLog.Warn("local_sandbox_wait_failed", "pgid", proc.Process.Pid, "error", err.Error())
		}
		// THE NOTICE, THEN WHAT IT SAID. Replacing the captured stderr with
		// the timeout line threw away the only account of what the command
		// was doing when it hung — a registry auth prompt, a pull stalling,
		// an image that does not exist — and left the operator "timed out
		// after 30s" and nothing to act on. stdout was kept on this path all
		// along; stderr is where a stuck child explains itself.
		if cancelledBy(caller) {
			return ExecResult{Stdout: stdout.String(), Stderr: stderr.String()},
				fmt.Errorf("sandbox: %s was stopped by its caller: %w", cmd.argv[0], caller.Err())
		}
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

// hostExit is the status a finished host process reports, and what it said on
// stderr, with a SIGNAL read the way a shell reads one: 128 plus its number,
// and the signal named after whatever the process printed.
//
// os.ProcessState.ExitCode answers -1 for a process a signal ended, and an
// ExecResult's ExitCode is read everywhere as an exit status: a setup command
// whose own shell was killed — an OOM kill, or a command that `exec`s into a
// program that dies of a signal — read "failed (exit -1)", a status no process
// can exit with, with nothing saying a signal ended it. A child the shell
// waits on already reads as the shell reports it (137 for a SIGKILL), and so
// does a container box's command, which `docker exec` reports the same way.
func hostExit(state *os.ProcessState, stderr string) (int, string) {
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return state.ExitCode(), stderr
	}
	signal := status.Signal()
	note := fmt.Sprintf("killed by signal %d (%s)", int(signal), signal)
	if said := strings.TrimRight(stderr, "\n"); said != "" {
		note = said + "\n" + note
	}
	return 128 + int(signal), note
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
