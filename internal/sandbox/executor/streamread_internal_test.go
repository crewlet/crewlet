package executor

import (
	"io"
	"strings"
	"testing"

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

// AN EVENT STREAM IS READ AS ONE VERSION OF IT: one still being written when
// its read reaches the size its open found is decoded to its last whole line,
// its unended last line never a line, and its transcript keeps only what its
// redaction settled, so a password's key a later line may answer is left out,
// saying the stream was still being written; one cut or written over
// meanwhile is refused as a piece, the run no success. A local collection
// under no bound reads it whole.
//
// Mutations, each red: hand on the unended last line of a stream still being
// written; end its transcript as though it had ended; leave the note out;
// take a cut stream for whole.
func TestAnEventStreamIsReadAsOneVersionOfIt(t *testing.T) {
	t.Parallel()
	// Its answer's last line a password's key, whose value a line written
	// later may hold: no transcript of a stream still being written shows it.
	const key = "the deploy's password:"
	lines := openCodeLines(20)
	whole := lines[:strings.LastIndexByte(lines[:len(lines)-1], '\n')+1] +
		`{"type":"text","part":{"messageID":"msg_1","text":"` + key + `"}}` + "\n" +
		lines[strings.LastIndexByte(lines[:len(lines)-1], '\n')+1:]
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
			// Opened with its last line, the run's end, not yet broken
			// (unbroken below): the break comes after the open, so that
			// line is not read as one.
			b.AppendInPlace(path, []byte("\n"+`{"type":"text","part":{"messageID":"msg_2","text":"later"}}`))
		}, func(t *testing.T, res sandbox.Result) {
			if !strings.Contains(res.Transcript, "the run's event stream was still being written") ||
				strings.Contains(res.Transcript, "later") || strings.Contains(res.Transcript, key) || res.Success {
				t.Errorf("a stream still being written collected as success %v, its transcript %.300q; want its "+
					"settled whole lines, saying so, and its unbroken last line, the run's end, not read",
					res.Success, res.Transcript)
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
			opened := whole
			if c.name == "still being written" {
				opened = strings.TrimSuffix(whole, "\n")
			}
			b.Put(p.Result(), opened)
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
