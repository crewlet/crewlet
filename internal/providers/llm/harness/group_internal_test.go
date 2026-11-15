package harness

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// A harness's group is signalled after its leader was reaped in three places
// (a call's end, a server's stop, a device sign-in's stop), and a reaped
// group's id is the kernel's to hand out again. These cases hold the one check
// all three now go through, [harnessGroup.signal], to procgroup's rule: a
// group is signalled only while it is still the one recorded at the start.

// NO GROUP IS SIGNALLED ONCE IT IS NOT THE ONE RECORDED. A real child is
// started in a group of its own and its identity recorded. While it runs, a
// signal reaches its group: the control that keeps the second half from
// passing on a check that refuses everything. Once it has exited and been
// reaped, every member of its group is gone and its id is free, and the same
// signal is not sent at all.
//
// Mutation: signal without asking whether the group is current, and the
// reaped group's pid is handed to send.
func TestNoGroupIsSignalledOnceItIsNotTheOneRecorded(t *testing.T) {
	t.Parallel()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=NoSuchTest") //nolint:gosec // os.Args[0] is this test binary
	cmd.Env = append(os.Environ(), helperEnv+"=1", "FAKE_ECHO_STDIN=1",
		procgrouptest.GORACE+"="+procgrouptest.StandInRaceOptions())
	release, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	procgroup.Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	group, err := recordGroup(cmd)
	if err != nil {
		_ = release.Close()
		t.Fatalf("recording the child's group: %v", err)
	}

	var sent sentTo
	if !group.signal(t.Context(), "a test", "a test's signal", sent.send) || len(sent.list()) != 1 {
		_ = release.Close()
		_ = cmd.Wait()
		t.Fatalf("a running child's group was not signalled (sent to %v); the check refuses "+
			"a group that is current, so its refusal below proves nothing", sent.list())
	}

	// The fake echoes its stdin to its end: closing it is the child's word to
	// go, and Wait reaps it.
	_ = release.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the child: %v", err)
	}
	var after sentTo
	if group.signal(t.Context(), "a test", "a test's signal", after.send) {
		t.Error("a reaped group was reported as still the one recorded")
	}
	if got := after.list(); len(got) != 0 {
		t.Errorf("a group whose every member was reaped was signalled at %v: its id is the "+
			"kernel's to hand to a stranger", got)
	}
}

// A GROUP THAT CANNOT BE READ IS NOT SIGNALLED, AND THE LOG SAYS SO. When the
// kernel will not say whether the group is still the one recorded (a /proc
// mounted with hidepid), the answer is neither yes nor no: the signal is
// withheld, since an unreadable record grants no permission to signal, and
// `harness_group_signal_withheld` names the group, the signal and why it was
// being sent.
//
// Sequential: the log is the process's.
//
// Mutation: read the error as a group that is gone, and nothing is logged;
// signal without asking, and the group is signalled.
func TestASignalToAGroupThatCannotBeReadIsWithheld(t *testing.T) {
	logs := captureLogs(t)
	group := harnessGroup{
		leader:  procgroup.Leader{PID: 4242},
		current: func() (bool, error) { return false, errors.New("the process table could not be read") },
	}
	var sent sentTo
	if group.signal(t.Context(), "the call ended", "SIGKILL", sent.send) {
		t.Error("a group the kernel would not describe was reported as the one recorded")
	}
	if got := sent.list(); len(got) != 0 {
		t.Errorf("a group the kernel would not describe was signalled at %v", got)
	}
	line := logs.String()
	for _, want := range []string{"harness_group_signal_withheld", "4242", "SIGKILL", "the call ended",
		"the process table could not be read"} {
		if !strings.Contains(line, want) {
			t.Errorf("the withheld signal's log lacks %q: %s", want, line)
		}
	}
}

// A SERVER WHOSE GROUP CANNOT BE READ IS STILL STOPPED. A server never exits
// on its own, and this one ignores SIGTERM, as does the helper it started in
// its group. With the kernel's record of the group unreadable, both of stop's
// group signals are withheld; the leader's own handle still reaches the
// server, so stop returns, as the run's teardown that calls it must.
//
// Mutation: send nothing where the group signal was withheld, and stop waits
// on the server for ever.
func TestAServerWhoseGroupCannotBeReadIsStillStopped(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	extra := map[string]string{"FAKE_STUBBORN": "1", "FAKE_PIDFILE": pidFile}
	p := fakeProvider(t, extra, nil)
	p.hooks = &captureHooks{current: func(procgroup.Leader) (bool, error) {
		return false, errors.New("the process table could not be read")
	}}
	srv, err := startServer(t.Context(), p, []string{"-test.run=TestFakeHarness"}, t.TempDir(),
		fakeRunEnv(extra), witnessGrace, nil, nil)
	if err != nil {
		t.Fatalf("startServer: %v", err)
	}
	helper := awaitPidFile(t, pidFile)
	t.Cleanup(func() {
		_ = srv.child.cmd.Process.Kill()
		reapTree(helper)
	})

	stopped := make(chan struct{})
	go func() {
		srv.stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second): //fixedwait:liveness
		t.Fatal("stop never returned for a server whose group could not be read")
	}
}

// sentTo records the pids a signal was sent to, in place of sending it.
type sentTo struct {
	mu   sync.Mutex
	pids []int
}

func (s *sentTo) send(pid int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pids = append(s.pids, pid)
	return nil
}

func (s *sentTo) list() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.pids...)
}
