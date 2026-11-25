// Package procgrouptest is what a test needs to stand up a real process tree
// and watch it end.
//
// Several suites re-execute their own test binary as a stand-in server, harness or
// launcher, then assert that the engine's teardown reached every process in
// the tree. Three things about that are easy to get wrong in a way that makes
// the assertion pass without testing anything, and all are platform-dependent,
// so they are written once here:
//
//   - A stand-in that is meant to stay up must actually stay up ([Hold]).
//   - "The process is gone" must not be read off kill(pid, 0), which reports a
//     zombie as alive, nor off /proc alone, which a darwin run does not have
//     ([AwaitGone]).
//   - Nor off one thread's state, which is all a Linux stat line or ps's STAT
//     carries: a main thread reads Z while another thread of its process runs
//     on. [StartExitedMainThread] stands such a process up, on Linux, for a
//     reading to be held to.
//
// A third is not a correctness trap but a cost every such suite paid without
// seeing it: a stand-in that is MEANT to end must end when it is done, rather
// than a second later ([StandInRaceOptions]).
package procgrouptest

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
)

// GORACE is the variable the race runtime reads its options from when a
// race-built process starts.
const GORACE = "GORACE"

// StandInRaceOptions is the value of [GORACE] a re-executed test binary should
// be launched with: the options this process was started with, followed by
// atexit_sleep_ms=0.
//
// A race-built binary that EXITS 0 sleeps atexit_sleep_ms (1000 by default) in
// the race runtime's finaliser, so every clean exit of a stand-in cost a second
// of nothing. This gives up that grace for a race in the STAND-IN's own exit;
// races elsewhere are reported as before. APPENDED rather than set, so the
// suite's own options (halt_on_error, log_path) reach it, the last winning. A
// caller passes it EXPLICITLY: a launcher building the child's environment from
// an allowlist (harness's) drops an inherited GORACE.
func StandInRaceOptions() string {
	return strings.TrimSpace(os.Getenv(GORACE) + " atexit_sleep_ms=0")
}

// Hold blocks the calling goroutine, and with it a helper process that has
// nothing else to do, for as long as any test could still be watching it.
//
// NOT `select {}`: with every goroutine blocked and no timer pending the Go
// runtime declares a deadlock and exits 2, except in a build linking
// runtime/cgo (Linux CI does, through net's resolver; darwin and CGO_ENABLED=0
// do not), so such a helper died at once everywhere but CI. A sleep keeps a
// timer pending, which the check honours everywhere. It returns after
// [HoldLimit]: one running then belongs to a test that failed to end it.
func Hold() {
	time.Sleep(HoldLimit) //fixedwait:simulated a helper process that stays up until the code under test ends it
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
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: every caller fails the case on the false it returns
			return false
		}
		time.Sleep(awaitPoll) //fixedwait:liveness
	}
}
