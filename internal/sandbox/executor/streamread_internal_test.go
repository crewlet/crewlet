package executor

import (
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// openCodeLines is an OpenCode run's event stream: n lines of its answer,
// then the line that ends the run.
func openCodeLines(n int) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(`{"type":"text","part":{"messageID":"msg_1","text":"step ` + strings.Repeat("x", 80) + ` ` +
			string(rune('a'+i%26)) + `"}}` + "\n")
	}
	return b.String() + `{"type":"step_finish","part":{"messageID":"msg_1","reason":"stop"}}` + "\n"
}

// steppingClock is a runner's clock that moves step at every reading.
func steppingClock(step time.Duration) func() time.Time {
	var now time.Time
	return func() time.Time {
		now = now.Add(step)
		return now
	}
}

// AN EVENT STREAM WHOSE READ RUNS PAST ITS BOUND SHOWS NO TRANSCRIPT, saying
// how long it had and how much it read ([sandbox.RunHandle.StreamsReadBy]),
// and is counted. A run whose result is a file read whole (Claude Code's)
// keeps the outcome that file gives; one whose result is its stream reads as
// stopped before saying how its run ended, its failure saying the read ran out
// of time, its partial answer not taken for its report.
//
// Mutations, each red: read on past the bound; show the transcript read so
// far; keep the partial answer as the report; turn a run its result file says
// succeeded into a failure; leave the count out.
func TestAnEventStreamPastItsBoundShowsNoTranscript(t *testing.T) {
	t.Parallel()
	const note = "the run's transcript is not shown: the collection's read of its event stream ran past the"
	for _, c := range []struct {
		name    string
		runner  func() *Runner
		box     func(b *sandbox.FakeSandbox, p Paths)
		success bool
	}{
		{"a result read whole", NewClaudeCode, func(b *sandbox.FakeSandbox, p Paths) {
			line := `{"type":"assistant","message":{"content":[{"type":"text","text":"` + strings.Repeat("y", 900) +
				`"}]}}` + "\n"
			b.Put(p.Stream(), strings.Repeat(line, 400)+
				`{"type":"result","subtype":"success","is_error":false,"result":"Built it."}`+"\n")
			b.Put(p.Result(), `{"type":"result","subtype":"success","is_error":false,"result":"Built it."}`)
			b.Put(p.ExitCode(), "0")
		}, true},
		{"a result its stream's", NewOpenCode, func(b *sandbox.FakeSandbox, p Paths) {
			b.Put(p.Result(), openCodeLines(4000))
			b.Put(p.ExitCode(), "0")
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runner := c.runner()
			runner.now = steppingClock(time.Second)
			b := sandbox.NewFakeSandbox("box-1")
			p := PathsFor(b)
			c.box(b, p)
			meter := &withholdings{}
			res, err := runner.Collect(sandbox.CountingWithheld(t.Context(), meter), b,
				sandbox.RunHandle{StreamsReadBy: time.Time{}.Add(3 * time.Second)}, queue.NewAnswerBudget(nil))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			if !strings.Contains(res.Transcript, note) || strings.Contains(res.Transcript, "yyyy") ||
				strings.Contains(res.Transcript, "step x") {
				t.Errorf("the transcript is %.200q; want none of the stream, and %q", res.Transcript, note)
			}
			if !slices.Contains(meter.all(), sandbox.Withholding{Piece: sandbox.WithheldEventStream,
				Reason: sandbox.WithheldPastBound}) {
				t.Errorf("the collection counted %v, want the event stream past its bound", meter.all())
			}
			switch {
			case c.success && (!res.Success || res.Text != "Built it."):
				t.Errorf("a run its result file says succeeded collected as success %v with %q (%q)", res.Success,
					res.Text, res.Error)
			case !c.success && (res.Success || strings.Contains(res.Text, "step x") ||
				!strings.Contains(res.Error, "ran past the") ||
				!strings.Contains(res.Error, "before the stream said how the run ended")):
				t.Errorf("a run whose result is its cut stream collected as success %v, text %.60q, failure %.300q; "+
					"want stopped before saying how it ended, saying why", res.Success, res.Text, res.Error)
			}
		})
	}
}

