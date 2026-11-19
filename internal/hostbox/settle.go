package hostbox

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"sort"
	"syscall"
	"time"
)

// ErrStillBusy reports a freshly written executable that stayed ETXTBSY for
// every attempt [Settle] made: something holds it open for writing, which is a
// fault to report, never a wait to extend.
var ErrStillBusy = errors.New("hostbox: a freshly written executable stayed busy: something holds it open for writing")

// settleAttempts is how many execs [Settle] tries. A variable so the suite can
// reach the refusal without spending the five seconds; nothing else sets it.
var settleAttempts = 100

// Settle waits until the executable just written at path can be exec'd.
//
// A FRESH EXECUTABLE CAN BE ETXTBSY: Linux refuses to exec a file any process
// holds open for writing, and a goroutine that forked during os.WriteFile
// holds it until its child execs: microseconds, an unreadable "failed to
// start" in a process that forks constantly. It runs path with args (a probe
// that exits at once) until an exec STARTS, whatever the exit. Only ETXTBSY is
// retried; any other failure is a file that will NEVER run and is returned at once. Each probe sees only [Inherit]'s
// allowlist, never the engine's secrets. Attempt n sleeps n ms (about FIVE
// SECONDS in all); [ErrStillBusy] names a file still held. One implementation
// because two callers need it (Gitlet's hook, test stand-in gits).
func Settle(ctx context.Context, path string, args ...string) error {
	env := probeEnv()
	for attempt := 1; attempt <= settleAttempts; attempt++ {
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Env = env
		err := cmd.Run()
		// A context that ended is why an exec failed or was killed, never a
		// sign that the file settled or that it cannot run (CommandContext
		// refuses to start under a done one).
		if ctx.Err() != nil {
			return fmt.Errorf("hostbox: waiting for %s to become executable: %w", path, context.Cause(ctx))
		}
		var exit *exec.ExitError
		switch {
		case err == nil, errors.As(err, &exit):
			// The exec started: whatever the probe answered, the file runs.
			return nil
		case !errors.Is(err, syscall.ETXTBSY):
			return fmt.Errorf("hostbox: %s cannot be executed: %w", path, err)
		}
		timer := time.NewTimer(time.Duration(attempt) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("hostbox: waiting for %s to become executable: %w", path, context.Cause(ctx))
		case <-timer.C:
		}
	}
	return fmt.Errorf("%w: %s", ErrStillBusy, path)
}

// probeEnv is the passthrough allowlist as NAME=value entries, sorted so a
// probe's environment is the same on every call — and never nil, because a
// nil exec.Cmd.Env hands the child the engine's whole environment.
func probeEnv() []string {
	inherited := Inherit()
	env := make([]string, 0, len(inherited))
	for name, value := range inherited {
		env = append(env, name+"="+value)
	}
	sort.Strings(env)
	return env
}
