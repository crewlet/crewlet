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

// A TEST BINARY THAT DIES TAKES ITS OWN PASS/FAIL RECORD WITH IT, and the
// output it left is then the only account of why.
//
// This is the shape of CI run 35312291605 on main. internal/engine failed and
// the whole of the evidence was one line — `FAIL …/internal/engine 133.609s`
// — because test2json had attributed the dying binary's output to whichever
// test was current, that test never reached a terminal record, and the buffer
// holding it was released by nothing. The verdict read "0 test(s) failed in
// 1 package(s)", which is true and says nothing.
//
// Both halves are asserted, because either alone leaves the run unreadable:
// the output has to REACH the log, and the note has to say why a package
// failed with no failing test in it.
func TestATestThatNeverReportedStillPrintsWhatItSaid(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	const pkg = "github.com/crewlet/crewlet/internal/engine"
	stream := strings.Join([]string{
		`{"Action":"run","Package":"` + pkg + `","Test":"TestDoomed"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestDoomed","Output":"=== RUN   TestDoomed\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestDoomed","Output":"panic: send on closed channel\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestDoomed","Output":"goroutine 1701 [running]:\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Output":"FAIL\tgithub.com/crewlet/crewlet/internal/engine\t133.609s\n"}`,
		`{"Action":"fail","Package":"` + pkg + `","Elapsed":133.609}`,
	}, "\n")

	r := read(bufio.NewScanner(strings.NewReader(stream)), f)
	f.Close()

	if len(r.failed) != 0 {
		t.Errorf("failed = %v, want none — the test never reported a result", r.failed)
	}
	if len(r.failedPkgs) != 1 {
		t.Errorf("failedPkgs = %v, want the one package", r.failedPkgs)
	}
	if len(r.unfinished) != 1 || r.unfinished[0] != "internal/engine TestDoomed" {
		t.Errorf("unfinished = %v, want the test that never reported", r.unfinished)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(raw)
	for _, want := range []string{
		"panic: send on closed channel",
		"goroutine 1701 [running]:",
		"TestDoomed",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("a dying binary's account was dropped — %q is not in the "+
				"rendered log, which is then the only thing a reader has:\n%s",
				want, rendered)
		}
	}

	_, note := Verdict(r, errors.New("exit status 1"), false)
	if !strings.Contains(note, "never reported a result") {
		t.Errorf("note = %q, want it to say why a package failed with no failing "+
			"test in it; the count alone reads as a contradiction", note)
	}
}

