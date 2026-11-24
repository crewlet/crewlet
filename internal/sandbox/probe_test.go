package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// scriptedRuntime writes an executable named name in a fresh directory that runs
// script, and answers its path — written where no fork can hold it open
// ([writeExecutable]), since the probe execs it at once.
func scriptedRuntime(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeExecutable(t, path, "#!/bin/sh\n"+script+"\n")
	return path
}

// THE CONTAINER CELL IS READY WHEN ITS DAEMON ANSWERS, and not otherwise: a
// runtime on PATH whose daemon is down launches no box, and one that hangs is
// cut off at the probe's budget rather than holding the probe. Each refusal
// names the one thing to do.
//
// THE HUNG DAEMON SLEEPS TEN MINUTES, far past the minute the case waits, so
// the probe answers only by ending its read at its budget, which its detail
// names; no bound on the machine's clock decides it.
//
// Mutation: report ready on a resolved binary alone, or wait on a hung daemon
// rather than end its read at the budget, and this fails.
func TestTheContainerCellIsReadyWhenItsDaemonAnswers(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		script string
		ready  bool
		detail string
		budget time.Duration
	}{
		"a daemon that answers": {script: `echo 24.0.7`, ready: true, detail: "Docker 24.0.7", budget: 5 * time.Second},
		"a daemon that is down": {script: `echo "Cannot connect to the Docker daemon" >&2; exit 1`, detail: "Cannot connect to the Docker daemon", budget: 5 * time.Second},
		"a daemon that never answers": {script: `exec sleep 600`, detail: "did not answer a version read within 200ms",
			budget: 200 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := scriptedRuntime(t, "docker", tc.script)
			probed := make(chan CellProbe, 1)
			go func() { probed <- probeRuntime(t.Context(), path, tc.budget) }()
			var got CellProbe
			select {
			case got = <-probed:
			case <-time.After(time.Minute): //fixedwait:liveness
				t.Fatal("the probe is still held by a daemon that has not answered; want its read ended at the budget")
			}
			if got.Ready != tc.ready || !strings.Contains(got.Detail, tc.detail) {
				t.Fatalf("probe = %+v, want ready %v saying %q", got, tc.ready, tc.detail)
			}
			if !tc.ready && got.Fix == "" {
				t.Error("a cell that is not ready names nothing to do about it")
			}
		})
	}
}

// A RUNTIME THAT IS NOT INSTALLED IS NOT READY, and the fix says PATH is read
// at start: installing it under a running engine changes nothing until the
// engine restarts.
func TestAMissingRuntimeIsNotReady(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got := ProbeContainer(t.Context(), "docker")
	if got.Ready || !strings.Contains(got.Fix, "restart the engine") {
		t.Fatalf("probe = %+v, want not ready, saying to restart after installing", got)
	}
}

// THE DIRECT CELL IS READY WHERE ITS BOX ROOT CAN BE WRITTEN, and not where it
// cannot.
func TestTheDirectCellNeedsAWritableBoxRoot(t *testing.T) {
	t.Parallel()
	if got := ProbeDirect(t.TempDir()); !got.Ready {
		t.Fatalf("probe of a writable state dir = %+v, want ready", got)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A state dir that is a FILE: its box root cannot be made under it.
	if got := ProbeDirect(blocked); got.Ready || got.Fix == "" {
		t.Fatalf("probe of an unwritable state dir = %+v, want not ready with a fix", got)
	}
}
