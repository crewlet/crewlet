package codingagent_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// What these protect: A RUN IS NEVER LOST TO THE SIZE OF ITS OWN LOG.
//
// A coding agent's stdout event stream and its stderr grow with the run, and
// every path used to read them whole and refuse them past 32 MiB. Each case
// below is a run the engine used to wedge or destroy for that alone. The fake
// refuses a whole read past the cap exactly as every real backend does, so a
// case that still read a stream whole would fail here rather than pass.

// bigToolStream is an OpenCode stream past the whole-read cap: tool events
// whose state carries each tool's whole output, which is what makes a real
// stream grow, ending with the events given.
func bigToolStream(tail ...string) string {
	output := strings.Repeat("o", 1<<20)
	var b strings.Builder
	for b.Len() <= sandbox.MaxFileBytes {
		b.WriteString(`{"type":"tool_use","part":{"tool":"read","state":{"status":"completed",` +
			`"input":{"filePath":"/home/user/workspace/big.log"},"output":"` + output + `"}}}` + "\n")
	}
	for _, line := range tail {
		b.WriteString(line + "\n")
	}
	return b.String()
}

// errBoxUnreadable stands in for a box whose file could not be read back.
var errBoxUnreadable = errors.New("envd: connection reset")

// A DEAD JOB WITH A STREAM PAST THE CAP IS DONE. The poll read the stream whole
// before it asked whether the job was alive, so the refusal returned first:
// the waiter read the error as "still running", refreshed the keepalive and
// kept the seat busy, every tick, for a job whose process group was gone.
func TestADeadJobWithAStreamPastTheCapIsDone(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.Put(paths(b).Result(), bigToolStream(`{"type":"text","part":{"text":"still going"}}`))
	b.ExecFunc = alive(false)

	done, err := runner.Poll(t.Context(), b, handle)
	if err != nil || !done {
		t.Fatalf("Poll = %v, %v; want the dead job found done whatever its stream's size", done, err)
	}
}

// A FINISHED-BUT-HUNG JOB WITH A STREAM PAST THE CAP IS DONE. Its terminal
// event is the stream's last line, which is all the poll reads.
func TestAFinishedButHungJobWithAStreamPastTheCapIsDone(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.Put(paths(b).Result(), bigToolStream(
		`{"type":"text","part":{"text":"Fixed it."}}`,
		`{"type":"step_finish","part":{"reason":"stop"}}`))
	b.ExecFunc = alive(true) // hung: the wrapper never returns

	done, err := runner.Poll(t.Context(), b, handle)
	if err != nil || !done {
		t.Fatalf("Poll = %v, %v; want the terminal event at the stream's end found", done, err)
	}
}

// A READ THAT FAILS NEVER SKIPS THE PROBE: a dead job is done however its box
// answered the read, and a live one is not done — with the read's error still
// reported beside the answer.
func TestAReadThatFailsNeverSkipsTheLivenessProbe(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	handle := start(t, runner, b)
	b.ReadErr = func(string) error { return errBoxUnreadable }

	b.ExecFunc = alive(false)
	if done, err := runner.Poll(t.Context(), b, handle); !done || err != nil {
		t.Errorf("a dead job behind an unreadable box polled %v, %v; want done", done, err)
	}
	b.ExecFunc = alive(true)
	if done, err := runner.Poll(t.Context(), b, handle); done || !errors.Is(err, errBoxUnreadable) {
		t.Errorf("a live job behind an unreadable box polled %v, %v; want not done and the read's error", done, err)
	}
}

// A CLI THAT EXITS CLEANLY HAS NO STREAM TO POLL. The done marker is its only
// signal, and reading its output on every tick was a transfer per box per
// tick that could never change the answer.
func TestACLIThatExitsCleanlyIsNotPolledThroughItsOutput(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewClaudeCode()
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
	if done, err := runner.Poll(t.Context(), b, handle); done || err != nil {
		t.Errorf("Poll = %v, %v; want only the marker and the probe asked", done, err)
	}
}

// A STREAM PAST THE CAP IS COLLECTED. The collection read it whole and was
// refused, which settled the run as unreachable and destroyed its turn —
// although the report file, the result of record, was a few lines long.
func TestACollectionReadsAStreamPastTheCap(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Result(), bigToolStream(
		`{"type":"tool_use","part":{"tool":"bash","state":{"status":"completed","input":{"command":"go test ./..."}}}}`,
		`{"type":"text","part":{"text":"All green."}}`,
		`{"type":"step_finish","part":{"reason":"stop"}}`))
	b.Put(p.Findings(), "Outcome: succeeded\nOpened https://github.com/acme/api/pull/12")
	b.Put(p.ExitCode(), "0")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !res.Success || !strings.HasPrefix(res.Text, "Outcome: succeeded") {
		t.Errorf("success %v, report %.40q; want the report collected", res.Success, res.Text)
	}
	if !strings.Contains(res.Transcript, "[tool] bash: go test ./...") || !strings.Contains(res.Transcript, "All green.") {
		t.Errorf("the transcript lost the stream's end: …%q", tailOf(res.Transcript, 200))
	}
	if strings.Contains(res.Transcript, "ooooooooo") {
		t.Error("a tool's output reached the transcript; only what ran belongs there")
	}
}

