// Command skipgate runs a `go test -json` command, renders its stream, and
// fails the run when a test skipped that nobody declared could.
//
//	go run ./internal/skipgate -- go test -json <packages>
//
// IT RUNS THE COMMAND rather than reading a pipe, and that is a correctness
// decision rather than a convenience. As a pipe consumer this program was the
// LAST command in the pipeline, so make saw only ITS status — and `go test`'s
// was gone. A producer killed mid-run (an OOM, a signal, a runner going away)
// emits a truncated stream with no package-level `fail` record in it, and the
// gate then reported a green build over a test run that never finished. Under
// make's default /bin/sh there is no PIPESTATUS to recover it with, and
// $${PIPESTATUS[0]} would have meant changing the shell for every recipe in
// the file. Running the command removes the question: the exit status is
// returned to the process that needs it.
//
// It is the enforcement CONTRIBUTING.md's "A skip is not a pass" section
// promised and nothing supplied. The doctrine named three external
// prerequisites — node, npm and a store cache directory — all of which are
// already covered by preflight guards, and said nothing about the other fifty
// skip sites in the tree. The gate for it was a checkbox in the pull request
// template.
//
// What a convention held by memory decays into is written down one file over,
// at .github/workflows/ci.yml's sign-off job: "Nothing checked this until it
// existed, and 61 of the 361 non-merge commits behind it carry no trailer."
// The skip rule had already decayed the same way, and provably:
//
//   - internal/api/queries' Integrations gate read a dashboard path the React
//     rewrite deleted, so it skipped and certified NOTHING, on every machine
//     and in CI, for the whole of that rewrite and beyond. Its sibling in the
//     same file had already been fixed for the identical bug.
//   - A queue conformance case skipped on BOTH backends and therefore ran
//     nowhere: two per-backend skips, each defensible alone, multiplying into
//     total coverage loss that no single-backend run could show.
//
// Neither is visible in a CI log. `go test` prints nothing about a skipped
// subtest without -v, and the suite job does not pass it.
//
// # Why an allowlist rather than a count or a ban
//
// A blanket ban is wrong, and CLAUDE.md says so about the tree's best skips:
// internal/store's capability cases "skip DELIBERATELY, and that is the
// mechanism rather than a gap: a feature Turso announces but does not reach Go
// yet turns into a passing test the day it lands." A COUNT is worse than
// either — it goes green when one skip is fixed and another appears, which is
// exactly the swap nobody would notice.
//
// So the allowlist is keyed on the test's own name and carries a reason, in
// allowed.go, and it is TWO-SIDED in the one direction that can be: an entry
// marked [Always] that did not skip is a stale entry and fails, because a
// structural skip that stopped skipping means the thing it described changed.
// An [Environment] entry is only checked in the unlisted direction, since
// whether it fires is a fact about the machine.
//
// # The report a CI shard leaves behind
//
//	go run ./internal/skipgate -report DIR -- go test -json <packages>
//
// writes two files into DIR, from the package-level records this program
// already reads to reach its verdict:
//
//   - ran.txt, every package whose test binary reported a result (pass, fail,
//     or skip for a package with no test files), one import path a line. ci.yml
//     runs each half of the suite as SHARDS on separate runners, and its
//     `tests` job holds the shards' ran.txt against the half they were cut
//     from: every package in exactly one shard, none in two, none in neither.
//     That is the cross-job half of an exact-cover guarantee whose in-process
//     half is internal/solo/partition's own check, and it is read from what
//     RAN rather than from what was planned, so it also catches a shard whose
//     command line was not the list the partition printed.
//   - timings.tsv, `importpath<TAB>seconds` for every package that passed or
//     had no tests, which is the file internal/solo/partition reads with
//     -weights to start the longest packages first and to balance the shards.
//     A FAILED package is left out: its elapsed time stops wherever the
//     failure stopped it, so it is not a measurement of the package, and
//     ci.yml keeps the timings of passing runs only.
//
// Both are written whatever the verdict, so a red shard's report still says
// what it got through; a report that cannot be written fails the run, since
// the job that reads it would otherwise fail later and say less.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// event is the slice of test2json's schema this needs.
type event struct {
	Action  string
	Package string
	Test    string
	Output  string
	// Elapsed is the seconds a package's test binary ran, on its
	// package-level pass, fail or skip record (and a test's, on its own).
	Elapsed float64
}

// finish is how one package's test binary ended.
type finish struct {
	// action is the package-level record's: pass, fail, or skip — the last
	// meaning the package has no test files.
	action  string
	elapsed float64
}

