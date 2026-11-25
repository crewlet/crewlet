package executor_test

import (
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// THE GATEWAY'S STOP WAITS FOR ITS LAST THREAD: a gateway whose main thread
// alone has exited, as a process's main thread does through its exit, runs
// on, and gwgone, which ends endgw's wait and decides its SIGKILL, takes it
// for running; once it is over, a zombie its parent has not reaped, for gone.
// Under every sh a box may run the script with; Linux alone, where gwgone
// reads /proc.
//
// Mutation: read ps's STAT, the main thread's state on Linux, as gwgone did,
// and the first reading takes the gateway for gone.
func TestTheGatewaysStopWaitsForItsLastThread(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		gateway := procgrouptest.StartExitedMainThread(t, nil)
		if gatewayGone(t, sh, gateway) {
			t.Errorf("gwgone takes a gateway whose main thread alone has exited, pid %d, for gone", gateway)
		}
		if err := syscall.Kill(gateway, syscall.SIGKILL); err != nil {
			t.Fatalf("ending the gateway: %v", err)
		}
		if !procgrouptest.AwaitGone(t, gateway, time.Minute) {
			t.Fatalf("the gateway, pid %d, runs on after SIGKILL", gateway)
		}
		if !gatewayGone(t, sh, gateway) {
			t.Errorf("gwgone takes a gateway that is over, a zombie, pid %d, for running", gateway)
		}
	})
}

// gatewayGone is what the gateway stop's gwgone answers of pid under sh.
func gatewayGone(t *testing.T, sh boxShell, pid int) bool {
	t.Helper()
	argv := sh.argv
	if len(argv) == 0 {
		argv = []string{"sh"}
	}
	script := sh.prelude + executor.HermesStopGateway(executor.Paths{Home: t.TempDir()}, time.Second) +
		"gp=" + strconv.Itoa(pid) + "\nif gwgone; then echo gone; else echo runs; fi\n"
	cmd := exec.CommandContext(t.Context(), argv[0], append(slices.Clone(argv[1:]), "-c", script)...) //nolint:gosec // the script under test
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gwgone under %s: %v\n%s", sh.name, err, out)
	}
	switch strings.TrimSpace(string(out)) {
	case "gone":
		return true
	case "runs":
		return false
	}
	t.Fatalf("gwgone under %s answered %q", sh.name, out)
	return false
}
