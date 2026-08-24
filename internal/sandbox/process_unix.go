//go:build unix

package sandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// detachAttr makes a spawned process a session and process-group leader.
//
// Three properties come from that one flag, and only a direct spawn gives all
// three — which is why the direct box spawns the coding job itself instead of
// backgrounding it inside a throwaway shell:
//
//   - The job leads its own process group, so pause and teardown signal the
//     WHOLE tree with one killpg. A job backgrounded inside a shell inherits
//     that shell's group and is reachable only individually, leaving the
//     coding agent's own children running.
//   - It is detached from the engine's controlling terminal, so a Ctrl-C in
//     an operator's shell does not reach into a running coding job.
//   - If the engine dies the job is reparented to init and keeps going, which
//     is what makes the detached lifecycle survive a restart at all.
func detachAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }

// signalGroup sends sig to the whole process group led by pid.
//
// Negating the pid is what makes it the GROUP: kill(-pgid) reaches every
// process the coding agent spawned, and the coding agents this drives all
// spawn children.
func signalGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return errors.New("sandbox: refusing to signal a non-positive pid")
	}
	return syscall.Kill(-pid, sig)
}

func killGroup(pid int)     { signalGroup(pid, syscall.SIGKILL) }
func termGroup(pid int)     { signalGroup(pid, syscall.SIGTERM) }
func stopGroup(pid int)     { signalGroup(pid, syscall.SIGSTOP) }
func continueGroup(pid int) { signalGroup(pid, syscall.SIGCONT) }

// groupExists probes the group for liveness without touching it.
func groupExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := signalGroup(pid, 0)
	// EPERM means the group exists and belongs to somebody else — which,
	// under a pid that was recycled, is exactly the case the start-time
	// guard above this exists to reject. Alive is the honest answer here;
	// processGroupAlive is where "and it could be ours" is decided.
	return err == nil || errors.Is(err, syscall.EPERM)
}

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
	if !groupExists(pid) {
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

	proc := exec.Command(cmd.argv[0], cmd.argv[1:]...)
	proc.Dir = cmd.cwd
	proc.Env = flattenEnv(cmd.env)
	proc.SysProcAttr = detachAttr()

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
		killGroup(proc.Process.Pid)
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
