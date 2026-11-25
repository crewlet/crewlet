package procgrouptest

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A PROCESS WHOSE MAIN THREAD ALONE HAS EXITED RUNS ON, read as
// internal/procgroup reads it: the stand-in's stat line reads Z beside threads
// that run, and [Running] answers it running; once it is over, a zombie nothing
// has reaped, [AwaitGone] finds it gone.
//
// Mutation: read every Z as exited, in procgroup's parseProcStat, and the
// stand-in reads gone while its threads run.
func TestAProcessWhoseMainThreadAloneExitedRunsOn(t *testing.T) {
	t.Parallel()
	pid := StartExitedMainThread(t, nil)
	if running, err := Running(pid); err != nil || !running {
		t.Fatalf("a process whose main thread alone has exited reads running %v (%v), want it running",
			running, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("ending the stand-in: %v", err)
	}
	if !AwaitGone(t, pid, time.Minute) {
		t.Fatalf("the stand-in, pid %d, runs on after SIGKILL", pid)
	}
}

// THE STAND-IN RUNS WITH NO COLLECTOR AND A SECOND P, its guards should the
// dead thread ever keep its P ([StartExitedMainThread]): its environment, read
// off a thread that runs (the kernel answers none for the exited main one),
// holds GOGC=off and GOMAXPROCS=2.
//
// Mutation: drop either from the stand-in's environment, and it is named here.
func TestTheStandInRunsWithNoCollectorAndASecondP(t *testing.T) {
	t.Parallel()
	pid := StartExitedMainThread(t, nil)
	tasks, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "task"))
	if err != nil {
		t.Fatalf("listing the stand-in's threads: %v", err)
	}
	var environ []string
	for _, task := range tasks {
		if task.Name() == strconv.Itoa(pid) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", task.Name(), "environ")) //nolint:gosec // the stand-in's own record
		if err != nil {
			t.Fatalf("reading the environment of the stand-in's thread %s: %v", task.Name(), err)
		}
		environ = strings.Split(string(raw), "\x00")
		break
	}
	for _, want := range []string{"GOGC=off", "GOMAXPROCS=2"} {
		if !slices.Contains(environ, want) {
			t.Errorf("the stand-in, pid %d, runs without %s", pid, want)
		}
	}
}
