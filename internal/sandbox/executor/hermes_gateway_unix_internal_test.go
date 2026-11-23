//go:build unix

package executor

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
)

// THE GATEWAY'S STOP ENDS ITS WHOLE PROCESS GROUP AND ONLY A GATEWAY: run
// against a stand-in that records itself as `gateway run` in a session of its
// own, with a child in its group, endgw sends the group SIGTERM and both go;
// a pid file naming another process is left alone.
//
// Mutation: signal the pid alone, or skip the command-line check, and a row
// goes red.
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
	stop := func(named int) {
		t.Helper()
		pidFile := `{"pid": ` + strconv.Itoa(named) + `, "kind": "hermes-gateway"}`
		if err := os.WriteFile(paths.HermesGatewayPID(), []byte(pidFile), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(sh, "-c", hermesStopGateway(paths, 5*time.Second)+"endgw")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("endgw: %v: %s", err, out)
		}
	}
	stop(stale.Process.Pid)
	if err := stale.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("endgw signalled a process that is no gateway: %v", err)
	}
	stop(pid)
	done := make(chan error, 1)
	go func() { done <- gw.Wait() }()
	if err := <-done; err == nil {
		t.Fatal("the gateway stand-in exited cleanly; it was never signalled")
	}
}
