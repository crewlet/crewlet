//go:build unix

package executor

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
)

// THE GATEWAY'S STOP ENDS ITS WHOLE PROCESS GROUP AND ONLY A GATEWAY: run
// against a stand-in in a session of its own, a `gateway run` with a child in
// its group, whose pid file records its start time as Hermes's does, endgw
// sends the group SIGTERM and both go; a pid file naming another process, or
// the stand-in under another start time, is left alone.
//
// Mutation: signal the pid alone, or skip the identity check, and a row goes red.
func TestTheGatewaysStopEndsItsGroupAndOnlyAGateway(t *testing.T) {
	t.Parallel()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's script", "install a POSIX shell")
	home := t.TempDir()
	paths := Paths{Home: home}
	if err := os.MkdirAll(paths.HermesHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	// The stand-in: a shell whose command line names the gateway, holding a
	// child in its group.
	gw := exec.Command(sh, "-c", "sleep 300 & wait", "gateway", "run")
	gw.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := gw.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Process.Kill(); _ = gw.Wait() })
	pid := gw.Process.Pid
	stale := exec.Command(sh, "-c", "sleep 300")
	if err := stale.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stale.Process.Kill(); _ = stale.Wait() })
	stop := func(named int, start string) {
		t.Helper()
		pidFile := `{"pid": ` + strconv.Itoa(named) + `, "kind": "hermes-gateway", "start_time": ` + start + `}`
		if err := os.WriteFile(paths.HermesGatewayPID(), []byte(pidFile), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(sh, "-c", hermesStopGateway(paths, 5*time.Second)+"endgw")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("endgw: %v: %s", err, out)
		}
	}
	// A pid reused: another process, under a start time not the one recorded.
	stop(stale.Process.Pid, "1")
	if err := stale.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("endgw signalled a process that is no gateway: %v", err)
	}
	if _, err := os.Stat("/proc/self/stat"); err == nil {
		stop(pid, "1")
		if err := gw.Process.Signal(syscall.Signal(0)); err != nil {
			t.Fatalf("endgw signalled a gateway its pid file records under another start time: %v", err)
		}
	}
	stop(pid, startTime(t, pid))
	done := make(chan error, 1)
	go func() { done <- gw.Wait() }()
	if err := <-done; err == nil {
		t.Fatal("the gateway stand-in exited cleanly; it was never signalled")
	}
}

// startTime is pid's start time as the kernel records it (/proc/<pid>/stat's
// 22nd field), as Hermes's pid file records it, or 0 where there is no /proc,
// where endgw reads the command line instead.
func startTime(t *testing.T, pid int) string {
	t.Helper()
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return "0"
	}
	fields := strings.Fields(string(raw[strings.LastIndexByte(string(raw), ')')+1:]))
	if len(fields) < 20 {
		t.Fatalf("/proc/%d/stat holds %d fields past its command", pid, len(fields))
	}
	return fields[19]
}
