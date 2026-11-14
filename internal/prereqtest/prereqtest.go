// Package prereqtest is the one rule for a program the suite needs that a
// machine may lack — a runtime a case drives a script under, found on PATH, or
// a build a make target fetches and names by a variable. Under the make
// targets that run the suite (`test`, `test-solo`, `test-norace`,
// `test-gitproc`, `bench`) a missing one FAILS the case; in a bare `go test`
// outside make it SKIPS, in the same words.
//
// # Why both, and why the make targets decide
//
// A prerequisite is never something a machine may lack (CONTRIBUTING.md, "A
// skip is not a pass"): a run that skipped a case for want of one has checked
// nothing that case holds, and reported a pass. So wherever a run is a gate it
// fails. But a one-package loop is how a contributor works on a package, and
// failing that loop for a program its own change never touches would make it
// unusable for no gain: nothing gates on that run. The line between the two is
// the one the tree already draws between a gate and a loop — the make targets
// — so each of those five sets [RequireEnv] to 1 (the Makefile's REQUIRED) and
// nothing else does. ci.yml runs Go tests only through those targets, so CI is
// held by the same variable.
//
// Never the CI variable: no make target sets it, so `make check` on a
// workstation would skip what CI fails, and a contributor's shell may set it
// or not for reasons of its own. And never a variable per prerequisite: one
// rule read under several names is several rules, each set by some targets and
// forgotten by others.
//
// The skip a bare `go test` takes is declared nowhere in
// internal/skipgate/allowed.go, and that is deliberate: skipgate runs only
// under the make targets, where the case fails instead, so an entry there would
// be an allowance nothing could ever use.
//
// # Test support, never a case's own logic
//
// Its name ends in "test", as the standard library's test-support packages do,
// so golangci-lint's harness rule and the words gate treat it as test code, and
// so does internal/fixedwait: it holds no wait.
package prereqtest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// RequireEnv is the variable that makes a missing prerequisite a failure: the
// make targets that run the suite set it to 1 (the Makefile's REQUIRED), and
// nothing reads it but this package.
const RequireEnv = "CREWLET_REQUIRE_PREREQUISITES"

// Required reports whether this run requires every prerequisite: true when
// [RequireEnv] is 1, false when it is unset or empty, and an error naming the
// variable for anything else, which is a mistake rather than either answer.
func Required() (bool, error) {
	switch v := os.Getenv(RequireEnv); v {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("%s=%q: set it to 1 (every prerequisite of the suite is required, as the make "+
			"targets that run the suite set it) or leave it unset", RequireEnv, v)
	}
}

// Missing ends t for a prerequisite this machine lacks. absent says what is
// missing and what needs it ("node is not on PATH, and the dashboard client
// replay runs under it"); remedy says what makes it present ("install any
// current node, as ci.yml does"). Where [Required] is true it FAILS t; where it
// is false it SKIPS t, in the same words; where it cannot tell it fails naming
// [RequireEnv].
func Missing(t testing.TB, absent, remedy string) {
	t.Helper()
	required, err := Required()
	if err != nil {
		t.Fatal(err)
	}
	if required {
		t.Fatalf("%s: %s. %s=1, which the make targets that run the suite set, requires it, so it fails here "+
			"rather than skip", absent, remedy, RequireEnv)
	}
	t.Skipf("%s: %s. Skipped, as only a bare `go test` outside make does: the make targets that run the suite "+
		"set %s=1, which requires it", absent, remedy, RequireEnv)
}

// OnPath is the program name as PATH finds it, for a case that runs it. Not on
// PATH, it is [Missing], absent saying that and needs saying what runs under
// it.
func OnPath(t testing.TB, name, needs, remedy string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		Missing(t, fmt.Sprintf("%s is not on PATH, and %s (%v)", name, needs, err), remedy)
	}
	return path
}

