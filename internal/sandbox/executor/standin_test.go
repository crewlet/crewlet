package executor_test

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

// Stand-in harnesses a case writes and then runs.
//
// A FILE THIS PROCESS WROTE CAN BE REFUSED WHEN IT IS RUN (ETXTBSY). os.WriteFile
// closes its own descriptor, but a case running in parallel that forked while it
// was open handed its child a copy, and Linux refuses to execute a file any
// process holds open for writing until that child executes in turn. The window
// is short and this package forks constantly — internal/sandbox's
// suite measured 17 refusals in 200 runs of a just-written script with
// eight goroutines forking beside it — and a refusal reads as nothing a case
// asserts: a run's script finds no harness, or a version probe answers nothing and
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

// claim is shell that succeeds in the one process that first runs it on path
// and fails in every other, then and after: the turn a stand-in takes ahead of
// the calls of it made beside it — the first of two packs at once to hold, say
// — taken by creating path, a file, under the shell's own noclobber (`set -C`),
// which a POSIX shell opens O_CREAT|O_EXCL: one create of a name, which the
// kernel grants one process alone. Noclobber is set in a subshell of its own,
// so no redirection the stand-in makes after it is held to it.
//
// NEVER `mkdir`, the obvious claim, whose exclusivity is its tool's. uutils
// coreutils' mkdir — /usr/bin/mkdir on Ubuntu 26.04, and so on CI's runners —
// has none through 0.10.0: it checks that the name is free, then creates it,
// and answers a create that failed with success wherever the name is a
// directory by then (fixed upstream in 0.11.0, "mkdir: do not ignore EEXIST
// on non-recursive creation"). Measured on ubuntu:26.04: two mkdirs of one
// name started together both succeeded in 2178 rounds of 3000, and two of
// these claims in none. Two packs that had each taken the first's turn each
// held for the other's word, out to the pack's two-minute bound (#946).
func claim(path string) string { return "( set -C; : > " + shQuote(path) + " ) 2> /dev/null" }

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

// A STAND-IN'S CLAIM IS TAKEN BY ONE PROCESS, however many make it at once — the
// guard for [claim]. Round after round, two of this machine's shells wait at
// one start line and then claim one name together, and in every round exactly
// one of them takes it.
//
// Mutation: claim with `mkdir`, and on a machine whose mkdir is uutils' through
// 0.10.0 — CI's — both shells take the claim in most rounds. GNU's mkdir is
// exclusive, so on a machine with GNU coreutils that mutant passes.
func TestAStandInsClaimIsTakenByOneProcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for round := range claimRounds {
		name := filepath.Join(dir, "claimed."+strconv.Itoa(round))
		start := filepath.Join(dir, "start."+strconv.Itoa(round))
		script := "while [ ! -e " + shQuote(start) + " ]; do :; done; " + claim(name)
		var shells [2]*exec.Cmd
		for i := range shells {
			shells[i] = exec.CommandContext(t.Context(), "/bin/sh", "-c", script)
			if err := shells[i].Start(); err != nil {
				t.Fatalf("starting a claim: %v", err)
			}
		}
		if err := os.WriteFile(start, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		won := 0
		for _, sh := range shells {
			var lost *exec.ExitError
			switch err := sh.Wait(); {
			case err == nil:
				won++
			case !errors.As(err, &lost):
				t.Fatalf("waiting on a claim: %v", err)
			}
		}
		if won != 1 {
			t.Fatalf("round %d: %d of two shells took one claim made at once, want one", round+1, won)
		}
	}
}

// claimRounds is how many rounds [TestAStandInsClaimIsTakenByOneProcess] makes.
//
// FIFTY: on CI's mkdir both shells took the claim in about three rounds of
// four (2178 of 3000, measured on ubuntu:26.04), so a claim that is not one
// process's has no round in fifty to hide in, at a hundred short shells.
const claimRounds = 50
