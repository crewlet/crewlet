package executor_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// What these protect: A RUN IS NEVER LOST TO THE SIZE OF ITS OWN LOG.
//
// A harness's stdout event stream and its stderr grow with the run, so none
// is read whole. The fake refuses a whole read past the cap [box] gives it, as
// every real backend does, and every case sizes from that cap, so a case that
// still read a stream whole fails here.

// toolStream is an OpenCode stream longer than past bytes, of tool events each
// carrying output bytes of a tool's output (what makes a real stream grow),
// ending with the events given.
func toolStream(output, past int, tail ...string) string {
	echoed := strings.Repeat("o", output)
	var b strings.Builder
	for b.Len() <= past {
		b.WriteString(`{"type":"tool_use","part":{"tool":"read","state":{"status":"completed",` +
			`"input":{"filePath":"/home/user/workspace/big.log"},"output":"` + echoed + `"}}}` + "\n")
	}
	for _, line := range tail {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// pastTheCap is a stream past the box's whole-read cap, in tool events of an
// eighth of the cap apiece: a handful carry it past, and no one line is near
// the bound a line is read to.
func pastTheCap(b *sandbox.FakeSandbox, tail ...string) string {
	return toolStream(b.ReadCap()/8, b.ReadCap(), tail...)
}

// errBoxUnreadable stands in for a box whose file could not be read back.
var errBoxUnreadable = errors.New("docker exec: connection reset")

// A DEAD JOB WITH A STREAM PAST THE CAP IS DONE. A refused read must not answer
// the poll before the liveness probe: the waiter takes the error for "still
// running", refreshes the keepalive and keeps the seat busy, every tick, for a
// job whose process group is gone.
func TestADeadJobWithAStreamPastTheCapIsDone(t *testing.T) {
	t.Parallel()
	runner := executor.NewOpenCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.Put(paths(b).Result(), pastTheCap(b, `{"type":"text","part":{"text":"still going"}}`))
	b.ExecFunc = alive(false)

	state, err := runner.Poll(t.Context(), b, handle)
	if err != nil || state != sandbox.JobEnded {
		t.Fatalf("Poll = %v, %v; want the dead job found ended whatever its stream's size", state, err)
	}
}

// A STREAM'S END NEVER ENDS A RUN: the done marker the wrapper writes after the
// harness exits is the end. An OpenCode run that printed its finish has not yet
// had its script add the session's usage or write the refreshed login back, so
// a live wrapper is a running job, whatever its stream last said.
func TestAStreamsEndNeverEndsARun(t *testing.T) {
	t.Parallel()
	for name, runner := range map[string]*executor.Runner{
		"opencode":    executor.NewOpenCode(),
		"claude-code": executor.NewClaudeCode(),
	} {
		b := box(t, runner)
		handle := start(t, runner, b)
		out := paths(b).Result()
		if name == "claude-code" {
			out = paths(b).Stream()
		}
		b.Put(out, strings.Join([]string{
			`{"type":"text","part":{"text":"Fixed it."}}`,
			`{"type":"step_finish","part":{"reason":"stop"}}`,
			`{"type":"result","subtype":"success","result":"Fixed it."}`,
		}, "\n"))
		b.ExecFunc = alive(true)
		if state, err := runner.Poll(t.Context(), b, handle); state != sandbox.JobRunning || err != nil {
			t.Errorf("%s: Poll = %v, %v; want a live wrapper still running", name, state, err)
		}
	}
}

// A READ THAT FAILS NEVER SKIPS THE PROBE: a dead job is done however its box
// answered the read, and a live one is not done — with the read's error still
// reported beside the answer.
func TestAReadThatFailsNeverSkipsTheLivenessProbe(t *testing.T) {
	t.Parallel()
	runner := executor.NewOpenCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ReadErr = func(string) error { return errBoxUnreadable }

	b.ExecFunc = alive(false)
	if state, err := runner.Poll(t.Context(), b, handle); state != sandbox.JobEnded || err != nil {
		t.Errorf("a dead job behind an unreadable box polled %v, %v; want ended", state, err)
	}
	b.ExecFunc = alive(true)
	if state, err := runner.Poll(t.Context(), b, handle); state != sandbox.JobRunning ||
		!errors.Is(err, errBoxUnreadable) {
		t.Errorf("a live job behind an unreadable box polled %v, %v; want running and the read's error",
			state, err)
	}
}

// A HARNESS THAT EXITS HAS NO STREAM TO POLL: the done marker is its only
// signal, and reading its output is a transfer per box per tick that cannot
// change the answer.
func TestAHarnessThatExitsIsNotPolledThroughItsOutput(t *testing.T) {
	t.Parallel()
	for name, runner := range map[string]*executor.Runner{
		"opencode":    executor.NewOpenCode(),
		"claude-code": executor.NewClaudeCode(),
	} {
		b := box(t, runner)
		handle := start(t, runner, b)
		p := paths(b)
		b.ReadErr = func(path string) error {
			if path != p.Done() {
				return errors.New("the poll read " + path)
			}
			return nil
		}
		b.ExecFunc = alive(true)
		if state, err := runner.Poll(t.Context(), b, handle); state != sandbox.JobRunning || err != nil {
			t.Errorf("%s: Poll = %v, %v; want only the marker and the probe asked", name, state, err)
		}
	}
}

// A STREAM PAST THE CAP IS COLLECTED: a refused whole read would settle the run
// as unreachable and destroy its turn, though the report file, the result of
// record, is a few lines long.
func TestACollectionReadsAStreamPastTheCap(t *testing.T) {
	t.Parallel()
	runner := executor.NewOpenCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Result(), pastTheCap(b,
		`{"type":"tool_use","part":{"tool":"bash","state":{"status":"completed","input":{"command":"go test ./..."}}}}`,
		`{"type":"text","part":{"text":"All green."}}`,
		`{"type":"step_finish","part":{"reason":"stop"}}`))
	b.Put(p.Findings(), "Outcome: succeeded\nOpened https://github.com/acme/api/pull/12")
	b.Put(p.ExitCode(), "0")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !res.Success || !strings.HasPrefix(res.Text, "Outcome: succeeded") {
		t.Errorf("success %v, report %.40q; want the report collected", res.Success, res.Text)
	}
	if !strings.Contains(res.Transcript, "] bash: go test ./...\n[ok ") || !strings.Contains(res.Transcript, "All green.") {
		t.Errorf("the transcript lost the stream's end: …%q", tailOf(res.Transcript, 200))
	}
	if strings.Contains(res.Transcript, "ooooooooo") {
		t.Error("a tool's output reached the transcript; only what ran belongs there")
	}
}

// AN OVER-LONG LINE IS SKIPPED AND SAID, never held: a line is read in one
// piece, so one past the bound is counted where it was and the events around
// it are still read.
//
// At [testLineBound]: the behaviour is the same at any bound, and a line past
// the real one is 32 MiB built and scanned under -race.
func TestALinePastTheBoundIsSkippedAndSaid(t *testing.T) {
	t.Parallel()
	runner := executor.Bounded(executor.NewOpenCode(), sandbox.MaxFailureBytes, testLineBound)
	b := box(t, runner)
	p := paths(b)
	huge := `{"type":"tool_use","part":{"tool":"read","state":{"output":"` +
		strings.Repeat("x", testLineBound) + `"}}}`
	b.Put(p.Result(), strings.Join([]string{
		`{"type":"tool_use","part":{"tool":"bash","state":{"input":{"command":"make build"}}}}`,
		huge,
		`{"type":"text","part":{"text":"Built it."}}`,
	}, "\n"))

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !strings.Contains(res.Transcript, "[note] 1 line(s) of output") || !strings.Contains(res.Transcript, "not read") ||
		!strings.Contains(res.Transcript, "past the 64 KiB one line of a run's output may hold") {
		t.Errorf("the skipped line is not said, with the bound it was past: %q", res.Transcript)
	}
	before := strings.Index(res.Transcript, "make build")
	note := strings.Index(res.Transcript, "[note] 1 line(s) of output")
	after := strings.Index(res.Transcript, "Built it.")
	if before < 0 || after < 0 || before > note || note > after {
		t.Errorf("the note is not where the line was: %q", res.Transcript)
	}
	if res.Text != "Built it." {
		t.Errorf("the answer around the skipped line = %q", res.Text)
	}
}

// THE ERROR STREAM'S END IS SHOWN, at most what a condensation can take, and
// the start it leaves out is said by size: it reaches the reviewer as the
// failure's detail, and its end names what broke. A run with no parsed
// transcript carries the same end behind the same note. At
// [testFailureBound], with an error stream two and a half times it.
//
// Mutation: keep the end to [sandbox.MaxFailureBytes] rather than the
// runner's bound, and this fails.
func TestTheErrorStreamsEndIsShownAndItsStartIsSaid(t *testing.T) {
	t.Parallel()
	runner := executor.Bounded(executor.NewClaudeCode(), testFailureBound, executor.MaxLineBytes)
	b := box(t, runner)
	p := paths(b)
	stderr := strings.Repeat("noise line\n", testFailureBound*5/2/11) + "FATAL: migrations/0007.sql is missing"
	b.Put(p.Err(), stderr)
	b.Put(p.ExitCode(), "1")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	note, shown, _ := strings.Cut(res.Transcript, "\n")
	switch {
	case len(shown) > testFailureBound:
		t.Errorf("the transcript carries %d bytes of the error stream, past the %d its runner holds it to",
			len(shown), testFailureBound)
	case !strings.HasSuffix(stderr, shown) || !strings.HasSuffix(shown, "FATAL: migrations/0007.sql is missing"):
		t.Errorf("the transcript is not the error stream's end: …%q", tailOf(res.Transcript, 120))
	case note != executor.UnreadNote(int64(len(stderr)-len(shown))):
		t.Errorf("the transcript opens with %q; want the note naming the %d bytes it left out",
			note, len(stderr)-len(shown))
	}
	if !strings.HasSuffix(res.Error, "FATAL: migrations/0007.sql is missing") {
		t.Errorf("the failure lost the error stream's last line: …%q", tailOf(res.Error, 120))
	}
	if !strings.Contains(res.Error, "are not shown: a run's failure shows the end of its redaction") {
		t.Errorf("the start left out is not said: %.300q", res.Error)
	}
	if len(res.Error) > testFailureBound {
		t.Errorf("the failure is %d bytes, past the %d its runner holds it to", len(res.Error), testFailureBound)
	}
}

// AN ERROR STREAM WHOSE REDACTION DID NOT END WITHIN ITS WAIT IS SAID, NOT
// SHOWN: on a clock that passes [sandbox.FailureReadWait] while the stream is
// read, the transcript and the failure give its size and why, and none of it.
//
// Mutation: show the end read so far at the wait, and the stream reaches both.
func TestAnErrorStreamPastItsWaitIsSaidNotShown(t *testing.T) {
	t.Parallel()
	var now time.Time
	runner := executor.Clocked(executor.Bounded(executor.NewClaudeCode(), testFailureBound, executor.MaxLineBytes),
		func() time.Time {
			now = now.Add(sandbox.FailureReadWait / 2)
			return now
		})
	b := box(t, runner)
	p := paths(b)
	stderr := strings.Repeat("noise line\n", 3*testFailureBound/11) + "FATAL: the last line"
	b.Put(p.Err(), stderr)
	b.Put(p.ExitCode(), "1")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	said := "(the error stream, " + executor.HumanSize(int64(len(stderr))) + ", is not shown: its redaction, " +
		"read from its first byte, did not reach its end within the 32s a collection gives it"
	for name, got := range map[string]string{"transcript": res.Transcript, "failure": res.Error} {
		if !strings.Contains(got, said) || strings.Contains(got, "noise line") || strings.Contains(got, "FATAL") {
			t.Errorf("the %s is …%q; want %q, none of the stream shown", name, tailOf(got, 200), said)
		}
	}
}

// AN ERROR STREAM IS READ ONLY WHERE IT IS SHOWN: a run whose events gave its
// transcript and which succeeded shows none of its error stream, so its
// collection reads none of it, however long it is.
//
// Mutation: read the error stream on every collection, and this case reads it.
func TestAnErrorStreamIsReadOnlyWhereItIsShown(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Stream(), `{"type":"assistant","message":{"content":[{"type":"text","text":"Built it."}]}}`+"\n"+
		`{"type":"result","subtype":"success","is_error":false,"result":"Built it."}`+"\n")
	b.Put(p.Result(), `{"type":"result","subtype":"success","is_error":false,"result":"Built it."}`)
	b.Put(p.Err(), strings.Repeat("a warning\n", 1000))
	b.Put(p.ExitCode(), "0")
	var read atomic.Int64
	b.Streams = func(path string, r io.Reader) io.Reader {
		if path != p.Err() {
			return r
		}
		return readCount{r, &read}
	}
	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !res.Success || res.Transcript == "" || strings.Contains(res.Transcript, "a warning") {
		t.Fatalf("the case's run collected as success %v with the transcript %.80q; it needs a succeeded "+
			"run whose events gave its transcript", res.Success, res.Transcript)
	}
	if n := read.Load(); n != 0 {
		t.Errorf("the collection read %d bytes of an error stream it shows none of", n)
	}
}

