package main

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// ran is the package set a case says the run covered.
func ran(pkgs ...string) map[string]bool {
	out := map[string]bool{}
	for _, p := range pkgs {
		out[p] = true
	}
	return out
}

// THE UNLISTED DIRECTION. A skip nothing declares is the whole point.
func TestAnUndeclaredSkipIsReported(t *testing.T) {
	t.Parallel()

	unlisted, stale := Judge([]Skip{
		{Package: "internal/nowhere", Test: "TestSomethingQuietlyOff"},
	}, ran("internal/nowhere"))

	if len(unlisted) != 1 || unlisted[0].Test != "TestSomethingQuietlyOff" {
		t.Errorf("unlisted = %v, want the one undeclared skip", unlisted)
	}
	if len(stale) != 0 {
		t.Errorf("stale = %v, want none — no Always entry names that package", stale)
	}
}

// A declared skip passes, whichever When it carries.
func TestADeclaredSkipIsAccepted(t *testing.T) {
	t.Parallel()

	for _, a := range allowed {
		if a.When != Environment {
			continue
		}
		unlisted, _ := Judge([]Skip{{Package: a.Package, Test: a.Test}}, ran(a.Package))
		if len(unlisted) != 0 {
			t.Errorf("%s %s is on the allowlist and was reported unlisted", a.Package, a.Test)
		}
		return
	}
	t.Fatal("no Environment entry to exercise; this case is asserting about nothing")
}

// THE STALENESS DIRECTION, and the scoping that makes it usable.
//
// Both halves matter and the second is why this gate was briefly unusable: an
// Always entry that did not fire is stale ONLY if its package ran. `make test`
// and `make test-solo` each cover one half of the tree, so without the
// scoping each target fails on the other's entries.
func TestAnAlwaysEntryIsStaleOnlyWhenItsPackageRan(t *testing.T) {
	t.Parallel()

	var probe Allowance
	for _, a := range allowed {
		if a.When == Always {
			probe = a
			break
		}
	}
	if probe.Test == "" {
		t.Fatal("no Always entry to exercise; this case is asserting about nothing")
	}

	t.Run("its package ran and it did not skip", func(t *testing.T) {
		t.Parallel()
		_, stale := Judge(nil, ran(probe.Package))
		if !slices.ContainsFunc(stale, func(s Skip) bool { return s.Test == probe.Test }) {
			t.Errorf("stale = %v, want %s — its package ran and it did not skip",
				stale, probe.Test)
		}
	})

	t.Run("its package did not run", func(t *testing.T) {
		t.Parallel()
		_, stale := Judge(nil, ran("internal/somewhere-else"))
		if slices.ContainsFunc(stale, func(s Skip) bool { return s.Test == probe.Test }) {
			t.Errorf("%s was called stale by a run that did not cover its package; "+
				"each test target covers one half of the tree", probe.Test)
		}
	})
}

// A PACKAGE-LEVEL skip record is not a test skipping.
//
// `go test -json` emits Action "skip" with no Test name for a package that has
// no test files, and this tree has several. Counting those reports a number
// nobody can act on and goes red the day somebody adds a package without tests
// — the trap any naive `jq 'select(.Action=="skip")'` falls into.
func TestAPackageWithNoTestFilesIsNotASkippedTest(t *testing.T) {
	t.Parallel()

	stream := strings.Join([]string{
		`{"Action":"skip","Package":"github.com/crewlet/crewlet/static"}`,
		`{"Action":"skip","Package":"github.com/crewlet/crewlet/internal/x","Test":"TestReal"}`,
	}, "\n")

	r := read(bufio.NewScanner(strings.NewReader(stream)), devNull(t))
	if len(r.skipped) != 1 || r.skipped[0].Test != "TestReal" {
		t.Errorf("skipped = %v, want only the named test", r.skipped)
	}
	if !r.ran["static"] || !r.ran["internal/x"] {
		t.Errorf("ran = %v, want both packages — a package-level record still says it ran", r.ran)
	}
}