// short drops the module prefix, so an allowlist entry reads as a path.
func short(pkg string) string {
	return strings.TrimPrefix(pkg, "github.com/crewlet/crewlet/")
}

// report is what one stream amounted to.
type report struct {
	skipped []Skip
	failed  []string
	// failedPkgs is the PACKAGE-level failures, and it is not redundant with
	// failed. A BUILD failure produces a package-level `fail` record and no
	// per-test record at all — nothing compiled, so no test ever ran — and a
	// gate reading only named tests would hand make a zero exit over a tree
	// that does not build. Measured before this field existed: a package with
	// an undefined symbol in a _test.go file went through here and exited 0.
	failedPkgs []string
	// orphaned is how many tests had output buffered and never reported a
	// result at all, which is what a binary that DIED under a test looks like
	// from here. The output itself is printed; this is the count, so the
	// verdict can say whether a package that failed with no failing test said
	// anything at all.
	orphaned int

	// tests is how many NAMED tests reported a result — passed, failed or
	// skipped.
	//
	// Separate from ran, and the separation is the whole vacuous-pass guard.
	// A package with NO TEST FILES still emits a package-level record, so a
	// run containing only such packages has a non-empty `ran` and would have
	// satisfied a guard written on it — reporting success with not one test
	// executed, which is the exact shape this program exists to refuse. `ran`
	// answers "which packages did this run cover", which is what the stale
	// check needs; this answers "did anything actually run".
	tests int

	// measuredSeen is which declared measurements actually reported, so a
	// renamed one is caught rather than silently stopping.
	measuredSeen map[string]bool

	// ran is every package the stream carried a record for.
	//
	// Load-bearing for the staleness half: BOTH test targets run a SUBSET of
	// the tree — `make test` the shared partition, `make test-solo` the rest,
	// and in CI each job runs one SHARD of its half — so without this, every
	// Always entry belonging to another run reads as "declared and did not
	// skip" and each run fails on the others' entries. Staleness is only a
	// question about a package that ran.
	ran map[string]bool

	// finished is every package whose binary reported its own result, by
	// FULL import path — the form `go test` was handed and the partition
	// prints, which is what the -report files are compared against. Narrower
	// than ran: a stream cut off mid-package has records for it and no result.
	finished map[string]finish
}

