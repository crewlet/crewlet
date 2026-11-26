package executor

import (
	"errors"
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
// goroutines fork beside 64 programs written into one direct box and each run
// as soon as it is written; only a descriptor a fork inherits refuses a run.
//
// Mutation: write the program with box.WriteFile in writeHelperBuild, and a
// share of the runs is refused as the file being busy.
func TestAProgramTheEngineHandsABoxRunsTheMomentItIsInPlace(t *testing.T) {
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
	refused := make(chan error, 64)
	for i := range 64 {
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
		t.Fatalf("%d of 64 programs were refused as busy when run the moment they were in place", n)
	}
	left, err := filepath.Glob(box.Home() + "/program-*.*")
	if err != nil || len(left) > 0 {
		t.Errorf("the home holds %q (%v) beside the programs, want no source or copy left", left, err)
	}
}
