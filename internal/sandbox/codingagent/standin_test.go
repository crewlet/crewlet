package codingagent_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// Stand-in CLIs a case writes and then runs.
//
// A FILE THIS PROCESS WROTE CAN BE REFUSED WHEN IT IS RUN (ETXTBSY). os.WriteFile
// closes its own descriptor, but a case running in parallel that forked while it
// was open handed its child a copy, and Linux refuses to execute a file any
// process holds open for writing until that child executes in turn. The window
// is short and this package forks constantly — internal/sandbox's
// suite measured 17 refusals in 200 runs of a just-written script with
// eight goroutines forking beside it — and a refusal reads as nothing a case
// asserts: a run's script finds no CLI, or a version probe answers nothing and
// the gate takes its fallback. So a stand-in is written by a process of its own
// (cp), which has exited before the case runs it: this process never holds it
// open for writing, and no fork of its can.

// writeStandIn installs script at path as an executable stand-in, written by a
// process other than this one.
func writeStandIn(t *testing.T, path, script string) {
	t.Helper()
	source := path + ".source"
	if err := os.WriteFile(source, []byte(script), 0o600); err != nil {
		t.Fatalf("write the stand-in's source: %v", err)
	}
	if out, err := exec.Command("cp", source, path).CombinedOutput(); err != nil { //nolint:noctx // a local copy of one file
		t.Fatalf("install the stand-in at %s: %v\n%s", path, err, out)
	}
	if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // the stand-in must run
		t.Fatalf("make the stand-in executable: %v", err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatalf("remove the stand-in's source: %v", err)
	}
}

// writeBoxStandIn is [writeStandIn] inside a box: the box's own process copies
// the stand-in into place from a source this process wrote beside it.
func writeBoxStandIn(t *testing.T, box sandbox.Sandbox, path, script string) {
	t.Helper()
	source := path + ".source"
	if err := box.WriteFile(t.Context(), source, []byte(script)); err != nil {
		t.Fatalf("write the stand-in's source: %v", err)
	}
	install := "cp " + shQuote(source) + " " + shQuote(path) + " && chmod +x " + shQuote(path) + " && rm " + shQuote(source)
	res, err := box.Exec(t.Context(), install, sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("install the stand-in at %s: %v (exit %d) %s", path, err, res.ExitCode, res.Stderr)
	}
}

// shQuote is s quoted for sh.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// A STAND-IN RUNS THE MOMENT IT IS HANDED OVER, whatever this process is forking
// meanwhile — the guard for [writeStandIn]. Eight goroutines fork beside 64
// stand-ins written and run at once: the forks are the point, because the
// descriptor a fork inherits is what refuses the run, and load from outside
// this process does not reproduce it.
//
// Mutation: write the stand-in with os.WriteFile in this process, and a share
// of the runs here is refused as the file being busy.
func TestAStandInRunsTheMomentItIsHandedOver(t *testing.T) {
	t.Parallel()
	stop := make(chan struct{})
	var forkers sync.WaitGroup
	for range 8 {
		forkers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
					_ = exec.Command("/bin/true").Run() //nolint:noctx // a fork, not a subject
				}
			}
		})
	}
	t.Cleanup(func() { close(stop); forkers.Wait() })

	dir := t.TempDir()
	var runs sync.WaitGroup
	refused := make(chan error, 64)
	for i := range 64 {
		runs.Go(func() {
			path := filepath.Join(dir, "stand-in-"+strconv.Itoa(i))
			writeStandIn(t, path, "#!/bin/sh\nexit 0\n")
			if err := exec.Command(path).Run(); errors.Is(err, syscall.ETXTBSY) { //nolint:noctx // the stand-in's one run
				refused <- err
			}
		})
	}
	runs.Wait()
	close(refused)
	if n := len(refused); n > 0 {
		t.Fatalf("%d of 64 stand-ins were refused as busy when run the moment they were written", n)
	}
}
