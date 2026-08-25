//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// processGroupAlive reports whether pid is a live process group that could be
// THIS box's.
//
// A pid on its own is not proof: pids are reused, and a recycled one pointing
// at some unrelated daemon would keep a dead box's directory alive forever. So
// the probe also requires the process to have started at or after the box was
// created — a box's own job cannot predate its box, and a recycled pid almost
// always belongs to something older.
//
// boxCreated is the box root's own mtime, which is the one thing that
// timestamp genuinely means: entries in the root are all made at Create and
// never again, so it is the box's birth time, not its last use.
//
// /proc/<pid>'s mtime is the process start time on Linux; where that is
// unavailable (any non-Linux unix) the pid probe stands alone, which fails
// towards KEEPING a directory rather than deleting live work.
func processGroupAlive(pid int, boxCreated time.Time) bool {
	if !procgroup.Exists(pid) {
		return false
	}
	info, err := os.Stat("/proc/" + strconv.Itoa(pid))
	if err != nil {
		return true
	}
	return !info.ModTime().Before(boxCreated.Add(-pidReuseGrace))
}

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
		return ExecResult{
			ExitCode: 124,
			Stdout:   stdout.String(),
			Stderr:   fmt.Sprintf("timed out after %s", timeout),
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
