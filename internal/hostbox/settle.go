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
// # A FRESHLY WRITTEN EXECUTABLE CAN BE ETXTBSY
//
// Linux refuses to exec a file that ANY process holds open for writing, and
// every goroutine that forked while os.WriteFile had the file open holds it
// until that child execs: Go opens with O_CLOEXEC, so the window is one
// fork/exec wide — microseconds, and wide enough to hit in a process that forks
// constantly. Measured in internal/sandbox's suite: 17 failures in 200 execs
// of a just-written script with eight goroutines forking beside it. The
// symptom is never an error anybody can read — a git that "failed to start", a
// runtime detected as absent — so the wait has to happen where the file is
// written, once.
//
// It runs path with args — a PROBE the script answers by exiting at once, so
// its body never runs — until an exec STARTS, and returns nil once one has,
// whatever the probe exited with: nothing reopens the file for writing
// afterwards, so every later exec starts too. Only ETXTBSY is waited on. Any
// other failure to start — no such file, no exec bit, no interpreter line, an
// interpreter that is not there — is a file that will NEVER run, and is
// returned at once naming path: reported as settled, it would surface at its
// first real use as somebody else's error, which is the very symptom this
// exists to prevent. A script with no probe of its own can be settled with no
// args, at the cost of running its body once.
//
// The probe runs with the PASSTHROUGH ALLOWLIST ([Inherit]) as its whole
// environment, never the engine's own: that holds the org's chat token, the
// store's DSN and the API keys, and a body run by a probe it did not answer
// would read every one of them. PATH and the locale are all a `#!/bin/sh`
// probe needs.
//
// Attempt n sleeps n milliseconds before the next, a hundred attempts: about
// FIVE SECONDS in all (1 + 2 + … + 100 ms) against a window measured in
// microseconds, so the last attempts are spent only on a file something
// really is holding open, which [ErrStillBusy] then names. The context's error
// when it ends first.
//
// ONE IMPLEMENTATION, because two callers need it — the code host's hook
// script and every stand-in git a test writes — and a platform rule written
// twice is the one that drifts. The sandbox suite closes the same window from
// the other side: it writes its stubs under syscall.ForkLock, so no fork of
// its process inherits one, which a writer can do only where it controls
// every fork beside it.
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
