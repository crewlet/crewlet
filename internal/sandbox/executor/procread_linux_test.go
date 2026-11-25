package executor_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/shellword"
)

// THE CASES READ A PROCESS AS INTERNAL/PROCGROUP DOES: one whose main thread
// alone has exited runs on, so a call left so still runs for [runs], a group
// it leads still runs for [liveInGroup], and a server left so still serves
// for [fakeOpenCode]'s up; once it is over, a zombie nothing has reaped, it
// runs nothing for any of them. Linux alone, where a main thread exits by
// itself.
//
// Mutation: read the process's stat line, its main thread's state, as each
// did, and its first reading takes the process for ended.
func TestTheCasesReadAProcessAsProcgroupDoes(t *testing.T) {
	t.Parallel()
	pid := procgrouptest.StartExitedMainThread(t, &syscall.SysProcAttr{Setpgid: true})
	serving := fakeServing(t, pid)
	read := func(as string, running bool) {
		t.Helper()
		if got := runs(t, pid); got != running {
			t.Errorf("runs reads %s, pid %d, running %v, want %v", as, pid, got, running)
		}
		if live, readable := liveInGroup(t, pid); !readable || (len(live) == 1) != running {
			t.Errorf("liveInGroup reads the group of %s, pid %d, live with %q (readable %v), want it live %v",
				as, pid, live, readable, running)
		}
		if got := serving(); got != running {
			t.Errorf("the fake's up reads its server as %s, pid %d, serving %v, want %v", as, pid, got, running)
		}
	}
	read("a process whose main thread alone has exited", true)
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("ending the process: %v", err)
	}
	if !procgrouptest.AwaitGone(t, pid, time.Minute) {
		t.Fatalf("pid %d runs on after SIGKILL", pid)
	}
	read("a zombie", false)
}

// fakeServing answers, each time it is called, whether [fakeOpenCode]'s up
// reads its server, pid, as serving: what an interrupt the fake takes records.
func fakeServing(t *testing.T, pid int) func() bool {
	t.Helper()
	dir := t.TempDir()
	fake := filepath.Join(dir, "opencode")
	writeStandIn(t, fake, strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shellword.Quote(dir), 1))
	if err := os.WriteFile(filepath.Join(dir, "serve.pid"), []byte(strconv.Itoa(pid)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return func() bool {
		t.Helper()
		if out, err := exec.CommandContext(t.Context(), fake, "api", "--standalone", "session.interrupt").
			CombinedOutput(); err != nil {
			t.Fatalf("the fake's interrupt: %v\n%s", err, out)
		}
		served := strings.Fields(readIn(t, dir, "interrupted"))
		if len(served) == 0 {
			t.Fatalf("the fake's interrupt recorded nothing")
		}
		return served[len(served)-1] == "serving"
	}
}
