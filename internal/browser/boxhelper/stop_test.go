package boxhelper_test

import (
	"errors"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
)

// stopIn runs stop for box, under path, as the engine runs it but in a plain
// shell, and answers its exit status.
func stopIn(t *testing.T, box *boxhelpertest.Box, path string) int {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", boxhelper.StopScript(box.Home))
	cmd.Env = append(box.Environ(), "PATH="+path)
	cmd.Dir = box.Home
	out, err := cmd.CombinedOutput()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		t.Logf("stop exited %d: %s", exit.ExitCode(), out)
		return exit.ExitCode()
	}
	if err != nil {
		t.Fatalf("running stop: %v", err)
	}
	return 0
}

// STOP ENDS THE BOX'S SUPERVISOR AND ITS BROWSER: the supervisor ensure
// started, and its Chromium, are gone once stop answers, the record saying
// stopped; a second stop, with nothing running, answers at once; and a box
// that holds no supervisor's record answers without running node at all.
//
// Mutations, each red: answer before the supervisor is stopped; run node
// where the box holds no record.
func TestStopEndsTheBoxsSupervisorAndItsBrowser(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		bin := filepath.Join(box.Home, "bin")
		if code := stopIn(t, box, t.TempDir()); code != 0 {
			t.Errorf("stop in a box with no supervisor's record, and no node, exited %d, want 0", code)
		}
		if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
			t.Fatalf("ensure answered %+v", got)
		}
		s := box.Supervisor(t)
		if code := stopIn(t, box, bin); code != 0 {
			t.Fatalf("stop exited %d", code)
		}
		if running, err := procgrouptest.Running(s.PID); err != nil || running {
			t.Errorf("the supervisor, pid %d, runs on after stop answered (%v)", s.PID, err)
		}
		if !procgrouptest.AwaitGone(t, s.Chromium.PID, goneWithin) {
			t.Errorf("its browser, pid %d, outlived its supervisor's stop by %s", s.Chromium.PID, goneWithin)
		}
		if got := box.Supervisor(t); got.State != "stopped" {
			t.Errorf("supervisor.json records %+v, want stopped", got)
		}
		if code := stopIn(t, box, bin); code != 0 {
			t.Errorf("a second stop, nothing running, exited %d, want 0", code)
		}
	})
}