// read consumes a test2json stream and renders it back as PLAIN `go test`
// output — package result lines, and a failing test's own output.
//
// NOT the whole stream. `-json` implies verbose, so echoing every Output field
// would print a line per passing subtest and make a CI log tens of times
// longer than it was before this was in the pipeline. A gate that does that is
// a gate somebody takes back out, and then the skips are invisible again. So
// per-test output is BUFFERED and flushed only when that test fails, which is
// what `go test` without -v does; package-level records are printed as they
// come, and those carry the `ok  pkg  1.234s` lines.
//
// A line that is not JSON is passed through rather than rejected: `go test`
// writes build errors and toolchain chatter around the stream, and a gate that
// swallowed those would hide the one failure nobody can debug without them.
func read(in *bufio.Scanner, out *os.File) report {
	r := report{ran: map[string]bool{}, measuredSeen: map[string]bool{}, finished: map[string]finish{}}
	buffered := map[string][]string{}

	for in.Scan() {
		line := in.Bytes()
		if len(line) == 0 || line[0] != '{' {
			fmt.Fprintln(out, string(line))
			continue
		}
		var e event
		if err := json.Unmarshal(line, &e); err != nil {
			fmt.Fprintln(out, string(line))
			continue
		}
		if e.Package != "" {
			r.ran[short(e.Package)] = true
		}

		// Test == "" is the PACKAGE-level record, and it is the one that
		// carries the result line a reader is looking for. It is also why a
		// naive `jq 'select(.Action=="skip")'` over-counts: a package with NO
		// TEST FILES reports Action "skip" with no test name, and this tree
		// has several.
		if e.Test == "" {
			if e.Output != "" {
				fmt.Fprint(out, e.Output)
			}
			switch e.Action {
			case "pass", "fail", "skip":
				r.finished[e.Package] = finish{action: e.Action, elapsed: e.Elapsed}
			}
			if e.Action == "fail" {
				r.failedPkgs = append(r.failedPkgs, short(e.Package))
				// AND WHATEVER ITS TESTS WERE SAYING WHEN IT DIED.
				//
				// Per-test output is buffered and flushed on that test's own
				// pass/fail/skip, which is what keeps this gate's log the
				// length `go test` would have produced. A test that never
				// reports one — the binary panicked under it, the race
				// detector aborted the process, TestMain exited — leaves its
				// buffer to be dropped, and the package then fails with a
				// bare `FAIL pkg 103s` line and NOT ONE WORD about why.
				//
				// Measured: internal/engine failed exactly that way twice on
				// one pull request, in a full CI log, with `0 test(s) failed
				// in 1 package(s)` underneath it. A gate whose whole subject
				// is that nothing fails invisibly cannot be the thing hiding
				// the failure — the module doc above already says so about
				// build errors ("a gate that swallowed those would hide the
				// one failure nobody can debug without them"), and this is
				// the same sentence for a crash.
				r.orphaned += flush(out, buffered, e.Package)
			}
			continue
		}

		key := e.Package + "\x00" + e.Test
		switch e.Action {
		case "output":
			buffered[key] = append(buffered[key], e.Output)
		case "pass", "fail", "skip":
			r.tests++
			if m := measurement(short(e.Package), e.Test); m != nil {
				r.measuredSeen[m.Package+"\x00"+m.Test] = true
			}
		}
		switch e.Action {
		case "skip":
			r.skipped = append(r.skipped, Skip{Package: short(e.Package), Test: e.Test})
			delete(buffered, key)
		case "pass":
			// A DECLARED MEASUREMENT PRINTS ON A GREEN RUN. Every other
			// passing test's output is dropped, which is what keeps this
			// gate's log the length `go test` would have produced.
			if measurement(short(e.Package), e.Test) != nil {
				for _, o := range buffered[key] {
					fmt.Fprint(out, o)
				}
			}
			delete(buffered, key)
		case "fail":
			r.failed = append(r.failed, short(e.Package)+" "+e.Test)
			for _, o := range buffered[key] {
				fmt.Fprint(out, o)
			}
			delete(buffered, key)
		}
	}
	// AND WHATEVER IS STILL HELD WHEN THE STREAM ENDS.
	//
	// The package branch above covers a package that reported `fail`. A
	// TRUNCATED stream has no package record at all — a producer killed by an
	// OOM or a signal, which the verdict's first branch exists for — and its
	// last test's output is the only evidence of where it got to. On a run
	// where every test reported there is nothing left here, so this costs a
	// green log nothing.
	for pkg := range r.ran {
		r.orphaned += flush(out, buffered, "github.com/crewlet/crewlet/"+pkg)
	}
	r.orphaned += flush(out, buffered, "")
	return r
}

// flush prints, and discards, every buffer left under a package whose tests
// never reported a result — and says that is what they are.
//
// Named rather than dumped: `go test` frames its output with `=== RUN` and
// `--- FAIL`, and a reader scanning a CI log for a test name finds nothing if
// the lines arrive bare. The sentence on the header is the fact the reader
// needs first, because it is the one thing `go test`'s own output never has to
// say: this test was RUNNING when the binary stopped.
func flush(out *os.File, buffered map[string][]string, pkg string) int {
	prefix := pkg + "\x00"
	names := make([]string, 0, len(buffered))
	for key := range buffered {
		if strings.HasPrefix(key, prefix) {
			names = append(names, key)
		}
	}
	slices.Sort(names)
	for _, key := range names {
		fmt.Fprintf(out, "=== ORPHANED %s %s (no pass, fail or skip was ever "+
			"reported for it — the binary stopped here)\n",
			short(pkg), strings.TrimPrefix(key, prefix))
		for _, o := range buffered[key] {
			fmt.Fprint(out, o)
		}
		delete(buffered, key)
	}
	return len(names)
}

// writeReport writes ran.txt and timings.tsv into dir; see the package doc.
//
// Both sorted by import path, so a report is a function of what ran and
// diffs cleanly against another shard's. Seconds to the millisecond, which is
// test2json's own precision.
//
// timings.tsv's shape is internal/solo/partition's to read, strictly, and it
// outlives this run: ci.yml keeps it as the next run's weights. Change it
// freely — that cache is keyed on a hash of this package's source and the
// partition's, so no run ever restores a file a different writer produced.
func writeReport(dir string, r report) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("-report %s: %w", dir, err)
	}
	var ran, timings strings.Builder
	for _, p := range slices.Sorted(maps.Keys(r.finished)) {
		f := r.finished[p]
		fmt.Fprintln(&ran, p)
		if f.action != "fail" {
			fmt.Fprintf(&timings, "%s\t%s\n", p, strconv.FormatFloat(f.elapsed, 'f', 3, 64))
		}
	}
	for name, body := range map[string]string{"ran.txt": ran.String(), "timings.tsv": timings.String()} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return fmt.Errorf("-report %s: %w", dir, err)
		}
	}
	return nil
}

