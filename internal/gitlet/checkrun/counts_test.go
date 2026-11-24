package checkrun_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/checkrun"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// lines is a buffer a runner's log lines are written to, read while the runner writes.
type lines struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// count is how many of the lines written name msg.
func (l *lines) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), `"msg":"`+msg+`"`)
}

// logging gives o a logger writing every level to a buffer a case reads.
func logging(o *checkrun.Options) *lines {
	l := &lines{}
	o.Logger = slog.New(slog.NewJSONHandler(l, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return l
}

// awaitLog waits until l holds a line naming msg, failing t past ten seconds.
func awaitLog(t *testing.T, l *lines, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for l.count(msg) == 0 {
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("no %s was logged", msg)
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
}

// numbered is n lines, each its number zero-padded to width bytes with its newline.
func numbered(n, width int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%0*d\n", width-1, i+1)
	}
	return b.String()
}

// A FAILED RUN COUNTS ITS OUTPUT'S LINES IN ITS BOX: an output shorter than the tail is counted
// whole from line 1; a longer one up to the tail's end, the tail opening on a whole line whether or
// not the byte before CheckTailBytes is a newline; a last line longer than the tail is its end
// alone, cut; in each, the tail's first line plus its own lines less one is the output's last.
//
// Mutation: leave KeepEnd's mark on a line kept in part and the ending uncut (red).
func TestAFailedRunCountsItsOutputsLinesInItsBox(t *testing.T) {
	t.Parallel()
	// 100-byte lines put the byte before a 16 KiB tail inside a line; a last line of 84 bytes
	// (16 384 mod 100) puts it on a newline.
	cutTail := numbered(410, 100)
	wholeTail := numbered(409, 100) + numbered(1, 84)
	f := newFleet(t)
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
	for i, tc := range []struct {
		output string
		lines  int
		cut    bool
	}{{numbered(1201, 13), 1201, false}, {cutTail, 410, false}, {wholeTail, 410, false},
		{"a\n" + strings.Repeat("x", gitlet.CheckTailBytes+100) + "\n", 2, true}} {
		key := f.queue(oidOf(fmt.Sprintf("%x", i+10)), def("unit", time.Minute))
		f.container.end(f.container.box(t), 1, tc.output)
		f.ended(key)
		got := f.detail(key)
		if got.LogLines != tc.lines || got.TailCut != tc.cut {
			t.Errorf("output %d counts %d lines, cut %v; want %d, cut %v", i, got.LogLines, got.TailCut,
				tc.lines, tc.cut)
		}
		if last := got.TailFirstLine + gitlet.TailLines(got.Tail) - 1; got.TailFirstLine < 1 ||
			last != got.LogLines {
			t.Errorf("output %d's tail is numbered from %d to %d, want it to end at %d", i,
				got.TailFirstLine, last, got.LogLines)
		}
	}
}

// A KEY'S BLOCK THE REDACTION TAKES OUT MOVES NO LINE ABOVE IT: an output whose tail holds a
// private key's block, which the tail shows as one marker, numbers its tail from line 1, as the
// lines it shows run.
//
// Mutation: count the output's newlines raw, less none the redaction took out (numbered past 1, red).
func TestAKeysBlockTheRedactionTakesOutMovesNoLineAboveIt(t *testing.T) {
	t.Parallel()
	const line = "MIIEowIBAAKCAQEAu1SU1LfVLPHCozMxH2Mo4lgOEePzNm0tRgeLezV6ffAt0gun"
	output := "starting\nthe key:\n-----BEGIN RSA PRIVATE KEY-----\n" + strings.Repeat(line+"\n", 4) +
		"Zq9XwV==\n-----END RSA PRIVATE KEY-----\nFAIL web\n"
	if strings.Contains(redact.Secrets(output), "Zq9XwV") {
		t.Fatal("the redaction keeps the key's body; the case needs one it takes out")
	}
	f := newFleet(t)
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
	key := f.queue(oidOf("a"), def("unit", time.Minute))
	f.container.end(f.container.box(t), 1, output)
	f.ended(key)
	got := f.detail(key)
	if strings.Contains(got.Tail, "Zq9XwV") || got.TailFirstLine != 1 ||
		got.LogLines != gitlet.TailLines(got.Tail) {
		t.Fatalf("the tail %q is numbered from %d of %d lines; want it redacted, from 1, its own lines",
			got.Tail, got.TailFirstLine, got.LogLines)
	}
}

// AN OUTPUT WITH NO FINAL NEWLINE COUNTS ITS LAST LINE: `a\nb` is two lines, its tail numbered
// from the first.
//
// Mutation: drop the unterminated line's one (1, fewer than the tail's own, so uncounted, red).
func TestAnOutputWithNoFinalNewlineCountsItsLastLine(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
	key := f.queue(oidOf("a"), def("unit", time.Minute))
	f.container.end(f.container.box(t), 1, "a\nb")
	f.ended(key)
	if got := f.detail(key); got.LogLines != 2 || got.TailFirstLine != 1 || got.TailCut {
		t.Fatalf("an output of a\\nb counts %d lines from %d, cut %v; want 2 from 1, whole", got.LogLines,
			got.TailFirstLine, got.TailCut)
	}
}

// junitCounting is a JUnit report of one suite stating tests, failures and skipped, failing its
// first case.
func junitCounting(tests, failures, skipped int) string {
	return fmt.Sprintf(`<testsuites><testsuite name="pkg" tests="%d" failures="%d" errors="0" skipped="%d">`+
		`<testcase classname="pkg" name="TestA"><failure message="no"/></testcase></testsuite></testsuites>`,
		tests, failures, skipped)
}

// A RUN COUNTS ITS JUNIT TESTS: a failed run's ending carries its report's tests, failed and
// skipped beside the summary it says them in; a passed run's carries them and no lines; a check
// naming no report carries none, and nor does a report whose failures are past its tests.
//
// Mutation: set the tests on the failed ending alone (the passed run carries none, red).
func TestARunCountsItsJUnitTests(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	f.runner(f.options("git-a", 1, &order{nodes: []string{"git-a"}}))
	reported := def("unit", time.Minute)
	reported.Report.JUnit = "out/junit.xml"
	for i, tc := range []struct {
		def    gitlet.CheckDef
		report string
		exit   int
		want   *gitlet.CheckTests
	}{
		{reported, junitCounting(214, 1, 3), 1, &gitlet.CheckTests{Total: 214, Failed: 1, Skipped: 3}},
		{reported, junitCounting(214, 1, 3), 0, &gitlet.CheckTests{Total: 214, Failed: 1, Skipped: 3}},
		{def("unit", time.Minute), "", 1, nil},
		{reported, junitCounting(2, 5, 0), 1, nil},
	} {
		key := f.queue(oidOf(fmt.Sprintf("%x", i+10)), tc.def)
		box := f.container.box(t)
		if tc.report != "" {
			box.Put(sandbox.DefaultHome+"/.crewlet-check/junit.xml", tc.report)
		}
		f.container.end(box, tc.exit, "FAIL\n")
		f.ended(key)
		got := f.detail(key)
		if (got.Tests == nil) != (tc.want == nil) || got.Tests != nil && *got.Tests != *tc.want {
			t.Errorf("run %d carries tests %+v, want %+v", i, got.Tests, tc.want)
		}
		if i == 0 && !strings.Contains(got.Summary, "1 of 214 tests failed (exited 1 after") {
			t.Errorf("run %d's summary = %q, want its report's counts", i, got.Summary)
		}
		if tc.exit == 0 && (got.LogLines != 0 || got.Tail != "") {
			t.Errorf("a passed run carries %d lines and tail %q, want none", got.LogLines, got.Tail)
		}
	}
	if got, err := checkrun.ReadJUnit(strings.NewReader(junitCounting(214, 1, 3))); err != nil ||
		got.Skipped != 3 || got.Counts == nil || *got.Counts != (gitlet.CheckTests{Total: 214, Failed: 1, Skipped: 3}) {
		t.Errorf("a report of 3 skipped read %+v (%v)", got, err)
	}
}

// A LINE COUNT THE BOX CANNOT MAKE LEAVES THE ENDING UNCOUNTED: a count that exits non-zero, one
// that is no number, and one fewer than the tail's own lines each end the run failed with its tail
// and no lines, the count logged as uncounted, never the ending refused.
//
// Mutation: carry the box's count whatever it says (the third ending is refused and never
// recorded, red).
func TestALineCountTheBoxCannotMakeLeavesTheEndingUncounted(t *testing.T) {
	t.Parallel()
	for name, answer := range map[string]sandbox.ExecResult{
		"exit 127":            {ExitCode: 127},
		"no number":           {Stdout: "garbage\n"},
		"fewer than the tail": {Stdout: "1\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t)
			o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
			logs := logging(&o)
			f.container.countLines = func(string) (sandbox.ExecResult, error) { return answer, nil }
			f.runner(o)
			key := f.queue(oidOf("a"), def("unit", time.Minute))
			f.container.end(f.container.box(t), 1, "a\nb\nc\n")
			row := f.ended(key)
			if got := f.detail(key); row.State != gitlet.CheckStateFailed || got.Tail != "a\nb\nc\n" ||
				got.LogLines != 0 {
				t.Fatalf("the run ended %s with tail %q and %d lines; want failed, its tail, no lines", row.State,
					got.Tail, got.LogLines)
			}
			awaitLog(t, logs, "code_check_lines_uncounted")
		})
	}
}

// errNoneReads stands in for the estate's refusal of an ending no data node reads (its unknown keys
// refused over a whole walk), which the engine's Options.Unread recognises.
var errNoneReads = errors.New("no data node reads the ending's counts")

// unreadCounts is a fleet's estate recording each ending's operation: where refuse is set its data
// nodes refuse an ending carrying counts, and where busy is the first ending is answered an error
// that is no such refusal.
type unreadCounts struct {
	estate
	mu           sync.Mutex
	refuse, busy bool
	ops          []string
}

func (u *unreadCounts) Checks(ctx context.Context, r gitlet.ChecksRequest) (gitlet.ChecksResult, error) {
	if f := r.Finished; f != nil {
		u.mu.Lock()
		u.ops = append(u.ops, r.OpID)
		busy := u.busy
		u.busy = false
		u.mu.Unlock()
		switch {
		case busy:
			return gitlet.ChecksResult{}, errors.New("the checks writer is busy")
		case u.refuse && (f.Tests != nil || f.LogLines != 0 || f.TailCut):
			return gitlet.ChecksResult{}, errNoneReads
		}
	}
	return u.estate.Checks(ctx, r)
}

// asked is every operation an ending was asked under.
func (u *unreadCounts) asked() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.ops...)
}