// IT RENDERS WHAT PLAIN `go test` RENDERS, and carries the verdict.
//
// Three properties, and each is load-bearing. A FAILING test's output is
// flushed, because that is the only thing anybody debugs from. A PASSING
// test's is not — `-json` implies verbose, so echoing everything would print a
// line per passing subtest and make a CI log tens of times longer than before
// this joined the pipeline, which is how a gate gets taken back out. And a
// failure is REPORTED, because in a pipeline make sees the last command's
// status, so `go test`'s own exit code is gone by the time this runs.
func TestItRendersLikePlainGoTestAndCarriesTheVerdict(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const pkg = "github.com/crewlet/crewlet/internal/x"
	stream := strings.Join([]string{
		`{"Action":"output","Package":"` + pkg + `","Test":"TestQuiet","Output":"noise from a passing test\n"}`,
		`{"Action":"pass","Package":"` + pkg + `","Test":"TestQuiet"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestA","Output":"--- FAIL: TestA\n"}`,
		`{"Action":"fail","Package":"` + pkg + `","Test":"TestA"}`,
		`{"Action":"output","Package":"` + pkg + `","Output":"FAIL\tinternal/x\t1.2s\n"}`,
		`not json at all, a toolchain line`,
	}, "\n")

	r := read(bufio.NewScanner(strings.NewReader(stream)), f)
	f.Close()

	if len(r.failed) != 1 {
		t.Errorf("failed = %v, want the one failure", r.failed)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(raw)

	if !strings.Contains(rendered, "--- FAIL: TestA") {
		t.Errorf("a failing test's output was not flushed, which is the only "+
			"thing anybody debugs from:\n%s", rendered)
	}
	if strings.Contains(rendered, "noise from a passing test") {
		t.Errorf("a passing test's output was echoed; -json implies verbose, so "+
			"this makes every CI log tens of times longer than plain `go test`:\n%s", rendered)
	}
	if !strings.Contains(rendered, "FAIL\tinternal/x") {
		t.Errorf("the package result line was not rendered:\n%s", rendered)
	}
	if !strings.Contains(rendered, "not json at all") {
		t.Errorf("a non-JSON line was swallowed; go test writes build errors "+
			"around the stream and a gate that ate them hides the one failure "+
			"nobody can debug without them:\n%s", rendered)
	}
}

// A BUILD FAILURE IS A FAILURE, and it looks like nothing from in here.
//
// When a package does not compile, `go test -json` emits a package-level
// `fail` with no per-test record at all — nothing was built, so no test ever
// ran. A gate counting only named tests hands make a zero exit over a tree
// that does not build, and it is the LAST command in the pipeline, so its
// status is the only one make sees. Measured before failedPkgs existed:
// internal/textcut with an undefined symbol in a _test.go file went through
// this program and exited 0.
func TestABuildFailureIsCarried(t *testing.T) {
	t.Parallel()

	const pkg = "github.com/crewlet/crewlet/internal/x"
	stream := strings.Join([]string{
		`{"Action":"output","Package":"` + pkg + `","Output":"# ` + pkg + ` [test]\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Output":"./x_test.go:5:1: undefined: nope\n"}`,
		`{"Action":"fail","Package":"` + pkg + `"}`,
	}, "\n")

	r := read(bufio.NewScanner(strings.NewReader(stream)), devNull(t))

	if len(r.failed) != 0 {
		t.Errorf("failed = %v, want none — nothing compiled, so no test ran", r.failed)
	}
	if len(r.failedPkgs) != 1 || r.failedPkgs[0] != "internal/x" {
		t.Errorf("failedPkgs = %v, want the package that would not build", r.failedPkgs)
	}
}

// THE VERDICT, and the two ways a build reports green over a suite that did
// not run. Both were review findings on the shape this replaced.
//
// The gate used to be the LAST command of a shell pipeline, so make saw only
// its status and `go test`'s was gone. A producer killed mid-run — an OOM, a
// signal, a runner going away — emits a truncated stream with no failure
// record in it, and every other check here reads that as a clean pass. And a
// producer that died before its FIRST record left the gate printing "no test
// skipped" and exiting 0, certifying nothing.
func TestARunThatDidNotFinishIsNotAPass(t *testing.T) {
	t.Parallel()

	killed := errors.New("signal: killed")

	cases := []struct {
		name     string
		r        report
		producer error
		broken   bool
		code     int
		says     string
	}{
		{
			name:     "killed after some packages, no failure recorded",
			r:        report{ran: ran("internal/a", "internal/b")},
			producer: killed,
			code:     1,
			says:     "did not finish",
		},
		{
			name:     "died before emitting anything",
			r:        report{ran: map[string]bool{}},
			producer: killed,
			code:     1,
			says:     "did not finish",
		},
		{
			name: "exited 0 having reported no package at all",
			r:    report{ran: map[string]bool{}},
			code: 1,
			says: "no suite ran",
		},
		{
			name:     "a real failure is reported as one, not as an unfinished run",
			r:        report{ran: ran("internal/a"), failed: []string{"internal/a TestX"}},
			producer: errors.New("exit status 1"),
			code:     1,
			says:     "1 test(s) failed",
		},
		{
			name:     "a build failure, which has no failing test in it",
			r:        report{ran: ran("internal/a"), failedPkgs: []string{"internal/a"}},
			producer: errors.New("exit status 2"),
			code:     1,
			says:     "in 1 package(s)",
		},
		{
			name:   "declarations disagree",
			r:      report{ran: ran("internal/a")},
			broken: true,
			code:   1,
			says:   "disagree",
		},
		{
			name: "a clean run",
			r:    report{ran: ran("internal/a", "internal/b")},
			code: 0,
			says: "no test skipped across 2",
		},
		{
			name: "a clean run with declared skips",
			r: report{
				ran:     ran("internal/a"),
				skipped: []Skip{{Package: "internal/a", Test: "TestT"}},
			},
			code: 0,
			says: "1 skip(s) across 1 package(s)",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			code, note := Verdict(c.r, c.producer, c.broken)
			if code != c.code {
				t.Errorf("exit = %d, want %d (%s)", code, c.code, note)
			}
			if !strings.Contains(note, c.says) {
				t.Errorf("note = %q, want it to mention %q", note, c.says)
			}
		})
	}
}

