package sandbox

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
)

// envdFrames writes each event to w as one Connect frame, then closes w.
func envdFrames(w *io.PipeWriter, events func(yield func(any) bool)) {
	for event := range events {
		body, err := json.Marshal(event)
		if err != nil {
			w.CloseWithError(err)
			return
		}
		frame := make([]byte, connectEnvelope+len(body))
		binary.BigEndian.PutUint32(frame[1:connectEnvelope], uint32(len(body)))
		copy(frame[connectEnvelope:], body)
		if _, err := w.Write(frame); err != nil {
			return
		}
	}
	w.Close()
}

// foldFrames is what foldStream folds the events into.
func foldFrames(t *testing.T, events func(yield func(any) bool)) e2bProcessResult {
	t.Helper()
	r, w := io.Pipe()
	go envdFrames(w, events)
	out, err := foldStream(t.Context(), r, false)
	r.CloseWithError(io.ErrClosedPipe)
	if err != nil {
		t.Fatalf("foldStream: %v", err)
	}
	return out
}

func envdData(stdout, stderr []byte) any {
	return map[string]any{"event": map[string]any{"data": map[string]any{"stdout": stdout, "stderr": stderr}}}
}

func envdEnd(code int, message string) any {
	return map[string]any{"event": map[string]any{"end": map[string]any{"exitCode": code, "error": message}}}
}

// AN E2B COMMAND'S OUTPUT IS KEPT AS EVERY BOX KEEPS IT: over 64 MiB of
// output frames, foldStream answers for each stream what a [capture] keeps
// of the same bytes, as a direct and a container box's commands do; and the
// end event's error stands for the error stream only where the command wrote
// none.
//
// Mutation: fold the frames into a strings.Builder, and the output is the
// whole 64 MiB.
func TestAnE2BCommandsOutputIsKeptAsEveryBoxKeepsIt(t *testing.T) {
	t.Parallel()
	var stdout, stderr capture
	out := foldFrames(t, func(yield func(any) bool) {
		piece := make([]byte, 0, 1<<20)
		for i := range 64 {
			piece = piece[:0]
			for n := 0; len(piece) < 1<<20-32; n++ {
				piece = fmt.Appendf(piece, "frame %d line %d\n", i, n)
			}
			errs := fmt.Appendf(nil, "warning %d\n", i)
			stdout.Write(piece)
			stderr.Write(errs)
			if !yield(envdData(piece, errs)) {
				return
			}
		}
		yield(envdEnd(1, "exit status 1"))
	})
	if !out.Ended || out.ExitCode != 1 {
		t.Fatalf("the command's end was read as ended %t, exit code %d", out.Ended, out.ExitCode)
	}
	if want := stdout.String(); out.Stdout != want {
		t.Errorf("stdout is %d bytes, ending …%q; a capture of the same output keeps %d", len(out.Stdout),
			out.Stdout[max(0, len(out.Stdout)-120):], len(want))
	}
	if want := stderr.String(); out.Stderr != want {
		t.Errorf("stderr is %d bytes, ending …%q; a capture of the same output keeps %d", len(out.Stderr),
			out.Stderr[max(0, len(out.Stderr)-120):], len(want))
	}
	if !strings.Contains(out.Stdout, "more bytes of output not kept") {
		t.Error("64 MiB of stdout was cut silently")
	}

	quiet := foldFrames(t, func(yield func(any) bool) { yield(envdEnd(1, "exit status 1")) })
	if quiet.Stderr != "exit status 1" {
		t.Errorf("a command that wrote no error stream: stderr %q, want the end event's error", quiet.Stderr)
	}
}
