package procgrouptest

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// exitedMainEnv makes this test binary, re-executed, a process whose main
// thread has exited while its other threads run on ([StartExitedMainThread]).
const exitedMainEnv = "CREWLET_PROCGROUPTEST_EXITED_MAIN"

// An init rather than a TestMain's dispatch: the runtime holds the main
// goroutine on the main thread until every init has run, so SYS_EXIT here ends
// that thread alone, where exit_group (os.Exit's) ends them all. Through
// syscall.Syscall, not RawSyscall: entering the syscall hands the thread's P
// back, where a raw one keeps it, and a P no thread will release stalls every
// stop-the-world (each collection's) and, with one P, every goroutine
// (measured). A goroutine reads fd 3, the pipe the test holds the write end
// of, and ends the process once it closes, so a test binary that dies leaves
// no stand-in behind. Without the marker it does nothing.
func init() {
	if _, ok := os.LookupEnv(exitedMainEnv); !ok {
		return
	}
	go func() {
		_, _ = io.Copy(io.Discard, os.NewFile(3, "the test's pipe"))
		os.Exit(0)
	}()
	_, _, errno := syscall.Syscall(syscall.SYS_EXIT, 0, 0, 0)
	fmt.Fprintln(os.Stderr, "exiting the main thread alone:", errno)
	os.Exit(2)
}

// StartExitedMainThread starts a process whose main thread has exited while
// its other threads run on, as Node's does for milliseconds through its exit,
// and answers its pid once its stat line says so: Z, the main thread's state,
// beside a num_threads above one. Such a process runs, and a reading of one
// thread's state takes it for one that has exited (internal/procgroup's
// parseProcStat says why). It is this test binary re-executed, started with
// attr (a process group to join, say), and is ended and reaped when t ends.
func StartExitedMainThread(t testing.TB, attr *syscall.SysProcAttr) int {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatalf("finding this test binary to re-execute: %v", err)
	}
	held, release, err := os.Pipe()
	if err != nil {
		t.Fatalf("a pipe to hold the stand-in by: %v", err)
	}
	cmd := exec.CommandContext(t.Context(), self) //nolint:gosec // this test binary
	// GOGC=off and GOMAXPROCS=2 guard the stand-in should its P ever stay with
	// the dead thread ([init]): no collection, the forced one every two
	// minutes included, then stops the world, and the pipe's watcher has a P.
	cmd.Env = append(os.Environ(), exitedMainEnv+"=1", GORACE+"="+StandInRaceOptions(), "GOGC=off", "GOMAXPROCS=2")
	cmd.ExtraFiles = []*os.File{held}
	cmd.SysProcAttr = attr
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Start()
	_ = held.Close()
	if err != nil {
		_ = release.Close()
		t.Fatalf("starting a stand-in whose main thread exits: %v", err)
	}
	t.Cleanup(func() {
		_ = release.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	deadline := time.Now().Add(time.Minute)
	for {
		state, threads := mainThread(t, pid)
		switch {
		case state == "Z" && threads > 1:
			return pid
		case state == "" || state == "Z" || state == "X" || state == "x":
			_ = cmd.Wait()
			t.Fatalf("the stand-in, pid %d, exited whole rather than its main thread alone (state %q, %d "+
				"threads): %s", pid, state, threads, stderr.String())
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline: the case fails on it
			t.Fatalf("the stand-in, pid %d, never read an exited main thread beside a running one (state %q, "+
				"%d threads)", pid, state, threads)
		}
		time.Sleep(awaitPoll) //fixedwait:liveness
	}
}

// mainThread is the state and num_threads, fields 3 and 20, of pid's stat
// line, counted from its last closing parenthesis; no state where it has gone.
func mainThread(t testing.TB, pid int) (string, int) {
	t.Helper()
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat") //nolint:gosec // the kernel's record of the stand-in
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
		return "", 0
	}
	if err != nil {
		t.Fatalf("reading the stand-in's record: %v", err)
	}
	end := bytes.LastIndexByte(raw, ')')
	fields := strings.Fields(string(raw[end+1:]))
	if end < 0 || len(fields) < 18 {
		t.Fatalf("the stand-in's record is no stat line: %q", raw)
	}
	threads, err := strconv.Atoi(fields[17])
	if err != nil {
		t.Fatalf("the stand-in's num_threads %q is not a count", fields[17])
	}
	return fields[0], threads
}