// EVERY ENTRY CARRIES A REASON, and the reason has a job.
//
// An allowlist whose entries say "backend cannot" is a list of excuses; the
// ones worth having say where the coverage lives instead. This cannot check
// that a sentence is true, so it checks the two things it can: that somebody
// wrote one, and that the When is a value this program acts on.
func TestEveryAllowanceIsWellFormed(t *testing.T) {
	t.Parallel()

	seen := map[Skip]bool{}
	for _, a := range allowed {
		key := Skip{Package: a.Package, Test: a.Test}
		switch {
		case a.Package == "" || a.Test == "":
			t.Errorf("an allowance names no test: %+v", a)
		case a.When != Always && a.When != Environment:
			t.Errorf("%s %s has When %q, which this gate does not act on",
				a.Package, a.Test, a.When)
		case len(a.Why) < 40:
			t.Errorf("%s %s has no real reason (%q); an entry must say what is "+
				"NOT being checked and where that coverage lives instead",
				a.Package, a.Test, a.Why)
		case seen[key]:
			t.Errorf("%s %s is listed twice", a.Package, a.Test)
		}
		seen[key] = true
	}
	if len(allowed) == 0 {
		t.Fatal("the allowlist is empty; this guard is asserting about nothing")
	}
	t.Logf("%d declared skips", len(allowed))
}

func devNull(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
