package executor_test

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
	"github.com/crewlet/crewlet/internal/shellword"
)

// Stand-in harnesses a case writes and then runs.
//
// A FILE THIS PROCESS WROTE CAN BE REFUSED WHEN RUN (ETXTBSY): a parallel case
// that forked while os.WriteFile held it open hands its child a writable
// descriptor, and Linux refuses to execute the file until that child execs.
// This package forks constantly, and a refusal surfaces as nothing a case
// asserts (no harness found, a version probe's fallback). So a stand-in is
// written by cp, which has exited before the case runs it.

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
	install := "cp " + shellword.Quote(source) + " " + shellword.Quote(path) + " && chmod +x " + shellword.Quote(path) + " && rm " + shellword.Quote(source)
	res, err := box.Exec(t.Context(), install, sandbox.ExecOptions{})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("install the stand-in at %s: %v (exit %d) %s", path, err, res.ExitCode, res.Stderr)
	}
}

// claim is shell that succeeds in the one process that first runs it on path
// and fails in every other, then and after: a stand-in's turn ahead of calls
// made beside it (the first of two packs to hold, say). It creates path under
// noclobber (`set -C`), which a POSIX shell opens O_CREAT|O_EXCL, an exclusive
// create; the subshell keeps the stand-in's later redirections free of it.
//
// NEVER `mkdir`: uutils coreutils' mkdir (Ubuntu 26.04's) through 0.10.0
// checks then creates, and reports success when a racing create made the
// directory, so two packs each took the first turn and waited on each other
// to the pack's two-minute bound (#946).
func claim(path string) string { return "( set -C; : > " + shellword.Quote(path) + " ) 2> /dev/null" }

// A STAND-IN RUNS THE MOMENT IT IS HANDED OVER, whatever this process is forking
// meanwhile: the guard for [writeStandIn]. Eight goroutines fork beside 64
// stand-ins written and run at once; the forks are the point, since only a
// descriptor a fork inherits refuses the run.
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

// A STAND-IN'S CLAIM IS TAKEN BY ONE PROCESS, however many make it at once: the
// guard for [claim]. Each round two shells wait at one start line, then claim
// one name together, and exactly one takes it.
//
// Mutation: claim with `mkdir`, and under uutils' mkdir through 0.10.0 (Ubuntu
// 26.04's) both shells take it in most rounds. GNU's mkdir is exclusive, so
// with GNU coreutils (CI's Ubuntu 24.04 runners) that mutant passes.
func TestAStandInsClaimIsTakenByOneProcess(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for round := range claimRounds {
		name := filepath.Join(dir, "claimed."+strconv.Itoa(round))
		start := filepath.Join(dir, "start."+strconv.Itoa(round))
		script := "while [ ! -e " + shellword.Quote(start) + " ]; do :; done; " + claim(name)
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

// claimRounds is how many rounds [TestAStandInsClaimIsTakenByOneProcess] makes:
// uutils' mkdir lost exclusivity in about three rounds of four, so a
// non-exclusive claim cannot survive fifty, at only a hundred short shells.
const claimRounds = 50

// boxPathWithout fills bin with every program this machine's PATH finds, as a
// box image has them, but those named in absent and the harness the case
// stands in, so a case about a box lacking a program runs every other part of
// the script on the tools a box has.
func boxPathWithout(t *testing.T, bin string, absent ...string) {
	t.Helper()
	skip := map[string]bool{"claude": true}
	for _, name := range absent {
		skip[name] = true
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if skip[name] || e.IsDir() {
				continue
			}
			path := filepath.Join(dir, name)
			if info, err := os.Stat(path); err != nil || info.IsDir() || info.Mode()&0o111 == 0 {
				continue
			}
			skip[name] = true // the first a PATH lookup finds
			if err := os.Symlink(path, filepath.Join(bin, name)); err != nil {
				t.Fatal(err)
			}
		}
	}
}
