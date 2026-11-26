package executor

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// A PROGRAM THE ENGINE HANDS A BOX RUNS THE MOMENT IT IS IN PLACE, whatever the
// engine forks meanwhile: the guard for [writeProgram], through
// [writeHelperBuild], which every runner's note helper is written by. Eight
// goroutines fork beside programs written into one direct box and each run as
// soon as it is written; only a descriptor a fork inherits refuses a run. NOT
// PARALLEL: its forks would hand any other case's open descriptor to a child,
// so it runs before the package's parallel cases are released.
//
// Mutation: write the program with box.WriteFile in writeHelperBuild, and runs
// are refused as the file being busy.
func TestAProgramTheEngineHandsABoxRunsTheMomentItIsInPlace(t *testing.T) {
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
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

	var runs sync.WaitGroup
	refused := make(chan error, programRuns)
	for i := range programRuns {
		runs.Go(func() {
			path := box.Home() + "/program-" + strconv.Itoa(i)
			if err := writeHelperBuild(t.Context(), box, path, []byte("#!/bin/sh\nexit 0\n")); err != nil {
				t.Errorf("writing %s: %v", path, err)
				return
			}
			if err := os.Chmod(path, 0o700); err != nil { //nolint:gosec // the program must run
				t.Errorf("making %s executable: %v", path, err)
				return
			}
			if err := exec.CommandContext(t.Context(), path).Run(); errors.Is(err, syscall.ETXTBSY) {
				refused <- err
			} else if err != nil {
				t.Errorf("running %s: %v", path, err)
			}
		})
	}
	runs.Wait()
	close(refused)
	if n := len(refused); n > 0 {
		t.Fatalf("%d of %d programs were refused as busy when run the moment they were in place", n, programRuns)
	}
	left, err := filepath.Glob(box.Home() + "/program-*.*")
	if err != nil || len(left) > 0 {
		t.Errorf("the home holds %q (%v) beside the programs, want no copy left", left, err)
	}
	if staged, err := os.ReadDir(PathsFor(box).ProgramStaging()); err != nil || len(staged) > 0 {
		t.Errorf("the staging holds %v (%v), want every program's bytes taken", staged, err)
	}
}

// A PROGRAM WHOSE COPY NEVER RAN LEAVES NOTHING PAST THE NEXT CLEAR: the
// engine wrote its bytes, the box was gone before it copied them into place,
// and the next run's clear removes them with the programs' staging.
//
// Mutation: drop [Paths.ProgramStaging] from the clear's directories.
func TestAProgramWhoseCopyNeverRanLeavesNothingPastTheNextClear(t *testing.T) {
	t.Parallel()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { box.Close(t.Context()) })
	p := PathsFor(box)
	if err := writeHelperBuild(t.Context(), copyNeverRuns{box}, p.PiNoteHelper(), []byte("#!/bin/sh\n")); err == nil {
		t.Fatal("the helper was written though its copy never ran")
	}
	if staged, err := os.ReadDir(p.ProgramStaging()); err != nil || len(staged) != 1 {
		t.Fatalf("the staging holds %v (%v), want the one program's bytes the copy never took", staged, err)
	}
	if err := NewPi().ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("the next run's clear: %v", err)
	}
	if _, err := os.Lstat(p.ProgramStaging()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the programs' staging is left after the next run's clear (%v)", err)
	}
}

// copyNeverRuns is a box gone before a program's copy reaches it: every
// command fails to be sent.
type copyNeverRuns struct{ sandbox.Sandbox }

func (copyNeverRuns) Exec(context.Context, string, sandbox.ExecOptions) (sandbox.ExecResult, error) {
	return sandbox.ExecResult{}, errors.New("the box is gone")
}

// programRuns is how many programs [TestAProgramTheEngineHandsABoxRunsTheMomentItIsInPlace]
// writes and runs: at 64 the mutation was refused 3 to 10 times in each of 20
// runs on a two-core VM, so twice that keeps every run of it red on a runner
// that forks less readily.
const programRuns = 128

// writeHostProgram puts body at path at mode, a program a case runs or hands a
// script that runs it. A file this process wrote can be refused when run, as
// [writeProgram] says of the engine's, so cp, which has exited before anything
// runs the file, writes it beside path, and it is moved over path.
func writeHostProgram(t *testing.T, path string, body []byte, mode os.FileMode) {
	t.Helper()
	source, next := path+".source", path+".next"
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatalf("writing %s: %v", source, err)
	}
	if out, err := exec.CommandContext(t.Context(), "cp", source, next).CombinedOutput(); err != nil {
		t.Fatalf("copying %s into place: %v\n%s", path, err, out)
	}
	if err := os.Chmod(next, mode); err != nil {
		t.Fatalf("setting %s's mode: %v", next, err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatalf("moving %s into place: %v", path, err)
	}
	if err := os.Remove(source); err != nil {
		t.Fatalf("removing %s: %v", source, err)
	}
}