// Named is the program env names, for a prerequisite the make targets name by
// a variable rather than put on PATH: an absolute path to an executable file,
// handed back. Unset (or empty), it is [Missing], absent naming env and what,
// what the program is. Named but not such a path, it FAILS t naming env under
// every setting of [RequireEnv]: a value somebody set is a mistake to correct,
// never a reason to skip.
func Named(t testing.TB, env, what, remedy string) string {
	t.Helper()
	path := os.Getenv(env)
	if path == "" {
		Missing(t, fmt.Sprintf("%s is unset, and it names %s", env, what), remedy)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("%s=%q is not an absolute path to %s: %s", env, path, what, remedy)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("%s=%s names nothing this case can run (%v): %s", env, path, err, remedy)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("%s=%s is not an executable file: %s", env, path, remedy)
	}
	return path
}

// Ended is how a helper handed to [Run] ended: Fatal is the failure it
// reported, Skip the skip, and both are empty when it returned.
type Ended struct {
	Fatal string
	Skip  string
}

// Run runs f with a testing.TB that records the first failure or skip f is
// handed and ends the goroutine running f there, as the real ones do, so a
// case can hold a helper to how it ends a case without ending its own. Every
// other method is t's.
//
// f runs on a goroutine of its own, so a Fatal stops f and nothing after it,
// and Run returns once f has returned or been stopped.
func Run(t testing.TB, f func(testing.TB)) Ended {
	t.Helper()
	r := &recorder{TB: t}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f(r)
	}()
	<-done
	return Ended{Fatal: r.fatal, Skip: r.skip}
}

// recorder is the testing.TB [Run] hands its helper.
type recorder struct {
	testing.TB
	fatal, skip string
}

// Helper does nothing: the recorder reports no line of its own.
func (r *recorder) Helper() {}

func (r *recorder) Fatal(args ...any) { r.stop(&r.fatal, fmt.Sprint(args...)) }

func (r *recorder) Fatalf(format string, args ...any) { r.stop(&r.fatal, fmt.Sprintf(format, args...)) }

func (r *recorder) FailNow() { r.stop(&r.fatal, "FailNow") }

func (r *recorder) Skip(args ...any) { r.stop(&r.skip, fmt.Sprint(args...)) }

func (r *recorder) Skipf(format string, args ...any) { r.stop(&r.skip, fmt.Sprintf(format, args...)) }

func (r *recorder) SkipNow() { r.stop(&r.skip, "SkipNow") }

// stop records message in field and ends the goroutine running the helper,
// the way testing's own Fatal and Skip end a case's.
func (r *recorder) stop(field *string, message string) {
	*field = message
	runtime.Goexit()
}

// ReadPins is the NAME=value lines of the pin file at path (scripts/git-pin.env,
// scripts/chromium-pin.env), its comments and blank lines skipped. A file it
// cannot read, a line that is neither, a name set twice, or an assignment with
// space or a carriage return around it fails t. The recipes that read the same
// file (scripts/git.sh, scripts/chromium.sh) take a value from a line that
// begins NAME= and runs to its newline, and refuse a name set twice: an
// indented assignment is no assignment to them, and a trailing blank or a
// carriage return is part of the value they read, which their checks then
// refuse. So a case never reads a value they would not.
func ReadPins(t testing.TB, path string) map[string]string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pins := map[string]string{}
	for line := range strings.Lines(string(body)) {
		line = strings.TrimSuffix(line, "\n")
		bare := strings.TrimSpace(line)
		if bare == "" || strings.HasPrefix(bare, "#") {
			continue
		}
		if line != bare {
			t.Fatalf("%s: %q has space or a carriage return around it, which the recipes reading the file do not "+
				"read as NAME=value: write NAME=value alone on its line", path, line)
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("%s: %q is neither a comment nor NAME=value", path, line)
		}
		if _, dup := pins[name]; dup {
			t.Fatalf("%s sets %s twice", path, name)
		}
		pins[name] = value
	}
	return pins
}