// readCount counts what is read through it.
type readCount struct {
	r io.Reader
	n *atomic.Int64
}

func (c readCount) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

// THE WHOLE FAILURE IS WHAT ONE CONDENSATION CAN TAKE, not the error stream
// inside it: the engine's own sentences (an unreadable piece, the exit status,
// the harness's error) open it, and a stream sized alone puts the whole past
// what the compactor reads, refused uncondensed though a model is configured.
//
// Every opening sentence at once, before the longest stream, in lines and as
// one unbroken line (kept by its end on a character and marked): the failure
// fits the runner's bound, keeps every sentence and the stream's last line, and
// says what it left out. Sized from [testFailureBound]: the stream half again
// past it, the harness's error a hundredth of it, so the stream still gets room
// after the sentences, the branch this case is about. Mutation: carry the
// stream as it was read, and the failure is past the bound.
func TestTheWholeFailureIsWhatOneCondensationCanTake(t *testing.T) {
	t.Parallel()
	for name, stderr := range map[string]string{
		"in lines": strings.Repeat(strings.Repeat("e", 99)+"\n", testFailureBound*3/2/100) + "FATAL: the last line",
		"one line": strings.Repeat("e", testFailureBound*3/2) + "FATAL: the last line",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner := executor.Bounded(executor.NewClaudeCode(), testFailureBound, executor.MaxLineBytes)
			b := box(t, runner)
			p := paths(b)
			const retrying = "the provider answered 529: overloaded, retrying\n"
			harnessError := strings.Repeat(retrying, testFailureBound/100/len(retrying)+1)
			result, err := json.Marshal(map[string]any{"type": "result", "subtype": "error_during_execution",
				"is_error": true, "errors": []string{harnessError}})
			if err != nil {
				t.Fatal(err)
			}
			b.Put(p.Result(), string(result))
			b.Put(p.Findings(), strings.Repeat("r", b.ReadCap()+1)) // a piece that cannot be read
			b.Put(p.Err(), stderr)
			b.Put(p.ExitCode(), "1")

			res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if len(res.Error) > testFailureBound {
				t.Fatalf("the failure is %d bytes, %d past the %d its runner holds it to",
					len(res.Error), len(res.Error)-testFailureBound, testFailureBound)
			}
			for _, want := range []string{
				"the report the harness wrote", // the refused piece, first
				"the harness exited with status 1",
				"error_during_execution: the provider answered 529",
				"are not shown: a run's failure shows the end of its redaction",
			} {
				if !strings.Contains(res.Error, want) {
					t.Errorf("the failure lost %q: %.400q", want, res.Error)
				}
			}
			if !strings.HasPrefix(res.Error, "the report the harness wrote") {
				t.Errorf("the failure opens %.120q; want the piece that could not be read first", res.Error)
			}
			if !strings.HasSuffix(res.Error, "FATAL: the last line") {
				t.Errorf("the failure lost the error stream's last line: …%q", tailOf(res.Error, 80))
			}
		})
	}
}

