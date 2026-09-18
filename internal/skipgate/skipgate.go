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
// # Rendering is a promise, and it was broken once here
//
// Reading the stream means OWNING what reaches the log, and a gate that eats
// the one thing a reader needs has done more damage than the convention it
// enforces ever prevented. That happened: per-test output was buffered and
// released only by that test's OWN terminal record, and a dying binary
// satisfies neither of the two ways out.
//
// CI run 35312291605 on main is the measurement: internal/engine failed with
// one line, `FAIL github.com/crewlet/crewlet/internal/engine 133.609s`, no
// test named, no panic, no trace, under a verdict reading "0 test(s) failed
// in 1 package(s)".
//
// There are two exits and the fix takes both, because closing one and
// measuring it against a case that went out the other is how a rescue looks
// finished and is not:
//
//   - the test the binary stopped in never reports at all, so its buffer is
//     still there when the stream ends — [flushOrphans];
//   - or it had ALREADY reported, because `--- PASS: TestX` is itself a frame
//     test2json keeps filing under, and a passing test's buffer is dropped
//     whole — [flushTails].
//
// [report.parked] is the third thing that had to be decided: a binary that
// dies leaves every t.Parallel() test sitting on `=== PAUSE` with an empty
// account, and 340 headers for those bury the one that carries the panic.
// A clean run prints exactly what it printed before — measured byte-identical
// over ordinary passes, failures, subtests and parallel logs.
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
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// event is the slice of test2json's schema this needs.
type event struct {
	Action  string
	Package string
	Test    string
	Output  string
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
	// unfinished is every test that produced output and never reported a
	// result, and it is the residue of a test binary that DIED rather than
	// failed. A panic in a goroutine no tRunner is recovering, a race report,
	// a runtime fatal, an OOM kill: the process stops where it stands, so the
	// test that was current never reaches a pass, fail or skip record —
	// while test2json has been attributing the dying binary's own output to
	// it the whole time. The package still reports `fail`, so the run is red
	// either way; what this names is the one account of WHY, which nothing
	// else in the stream carries.
	//
	// Measured on main: CI run 35312291605 failed with exactly one line of
	// evidence, `FAIL github.com/crewlet/crewlet/internal/engine 133.609s`,
	// under a verdict reading "0 test(s) failed in 1 package(s)". The panic
	// was in the buffer and this program dropped it.
	unfinished []string
	// parked is how many orphans held nothing but test2json's own framing:
	// tests sitting between `=== PAUSE` and a resume that never came, which
	// is what every parallel test in the binary is at the moment one dies.
	// A count rather than names — 340 of them say one thing, which is that
	// the process stopped mid-run.
	parked int
	// afterResult is every test the binary talked over: it reported its own
	// result and then had more attributed to it, because test2json keeps
	// filing under the last test it saw framed and a `--- PASS:` line is a
	// frame. See [flushTails] — it is the same swallow as
	// [report.unfinished] reached by the other exit, and a passing test's
	// buffer used to be dropped whole.
	afterResult []string
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
	// the tree — `make test` the shared partition, `make test-solo` the rest
	// — so without this, every Always entry belonging to the other half
	// reads as "declared and did not skip" and each target fails on the other
	// one's entries. Staleness is only a question about a package that ran.
	ran map[string]bool
}