// AN EVENT STREAM IS READ AS ONE VERSION OF IT: one still being written when
// its read reaches the size its open found is decoded to its last whole line,
// its unended last line never a line, and its transcript says the stream was
// still being written; one cut or written over meanwhile is refused as a
// piece, the run no success. A local collection under no bound reads it whole.
//
// Mutations, each red: hand on the unended last line of a stream still being
// written; leave the note out; take a cut stream for whole.
func TestAnEventStreamIsReadAsOneVersionOfIt(t *testing.T) {
	t.Parallel()
	whole := openCodeLines(20)
	for _, c := range []struct {
		name  string
		edit  func(b *sandbox.FakeSandbox, path string)
		check func(t *testing.T, res sandbox.Result)
	}{
		{"untouched", func(*sandbox.FakeSandbox, string) {}, func(t *testing.T, res sandbox.Result) {
			if !res.Success || strings.Contains(res.Transcript, "still being written") {
				t.Errorf("an untouched stream collected as success %v, transcript %.120q", res.Success, res.Transcript)
			}
		}},
		{"still being written", func(b *sandbox.FakeSandbox, path string) {
			b.AppendInPlace(path, []byte(`{"type":"text","part":{"messageID":"msg_2","text":"lat`))
		}, func(t *testing.T, res sandbox.Result) {
			if !strings.Contains(res.Transcript, "the run's event stream was still being written") ||
				strings.Contains(res.Transcript, "lat") {
				t.Errorf("a stream still being written left the transcript %.300q; want its whole lines, "+
					"saying so", res.Transcript)
			}
		}},
		{"cut while read", func(b *sandbox.FakeSandbox, path string) {
			b.TruncateInPlace(path, len(whole)/2)
		}, func(t *testing.T, res sandbox.Result) {
			if res.Success || !strings.Contains(res.Error, "changed while the collection read it") {
				t.Errorf("a stream cut while read collected as success %v with %.300q; want it refused",
					res.Success, res.Error)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			runner := NewOpenCode()
			b := sandbox.NewFakeSandbox("box-1")
			p := PathsFor(b)
			b.Put(p.Result(), whole)
			b.Put(p.ExitCode(), "0")
			b.Streams = func(path string, r io.Reader) io.Reader {
				if path == p.Result() {
					c.edit(b, path)
				}
				return r
			}
			res, err := runner.Collect(t.Context(), b, sandbox.RunHandle{}, queue.NewAnswerBudget(nil))
			if err != nil {
				t.Fatalf("Collect: %v", err)
			}
			c.check(t, res)
		})
	}
}

// EVERY RUNNER'S EVENT DECODER ENDS ITS TRANSCRIPT WHERE A STREAM STILL BEING
// WRITTEN SETTLED ([streamUnended]), so no runner's transcript takes the size
// its read found for its stream's end.
//
// Mutation: leave a runner's decoder out.
func TestEveryRunnersDecoderEndsItsTranscriptWhereItsStreamSettled(t *testing.T) {
	t.Parallel()
	for _, h := range Registered() {
		if _, ok := h.Events().(streamUnended); !ok {
			t.Errorf("%s's event decoder does not end its transcript where its stream settled", h.Name())
		}
	}
}

// A TRANSCRIPT OF A STREAM STILL BEING WRITTEN KEEPS ONLY WHAT ITS REDACTION
// SETTLED, to its last whole line: held, a line ending in a password's key,
// whose value a later line may hold, is left out with all after it; streamed
// past its hold with nothing its redaction made final, a note stands for it.
//
// Mutation: take the transcript as though its stream had ended.
func TestATranscriptOfAStreamStillBeingWrittenKeepsWhatSettled(t *testing.T) {
	t.Parallel()
	entries := []string{"compiling the build", "linking the build", "the deploy's password:"}
	for hold, want := range map[int]string{
		1 << 20: "compiling the build\nlinking the build",
		16:      "is not kept: its event stream was still being written",
	} {
		tr := transcriptLines{hold: hold}
		for _, e := range entries {
			tr.add(e)
		}
		tr.unended = true
		if text, _, _ := tr.result(); hold == 16 && !strings.Contains(text, want) || hold != 16 && text != want {
			t.Errorf("at a hold of %d the transcript is %q; want %q", hold, text, want)
		}
	}
}