// A FAILURE WHOSE OWN SENTENCES FILL THE BOUND SAYS THE ERROR STREAM WAS NOT
// SHOWN, rather than showing a fragment of it or nothing at all: the harness's
// error is taken as it is — it is what nobody can find elsewhere — and the
// coordinator holds the whole to what a condensation reads.
func TestAFailureWhoseSentencesFillTheBoundSaysItsStreamWasNotShown(t *testing.T) {
	t.Parallel()
	runner := executor.Bounded(executor.NewClaudeCode(), testFailureBound, executor.MaxLineBytes)
	b := box(t, runner)
	p := paths(b)
	result, err := json.Marshal(map[string]any{"type": "result", "subtype": "error_during_execution",
		"is_error": true, "errors": []string{strings.Repeat("x", testFailureBound)}})
	if err != nil {
		t.Fatal(err)
	}
	b.Put(p.Result(), string(result))
	b.Put(p.Err(), "a warning\nthe last line")
	b.Put(p.ExitCode(), "1")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !strings.HasSuffix(res.Error, "(the error stream, 23 bytes, is not shown: the engine's own "+
		"account of the run fills what one condensation can take)") {
		t.Errorf("the failure ends …%q; want the error stream said, not shown", tailOf(res.Error, 160))
	}
}

// AN ERROR STREAM WHOSE END IS BLANK LINES IS SAID, NOT SHOWN: after the
// start it leaves out, or alone, the transcript and the failure give its size
// and why none of it is there. A line longer than the end shows its own end,
// marked, as the whole stream's redaction ends.
//
// Mutation: leave the note out, or decide it on the end before it is trimmed,
// and the run says nothing of its error stream.
func TestAnErrorStreamWhoseEndIsBlankIsSaidNotShown(t *testing.T) {
	t.Parallel()
	long := "starting\n" + strings.Repeat("e", 2*testFailureBound)
	const blank = "the end a run's failure shows of it holds only blank lines)"
	for _, c := range []struct{ name, stderr, said string }{
		{"a long line, then a blank line", long + "\n\n", blank},
		{"a long line, then a line of spaces", long + "\n \n", blank},
		{"blank lines alone", "starting\n" + strings.Repeat("\n", 2*testFailureBound), blank},
		{"a long line to the end", long + "FATAL: the last line", ""},
		{"a long line, ended", long + "\n", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runner := executor.Bounded(executor.NewClaudeCode(), testFailureBound, executor.MaxLineBytes)
			b := box(t, runner)
			p := paths(b)
			b.Put(p.Err(), c.stderr)
			b.Put(p.ExitCode(), "1")

			res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			for name, got := range map[string]string{"transcript": res.Transcript, "failure": res.Error} {
				if c.said == "" {
					if !strings.Contains(got, "…eee") || !strings.HasSuffix(strings.TrimSpace(c.stderr), tailOf(got, 20)) {
						t.Errorf("the %s ends …%q; want the long line's end, marked", name, tailOf(got, 160))
					}
					continue
				}
				said := "(the error stream, " + executor.HumanSize(int64(len(c.stderr))) + ", is not shown: " + c.said
				if !strings.HasSuffix(got, said) || strings.Contains(got, "eeee") {
					t.Errorf("the %s ends …%q; want %q, none of the stream shown", name, tailOf(got, 160), said)
				}
			}
		})
	}
}