// AN ENDING NO DATA NODE READS IS RECORDED WITHOUT ITS COUNTS: refused for its counts over a whole
// walk, it is sent again without them under the same operation and recorded, logged once; with no
// Unread it is not recorded; and an ending answered busy first keeps its counts.
//
// Mutation: strip the counts on every error (the busy ending loses them, red).
func TestAnEndingNoDataNodeReadsIsRecordedWithoutItsCounts(t *testing.T) {
	t.Parallel()
	run := func(t *testing.T, refuse, busy bool, unread func(error) bool) (*fleet, *unreadCounts, *lines,
		gitlet.CheckKey) {
		f := newFleet(t)
		o := f.options("git-a", 1, &order{nodes: []string{"git-a"}})
		e := &unreadCounts{estate: estate{f.d}, refuse: refuse, busy: busy}
		o.Estate, o.Unread = e, unread
		logs := logging(&o)
		f.runner(o)
		key := f.queue(oidOf("a"), def("unit", time.Minute))
		f.container.end(f.container.box(t), 1, "a\nb\n")
		return f, e, logs, key
	}
	recognised := func(err error) bool { return errors.Is(err, errNoneReads) }

	t.Run("recognised", func(t *testing.T) {
		t.Parallel()
		f, e, logs, key := run(t, true, false, recognised)
		if row := f.ended(key); row.State != gitlet.CheckStateFailed {
			t.Fatalf("the ending was recorded %s, want failed", row.State)
		}
		if got := f.detail(key); got.LogLines != 0 || got.Tail != "a\nb\n" {
			t.Errorf("the ending recorded %d lines and tail %q, want its tail and no counts", got.LogLines, got.Tail)
		}
		if ops := e.asked(); len(ops) != 2 || ops[0] != ops[1] {
			t.Errorf("the ending was asked under %v, want twice under one operation", ops)
		}
		if n := logs.count("code_check_counts_unread"); n != 1 {
			t.Errorf("code_check_counts_unread was logged %d times, want once", n)
		}
	})
	t.Run("no Unread", func(t *testing.T) {
		t.Parallel()
		_, _, logs, _ := run(t, true, false, nil)
		awaitLog(t, logs, "code_check_end_unrecorded")
		if n := logs.count("code_check_counts_unread"); n != 0 {
			t.Errorf("a runner with no Unread stripped an ending's counts %d times", n)
		}
	})
	t.Run("busy first", func(t *testing.T) {
		t.Parallel()
		f, _, logs, key := run(t, false, true, recognised)
		f.ended(key)
		if got := f.detail(key); got.LogLines != 2 || logs.count("code_check_counts_unread") != 0 {
			t.Errorf("an ending answered busy first recorded %d lines, want its 2", got.LogLines)
		}
	})
}
