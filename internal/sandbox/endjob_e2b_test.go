package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// starts is how many commands the stub's envd has been asked to run.
func (s *e2bStub) starts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r == "POST /process.Process/Start" {
			n++
		}
	}
	return n
}

// startedScript is the shell line the last command envd was asked to run
// carried, as the run's envd reads it: `/bin/bash -l -c <script>`.
func (s *e2bStub) startedScript(t *testing.T) string {
	t.Helper()
	process, _ := s.body("/process.Process/Start")["process"].(map[string]any)
	args, _ := process["args"].([]any)
	if len(args) != 3 {
		t.Fatalf("envd was asked to run %v", process)
	}
	script, _ := args[2].(string)
	return script
}

// AN E2B BOX ENDS AND KILLS ITS JOB AS ITS USER. An end request is SIGUSR1 to
// the job's script by its pid in the VM, sent as the box's user, and a pid
// that answers nothing there is refused as no process of the job; a pid below
// 2 — the VM's init, and the forms of a pid that address groups — is refused
// before envd is asked anything. The kill is every process of the box's user:
// envd starts a job in envd's own process group, which nothing may signal.
//
// Mutation: drop the floor on the pid, and pid 1 is asked of envd; spell the
// request with `-s USR1` or the kill with a `--`, and busybox ash reads the
// kill's group as a number it refuses (end-job-scratch/exp-kill.sh).
func TestAnE2BBoxEndsAndKillsItsJobAsItsUser(t *testing.T) {
	t.Parallel()
	stub := newE2BStub()
	provider, _ := newE2B(t, stub)
	box, err := provider.Create(context.Background(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	before := stub.starts()
	for _, pid := range []int{1, 0, -4242} {
		if err := box.EndJob(ctx, "4242", pid); !errors.Is(err, sandbox.ErrNotJobProcess) {
			t.Fatalf("an end request to pid %d was answered %v, want ErrNotJobProcess", pid, err)
		}
	}
	if stub.starts() != before {
		t.Fatal("envd was asked to signal a pid below 2")
	}

	stub.frames = []any{startEvent(7), dataEvent("crewlet-end-sent\n", ""), endEvent(0)}
	if err := box.EndJob(ctx, "4242", 4243); err != nil {
		t.Fatalf("EndJob: %v", err)
	}
	if script := stub.startedScript(t); !strings.Contains(script, "kill -USR1 4243 ") {
		t.Fatalf("the end request ran %q, want SIGUSR1 to the script's pid", script)
	}
	if got := stub.header("/process.Process/Start", "X-User"); got != "user" {
		t.Fatalf("the end request ran as %q, want the box's user", got)
	}

	stub.frames = []any{startEvent(8), dataEvent("crewlet-end-gone\n", ""), endEvent(0)}
	if err := box.EndJob(ctx, "4242", 4243); !errors.Is(err, sandbox.ErrNotJobProcess) {
		t.Fatalf("an end request to a pid the VM does not have was answered %v, want ErrNotJobProcess", err)
	}
	// AN EXEC THAT NEVER RAN THE REQUEST IS NO ANSWER ABOUT THE PID.
	stub.frames = []any{startEvent(9), endEvent(127)}
	if err := box.EndJob(ctx, "4242", 4243); err == nil || errors.Is(err, sandbox.ErrNotJobProcess) {
		t.Fatalf("an end request the box never ran was answered %v, want a failure to ask", err)
	}

	stub.frames = []any{startEvent(10), endEvent(0)}
	if err := box.KillJob(ctx, "4242"); err != nil {
		t.Fatalf("KillJob: %v", err)
	}
	script := stub.startedScript(t)
	for _, want := range []string{"kill -TERM -1 ", "kill -KILL -1\n", "kill -0 -1 "} {
		if !strings.Contains(script, want) {
			t.Errorf("the kill ran %q, which has no %q", script, want)
		}
	}
	if strings.Contains(script, " -- ") || strings.Contains(script, "-s ") {
		t.Errorf("the kill ran %q, a spelling busybox ash reads otherwise", script)
	}
	if got := stub.header("/process.Process/Start", "X-User"); got != "user" {
		t.Fatalf("the kill ran as %q, want the box's user", got)
	}
}
