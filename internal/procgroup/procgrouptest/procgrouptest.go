// Package procgrouptest is what a test needs to stand up a real process tree
// and watch it end.
//
// Several suites re-execute their own test binary as a stand-in server, CLI or
// launcher, then assert that the engine's teardown reached every process in
// the tree. Two things about that are easy to get wrong in a way that makes the
// assertion pass without testing anything, and both are platform-dependent, so
// they are written once here:
//
//   - A stand-in that is meant to stay up must actually stay up ([Hold]).
//   - "The process is gone" must not be read off kill(pid, 0), which reports a
//     zombie as alive, nor off /proc alone, which a darwin run does not have
//     ([AwaitGone]).
package procgrouptest

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// Hold blocks the calling goroutine, and with it a helper process that has
// nothing else to do, for as long as any test could still be watching it.
//
// NOT `select {}`, which is what these helpers used, and which only looks like
// the same thing. When every goroutine is blocked and no timer is pending, the
// Go runtime declares a deadlock and exits the process with status 2. The one
// build in which that check cannot fire is one that links runtime/cgo, whose
// extra thread counts as running: the Linux CI suite links it through package
// net's resolver, darwin never does, and neither does a Linux build with
// CGO_ENABLED=0. So a "mute server" or a "grandchild that outlives its parent"
// written with `select {}` died within milliseconds everywhere but CI, and the
// suites around it either failed for a reason unrelated to the code under test
// or, where they only asserted that something ended, passed without the
// process ever having run. A sleep keeps a timer pending, which the deadlock
// check honours on every platform and linkage.
//
// It returns after [HoldLimit]. A helper exists to be ended by the code under
// test, so one still running then belongs to a test that failed to end it, and
// holding on would leave a stray process on the developer's machine for good.
func Hold() {
	time.Sleep(HoldLimit)
}

// HoldLimit is how long [Hold] holds. It is the default -timeout of `go test`,
// the longest a test binary runs unless somebody asks for more, and orders of
// magnitude past the seconds any suite spends observing a helper.
const HoldLimit = 10 * time.Minute

// awaitPoll is how often [AwaitGone] reads the kernel's record. Short, because
// a signalled process usually goes on the first reading and the caller is a
// test waiting on it.
const awaitPoll = 20 * time.Millisecond

// Running reports whether pid is a process that can still run: it exists and
// has not exited. A zombie has exited and holds nothing but its table entry, so
// it is not running, which is the distinction kill(pid, 0) cannot draw.
func Running(pid int) (bool, error) {
	proc, found, err := procgroup.Inspect(pid)
	if err != nil {
		return false, err
	}
	return found && !proc.Zombie, nil
}

// AwaitGone waits up to within for pid to stop running and reports whether it
// did.
//
// A record the kernel will not give fails the test outright rather than
// counting either way: an assertion that something died must not pass on
// "unknown", and a helper that reported it as still running would bury the
// real error under a timeout.
func AwaitGone(t testing.TB, pid int, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		running, err := Running(pid)
		if err != nil {
			t.Fatalf("reading the kernel's record of pid %d: %v", pid, err)
		}
		if !running {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(awaitPoll)
	}
}