func main() {
	fs := flag.NewFlagSet("skipgate", flag.ContinueOnError)
	reportDir := fs.String("report", "", "write ran.txt and timings.tsv for this run into `dir`")
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: %s [-report DIR] -- go test -json <packages>\n", os.Args[0])
		fs.PrintDefaults()
	}
	// The flag package stops at the first non-flag argument and consumes a
	// `--`, so both `skipgate -- go test …` and `skipgate go test …` hand the
	// command over whole: no flag of `go test`'s is ever read as this one's.
	if err := fs.Parse(os.Args[1:]); err != nil {
		os.Exit(2)
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fs.Usage()
		os.Exit(2)
	}

	cmd := exec.CommandContext(context.Background(), argv[0], argv[1:]...)
	cmd.Stderr = os.Stderr
	stream, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "skipgate: %v\n", err)
		os.Exit(1)
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "skipgate: starting %q: %v\n", argv[0], err)
		os.Exit(1)
	}

	in := bufio.NewScanner(stream)
	// go test -json emits one object per line, and an Output field can carry
	// a very long line — a t.Logf of a config document, say. The default 64
	// KiB token would end the stream mid-run and take the gate's whole
	// judgement with it.
	in.Buffer(make([]byte, 0, 1<<20), 16<<20)

	r := read(in, os.Stdout)
	scanErr := in.Err()
	// DRAINED BEFORE WAITING, always. If the scan stopped early — a line past
	// the buffer cap is the reachable case, since a t.Log can carry one — the
	// producer is left writing into a pipe nobody reads, blocks on it, and
	// cmd.Wait() below never returns. A gate that HANGS is worse than either
	// answer it could have given, and it would look like a slow test run
	// rather than a broken one. Whatever is left goes nowhere; the scan error
	// is still what decides.
	_, _ = io.Copy(io.Discard, stream)
	// Waited for BEFORE anything is decided, so the producer's own verdict is
	// in hand rather than inferred from what it managed to say.
	producer := cmd.Wait()
	// The report first, before any branch can exit: it says what the run got
	// through, which a stream that could not be read is the case for most.
	if *reportDir != "" {
		if err := writeReport(*reportDir, r); err != nil {
			fmt.Fprintf(os.Stderr, "\nskipgate: %v\n", err)
			os.Exit(1)
		}
	}
	if scanErr != nil {
		fmt.Fprintf(os.Stderr, "\nskipgate: reading the test stream: %v\n", scanErr)
		os.Exit(1)
	}

	unlisted, stale := Judge(r.skipped, r.ran)
	staleMeasured := JudgeMeasured(r.measuredSeen, r.ran)

	for _, s := range unlisted {
		fmt.Fprintf(os.Stderr, "\nskipgate: %s %s SKIPPED and nothing says it may.\n"+
			"A skip is not a pass: that case asserted nothing on this run, and no\n"+
			"CI log would have said so. Either fix what made it skip, or add it to\n"+
			"internal/skipgate/allowed.go with the reason and a When.\n",
			s.Package, s.Test)
	}
	for _, s := range stale {
		fmt.Fprintf(os.Stderr, "\nskipgate: %s %s is allowed as When: Always and did NOT skip.\n"+
			"A structural skip that stopped skipping means the thing it described\n"+
			"changed — the capability arrived, or the case was rewritten. Drop the\n"+
			"entry in internal/skipgate/allowed.go, or mark it Environment if it\n"+
			"was never structural.\n",
			s.Package, s.Test)
	}

	for _, m := range staleMeasured {
		fmt.Fprintf(os.Stderr, "\nskipgate: %s %s is declared in the measured table "+
			"and never reported.\nA measurement that stopped running prints nothing "+
			"on every green run, which is the\nexact failure that table was added to "+
			"fix. Rename or drop the entry in\ninternal/skipgate/allowed.go.\n",
			m.Package, m.Test)
	}

	code, note := Verdict(r, producer,
		len(unlisted) > 0 || len(stale) > 0 || len(staleMeasured) > 0)
	fmt.Fprintln(os.Stderr, note)
	os.Exit(code)
}