// read consumes a test2json stream and renders it back as PLAIN `go test`
// output — package result lines, and a failing test's own output.
//
// NOT the whole stream. `-json` implies verbose, so echoing every Output field
// would print a line per passing subtest and make a CI log tens of times
// longer than it was before this was in the pipeline. A gate that does that is
// a gate somebody takes back out, and then the skips are invisible again. So
// per-test output is BUFFERED and flushed when that test fails, which is what
// `go test` without -v does; package-level records are printed as they come,
// and those carry the `ok  pkg  1.234s` lines.
//
// AND FLUSHED AT END OF STREAM if the test never reported at all, which is the
// one case plain `go test` renders and a buffer alone cannot. A binary that
// dies where it stands leaves its panic in that buffer under the name of
// whichever test was current, and nothing afterwards asks for it. See
// [report.unfinished].
//
// A line that is not JSON is passed through rather than rejected: `go test`
// writes build errors and toolchain chatter around the stream, and a gate that
// swallowed those would hide the one failure nobody can debug without them.
func read(in *bufio.Scanner, out *os.File) report {
	r := report{ran: map[string]bool{}, measuredSeen: map[string]bool{}}
	buffered := map[string][]string{}
	tails := map[string][]tail{}

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
			if e.Action == "fail" {
				r.failedPkgs = append(r.failedPkgs, short(e.Package))
			}
			// BESIDE ITS OWN RESULT LINE, not at the end of the log.
			// `go test` interleaves package blocks — measured over three
			// packages at -p 4, the order was i1, i2, i1, i2, i3, … — so a
			// drain that only ran at end of stream would put a panic the
			// whole length of a 200-package run away from the `FAIL pkg`
			// line it belongs to. This package is finished, so anything
			// still buffered under it never will be.
			if e.Action == "pass" || e.Action == "fail" || e.Action == "skip" {
				flushOrphans(out, &r, buffered, e.Package)
				// ONLY A FAILING PACKAGE HAS ANYTHING TO EXPLAIN. On a
				// green one this drops what it held, which is the whole
				// difference between a diagnostic and 94 lines of noise
				// — measured, over one `make test`: that many tests have
				// a goroutine still logging after they pass, and every
				// one of them printed a header before this branch
				// existed.
				if e.Action == "fail" {
					flushTails(out, &r, tails[short(e.Package)])
				}
				delete(tails, short(e.Package))
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
			keepTail(tails, e.Package, e.Test, buffered[key])
			delete(buffered, key)
		case "pass":
			// A DECLARED MEASUREMENT PRINTS ON A GREEN RUN. Every other
			// passing test's output is dropped, which is what keeps this
			// gate's log the length `go test` would have produced.
			if measurement(short(e.Package), e.Test) != nil {
				for _, o := range buffered[key] {
					fmt.Fprint(out, o)
				}
			} else {
				keepTail(tails, e.Package, e.Test, buffered[key])
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

	// THE BACKSTOP. Everything above drains a package as it finishes, so on
	// any run that got that far this is empty — but a producer killed
	// mid-package never emits that record, and its buffer is exactly the
	// account of why it died.
	flushOrphans(out, &r, buffered, "")
	// A PACKAGE THAT NEVER REPORTED did not pass, so whatever it printed
	// after a test of its own is still owed to the reader — this is the
	// truncated-stream case the backstop above is for, one level down.
	for _, pkg := range slices.Sorted(maps.Keys(tails)) {
		flushTails(out, &r, tails[pkg])
	}
	return r
}

// tail is what one test had attributed to it after its own result line,
// under a name already qualified by its package.
type tail struct {
	test  string
	lines []string
}

// keepTail holds a test's aftermath until its package reports, because
// whether it is worth printing is a fact about the PACKAGE rather than about
// the test: see the branch in [read] that spends it.
func keepTail(tails map[string][]tail, pkg, test string, said []string) {
	t := afterResult(said)
	if !spoke(t) {
		return
	}
	tails[short(pkg)] = append(tails[short(pkg)], tail{test: short(pkg) + " " + test, lines: t})
}

// flushOrphans renders every buffered test that never reported a result, and
// takes it out of the buffer. pkg scopes it to one package; "" is whatever is
// left when the stream ends.
//
// On a run that finished this does nothing at all: every terminal per-test
// action above either flushes that test's buffer or deletes it, so there is
// no clean-log cost to pay — measured byte-identical over a 45-package,
// 4305-test run.
//
// SORTED, because map order is random and a diagnostic whose sections move
// between runs is one nobody can compare across two of them.
//
// The header goes to `out` beside the output it introduces rather than to
// stderr with this program's other diagnostics: the two streams are
// interleaved by whoever runs make, and a header that can land pages away
// from its body is worse than no header at all.
func flushOrphans(out *os.File, r *report, buffered map[string][]string, pkg string) {
	for _, key := range slices.Sorted(maps.Keys(buffered)) {
		p, test, _ := strings.Cut(key, "\x00")
		if pkg != "" && p != pkg {
			continue
		}
		said := buffered[key]
		delete(buffered, key)
		// A PARKED TEST HAS NO ACCOUNT, and on a dying binary almost every
		// orphan is one. When the process stopped, every test that had
		// called t.Parallel() was sitting between `=== PAUSE` and a resume
		// it never got; their buffers hold test2json's own framing and
		// nothing else. Measured on the real failure: 341 orphans, 340 of
		// them parked, and printing a header for each buried the one that
		// carried the panic under a page of noise — which is the same
		// defect as swallowing it, arrived at from the other side.
		//
		// They are still COUNTED, because "the binary stopped with 340
		// tests in flight" is a fact about when it died, and one number
		// says it where 340 names do not.
		if !spoke(said) {
			r.parked++
			continue
		}
		r.unfinished = append(r.unfinished, short(p)+" "+test)
		// THE NAME IS WHERE THE STREAM STOPPED, NOT NECESSARILY THE
		// CULPRIT, and saying so is the difference between a diagnostic
		// and a wrong diagnosis. test2json tags output with the last test
		// it saw FRAMED (`=== RUN` / `=== CONT`), and clears that only on
		// the binary's own closing PASS/FAIL line, which a dying binary
		// never prints. Under -parallel the framed test is routinely a
		// bystander: measured, a panic leaked by TestParC came back tagged
		// TestParB. The stack names the goroutine; this names the frame.
		fmt.Fprintf(out, "\nskipgate: %s %s never reported a result — the test "+
			"binary stopped while it was the one framed, and everything it "+
			"printed follows. That name is where the stream stopped rather than "+
			"a verdict: with tests in parallel it can be a bystander, and the "+
			"stack below is what names the goroutine.\n", short(p), test)
		for _, o := range said {
			fmt.Fprint(out, o)
		}
	}
}

func main() {
	argv := os.Args[1:]
	if len(argv) > 0 && argv[0] == "--" {
		argv = argv[1:]
	}
	if len(argv) == 0 {
		fmt.Fprintf(os.Stderr, "usage: %s -- go test -json <packages>\n", os.Args[0])
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
			"package(s). Nothing here can say the suite passed.%s",
			producer, len(r.ran), died(r))

	case len(r.failed) > 0 || len(r.failedPkgs) > 0:
		// DECIDED BEFORE "not one test ran", because a build failure is both:
		// nothing executed AND a package reported failure, and naming the
		// compile error is what the reader can act on.
		//
		// BOTH counts, because they are different failures. A package can fail
		// with no failing test in it — that is what a build error looks like
		// from here, and reporting only named tests would pass a tree that
		// does not compile.
		return 1, fmt.Sprintf("\nskipgate: %d test(s) failed in %d package(s)%s",
			len(r.failed), len(r.failedPkgs), died(r))

	case len(r.unfinished) > 0 || r.parked > 0 || len(r.afterResult) > 0:
		// A TEST STOPPED MID-RUN AND NOTHING ELSE CALLED THE RUN FAILED. The
		// branch above catches this whenever the package reported `fail`,
		// which is the ordinary case; this is the same residue in a stream
		// that never got that far, and it is the [Verdict] doctrine applied
		// one level down — a run that did not finish is not a pass, and that
		// holds for one test binary exactly as it holds for the whole
		// command.
		return 1, fmt.Sprintf("\nskipgate: every package reported, and%s", died(r))

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

// flushTails renders what was attributed to a test AFTER it had already
// reported its own result, for a package that went on to FAIL.
//
// THE OTHER HALF OF THE SAME HOLE, and the one that hid the panic in the
// experiment written to prove the first half. [flushOrphans] rescues a test
// that never reported; this rescues one that reported and was then talked
// over. test2json keeps attributing to the last test it saw framed, and a
// `--- PASS: TestX` line IS a frame, so a goroutine that panics a moment
// after TestX returned has its whole banner filed under TestX — which then
// reaches a perfectly ordinary `pass`, and a passing test's buffer is
// dropped. Measured: a package whose TestLeaksAPanickingGoroutine returned
// and then segfaulted printed, through this gate, one `FAIL dying 0.306s`
// and nothing else.
//
// Anything past that result line is the PROCESS talking rather than the
// test's own report — but plenty of processes talk harmlessly. Measured over
// one `make test`: 94 passing tests have a goroutine still logging after they
// return, so printing every tail would have added 94 headers to a green log
// and made this the gate somebody takes back out. [keepTail] holds them and
// only a package that FAILED spends them, because only a failure has
// something to explain.
func flushTails(out *os.File, r *report, held []tail) {
	for _, t := range held {
		r.afterResult = append(r.afterResult, t.test)
		fmt.Fprintf(out, "\nskipgate: %s had already reported its own result when "+
			"the binary printed this under its name, so this is the process "+
			"talking rather than the test. The stack below is what names the "+
			"goroutine.\n", t.test)
		for _, l := range t.lines {
			fmt.Fprint(out, l)
		}
	}
}

// afterResult returns the lines following a test's own result line.
//
// The LAST such line, because a parent's buffer can carry its subtests'
// indented results behind its own. No result line at all means the test never
// reported one, which is [flushOrphans]'s case rather than this one.
func afterResult(said []string) []string {
	lines := flatten(said)
	cut := -1
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "--- ") {
			cut = i
		}
	}
	if cut < 0 {
		return nil
	}
	return lines[cut+1:]
}

// flatten turns buffered Output fields into whole lines, keeping the newline
// so a flush reproduces the bytes `go test` would have written.
func flatten(said []string) []string {
	var out []string
	for _, chunk := range said {
		for {
			i := strings.IndexByte(chunk, '\n')
			if i < 0 {
				break
			}
			out = append(out, chunk[:i+1])
			chunk = chunk[i+1:]
		}
		if chunk != "" {
			out = append(out, chunk)
		}
	}
	return out
}

// spoke reports whether a buffer holds anything the BINARY said, as opposed
// to test2json's own frame lines.
//
// `=== RUN`, `=== PAUSE`, `=== CONT` and `=== NAME` are the converter
// narrating which test it is attributing to; plain `go test` prints them only
// under -v. A buffer of nothing else is a test that was parked when the
// process stopped, and it has nothing to tell anybody.
func spoke(lines []string) bool {
	for _, l := range lines {
		for _, one := range strings.Split(l, "\n") {
			one = strings.TrimSpace(one)
			if one != "" && !strings.HasPrefix(one, "=== ") {
				return true
			}
		}
	}
	return false
}

// died renders the unfinished tests as a clause, or nothing when there are
// none. It is separate so both failure branches of [Verdict] say it the same
// way: the count in the note above is the number a reader will try to
// reconcile with the log, and "0 test(s) failed in 1 package(s)" with no
// explanation is precisely the line that explained nothing.
func died(r report) string {
	var parked string
	if r.parked > 0 {
		parked = fmt.Sprintf("\n%d further test(s) were in flight and printed "+
			"nothing — parked on t.Parallel() when the process stopped.", r.parked)
	}
	var talked string
	if len(r.afterResult) > 0 {
		talked = fmt.Sprintf("\nThe binary also printed under the name of %d test(s) "+
			"that had already reported: %s", len(r.afterResult),
			strings.Join(r.afterResult, ", "))
	}
	switch {
	case len(r.unfinished) == 0 && r.parked == 0 && len(r.afterResult) == 0:
		return ""
	case len(r.unfinished) == 0 && r.parked == 0:
		return talked
	case len(r.unfinished) == 0:
		return "\nA test binary stopped rather than finished and said nothing " +
			"about why. That is what a kill from outside looks like — an OOM, a " +
			"runner going away — and equally what an os.Exit from inside looks " +
			"like, so read the exit status above: seat.Watchdog's hard exit is " +
			"75." + parked + talked
	}
	return fmt.Sprintf("\n%d test(s) never reported a result, so a test binary "+
		"stopped rather than finished: %s\nWhat each of them printed is above, "+
		"beside its package's own result line. Those names are where the stream "+
		"stopped, not a verdict.%s",
		len(r.unfinished), strings.Join(r.unfinished, ", "), parked+talked)
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