// AN OVER-LONG LINE IS SKIPPED AND SAID, never held: one line is read in one
// piece, so a line past what one piece may be is counted where it was, and
// the events around it are still read.
func TestALinePastTheBoundIsSkippedAndSaid(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	p := paths(b)
	huge := `{"type":"tool_use","part":{"tool":"read","state":{"output":"` +
		strings.Repeat("x", sandbox.MaxFileBytes) + `"}}}`
	b.Put(p.Result(), strings.Join([]string{
		`{"type":"tool_use","part":{"tool":"bash","state":{"input":{"command":"make build"}}}}`,
		huge,
		`{"type":"text","part":{"text":"Built it."}}`,
	}, "\n"))

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !strings.Contains(res.Transcript, "1 line(s) of output") || !strings.Contains(res.Transcript, "not read") {
		t.Errorf("the skipped line is not said: %q", res.Transcript)
	}
	before := strings.Index(res.Transcript, "make build")
	note := strings.Index(res.Transcript, "1 line(s) of output")
	after := strings.Index(res.Transcript, "Built it.")
	if before < 0 || after < 0 || before > note || note > after {
		t.Errorf("the note is not where the line was: %q", res.Transcript)
	}
	if res.Text != "Built it." {
		t.Errorf("the answer around the skipped line = %q", res.Text)
	}
}

// THE ERROR STREAM IS READ FROM ITS END, at most what a condensation can take,
// and what was not read is said by size. It reaches the resumed executor as
// the failure's detail, so its unread start is marked rather than silent; and
// its end — the line naming what broke — is always there.
func TestTheErrorStreamIsReadFromItsEndAndItsStartIsSaid(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	stderr := strings.Repeat("noise line\n", 5<<20/11) + "FATAL: migrations/0007.sql is missing"
	b.Put(p.Err(), stderr)
	b.Put(p.ExitCode(), "1")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !strings.HasSuffix(res.Error, "FATAL: migrations/0007.sql is missing") {
		t.Errorf("the failure lost the error stream's last line: …%q", tailOf(res.Error, 120))
	}
	if !strings.Contains(res.Error, "were not read: a run's failure is read from its end") {
		t.Errorf("the unread start is not said: %.300q", res.Error)
	}
	if len(res.Error) > 2<<20+1024 {
		t.Errorf("the failure is %d bytes, past what a condensation can take", len(res.Error))
	}
}

// A REPORT PAST THE CAP DEGRADES ONLY ITSELF. It is described by its size and
// the run reads as not succeeded with that as the reason — but the run's
// tokens, refs and transcript are still there to be charged and published. A
// refusal used to fail the whole collection, which took the charge and the
// record with it.
func TestAReportPastTheCapDegradesOnlyItself(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Result(), `{"type":"result","result":"Opened https://github.com/acme/api/pull/3","subtype":"success",`+
		`"usage":{"input_tokens":100,"output_tokens":50}}`)
	b.Put(p.Findings(), strings.Repeat("r", sandbox.MaxFileBytes+1))
	b.Put(p.Err(), "a warning")
	b.Put(p.ExitCode(), "0")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v; want the refusal to stay inside its piece", err)
	}
	if res.Success {
		t.Error("a run whose report could not be read reads as a success")
	}
	if !strings.Contains(res.Error, "the report the coding agent wrote") || !strings.Contains(res.Error, "32.0 MiB") {
		t.Errorf("the refusal is not described by size: %q", res.Error)
	}
	if res.InputTokens != 100 || res.OutputTokens != 50 {
		t.Errorf("tokens %d/%d; want the run's spend still collected", res.InputTokens, res.OutputTokens)
	}
	if len(res.DeliveredRefs) != 1 || res.Transcript != "a warning" {
		t.Errorf("refs %v, transcript %q; want both still collected", res.DeliveredRefs, res.Transcript)
	}
}

// A QUESTION PAST THE CAP IS NOT PARKED ON. Nobody could be shown it, so the
// run does not wait for an answer to it; it reads as not succeeded, saying so.
func TestAQuestionPastTheCapIsNotParkedOn(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewClaudeCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Findings(), "Outcome: blocked")
	b.Put(p.Ask(), `{"question":"`+strings.Repeat("q", sandbox.MaxFileBytes)+`","to":"team"}`)
	b.Put(p.ExitCode(), "0")

	res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.NeedsInput || res.Success {
		t.Errorf("needs input %v, success %v; want neither", res.NeedsInput, res.Success)
	}
	if !strings.Contains(res.Error, "the question the coding agent recorded") {
		t.Errorf("the refusal is not said: %q", res.Error)
	}
	if res.Text != "Outcome: blocked" {
		t.Errorf("the report = %q; want it still collected", res.Text)
	}
}

// A BOX THAT COULD NOT BE READ IS AN ERROR, not a smaller result: that is the
// box failing, which the coordinator answers, and it is not a piece that was
// too large to read.
func TestAnUnreadableBoxIsAnErrorNotAPiece(t *testing.T) {
	t.Parallel()
	runner := codingagent.NewOpenCode()
	b := box(t, runner)
	p := paths(b)
	b.Put(p.Findings(), "Outcome: succeeded")
	for _, path := range []string{p.Result(), p.Err(), p.ExitCode(), p.Findings(), p.Ask()} {
		b.ReadErr = func(read string) error {
			if read == path {
				return errBoxUnreadable
			}
			return nil
		}
		if _, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}); !errors.Is(err, errBoxUnreadable) {
			t.Errorf("an unreadable %s collected with %v; want the read's error", path, err)
		}
	}
}

func tailOf(s string, n int) string { return s[max(0, len(s)-n):] }