// Verdict decides the run from what the stream said and what the producer did.
//
// Pure over values, because every branch here is a way a build reports green
// over a suite that did not run, and a rule exercised only by shelling out to
// `go test` is a rule nobody re-reads. Two of these were review findings on
// the shape that preceded it.
func Verdict(r report, producer error, declarationsBroken bool) (int, string) {
	switch {
	case producer != nil && len(r.failed) == 0 && len(r.failedPkgs) == 0:
		// THE PRODUCER FAILED AND THE STREAM DID NOT SAY WHY. A killed or
		// aborted `go test` is exactly this: a truncated stream carrying no
		// failure record, which every other branch would read as a clean run.
		// Decided first, because what it means is that the run did not finish
		// rather than that it passed.
		return 1, fmt.Sprintf("\nskipgate: the test command exited %v without reporting "+
			"a failure, so the run did not finish — its stream ends after %d "+
			"package(s). Nothing here can say the suite passed.", producer, len(r.ran))

	case len(r.failed) > 0 || len(r.failedPkgs) > 0:
		// DECIDED BEFORE "not one test ran", because a build failure is both:
		// nothing executed AND a package reported failure, and naming the
		// compile error is what the reader can act on.
		//
		// BOTH counts, because they are different failures. A package can fail
		// with no failing test in it — that is what a build error looks like
		// from here, and reporting only named tests would pass a tree that
		// does not compile.
		//
		// AND THE PACKAGES ARE NAMED. `0 test(s) failed in 1 package(s)` is
		// the line this printed, twice, on a CI run nobody could diagnose: a
		// count says a package failed and refuses to say which, in a stream
		// covering 120 of them. The number was never the useful half.
		note := fmt.Sprintf("\nskipgate: %d test(s) failed in %d package(s): %s",
			len(r.failed), len(r.failedPkgs), strings.Join(r.failedPkgs, ", "))
		if len(r.failed) == 0 {
			// A PACKAGE FAILED WITH NO FAILING TEST, which is three
			// different things and the reader has to be told which to look
			// for: it does not compile, its binary died under a test (a
			// panic, the race detector, a TestMain that exited), or it
			// failed after its last test finished. The orphaned count is
			// what separates the first from the rest — a build error prints
			// its own output at package level and leaves nothing buffered.
			note += "\nNo test in it reported a failure, so this is a package that " +
				"did not build, a binary that stopped under a test, or a\nTestMain " +
				"that exited non-zero."
			if r.orphaned > 0 {
				note += fmt.Sprintf(" %d test(s) had output and never reported "+
					"a result;\ntheir output is above, under ORPHANED.", r.orphaned)
			} else {
				note += " Nothing was buffered under a test, so the output above is " +
					"all there is."
			}
		}
		return 1, note

	case r.tests == 0:
		// NOT ONE TEST RAN. A toolchain error, an unusable package list, a
		// producer that died before its first record — or a package set that
		// contains no tests at all, which a guard counting PACKAGES would
		// have waved through, since a package with no test files still
		// reports itself. The shape this replaced printed "no test skipped"
		// and exited 0, certifying nothing.
		return 1, fmt.Sprintf("\nskipgate: not one test reported a result across "+
			"%d package(s). The test command produced no usable stream, so no "+
			"suite ran.", len(r.ran))

	case declarationsBroken:
		return 1, "\nskipgate: the declared skips and the observed ones disagree"

	case len(r.skipped) == 0:
		// Ordinary: every Environment entry's prerequisite was present. It can
		// no longer mean "nothing ran" — that is caught above.
		return 0, fmt.Sprintf("skipgate: %d test(s), none skipped, across %d package(s)",
			r.tests, len(r.ran))

	default:
		return 0, fmt.Sprintf("skipgate: %d skip(s) of %d test(s) across %d package(s), all declared",
			len(r.skipped), r.tests, len(r.ran))
	}
}

// Judge splits observed skips into the ones nothing declared, and the [Always]
// entries that did not fire in a package this run actually covered.
//
// Pure over values, so the arithmetic of the gate is testable without running
// a suite — the same reason internal/textindex and internal/solo/partition
// keep theirs out of the I/O.
func Judge(observed []Skip, ran map[string]bool) (unlisted, stale []Skip) {
	for _, s := range observed {
		if entry(s) == nil {
			unlisted = append(unlisted, s)
		}
	}
	for _, a := range allowed {
		// Not run here says nothing about the entry. See [report.ran]: each
		// test target covers one half of the tree.
		if a.When != Always || !ran[a.Package] {
			continue
		}
		if !slices.ContainsFunc(observed, func(s Skip) bool {
			return s.Package == a.Package && s.Test == a.Test
		}) {
			stale = append(stale, Skip{Package: a.Package, Test: a.Test})
		}
	}
	return unlisted, stale
}

// entry finds the allowance covering one observed skip.
func entry(s Skip) *Allowance {
	for i := range allowed {
		if allowed[i].Package == s.Package && allowed[i].Test == s.Test {
			return &allowed[i]
		}
	}
	return nil
}