// A REPORT PAST THE CAP DEGRADES ONLY ITSELF: it is described by its size and
// the run reads as not succeeded for it, but the run's tokens, refs and
// transcript are still collected, to be charged and published.
func TestAReportPastTheCapDegradesOnlyItself(t *testing.T) {
	t.Parallel()
	runner := executor.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Result(), `{"type":"result","result":"Opened https://github.com/acme/api/pull/3","subtype":"success",`+
		`"usage":{"input_tokens":100,"output_tokens":50}}`)
	b.Put(p.Findings(), strings.Repeat("r", b.ReadCap()+1))
	b.Put(p.Err(), "a warning")
	b.Put(p.ExitCode(), "0")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v; want the refusal to stay inside its piece", err)
	}
	if res.Success {
		t.Error("a run whose report could not be read reads as a success")
	}
	// By the cap the BOX refused it at, which is the fake's own here.
	capText := fmt.Sprintf("past the %.1f MiB the engine reads back", float64(b.ReadCap())/(1<<20))
	if !strings.Contains(res.Error, "the report the harness wrote") || !strings.Contains(res.Error, capText) {
		t.Errorf("the refusal is not described by size and the box's cap (%q): %q", capText, res.Error)
	}
	if res.InputTokens != 100 || res.OutputTokens != 50 {
		t.Errorf("tokens %d/%d; want the run's spend still collected", res.InputTokens, res.OutputTokens)
	}
	if len(res.DeliveredRefs) != 1 || res.Transcript != "a warning" {
		t.Errorf("refs %v, transcript %q; want both still collected", res.DeliveredRefs, res.Transcript)
	}
}

// A BOX THAT COULD NOT BE READ IS AN ERROR, not a smaller result: that is the
// box failing, which the coordinator answers, and it is not a piece that was
// too large to read.
func TestAnUnreadableBoxIsAnErrorNotAPiece(t *testing.T) {
	t.Parallel()
	runner := executor.NewOpenCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Findings(), "Outcome: succeeded")
	for _, path := range []string{p.Result(), p.Err(), p.ExitCode(), p.Findings()} {
		b.ReadErr = func(read string) error {
			if read == path {
				return errBoxUnreadable
			}
			return nil
		}
		if _, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil)); !errors.Is(err, errBoxUnreadable) {
			t.Errorf("an unreadable %s collected with %v; want the read's error", path, err)
		}
	}
}

func tailOf(s string, n int) string { return s[max(0, len(s)-n):] }

// testFailureBound and testLineBound are the bounds the cases that go past
// one run their runner at ([executor.Bounded]): 64 KiB each, room for every
// sentence a failure opens with and a stream behind them.
const (
	testFailureBound = 64 << 10
	testLineBound    = 64 << 10
)