// WHAT ARRIVES AFTER A TEST'S OWN RESULT LINE IS THE PROCESS, NOT THE TEST.
//
// The other exit from the same hole, and the one that hid the panic in the
// experiment written to prove the first. test2json files under the last test
// it saw FRAMED, and `--- PASS: TestX` is a frame — so a goroutine that
// panics a moment after TestX returned lands under TestX, which then reports
// an ordinary `pass`, and a passing test's buffer is dropped whole. Measured
// end to end: a package whose leaked goroutine segfaulted after its test
// returned rendered as `FAIL dying 0.306s` and nothing else.
//
// Both halves are asserted here, because rescuing the tail is only correct if
// the test's OWN output is still dropped — otherwise this gate starts
// echoing every passing test and becomes the one nobody keeps.
func TestWhatFollowsATestsResultLineIsRescued(t *testing.T) {
	t.Parallel()

	const pkg = "github.com/crewlet/crewlet/internal/engine"
	stream := strings.Join([]string{
		`{"Action":"output","Package":"` + pkg + `","Test":"TestLeaks","Output":"=== RUN   TestLeaks\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestLeaks","Output":"    engine_test.go:9: a log nobody should see\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestLeaks","Output":"--- PASS: TestLeaks (0.10s)\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestLeaks","Output":"panic: nil map write\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestLeaks","Output":"created by engine.TestLeaks in goroutine 18\n"}`,
		`{"Action":"pass","Package":"` + pkg + `","Test":"TestLeaks"}`,
		`{"Action":"fail","Package":"` + pkg + `"}`,
	}, "\n")

	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	r := read(bufio.NewScanner(strings.NewReader(stream)), f)
	f.Close()

	if len(r.afterResult) != 1 || r.afterResult[0] != "internal/engine TestLeaks" {
		t.Errorf("afterResult = %v, want the test the binary talked over", r.afterResult)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(raw)
	if !strings.Contains(rendered, "panic: nil map write") {
		t.Errorf("the panic was dropped because the test it was filed under "+
			"passed; that is the whole defect:\n%s", rendered)
	}
	if strings.Contains(rendered, "a log nobody should see") {
		t.Errorf("a passing test's OWN output was echoed. Only what follows its "+
			"result line is the process talking; the rest is what this gate "+
			"exists not to print:\n%s", rendered)
	}
}

// AND A GREEN PACKAGE SPENDS NONE OF IT.
//
// Only a failure has something to explain. Plenty of processes talk
// harmlessly after a test returns — measured over one `make test`, 94 passing
// tests have a goroutine still logging when they do — so a gate that printed
// every tail would have added 94 headers to a green log, which is the cost
// [read] exists to avoid and the reason a gate gets taken back out.
func TestAGreenPackagesAftermathIsDropped(t *testing.T) {
	t.Parallel()

	const pkg = "github.com/crewlet/crewlet/internal/api/livestate"
	stream := strings.Join([]string{
		`{"Action":"output","Package":"` + pkg + `","Test":"TestFine","Output":"=== RUN   TestFine\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestFine","Output":"--- PASS: TestFine (0.10s)\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestFine","Output":"INFO api.livestate a goroutine still logging\n"}`,
		`{"Action":"pass","Package":"` + pkg + `","Test":"TestFine"}`,
		`{"Action":"output","Package":"` + pkg + `","Output":"ok  \tinternal/api/livestate\t5.7s\n"}`,
		`{"Action":"pass","Package":"` + pkg + `"}`,
	}, "\n")

	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	r := read(bufio.NewScanner(strings.NewReader(stream)), f)
	f.Close()

	if len(r.afterResult) != 0 {
		t.Errorf("afterResult = %v, want none — the package passed, so there is "+
			"nothing to explain", r.afterResult)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rendered := string(raw); strings.Contains(rendered, "a goroutine still logging") {
		t.Errorf("a passing package's aftermath was printed. 94 of these in one "+
			"`make test` is what makes a gate get removed:\n%s", rendered)
	}
}

// A PARKED TEST IS COUNTED, NOT PRINTED.
//
// When a binary dies, every test that had called t.Parallel() is sitting
// between `=== PAUSE` and a resume that never comes, and its buffer holds
// test2json's framing and nothing else. Measured on the real failure: 341
// orphans, 340 of them parked — printing a header for each buried the one
// that carried the panic, which is the same defect as dropping it.
func TestParkedTestsAreCountedRatherThanPrinted(t *testing.T) {
	t.Parallel()

	const pkg = "github.com/crewlet/crewlet/internal/engine"
	stream := strings.Join([]string{
		`{"Action":"output","Package":"` + pkg + `","Test":"TestParked","Output":"=== RUN   TestParked\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestParked","Output":"=== PAUSE TestParked\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestSpoke","Output":"=== RUN   TestSpoke\n"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestSpoke","Output":"panic: the one account\n"}`,
		`{"Action":"fail","Package":"` + pkg + `"}`,
	}, "\n")

	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	r := read(bufio.NewScanner(strings.NewReader(stream)), f)
	f.Close()

	if r.parked != 1 {
		t.Errorf("parked = %d, want the one test that said nothing", r.parked)
	}
	if len(r.unfinished) != 1 || r.unfinished[0] != "internal/engine TestSpoke" {
		t.Errorf("unfinished = %v, want only the one with an account", r.unfinished)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rendered := string(raw); strings.Contains(rendered, "TestParked never reported") {
		t.Errorf("a parked test got a header of its own; 340 of those are what "+
			"buried the panic on the real failure:\n%s", rendered)
	}
	if _, note := Verdict(r, errors.New("exit status 1"), false); !strings.Contains(note, "in flight") {
		t.Errorf("note = %q, want the parked count — it is how a reader tells a "+
			"binary that stopped mid-run from one that stopped at the end", note)
	}
}

// AND IT PRINTS BESIDE ITS OWN RESULT LINE.
//
// `go test` interleaves package blocks — at -p 4 over three packages the
// order came back i1, i2, i1, i2, i3, … — so a drain that only ran when the
// stream ended would put the panic at the bottom of the log, separated from
// the `FAIL pkg` line it explains by every other package in the run. Over
// `make test`'s partition that is the whole log.
func TestADyingPackagesAccountLandsBesideItsOwnResultLine(t *testing.T) {
	t.Parallel()

	const dying = "github.com/crewlet/crewlet/internal/engine"
	const after = "github.com/crewlet/crewlet/internal/notify"
	stream := strings.Join([]string{
		`{"Action":"output","Package":"` + dying + `","Test":"TestDoomed","Output":"panic: the account\n"}`,
		`{"Action":"output","Package":"` + after + `","Test":"TestFine","Output":"=== RUN   TestFine\n"}`,
		`{"Action":"output","Package":"` + dying + `","Output":"FAIL\tinternal/engine\t133.609s\n"}`,
		`{"Action":"fail","Package":"` + dying + `"}`,
		`{"Action":"pass","Package":"` + after + `","Test":"TestFine"}`,
		`{"Action":"output","Package":"` + after + `","Output":"ok  \tinternal/notify\t1.6s\n"}`,
		`{"Action":"pass","Package":"` + after + `"}`,
	}, "\n")

	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	read(bufio.NewScanner(strings.NewReader(stream)), f)
	f.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rendered := string(raw)
	account := strings.Index(rendered, "panic: the account")
	laterPkg := strings.Index(rendered, "ok  \tinternal/notify")
	switch {
	case account < 0:
		t.Fatalf("the account was not rendered at all:\n%s", rendered)
	case laterPkg < 0:
		t.Fatalf("the later package's result line is missing:\n%s", rendered)
	case account > laterPkg:
		t.Errorf("the account printed after a later package's result line, so it "+
			"drained at end of stream rather than beside its own FAIL. Over a "+
			"200-package run that is the length of the whole log:\n%s", rendered)
	}
}

// AND A RUN THAT FINISHED PAYS NOTHING FOR IT.
//
// The flush above is a promise about the failing case only. Every terminal
// record already flushes or drops its own buffer, so a green stream leaves
// nothing behind — if it did, this gate would start echoing a line per
// passing subtest, which is the cost [read] exists to avoid and the reason a
// gate gets taken back out.
func TestAFinishedRunLeavesNothingBuffered(t *testing.T) {
	t.Parallel()

	const pkg = "github.com/crewlet/crewlet/internal/x"
	stream := strings.Join([]string{
		`{"Action":"output","Package":"` + pkg + `","Test":"TestQuiet","Output":"noise from a passing test\n"}`,
		`{"Action":"pass","Package":"` + pkg + `","Test":"TestQuiet"}`,
		`{"Action":"output","Package":"` + pkg + `","Test":"TestGone","Output":"noise from a skipped test\n"}`,
		`{"Action":"skip","Package":"` + pkg + `","Test":"TestGone"}`,
		`{"Action":"output","Package":"` + pkg + `","Output":"ok  \tinternal/x\t1.2s\n"}`,
		`{"Action":"pass","Package":"` + pkg + `"}`,
	}, "\n")

	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	r := read(bufio.NewScanner(strings.NewReader(stream)), f)
	f.Close()

	if len(r.unfinished) != 0 {
		t.Errorf("unfinished = %v, want none — every test reported", r.unfinished)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if rendered := string(raw); strings.Contains(rendered, "noise from") {
		t.Errorf("a finished run echoed a non-failing test's output; that is the "+
			"cost this gate exists not to impose:\n%s", rendered)
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
			r:        report{ran: ran("internal/a", "internal/b"), tests: 3},
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
			r:        report{ran: ran("internal/a"), tests: 4, failed: []string{"internal/a TestX"}},
			producer: errors.New("exit status 1"),
			code:     1,
			says:     "1 test(s) failed",
		},
		{
			name:     "a build failure, which has no failing test in it",
			r:        report{ran: ran("internal/a"), tests: 0, failedPkgs: []string{"internal/a"}},
			producer: errors.New("exit status 2"),
			code:     1,
			says:     "in 1 package(s)",
		},
		{
			name: "a package failed with no failing test, because a binary died",
			r: report{
				ran:        ran("internal/engine"),
				tests:      587,
				failedPkgs: []string{"internal/engine"},
				unfinished: []string{"internal/engine TestDoomed"},
			},
			producer: errors.New("exit status 1"),
			code:     1,
			says:     "never reported a result",
		},
		{
			// The ordinary shape of a dying binary: the producer exits
			// non-zero and the stream carries no failure record. That is
			// already "the run did not finish" — what changes is that the
			// note now NAMES the test it stopped in, which is the whole
			// difference between a debuggable log and run 35312291605.
			name: "killed mid-test, and the note names the test",
			r: report{
				ran:        ran("internal/engine"),
				tests:      587,
				unfinished: []string{"internal/engine TestDoomed"},
			},
			producer: killed,
			code:     1,
			says:     "internal/engine TestDoomed",
		},
		{
			// The defensive half: a producer that exited 0 over a test that
			// never reported. Nothing in the tree is known to produce it,
			// and a gate whose only answer to it would be "clean run" is the
			// shape this program exists to refuse.
			name: "a test never reported although the command exited 0",
			r: report{
				ran:        ran("internal/engine"),
				tests:      587,
				unfinished: []string{"internal/engine TestDoomed"},
			},
			code: 1,
			says: "never reported a result",
		},
		{
			name:   "declarations disagree",
			r:      report{ran: ran("internal/a"), tests: 7},
			broken: true,
			code:   1,
			says:   "disagree",
		},
		{
			name: "a clean run",
			r:    report{ran: ran("internal/a", "internal/b"), tests: 12},
			code: 0,
			says: "12 test(s), none skipped",
		},
		{
			name: "a clean run with declared skips",
			r: report{
				ran:     ran("internal/a"),
				tests:   5,
				skipped: []Skip{{Package: "internal/a", Test: "TestT"}},
			},
			code: 0,
			says: "1 skip(s) of 5 test(s)",
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
