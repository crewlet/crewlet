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

// [Local] is POSIX alone: each guarantee it gives is a POSIX primitive —
// process groups and killpg to reach a harness's whole tree, SIGSTOP/SIGCONT
// for the clarification pause, the kernel's start time for the pid-reuse
// guard. No other platform builds the engine at all (internal/store's
// platform.go), so there is no fallback to refuse at run time.

// runHost runs one command on the engine host and captures it.
//
// The command gets its own session, as a launched job does, so a timeout kills
// the whole tree: a stuck child of `docker run` outlives a kill of the process
// the engine holds.
//
// ITS OWN TIMEOUT, or the caller's deadline, IS EXIT 124; A CALLER THAT
// CANCELLED IS AN ERROR wrapping that cancellation. The tree dies either way,
// but a cancelled command ran out of nothing: as 124 it would read as the box
// timing out, or an image the runtime would not start, for a launch this node
// cancelled.
func runHost(ctx context.Context, cmd hostCommand) (ExecResult, error) {
	if len(cmd.argv) == 0 {
		return ExecResult{}, errors.New("sandbox: runHost needs a command")
	}
	// A MISSING ENVIRONMENT IS NOT AN EMPTY ONE: os/exec hands a child whose
	// Env is nil the ENGINE's — its keyring, Tier A tokens and every
	// credential. Refused, so a caller that forgets leaks none of it.
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
		// Reap it, so the liveness probe does not read a zombie as alive.
		// Wait answers the kill's own exit, which the timeout reports;
		// anything else (the output copy failing) is logged, not lost.
		if err := exitOnly(<-done); err != nil {
			localLog.Warn("local_sandbox_wait_failed", "pgid", proc.Process.Pid, "error", err.Error())
		}
		// THE NOTICE, THEN WHAT IT SAID: stderr is where a stuck child
		// explains itself (a registry auth prompt, a stalled pull, a missing
		// image), and the timeout line alone leaves nothing to act on.
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
// os.ProcessState.ExitCode answers -1 for a signalled process, but ExitCode is
// read everywhere as an exit status: a setup shell killed by OOM would read
// "failed (exit -1)", a status no process exits with, naming no signal. 128+n
// matches how a shell reports its child (137 for SIGKILL) and how `docker
// exec` reports a container box's command.
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

// askEnd sends script, a pid a direct box's job recorded for itself, the end
// request — SIGUSR1 — only while it is a living member of the job's recorded
// group ([procgroup.Leader.SignalMember]).
func askEnd(job procgroup.Leader, script int) error {
	return job.SignalMember(script, syscall.SIGUSR1)
}
